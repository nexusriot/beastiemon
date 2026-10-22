//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestServedSurface(t *testing.T) {
	d := startDaemon(t, baseConfig())
	d.waitSample()

	t.Run("static and api", func(t *testing.T) {
		for _, c := range []struct {
			path string
			want int
		}{
			{"/healthz", 200},
			{"/", 200},
			{"/app.js", 200},
			{"/style.css", 200},
			{"/api/host", 200},
			{"/api/metrics", 200},
			{"/api/alerts", 200},
			{"/metrics", 200},
			{"/nope", 404},
			// The embedded filesystem holds only the dashboard's assets; the
			// package's own sources must not be reachable.
			{"/assets.go", 404},
			{"/../../etc/passwd", 404},
		} {
			if code, _ := d.get(c.path); code != c.want {
				t.Errorf("GET %s = %d, want %d", c.path, code, c.want)
			}
		}
	})

	t.Run("snapshot shape", func(t *testing.T) {
		var snap struct {
			TS  string `json:"ts"`
			CPU struct {
				Total   float64   `json:"total"`
				Idle    float64   `json:"idle"`
				PerCore []float64 `json:"per_core"`
			} `json:"cpu"`
			Mem struct {
				Total   uint64  `json:"total"`
				UsedPct float64 `json:"used_pct"`
			} `json:"mem"`
			Load struct {
				Load1 float64 `json:"load1"`
			} `json:"load"`
			Procs []struct {
				PID  int    `json:"pid"`
				Name string `json:"name"`
			} `json:"procs"`
			Uptime int64 `json:"uptime"`
		}
		d.getJSON("/api/metrics", &snap)

		if snap.TS == "" {
			t.Error("snapshot has no timestamp")
		}
		if snap.Mem.Total == 0 {
			t.Error("memory total is zero")
		}
		if snap.CPU.Total+snap.CPU.Idle == 0 {
			t.Error("cpu reports neither use nor idle")
		}
		if len(snap.CPU.PerCore) == 0 {
			t.Error("no per-core cpu readings")
		}
		if len(snap.Procs) == 0 {
			t.Error("no processes ranked")
		}
		for _, p := range snap.Procs {
			if p.Name == "idle" {
				t.Error("the kernel idle process must be excluded from top procs")
			}
		}
	})

	t.Run("host info", func(t *testing.T) {
		var host struct {
			Hostname string `json:"hostname"`
			OS       string `json:"os"`
		}
		d.getJSON("/api/host", &host)
		if host.Hostname == "" || host.OS == "" {
			t.Errorf("host info is incomplete: %+v", host)
		}
	})
}

// Until the first sample lands there is nothing to report, and saying so with
// 503 is what lets a client tell "starting up" from "broken".
func TestNoDataBeforeFirstSample(t *testing.T) {
	d := startDaemon(t, strings.Replace(baseConfig(), `interval = "200ms"`, `interval = "10s"`, 1))

	for _, path := range []string{"/api/metrics", "/metrics"} {
		if code, _ := d.get(path); code != http.StatusServiceUnavailable {
			t.Errorf("GET %s before the first sample = %d, want 503", path, code)
		}
	}
	// /healthz is for liveness, so it answers from the start.
	if code, _ := d.get("/healthz"); code != http.StatusOK {
		t.Errorf("GET /healthz before the first sample = %d, want 200", code)
	}
}

type series struct {
	Labels []string    `json:"labels"`
	Data   [][]float64 `json:"data"`
}

func (s series) check(t *testing.T, name string) {
	t.Helper()
	if len(s.Labels) == 0 || s.Labels[0] != "ts" {
		t.Errorf("%s: first label is %v, want ts", name, s.Labels)
		return
	}
	if len(s.Data) != len(s.Labels) {
		t.Errorf("%s: %d columns for %d labels", name, len(s.Data), len(s.Labels))
		return
	}
	for i, col := range s.Data {
		if len(col) != len(s.Data[0]) {
			t.Errorf("%s: column %q has %d points, ts has %d",
				name, s.Labels[i], len(col), len(s.Data[0]))
		}
	}
}

