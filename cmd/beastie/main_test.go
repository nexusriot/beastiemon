package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nexusriot/beastiemon/internal/collect"
	"github.com/nexusriot/beastiemon/internal/config"
)

func TestWantColor(t *testing.T) {
	cases := []struct {
		name       string
		isTTY      bool
		noColorEnv string
		noColorArg bool
		want       bool
	}{
		{"tty, nothing set", true, "", false, true},
		{"not a tty", false, "", false, false},
		{"NO_COLOR set", true, "1", false, false},
		{"--no-color", true, "", true, false},
		{"piped even without flags", false, "", false, false},
	}
	for _, c := range cases {
		if got := wantColor(c.isTTY, c.noColorEnv, c.noColorArg); got != c.want {
			t.Errorf("%s: wantColor=%v, want %v", c.name, got, c.want)
		}
	}
}

func TestEvalCheck(t *testing.T) {
	cases := []struct {
		val, warn, crit float64
		status          string
		code            int
	}{
		{42, 80, 90, "OK", 0},
		{85, 80, 90, "WARNING", 1},
		{95, 80, 90, "CRITICAL", 2},
		{95, 0, 0, "OK", 0},        // thresholds unset → always OK
		{90, 0, 90, "CRITICAL", 2}, // crit boundary is inclusive
	}
	for _, c := range cases {
		status, code := evalCheck(c.val, c.warn, c.crit)
		if status != c.status || code != c.code {
			t.Errorf("evalCheck(%v,%v,%v)=%s/%d, want %s/%d",
				c.val, c.warn, c.crit, status, code, c.status, c.code)
		}
	}
}

func TestCheckValue(t *testing.T) {
	snap := collect.Snapshot{
		CPU:   collect.CPUStats{Total: 42},
		Mem:   collect.MemStats{UsedPct: 61, SwapPct: 5},
		FS:    []collect.FSStats{{Mount: "/", UsedPct: 30}, {Mount: "/var", UsedPct: 88}},
		Temps: []collect.TempStat{{Name: "cpu0", Celsius: 50}, {Name: "cpu1", Celsius: 71}},
	}
	for _, c := range []struct {
		metric string
		want   float64
		ok     bool
	}{
		{"cpu", 42, true},
		{"mem", 61, true},
		{"swap", 5, true},
		{"fs", 88, true},   // max across mounts
		{"temp", 71, true}, // hottest sensor
		{"bogus", 0, false},
	} {
		val, _, ok := checkValue(snap, c.metric)
		if ok != c.ok || (ok && val != c.want) {
			t.Errorf("checkValue(%s)=%v,%v want %v,%v", c.metric, val, ok, c.want, c.ok)
		}
	}
}

func TestCollectTimeout(t *testing.T) {
	cases := []struct {
		interval, want time.Duration
	}{
		{time.Second, 5 * time.Second},       // floor: never below the old fixed bound
		{5 * time.Second, 12 * time.Second},  // must outlast the warm-up interval
		{30 * time.Second, 62 * time.Second}, // scales with large intervals
		{0, 5 * time.Second},                 // degenerate interval still bounded
	}
	for _, c := range cases {
		if got := collectTimeout(c.interval); got != c.want {
			t.Errorf("collectTimeout(%v) = %v, want %v", c.interval, got, c.want)
		}
	}
}

func TestCheckValueNetDisk(t *testing.T) {
	snap := collect.Snapshot{
		Net: []collect.NetStats{
			{Interface: "em0", RxBps: 100, TxBps: 50},
			{Interface: "em1", RxBps: 25, TxBps: 25},
		},
		Disk: []collect.DiskStats{
			{Device: "ada0", ReadBps: 1000, WriteBps: 500},
			{Device: "ada1", ReadBps: 200, WriteBps: 300},
		},
	}
	if val, label, ok := checkValue(snap, "net"); !ok || val != 200 || label != "net.total_bps" {
		t.Fatalf("net: want 200 net.total_bps, got %v %q %v", val, label, ok)
	}
	if val, label, ok := checkValue(snap, "disk"); !ok || val != 2000 || label != "disk.total_bps" {
		t.Fatalf("disk: want 2000 disk.total_bps, got %v %q %v", val, label, ok)
	}
}

func TestRemoteBase(t *testing.T) {
	cfg := config.Default()
	cases := []struct {
		target string
		base   string
	}{
		{"auto", "http://127.0.0.1:8088"}, // from config's server.listen default
		{"host:9999", "http://host:9999"},
		{"http://h:1/", "http://h:1"},
		{"https://h", "https://h"},
		{"/var/run/beastied.sock", "http://beastied"},
	}
	for _, c := range cases {
		_, base, err := remoteBase(cfg, c.target)
		if err != nil || base != c.base {
			t.Errorf("remoteBase(%q) = %q, %v; want %q", c.target, base, err, c.base)
		}
	}

	// auto with no listen address in config is an error.
	empty := config.Config{}
	if _, _, err := remoteBase(empty, "auto"); err == nil {
		t.Error("remoteBase(auto) with empty listen should fail")
	}
}

func TestFetchRemote(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/api/metrics" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(collect.Snapshot{
			Time: time.Now(),
			CPU:  collect.CPUStats{Total: 33},
		})
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.Auth.Token = "sekrit"
	snap, err := fetchRemote(cfg, srv.URL)
	if err != nil {
		t.Fatalf("fetchRemote: %v", err)
	}
	if snap.CPU.Total != 33 {
		t.Fatalf("want CPU total 33, got %v", snap.CPU.Total)
	}
	if gotAuth != "Bearer sekrit" {
		t.Fatalf("want bearer auth from config, got %q", gotAuth)
	}
}

func TestFetchRemoteNoData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no data yet", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	if _, err := fetchRemote(config.Default(), srv.URL); err == nil {
		t.Fatal("503 should surface as an error")
	}
}
