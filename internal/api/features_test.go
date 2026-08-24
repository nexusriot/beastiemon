package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nexusriot/beastiemon/internal/alert"
	"github.com/nexusriot/beastiemon/internal/collect"
	"github.com/nexusriot/beastiemon/internal/config"
)

func sampleSnap(t time.Time) collect.Snapshot {
	return collect.Snapshot{
		Time: t,
		CPU:  collect.CPUStats{Total: 42, User: 30, Sys: 12, Idle: 58},
		Mem:  collect.MemStats{Total: 1000, Used: 600, Free: 400, UsedPct: 60},
		Net:  []collect.NetStats{{Interface: "em0", RxBps: 100, TxBps: 50}},
		FS:   []collect.FSStats{{Mount: "/", UsedPct: 55}},
		Procs: []collect.ProcStat{
			{PID: 42, Name: "beastied", CPUPct: 7.5, MemPct: 1.2, RSS: 1024},
		},
	}
}

func TestPrometheusExporter(t *testing.T) {
	s := testServer(config.AuthConfig{})

	if rec := get(s, "/metrics", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no data: want 503, got %d", rec.Code)
	}

	s.Ingest(sampleSnap(time.Now()))
	rec := get(s, "/metrics", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("want text/plain content type, got %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`beastie_cpu_percent{mode="total"} 42`,
		`beastie_mem_used_percent 60`,
		`beastie_net_bps{iface="em0",dir="rx"} 100`,
		`beastie_fs_used_percent{mount="/"} 55`,
		`beastie_proc_cpu_percent{pid="42",name="beastied"} 7.5`,
		`# TYPE beastie_cpu_percent gauge`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q\n---\n%s", want, body)
		}
	}
}