func TestSeries(t *testing.T) {
	d := startDaemon(t, baseConfig())
	d.waitSample()
	// Enough samples that a range has several points to shape-check.
	time.Sleep(time.Second)

	t.Run("every metric", func(t *testing.T) {
		// Every metric the README lists answers, including the ones whose
		// collectors are FreeBSD-only: on Linux they report no entries rather
		// than failing, which is what keeps the dashboard's cards honest.
		for _, m := range []string{
			"cpu", "mem", "load", "net", "neterr", "disk", "temp", "fs",
			"proc", "procwatch", "zfs", "arc", "jail", "pf",
		} {
			var s series
			d.getJSON("/api/series?metric="+m+"&range=5m", &s)
			s.check(t, m)
		}
	})

	t.Run("cpu options", func(t *testing.T) {
		var full, bare, banded series
		d.getJSON("/api/series?metric=cpu&range=1m", &full)
		d.getJSON("/api/series?metric=cpu&range=1m&cores=0", &bare)
		d.getJSON("/api/series?metric=cpu&range=1m&band=1&cores=0", &banded)

		if len(full.Labels) <= len(bare.Labels) {
			t.Errorf("cores=0 should drop the per-core columns: %v vs %v",
				full.Labels, bare.Labels)
		}
		// Each value series gains a paired _min and _max.
		if want := (len(bare.Labels)-1)*3 + 1; len(banded.Labels) != want {
			t.Errorf("band=1 gave %d columns, want %d: %v",
				len(banded.Labels), want, banded.Labels)
		}
		for _, l := range []string{"total_min", "total_max"} {
			if !hasLabel(banded, l) {
				t.Errorf("band=1 is missing %s: %v", l, banded.Labels)
			}
		}
	})

	t.Run("range handling", func(t *testing.T) {
		var def, huge series
		d.getJSON("/api/series?metric=cpu&range=5m&cores=0", &def)
		// A seconds value past what a duration can hold used to wrap negative
		// and answer with nothing at all. Big means "everything", not "none".
		d.getJSON("/api/series?metric=cpu&range=9223372036854775807&cores=0", &huge)
		if len(huge.Data[0]) < len(def.Data[0]) {
			t.Errorf("a huge range returned %d points, fewer than 5m's %d",
				len(huge.Data[0]), len(def.Data[0]))
		}
	})

	t.Run("unknown metric is rejected", func(t *testing.T) {
		if code, _ := d.get("/api/series?metric=nonsense&range=1m"); code != http.StatusBadRequest {
			t.Errorf("unknown metric = %d, want 400", code)
		}
	})
}

func hasLabel(s series, want string) bool {
	for _, l := range s.Labels {
		if l == want {
			return true
		}
	}
	return false
}

func TestPrometheusExposition(t *testing.T) {
	d := startDaemon(t, baseConfig())
	d.waitSample()

	code, body := d.get("/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics = %d", code)
	}
	text := string(body)
	for _, want := range []string{
		"beastie_cpu_percent", "beastie_mem_used_percent", "beastie_load",
		"beastie_uptime_seconds", "beastie_net_errors", "beastie_net_drops",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("/metrics is missing %s", want)
		}
	}
	// Every series needs its HELP and TYPE, or a scraper rejects the page.
	var help, typ int
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "# HELP "):
			help++
		case strings.HasPrefix(line, "# TYPE "):
			typ++
		case line == "" || strings.HasPrefix(line, "#"):
		default:
			if !strings.Contains(line, " ") {
				t.Errorf("malformed sample line: %q", line)
			}
		}
	}
	if help != typ || help == 0 {
		t.Errorf("%d HELP lines against %d TYPE lines", help, typ)
	}
}

