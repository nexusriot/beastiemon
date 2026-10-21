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

// fakeStore is a HistoryStore + RollupStore whose rows the test controls, so
// the store↔ring merge can be exercised without SQLite.
type fakeStore struct{ rows []collect.Snapshot }

func (f *fakeStore) Push(collect.Snapshot) {}

func (f *fakeStore) Since(t time.Time) []collect.Snapshot {
	var out []collect.Snapshot
	for _, s := range f.rows {
		if !s.Time.Before(t) {
			out = append(out, s)
		}
	}
	return out
}

func (f *fakeStore) RollupSince(t time.Time) (avg, lo, hi []collect.Snapshot) {
	rows := f.Since(t)
	return rows, rows, rows
}

// The store writes bucket-truncated timestamps while the ring keeps sub-second
// precision, so a store bucket can be strictly earlier as a time.Time yet land
// on the same whole second as the oldest ring sample. /api/series renders
// whole seconds, so emitting both produces a duplicate x value — and uPlot
// requires a strictly ascending x series.
func TestSeriesStoreRingSeamHasNoDuplicateTimestamps(t *testing.T) {
	bucket := time.Now().Truncate(10 * time.Second).Add(-30 * time.Second)
	s := testServer(config.AuthConfig{})
	s.SetStore(&fakeStore{rows: []collect.Snapshot{
		sampleSnap(bucket),
		sampleSnap(bucket.Add(10 * time.Second)),
		// Same whole second as the first ring sample below, 336ms earlier.
		sampleSnap(bucket.Add(20 * time.Second)),
	}})
	// Ring samples carry sub-second precision, as the sampler produces them.
	s.Ingest(sampleSnap(bucket.Add(20*time.Second + 336*time.Millisecond)))
	s.Ingest(sampleSnap(bucket.Add(21*time.Second + 336*time.Millisecond)))

	for _, path := range []string{
		"/api/series?metric=cpu&range=1h",
		"/api/series?metric=cpu&range=1h&band=1",
		"/api/series?metric=mem&range=1h",
	} {
		rec := get(s, path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: want 200, got %d", path, rec.Code)
		}
		var resp struct {
			Labels []string    `json:"labels"`
			Data   [][]float64 `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s: decode: %v", path, err)
		}
		ts := resp.Data[0]
		if len(ts) != 4 {
			t.Fatalf("%s: want 4 rows (2 store + 2 ring), got %d: %v", path, len(ts), ts)
		}
		for i := 1; i < len(ts); i++ {
			if ts[i] <= ts[i-1] {
				t.Fatalf("%s: timestamps not strictly ascending at %d: %v", path, i, ts)
			}
		}
		// Every column must stay index-aligned with the timestamp column.
		for i, col := range resp.Data {
			if len(col) != len(ts) {
				t.Fatalf("%s: column %d has %d rows, want %d", path, i, len(col), len(ts))
			}
		}
	}
}

func pfSnap(t time.Time) collect.Snapshot {
	snap := sampleSnap(t)
	snap.Net[0].RxErr = 3
	snap.Net[0].TxDrop = 1
	snap.CPU.PerCore = []float64{10, 20, 30, 40}
	snap.PF = &collect.PFStats{
		Enabled: true, States: 500, StateLimit: 10000, StatePct: 5,
		MatchRate: 54, SearchRate: 4362, DropRate: 2,
	}
	snap.Watch = []collect.ProcWatch{{Name: "sshd", Count: 1}, {Name: "nginx", Count: 0}}
	snap.Datasets = []collect.ZFSDataset{{Name: "zroot/var", Used: 900, Avail: 100}}
	return snap
}

func seriesOf(t *testing.T, s *Server, path string) (labels []string, data [][]float64) {
	t.Helper()
	rec := get(s, path, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: want 200, got %d", path, rec.Code)
	}
	var resp struct {
		Labels []string    `json:"labels"`
		Data   [][]float64 `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("%s: decode: %v", path, err)
	}
	return resp.Labels, resp.Data
}

func TestSeriesNetErr(t *testing.T) {
	s := testServer(config.AuthConfig{})
	s.Ingest(pfSnap(time.Now()))

	labels, data := seriesOf(t, s, "/api/series?metric=neterr&range=1h")
	want := []string{"ts", "rx_err", "tx_err", "rx_drop", "tx_drop"}
	if len(labels) != len(want) {
		t.Fatalf("labels: want %v, got %v", want, labels)
	}
	if data[1][0] != 3 || data[4][0] != 1 {
		t.Fatalf("want rx_err 3 and tx_drop 1, got %v", data)
	}
}

func TestSeriesPF(t *testing.T) {
	s := testServer(config.AuthConfig{})
	s.Ingest(pfSnap(time.Now()))

	labels, data := seriesOf(t, s, "/api/series?metric=pf&range=1h")
	if len(labels) != 5 || labels[1] != "states" || labels[4] != "drop_rate" {
		t.Fatalf("labels: %v", labels)
	}
	if data[1][0] != 500 || data[4][0] != 2 {
		t.Fatalf("want states 500 and drop_rate 2, got %v", data)
	}
}

func TestSeriesProcWatch(t *testing.T) {
	s := testServer(config.AuthConfig{})
	s.Ingest(pfSnap(time.Now()))

	labels, data := seriesOf(t, s, "/api/series?metric=procwatch&range=1h")
	if len(labels) != 3 || labels[1] != "sshd" || labels[2] != "nginx" {
		t.Fatalf("labels: want [ts sshd nginx], got %v", labels)
	}
	// A stopped service is a 0 in the series, not a gap — that is what makes
	// "when did it die" answerable.
	if data[1][0] != 1 || data[2][0] != 0 {
		t.Fatalf("want sshd 1 and nginx 0, got %v", data)
	}

	// ?name= narrows to one watched process.
	labels, _ = seriesOf(t, s, "/api/series?metric=procwatch&range=1h&name=nginx")
	if len(labels) != 2 || labels[1] != "nginx" {
		t.Fatalf("filtered labels: %v", labels)
	}
}

// cores=0 drops the per-core columns for clients that render cores from the
// live stream. On a big machine these dominate the payload, and band=1
// triples them.
func TestSeriesCoresOptOut(t *testing.T) {
	s := testServer(config.AuthConfig{})
	s.Ingest(pfSnap(time.Now()))

	withCores, _ := seriesOf(t, s, "/api/series?metric=cpu&range=1h")
	if len(withCores) != 9 { // ts + 4 modes + 4 cores
		t.Fatalf("default should include per-core columns, got %v", withCores)
	}
	without, _ := seriesOf(t, s, "/api/series?metric=cpu&range=1h&cores=0")
	if len(without) != 5 { // ts + 4 modes
		t.Fatalf("cores=0 should drop them, got %v", without)
	}
	// Under band=1 each dropped column would have cost three.
	banded, _ := seriesOf(t, s, "/api/series?metric=cpu&range=1h&band=1&cores=0")
	if len(banded) != 13 { // ts + 4 modes x (value, min, max)
		t.Fatalf("banded cores=0: got %v", banded)
	}
}

func TestPrometheusNewSeries(t *testing.T) {
	s := testServer(config.AuthConfig{})
	snap := pfSnap(time.Now())
	snap.ZFS = []collect.ZFSStats{
		{Pool: "zroot", UsedPct: 70, Health: "DEGRADED", ScanAgeHours: 12, ScanKnown: true},
		{Pool: "cold", UsedPct: 5, Health: "ONLINE"}, // never scrubbed
	}
	s.Ingest(snap)

	body := get(s, "/metrics", nil).Body.String()
	for _, want := range []string{
		`beastie_net_errors{iface="em0",dir="rx"} 3`,
		`beastie_net_drops{iface="em0",dir="tx"} 1`,
		`beastie_proc_watch_count{name="nginx"} 0`,
		`beastie_pf_states 500`,
		`beastie_pf_state_percent 5`,
		`beastie_pf_rate{kind="drop"} 2`,
		// Health as an ordinal, so a Prometheus rule can alert on it.
		`beastie_zfs_health{pool="zroot",state="DEGRADED"} 1`,
		`beastie_zfs_scan_age_hours{pool="zroot"} 12`,
		`beastie_zfs_dataset_bytes{dataset="zroot/var",kind="used"} 900`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q", want)
		}
	}
	// A pool that has never been scrubbed has no age; emitting 0 would read
	// as "scrubbed just now".
	if strings.Contains(body, `beastie_zfs_scan_age_hours{pool="cold"}`) {
		t.Error("a never-scrubbed pool must not report a scan age")
	}
}
