package main

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/nexusriot/beastiemon/internal/alert"
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
		status, code := evalCheck(c.val, c.warn, c.crit, false)
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
		val, _, ok := checkValue(snap, c.metric, "")
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
	if val, label, ok := checkValue(snap, "net", ""); !ok || val != 200 || label != "net.total_bps" {
		t.Fatalf("net: want 200 net.total_bps, got %v %q %v", val, label, ok)
	}
	if val, label, ok := checkValue(snap, "disk", ""); !ok || val != 2000 || label != "disk.total_bps" {
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

// Every command the usage text advertises as a metric view must be accepted;
// anything else falls through to the "unknown command" exit-2 path.
func TestMetricCommands(t *testing.T) {
	for _, c := range []string{"status", "cpu", "mem", "net", "disk", "fs", "temp", "proc", "load"} {
		if !metricCommands[c] {
			t.Errorf("command %q should be recognised", c)
		}
	}
	for _, c := range []string{"", "frobnicate", "top", "check", "version", "help", "CPU"} {
		if metricCommands[c] {
			t.Errorf("command %q should not be in metricCommands", c)
		}
	}
}

// An array metric with no entries at all — no sensors loaded, no matching
// mount, no interfaces — must not come back as a green 0.00. Both README and
// DESIGN.md promise UNKNOWN over a false "OK: … = 0.00", and the alert engine
// already reports "unknown" for exactly this case.
func TestCheckValueEmptyMetricsAreUnknown(t *testing.T) {
	empty := collect.Snapshot{CPU: collect.CPUStats{Total: 3}}
	for _, metric := range []string{"fs", "temp", "net", "disk"} {
		val, label, ok := checkValue(empty, metric, "")
		if ok {
			t.Errorf("checkValue(%s) on an empty snapshot returned ok with %v", metric, val)
		}
		// A label is still returned so runCheck can say which metric had
		// nothing to read, rather than claiming the metric name was a typo.
		if label == "" {
			t.Errorf("checkValue(%s): want a label to distinguish this from an unknown metric", metric)
		}
	}
	// An unknown metric stays label-free, which is how runCheck tells them apart.
	if _, label, ok := checkValue(empty, "bogus", ""); ok || label != "" {
		t.Errorf(`checkValue("bogus") = %q, %v; want "", false`, label, ok)
	}
	// Scalar metrics are always readable and must keep reporting.
	if val, _, ok := checkValue(empty, "cpu", ""); !ok || val != 3 {
		t.Errorf("checkValue(cpu) = %v, %v; want 3, true", val, ok)
	}
}

// Negative or NaN percentages reach bar() whenever a collector can't read a
// total (a zero-sized filesystem makes gopsutil hand back NaN), and
// strings.Repeat panics on a negative count.
func TestBarClampsOutOfRangePercent(t *testing.T) {
	for _, pct := range []float64{math.NaN(), -5, -0.0001, 0, 50, 100, 150, math.Inf(1), math.Inf(-1)} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("bar(%v) panicked: %v", pct, r)
				}
			}()
			bar(pct, 20)
		}()
	}
}

// `top` repaints at the interval only in remote mode; local sampling pays a
// delta warm-up per frame, so the footer must not advertise the bare interval.
func TestTopCadence(t *testing.T) {
	if got := topCadence(time.Second, false); got != 2*time.Second {
		t.Errorf("local cadence = %v, want 2s", got)
	}
	if got := topCadence(time.Second, true); got != time.Second {
		t.Errorf("remote cadence = %v, want 1s", got)
	}
}

// --below flips the comparison for metrics where small is bad. The two
// directions are exact complements, so each case here also pins down that
// the opposite direction would have answered differently.
func TestEvalCheckBelow(t *testing.T) {
	cases := []struct {
		name            string
		val, warn, crit float64
		below           bool
		status          string
		code            int
	}{
		{"service down", 0, 0, 1, true, "CRITICAL", 2},
		{"service up", 1, 0, 1, true, "OK", 0},
		{"two running, want two", 2, 0, 2, true, "OK", 0},
		{"one running, want two", 1, 0, 2, true, "CRITICAL", 2},
		{"warn band", 3, 5, 2, true, "WARNING", 1},
		{"free space ok", 40, 20, 10, true, "OK", 0},
		{"free space low", 15, 20, 10, true, "WARNING", 1},
		// Without --below the same numbers read the other way round.
		{"same numbers, default direction", 0, 0, 1, false, "OK", 0},
	}
	for _, c := range cases {
		status, code := evalCheck(c.val, c.warn, c.crit, c.below)
		if status != c.status || code != c.code {
			t.Errorf("%s: evalCheck(%v,%v,%v,below=%v) = %s/%d, want %s/%d",
				c.name, c.val, c.warn, c.crit, c.below, status, code, c.status, c.code)
		}
	}
}