func TestStreaming(t *testing.T) {
	d := startDaemon(t, baseConfig())
	d.waitSample()

	t.Run("server-sent events", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, d.baseURL+"/api/stream", nil)
		resp, err := d.client.Do(req)
		if err != nil {
			t.Fatalf("GET /api/stream: %v", err)
		}
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
			t.Errorf("content type %q, want text/event-stream", ct)
		}

		frames := readSSE(t, resp.Body, 3, 15*time.Second)
		if len(frames) < 3 {
			t.Fatalf("got %d frames, want 3", len(frames))
		}
		for i, f := range frames {
			var snap map[string]any
			if err := json.Unmarshal([]byte(f), &snap); err != nil {
				t.Errorf("frame %d is not JSON: %v", i, err)
			}
			if _, ok := snap["cpu"]; !ok {
				t.Errorf("frame %d carries no cpu", i)
			}
		}
	})

	t.Run("websocket", func(t *testing.T) {
		url := "ws://" + strings.TrimPrefix(d.baseURL, "http://") + "/api/ws"
		c, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			t.Fatalf("dialing %s: %v", url, err)
		}
		defer c.Close()
		for i := 0; i < 3; i++ {
			c.SetReadDeadline(time.Now().Add(15 * time.Second))
			typ, msg, err := c.ReadMessage()
			if err != nil {
				t.Fatalf("frame %d: %v", i, err)
			}
			if typ != websocket.TextMessage {
				t.Errorf("frame %d is type %d, want text", i, typ)
			}
			var snap map[string]any
			if err := json.Unmarshal(msg, &snap); err != nil {
				t.Errorf("frame %d is not JSON: %v", i, err)
			}
		}
	})

	t.Run("many clients are all fed", func(t *testing.T) {
		const clients = 12
		type result struct{ frames int }
		results := make(chan result, clients)
		for i := 0; i < clients; i++ {
			go func() {
				resp, err := d.client.Get(d.baseURL + "/api/stream")
				if err != nil {
					results <- result{}
					return
				}
				defer resp.Body.Close()
				results <- result{len(readSSE(t, resp.Body, 3, 15*time.Second))}
			}()
		}
		for i := 0; i < clients; i++ {
			if r := <-results; r.frames < 3 {
				t.Errorf("a concurrent client got %d frames, want 3", r.frames)
			}
		}
		// And the daemon is still answering normally afterwards.
		if code, _ := d.get("/api/metrics"); code != http.StatusOK {
			t.Errorf("after the fan-out, /api/metrics = %d", code)
		}
	})
}

func TestAuthGate(t *testing.T) {
	cfg := baseConfig() + `
[auth]
username = "admin"
password = "s3cret"
token    = "tok-123"
`
	d := startDaemon(t, cfg)
	d.waitSampleWith(map[string]string{"Authorization": "Bearer tok-123"})

	guarded := []string{"/", "/api/host", "/api/metrics", "/api/alerts", "/metrics"}

	t.Run("no credentials", func(t *testing.T) {
		for _, p := range guarded {
			code, _ := d.get(p)
			if code != http.StatusUnauthorized {
				t.Errorf("GET %s without credentials = %d, want 401", p, code)
			}
		}
		// Liveness probes have to keep working.
		if code, _ := d.get("/healthz"); code != http.StatusOK {
			t.Errorf("/healthz = %d, want 200 even with auth on", code)
		}
	})

	t.Run("challenge", func(t *testing.T) {
		resp, err := d.client.Get(d.baseURL + "/api/metrics")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, "Basic") {
			t.Errorf("challenge header is %q, want a Basic realm", got)
		}
	})

	t.Run("accepted credentials", func(t *testing.T) {
		for _, p := range guarded {
			if code, _ := d.getWith(p, map[string]string{
				"Authorization": "Bearer tok-123",
			}); code != http.StatusOK {
				t.Errorf("GET %s with a bearer token = %d, want 200", p, code)
			}
			// basic:  admin:s3cret
			if code, _ := d.getWith(p, map[string]string{
				"Authorization": "Basic YWRtaW46czNjcmV0",
			}); code != http.StatusOK {
				t.Errorf("GET %s with basic auth = %d, want 200", p, code)
			}
		}
		// The SSE endpoint cannot set headers from a browser, so it takes the
		// token in the query string.
		if code, _ := d.get("/api/metrics?token=tok-123"); code != http.StatusOK {
			t.Errorf("?token= = %d, want 200", code)
		}
	})

	t.Run("rejected credentials", func(t *testing.T) {
		for _, h := range []string{
			"Bearer wrong", "Bearer", "Basic", "Basic bm9wZTpub3Bl", "Token tok-123",
		} {
			if code, _ := d.getWith("/api/metrics", map[string]string{
				"Authorization": h,
			}); code != http.StatusUnauthorized {
				t.Errorf("Authorization %q = %d, want 401", h, code)
			}
		}
		if code, _ := d.get("/api/metrics?token=wrong"); code != http.StatusUnauthorized {
			t.Errorf("a wrong ?token= = %d, want 401", code)
		}
	})
}