func TestSeriesFS(t *testing.T) {
	s := testServer(config.AuthConfig{})
	s.Ingest(sampleSnap(time.Now()))

	rec := get(s, "/api/series?metric=fs&range=1h", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var resp struct {
		Labels []string    `json:"labels"`
		Data   [][]float64 `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Labels) != 2 || resp.Labels[1] != "/" {
		t.Fatalf("labels: want [ts /], got %v", resp.Labels)
	}
	if len(resp.Data) != 2 || len(resp.Data[1]) != 1 || resp.Data[1][0] != 55 {
		t.Fatalf("data: want mount series [55], got %v", resp.Data)
	}
}

func TestSeriesProc(t *testing.T) {
	s := testServer(config.AuthConfig{})
	s.Ingest(sampleSnap(time.Now()))

	rec := get(s, "/api/series?metric=proc&range=1h", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var resp struct {
		Labels []string    `json:"labels"`
		Data   [][]float64 `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Labels) != 2 || resp.Labels[1] != "42 beastied" {
		t.Fatalf("labels: want [ts, '42 beastied'], got %v", resp.Labels)
	}
	if resp.Data[1][0] != 7.5 {
		t.Fatalf("data: want cpu series [7.5], got %v", resp.Data[1])
	}
}

func TestSeriesBand(t *testing.T) {
	s := testServer(config.AuthConfig{})
	s.Ingest(sampleSnap(time.Now()))

	rec := get(s, "/api/series?metric=cpu&range=1h&band=1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var resp struct {
		Labels []string    `json:"labels"`
		Data   [][]float64 `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	idx := map[string]int{}
	for i, l := range resp.Labels {
		idx[l] = i
	}
	for _, want := range []string{"ts", "total", "total_min", "total_max"} {
		if _, ok := idx[want]; !ok {
			t.Fatalf("band labels missing %q: %v", want, resp.Labels)
		}
	}
	// Ring samples are full-resolution, so the envelope collapses onto the value.
	if resp.Data[idx["total"]][0] != 42 || resp.Data[idx["total_min"]][0] != 42 || resp.Data[idx["total_max"]][0] != 42 {
		t.Fatalf("ring band should equal the value 42, got %v/%v/%v",
			resp.Data[idx["total"]][0], resp.Data[idx["total_min"]][0], resp.Data[idx["total_max"]][0])
	}
}

func TestWebSocketStream(t *testing.T) {
	s := testServer(config.AuthConfig{})
	srv := httptest.NewServer(s)
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/ws"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))

	got := make(chan collect.Snapshot, 1)
	errc := make(chan error, 1)
	go func() {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			errc <- err
			return
		}
		var snap collect.Snapshot
		if err := json.Unmarshal(msg, &snap); err != nil {
			errc <- err
			return
		}
		got <- snap
	}()

	// Publish repeatedly to beat the subscribe race (the handler subscribes
	// just after the upgrade completes; earlier publishes fan out to nobody).
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case snap := <-got:
			if snap.CPU.Total != 42 {
				t.Fatalf("want CPU.Total 42, got %v", snap.CPU.Total)
			}
			return
		case err := <-errc:
			t.Fatalf("read: %v", err)
		case <-tick.C:
			s.Ingest(sampleSnap(time.Now()))
		}
	}
}

func bsdSnap(t time.Time) collect.Snapshot {
	snap := sampleSnap(t)
	snap.ZFS = []collect.ZFSStats{{Pool: "zroot", Size: 1000, Alloc: 700, Free: 300, UsedPct: 70, Health: "ONLINE"}}
	snap.ARC = &collect.ARCStats{Size: 512, Target: 1024, HitRate: 90}
	snap.Jails = []collect.JailStat{{JID: 3, Name: "web", Procs: 12}}
	return snap
}

func TestSeriesZFS(t *testing.T) {
	s := testServer(config.AuthConfig{})
	s.Ingest(bsdSnap(time.Now()))

	var resp struct {
		Labels []string    `json:"labels"`
		Data   [][]float64 `json:"data"`
	}
	rec := get(s, "/api/series?metric=zfs&range=1h", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Labels) != 2 || resp.Labels[1] != "zroot" {
		t.Fatalf("labels: want [ts zroot], got %v", resp.Labels)
	}
	if resp.Data[1][0] != 70 {
		t.Fatalf("data: want used_pct series [70], got %v", resp.Data[1])
	}
}

func TestSeriesARC(t *testing.T) {
	s := testServer(config.AuthConfig{})
	s.Ingest(bsdSnap(time.Now()))

	var resp struct {
		Labels []string    `json:"labels"`
		Data   [][]float64 `json:"data"`
	}
	rec := get(s, "/api/series?metric=arc&range=1h", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []string{"ts", "size", "target", "hit_rate"}
	if len(resp.Labels) != 4 || resp.Labels[1] != want[1] || resp.Labels[3] != want[3] {
		t.Fatalf("labels: want %v, got %v", want, resp.Labels)
	}
	if resp.Data[1][0] != 512 || resp.Data[3][0] != 90 {
		t.Fatalf("data: want size 512 / hit_rate 90, got %v", resp.Data)
	}
}

func TestSeriesJail(t *testing.T) {
	s := testServer(config.AuthConfig{})
	s.Ingest(bsdSnap(time.Now()))

	var resp struct {
		Labels []string    `json:"labels"`
		Data   [][]float64 `json:"data"`
	}
	rec := get(s, "/api/series?metric=jail&range=1h", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Labels) != 2 || resp.Labels[1] != "3 web" {
		t.Fatalf("labels: want [ts, '3 web'], got %v", resp.Labels)
	}
	if resp.Data[1][0] != 12 {
		t.Fatalf("data: want procs series [12], got %v", resp.Data[1])
	}
}

func TestAlertsEndpoint(t *testing.T) {
	s := testServer(config.AuthConfig{})

	// No engine attached: enabled=false, empty arrays (not null).
	rec := get(s, "/api/alerts", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var off struct {
		Enabled bool              `json:"enabled"`
		Rules   []json.RawMessage `json:"rules"`
		Events  []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &off); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if off.Enabled || off.Rules == nil || off.Events == nil {
		t.Fatalf("disabled response should carry empty arrays: %s", rec.Body.String())
	}

	// With an engine: a tripped rule shows as firing and produces an event.
	eng := alert.New(config.AlertsConfig{Rules: []config.AlertRule{{
		Name: "cpu-hot", Metric: "cpu", Field: "total", Op: ">", Threshold: 40,
	}}})
	s.SetAlerts(eng)
	eng.Eval(sampleSnap(time.Now())) // CPU.Total = 42 > 40 → fires (For=0)

	rec = get(s, "/api/alerts", nil)
	var on struct {
		Enabled bool               `json:"enabled"`
		Rules   []alert.RuleStatus `json:"rules"`
		Events  []alert.Event      `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &on); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !on.Enabled || len(on.Rules) != 1 || len(on.Events) != 1 {
		t.Fatalf("want 1 rule + 1 event, got %s", rec.Body.String())
	}
	if on.Rules[0].State != "firing" || on.Rules[0].Value != 42 {
		t.Fatalf("rule state: %+v", on.Rules[0])
	}
	if on.Events[0].State != "firing" || on.Events[0].Rule != "cpu-hot" {
		t.Fatalf("event: %+v", on.Events[0])
	}
}