// --field routes through the alert engine's metric table, so a check and a
// [[alerts.rule]] written the same way measure the same thing.
func TestCheckValueWithField(t *testing.T) {
	snap := collect.Snapshot{
		CPU: collect.CPUStats{Total: 42, User: 30},
		FS: []collect.FSStats{
			{Mount: "/", UsedPct: 30}, {Mount: "/var", UsedPct: 88},
		},
		Net:   []collect.NetStats{{Interface: "em0", RxBps: 100, TxBps: 50, RxErr: 7}},
		ZFS:   []collect.ZFSStats{{Pool: "tank", UsedPct: 70, Health: "DEGRADED"}},
		Watch: []collect.ProcWatch{{Name: "sshd", Count: 0}},
	}
	cases := []struct {
		metric, field string
		want          float64
		label         string
	}{
		// Defaults keep the labels the command has always printed.
		{"cpu", "", 42, "cpu.total%"},
		{"fs", "", 88, "fs.max_used%"},
		{"net", "", 150, "net.total_bps"},
		// An explicit selector names itself.
		{"cpu", "user", 30, "cpu.user"},
		{"fs", "/var", 88, "fs./var"},
		{"fs", "/", 30, "fs./"},
		{"net", "rx_err", 7, "net.rx_err"},
		{"zfs", "health", 1, "zfs.health"},
		{"proc", "sshd", 0, "proc.sshd"},
	}
	for _, c := range cases {
		val, label, ok := checkValue(snap, c.metric, c.field)
		if !ok {
			t.Errorf("%s --field %q: not readable", c.metric, c.field)
			continue
		}
		if val != c.want || label != c.label {
			t.Errorf("%s --field %q = %v/%q, want %v/%q",
				c.metric, c.field, val, label, c.want, c.label)
		}
	}
}

// An unknown field and an unreadable one are different problems with
// different fixes, so checkValue has to let runCheck tell them apart: a
// typo returns no label, missing data keeps one.
func TestCheckValueDistinguishesTypoFromMissingData(t *testing.T) {
	snap := collect.Snapshot{CPU: collect.CPUStats{Total: 1}}

	if _, label, ok := checkValue(snap, "cpu", "totl"); ok || label != "" {
		t.Errorf(`cpu --field totl = %q, %v; want "", false (a typo)`, label, ok)
	}
	// A mount that isn't mounted is a real selector against absent data.
	if _, label, ok := checkValue(snap, "fs", "/var"); ok || label != "fs./var" {
		t.Errorf(`fs --field /var = %q, %v; want "fs./var", false (missing data)`, label, ok)
	}
	// proc has no meaningful default, so no field at all is a usage error.
	if _, label, ok := checkValue(snap, "proc", ""); ok || label != "" {
		t.Errorf(`proc with no field = %q, %v; want "", false`, label, ok)
	}
}

// Every metric the alert engine knows must be checkable, or `beastie check`
// and the rule table would advertise different vocabularies.
func TestCheckDefaultsCoverEveryAlertMetric(t *testing.T) {
	for _, m := range alert.Metrics() {
		if _, ok := checkDefaults[m]; !ok {
			t.Errorf("metric %q is alertable but has no check default", m)
		}
	}
	for m := range checkDefaults {
		if !slices.Contains(alert.Metrics(), m) {
			t.Errorf("check offers metric %q that the alert engine doesn't know", m)
		}
	}
}

// A snapshot that could not be taken must not be printed as if it were real.
// Before this, an unreachable --remote daemon still produced a full (all-zero)
// render and exit 0 — and under --json a zero-filled object on stdout — so a
// script could not tell a dead daemon from an idle host. check mode already
// reported UNKNOWN/3 for the same failure; the plain commands now exit 1.
func TestMustSnapExitsWhenCollectionFails(t *testing.T) {
	restore := swapExit(t)
	defer restore()

	failing := func() (collect.Snapshot, error) {
		return collect.Snapshot{}, errors.New("remote 127.0.0.1:9999: connection refused")
	}
	mustSnap(failing)
	if gotExit == nil {
		t.Fatal("a failed collection must exit, not print zeros and succeed")
	}
	if *gotExit != 1 {
		t.Errorf("exit code = %d, want 1", *gotExit)
	}

	gotExit = nil
	working := func() (collect.Snapshot, error) {
		return collect.Snapshot{CPU: collect.CPUStats{Total: 12.5}}, nil
	}
	if snap := mustSnap(working); snap.CPU.Total != 12.5 {
		t.Errorf("a good snapshot should pass through, got %+v", snap.CPU)
	}
	if gotExit != nil {
		t.Errorf("a successful collection must not exit (got %d)", *gotExit)
	}
}