func TestUnixSocketListener(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "beastied.sock")
	cfg := strings.ReplaceAll(baseConfig(), "%ADDR%", sock)
	d := startDaemonAt(t, sock, cfg)
	d.waitSample()

	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("socket was not created: %v", err)
	}
	if code, _ := d.get("/api/metrics"); code != http.StatusOK {
		t.Errorf("GET over the socket = %d, want 200", code)
	}

	// The CLI reaches it both by path and through "auto", which reads
	// server.listen out of the same config.
	for _, target := range []string{sock, "auto"} {
		if _, errOut, code := d.cli("--remote", target, "cpu"); code != 0 {
			t.Errorf("beastie --remote %s: exit %d, stderr %s", target, code, errOut)
		}
	}
}

func TestHistoryStore(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "history.db")
	cfg := baseConfig() + fmt.Sprintf(`
[store]
path = %q
resolution = "1s"

[alerts]
webhook = "http://127.0.0.1:1/never"

[[alerts.rule]]
name      = "always"
metric    = "mem"
field     = "used_pct"
op        = ">"
threshold = 0
`, db)

	d := startDaemon(t, cfg)
	d.waitSample()
	time.Sleep(2 * time.Second) // let a row or two reach the store

	if _, err := os.Stat(db); err != nil {
		t.Fatalf("history database was not created: %v", err)
	}

	// A range beyond the ring is answered from the store rather than refused.
	var s series
	d.getJSON("/api/series?metric=cpu&range=24h&cores=0", &s)
	s.check(t, "cpu over 24h")
	if len(s.Data[0]) == 0 {
		t.Error("a long range came back empty with a store configured")
	}

	var before alertsResponse
	d.getJSON("/api/alerts", &before)
	if len(before.Events) == 0 {
		t.Fatal("the rule fired but no event was recorded")
	}

	// Events live in SQLite, so a restart keeps them.
	d.stop()
	d2 := startDaemon(t, cfg)
	t.Cleanup(d2.stop)
	d2.waitSample()

	var after alertsResponse
	d2.getJSON("/api/alerts", &after)
	if len(after.Events) == 0 {
		t.Error("alert events did not survive a restart with [store] on")
	}
}

func TestReload(t *testing.T) {
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	cfg := strings.ReplaceAll(baseConfig(), "%ADDR%", addr)
	d := startDaemonAt(t, addr, cfg)
	d.waitSample()

	if code, _ := d.get("/api/metrics"); code != http.StatusOK {
		t.Fatalf("before reload: %d", code)
	}

	// Add auth and a store path, then SIGHUP. Auth applies in place; the store
	// path does not — and the line the daemon logs has to say so rather than
	// promising persistence that is not happening.
	updated := cfg + fmt.Sprintf(`
[auth]
token = "after-reload"

[store]
path = %q
`, filepath.Join(t.TempDir(), "late.db"))
	if err := os.WriteFile(d.cfgPath, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	d.hup()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if code, _ := d.get("/api/metrics"); code == http.StatusUnauthorized {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code, _ := d.get("/api/metrics"); code != http.StatusUnauthorized {
		t.Errorf("auth did not take effect on reload: %d\n%s", code, d.log())
	}
	if code, _ := d.getWith("/api/metrics", map[string]string{
		"Authorization": "Bearer after-reload",
	}); code != http.StatusOK {
		t.Errorf("the new token was not accepted: %d", code)
	}

	if !strings.Contains(d.log(), "store path change needs a restart") {
		t.Errorf("reload should say the store path needs a restart:\n%s", d.log())
	}
	if !strings.Contains(d.log(), "reloaded configuration (auth on, store off") {
		t.Errorf("reload must report the store as off, since none was opened:\n%s", d.log())
	}

	// A broken config must not take the running daemon with it.
	if err := os.WriteFile(d.cfgPath, []byte("[server\nlisten = broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d.hup()
	if !d.waitLog("keeping running config", 10*time.Second) {
		t.Errorf("a malformed reload should be reported and ignored:\n%s", d.log())
	}
	if code, _ := d.getWith("/api/metrics", map[string]string{
		"Authorization": "Bearer after-reload",
	}); code != http.StatusOK {
		t.Errorf("the daemon stopped serving after a bad reload: %d", code)
	}
}

func readSSE(t *testing.T, r interface{ Read([]byte) (int, error) }, want int, within time.Duration) []string {
	t.Helper()
	var frames []string
	buf := make([]byte, 4096)
	var acc strings.Builder
	deadline := time.Now().Add(within)
	for len(frames) < want && time.Now().Before(deadline) {
		n, err := r.Read(buf)
		if n > 0 {
			acc.Write(buf[:n])
			for {
				s := acc.String()
				i := strings.Index(s, "\n\n")
				if i < 0 {
					break
				}
				for _, line := range strings.Split(s[:i], "\n") {
					if data, ok := strings.CutPrefix(line, "data: "); ok {
						frames = append(frames, data)
					}
				}
				acc.Reset()
				acc.WriteString(s[i+2:])
			}
		}
		if err != nil {
			break
		}
	}
	return frames
}

// The sample config offers interval = "500ms", and the whole point of a
// sub-second interval is finer resolution. The series timestamp column used
// to be whole seconds, so every sample inside one second collided: uPlot
// binary-searches that column, and the dashboard drops any live point that
// does not advance it, so the extra samples were collected and then thrown
// away on the way to the chart.
func TestSubSecondIntervalKeepsItsResolution(t *testing.T) {
	cfg := strings.Replace(baseConfig(), `interval = "200ms"`, `interval = "250ms"`, 1)
	d := startDaemon(t, cfg)
	d.waitSample()
	time.Sleep(2 * time.Second) // ~8 samples, spanning at least two seconds

	var s series
	d.getJSON("/api/series?metric=cpu&range=5m&cores=0", &s)
	ts := s.Data[0]
	if len(ts) < 5 {
		t.Fatalf("only %d samples after 2s at 250ms", len(ts))
	}
	for i := 1; i < len(ts); i++ {
		if ts[i] <= ts[i-1] {
			t.Fatalf("timestamps must strictly increase: ts[%d]=%v <= ts[%d]=%v",
				i, ts[i], i-1, ts[i-1])
		}
	}
	// More than one sample per second has to survive, or the interval setting
	// bought nothing.
	span := ts[len(ts)-1] - ts[0]
	if span <= 0 {
		t.Fatalf("the window spans no time at all: %v", ts)
	}
	if rate := float64(len(ts)-1) / span; rate < 2 {
		t.Errorf("%.1f samples per second over %.2fs, want ~4 at a 250ms interval",
			rate, span)
	}
}