// `top` is the one view where a failed sample should cost a frame, not the
// session: it reports the error and tells the caller to skip the repaint
// rather than drawing a screen of zeros.
func TestPollSnapSkipsFrameWithoutExiting(t *testing.T) {
	restore := swapExit(t)
	defer restore()

	failing := func() (collect.Snapshot, error) {
		return collect.Snapshot{}, errors.New("sample timed out")
	}
	if _, ok := pollSnap(failing); ok {
		t.Error("a failed sample must report ok=false so the frame is skipped")
	}
	if gotExit != nil {
		t.Errorf("top must not exit on one bad sample (got %d)", *gotExit)
	}

	working := func() (collect.Snapshot, error) {
		return collect.Snapshot{CPU: collect.CPUStats{Total: 7}}, nil
	}
	snap, ok := pollSnap(working)
	if !ok || snap.CPU.Total != 7 {
		t.Errorf("a good sample should repaint: ok=%v snap=%+v", ok, snap.CPU)
	}
}

// gotExit records the status passed to osExit while swapExit is in force.
var gotExit *int

// swapExit redirects osExit into gotExit and silences stderr, so the tests can
// observe the exit status of paths that would otherwise end the test binary.
func swapExit(t *testing.T) func() {
	t.Helper()
	realExit, realErr := osExit, os.Stderr
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	os.Stderr = devnull
	gotExit = nil
	osExit = func(code int) { gotExit = &code }
	return func() {
		osExit, os.Stderr = realExit, realErr
		devnull.Close()
		gotExit = nil
	}
}

// server.listen is a bind address, and the wildcard forms name every local
// interface rather than a peer: connecting to 0.0.0.0 fails outright on
// FreeBSD. Since README's "Exposing on the LAN" section tells installs to bind
// "0.0.0.0:8088", --remote auto has to turn that into loopback or every
// documented `beastie --remote auto …` on a LAN host fails.
func TestRemoteAutoRewritesWildcardBind(t *testing.T) {
	cases := []struct {
		listen string
		want   string
	}{
		{"0.0.0.0:8088", "http://127.0.0.1:8088"},
		{":8088", "http://127.0.0.1:8088"},
		{"[::]:8088", "http://[::1]:8088"},
		// A concrete address is what the daemon actually answers on.
		{"127.0.0.1:8088", "http://127.0.0.1:8088"},
		{"192.168.1.10:9000", "http://192.168.1.10:9000"},
		{"monitor.local:8088", "http://monitor.local:8088"},
	}
	for _, c := range cases {
		cfg := config.Default()
		cfg.Server.Listen = c.listen
		_, base, err := remoteBase(cfg, "auto")
		if err != nil {
			t.Errorf("listen %q: unexpected error %v", c.listen, err)
			continue
		}
		if base != c.want {
			t.Errorf("listen %q: base = %q, want %q", c.listen, base, c.want)
		}
	}

	// A Unix socket listen path is already dialable and must not be rewritten.
	cfg := config.Default()
	cfg.Server.Listen = "/var/run/beastied/beastied.sock"
	if _, base, err := remoteBase(cfg, "auto"); err != nil || base != "http://beastied" {
		t.Errorf("socket listen: base = %q err = %v, want the socket transport", base, err)
	}
}

// The flag package stops parsing at the first non-flag argument, so a flag
// written after the command was dropped without a word: `beastie cpu --json`
// printed coloured text and exited 0, which a script piping into jq reads as
// valid output. Only `check` takes its own flags after the subcommand.
func TestFlagLikeArgs(t *testing.T) {
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"--json"}, []string{"--json"}},
		{[]string{"-json"}, []string{"-json"}},
		{[]string{"--no-color", "--json"}, []string{"--no-color", "--json"}},
		{[]string{"extra", "--json"}, []string{"--json"}},
		// Not flags: a bare dash, a negative number as a positional, nothing.
		{[]string{"-"}, nil},
		{[]string{}, nil},
		{nil, nil},
		{[]string{"something"}, nil},
	}
	for _, c := range cases {
		got := flagLikeArgs(c.args)
		if len(got) != len(c.want) {
			t.Errorf("flagLikeArgs(%q) = %q, want %q", c.args, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("flagLikeArgs(%q) = %q, want %q", c.args, got, c.want)
				break
			}
		}
	}
}

// Plain `beastie` (and `beastie --json`) passes no positional arguments at
// all, so flag.Args() is empty and the command defaults to status. Slicing
// that for trailing flags has to cope with the empty case — taking args[1:]
// blindly panicked on the most common invocation there is.
func TestTrailingArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"no command at all", []string{}, nil},
		{"nil args", nil, nil},
		{"command only", []string{"cpu"}, nil},
		{"command plus flag", []string{"cpu", "--json"}, []string{"--json"}},
		{"command plus several", []string{"cpu", "--json", "x"}, []string{"--json", "x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := trailingArgs(c.args)
			if len(got) != len(c.want) {
				t.Fatalf("trailingArgs(%q) = %q, want %q", c.args, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("trailingArgs(%q) = %q, want %q", c.args, got, c.want)
				}
			}
		})
	}

	// The two together are what main does, and must stay panic-free.
	for _, args := range [][]string{nil, {}, {"cpu"}, {"status", "--json"}} {
		if bad := flagLikeArgs(trailingArgs(args)); len(bad) > 1 {
			t.Errorf("unexpected flags from %q: %q", args, bad)
		}
	}
}
