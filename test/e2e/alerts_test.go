//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type ruleStatus struct {
	Name     string  `json:"name"`
	Metric   string  `json:"metric"`
	Field    string  `json:"field"`
	State    string  `json:"state"`
	Value    float64 `json:"value"`
	LastFire string  `json:"last_fire"`
}

type alertEvent struct {
	Rule  string `json:"rule"`
	State string `json:"state"`
}

type alertsResponse struct {
	Enabled bool         `json:"enabled"`
	Rules   []ruleStatus `json:"rules"`
	Events  []alertEvent `json:"events"`
}

func (a alertsResponse) rule(name string) (ruleStatus, bool) {
	for _, r := range a.Rules {
		if r.Name == name {
			return r, true
		}
	}
	return ruleStatus{}, false
}

// hook is a webhook endpoint that records what the daemon POSTs to it.
type hook struct {
	*httptest.Server
	mu   sync.Mutex
	body []struct{ path, body string }
}

func newHook(t *testing.T) *hook {
	h := &hook{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.body = append(h.body, struct{ path, body string }{r.URL.Path, string(b)})
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(h.Close)
	return h
}

// wait blocks until at least n posts have arrived, and returns them.
func (h *hook) wait(t *testing.T, n int, within time.Duration) []struct{ path, body string } {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		got := len(h.body)
		h.mu.Unlock()
		if got >= n {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]struct{ path, body string }(nil), h.body...)
}

func TestAlertLifecycle(t *testing.T) {
	h := newHook(t)
	// A rule on a metric that is always true fires at once; "for" delays the
	// second one so the pending → firing transition is exercised too.
	cfg := baseConfig() + fmt.Sprintf(`
[alerts]
webhook = %q

[[alerts.rule]]
name      = "mem-present"
metric    = "mem"
field     = "used_pct"
op        = ">"
threshold = 0

[[alerts.rule]]
name      = "sustained"
metric    = "mem"
field     = "used_pct"
op        = ">"
threshold = 0
for       = "1s"

[[alerts.rule]]
name      = "never"
metric    = "cpu"
field     = "total"
op        = ">"
threshold = 100000

[[alerts.rule]]
name      = "slack-shaped"
metric    = "load"
field     = "load1"
op        = ">="
threshold = 0
webhook   = %q
format    = "slack"

[[alerts.rule]]
name      = "discord-shaped"
metric    = "load"
field     = "load1"
op        = ">="
threshold = 0
webhook   = %q
format    = "discord"
`, h.URL+"/raw", h.URL+"/slack", h.URL+"/discord")

	d := startDaemon(t, cfg)
	d.waitSample()

	posts := h.wait(t, 4, 20*time.Second)
	byPath := map[string]string{}
	for _, p := range posts {
		if _, seen := byPath[p.path]; !seen {
			byPath[p.path] = p.body
		}
	}

	t.Run("raw payload", func(t *testing.T) {
		body, ok := byPath["/raw"]
		if !ok {
			t.Fatalf("no POST to the section webhook; got %v", byPath)
		}
		var ev struct {
			Rule      string  `json:"rule"`
			Metric    string  `json:"metric"`
			Field     string  `json:"field"`
			Op        string  `json:"op"`
			Threshold float64 `json:"threshold"`
			Value     float64 `json:"value"`
			State     string  `json:"state"`
			TS        string  `json:"ts"`
		}
		if err := json.Unmarshal([]byte(body), &ev); err != nil {
			t.Fatalf("raw payload is not an event: %v (%s)", err, body)
		}
		if ev.State != "firing" || ev.Metric != "mem" || ev.TS == "" {
			t.Errorf("unexpected raw payload: %+v", ev)
		}
	})

	t.Run("chat payloads", func(t *testing.T) {
		for path, key := range map[string]string{"/slack": "text", "/discord": "content"} {
			body, ok := byPath[path]
			if !ok {
				t.Errorf("no POST to %s; got %v", path, byPath)
				continue
			}
			var m map[string]string
			if err := json.Unmarshal([]byte(body), &m); err != nil {
				t.Errorf("%s payload is not an object: %v", path, err)
				continue
			}
			if _, ok := m[key]; !ok {
				t.Errorf("%s payload has no %q field: %s", path, key, body)
			}
			if len(m) != 1 {
				t.Errorf("%s payload should carry only %q: %s", path, key, body)
			}
		}
	})

	t.Run("states", func(t *testing.T) {
		var got alertsResponse
		d.getJSON("/api/alerts", &got)
		if !got.Enabled {
			t.Error("alerts report themselves disabled")
		}
		for name, want := range map[string]string{
			"mem-present":    "firing",
			"sustained":      "firing",
			"never":          "ok",
			"slack-shaped":   "firing",
			"discord-shaped": "firing",
		} {
			r, ok := got.rule(name)
			if !ok {
				t.Errorf("rule %q is missing from /api/alerts", name)
				continue
			}
			if r.State != want {
				t.Errorf("rule %q is %q, want %q", name, r.State, want)
			}
			// A firing rule has to carry the moment it fired, or anything
			// ageing or sorting events by it reads year one.
			if r.State == "firing" && strings.HasPrefix(r.LastFire, "0001-01-01") {
				t.Errorf("rule %q is firing with no last_fire", name)
			}
		}
		if len(got.Events) == 0 {
			t.Error("no events recorded")
		}
	})
}

// A rule whose metric cannot be read must say "unknown" rather than report a
// green zero, and the daemon must say why at startup — not only under -t.
func TestUnreadableRulesAreLoud(t *testing.T) {
	cfg := baseConfig() + `
[alerts]
webhook = "http://127.0.0.1:1/never"

[[alerts.rule]]
name      = "field-typo"
metric    = "cpu"
field     = "totl"
op        = ">"
threshold = 1

[[alerts.rule]]
name      = "unwatched"
metric    = "proc"
field     = "nginx"
op        = "<"
threshold = 1
`
	d := startDaemon(t, cfg)
	d.waitSample()

	var got alertsResponse
	d.getJSON("/api/alerts", &got)
	for _, name := range []string{"field-typo", "unwatched"} {
		r, ok := got.rule(name)
		if !ok {
			t.Errorf("rule %q is missing", name)
			continue
		}
		if r.State != "unknown" {
			t.Errorf("rule %q is %q, want unknown", name, r.State)
		}
	}

	log := d.log()
	for _, want := range []string{`"field-typo"`, `"unwatched"`, "proc_watch"} {
		if !strings.Contains(log, want) {
			t.Errorf("startup log does not mention %s:\n%s", want, log)
		}
	}
}

// proc_watch is what lets a rule notice a service that stopped, since an idle
// daemon never reaches the top-N process list.
func TestProcWatch(t *testing.T) {
	cfg := strings.Replace(baseConfig(), `top_procs = 5`,
		`top_procs = 5`+"\n"+`proc_watch = ["beastied", "definitely-not-running"]`, 1)
	d := startDaemon(t, cfg)
	d.waitSample()

	var snap struct {
		Watch []struct {
			Name  string `json:"name"`
			Count int    `json:"count"`
		} `json:"watch"`
	}
	d.getJSON("/api/metrics", &snap)
	counts := map[string]int{}
	for _, w := range snap.Watch {
		counts[w.Name] = w.Count
	}
	if counts["beastied"] < 1 {
		t.Errorf("beastied watches itself and should be counted: %+v", snap.Watch)
	}
	if n, ok := counts["definitely-not-running"]; !ok || n != 0 {
		t.Errorf("an absent process should be counted as 0, got %v", snap.Watch)
	}

	var s series
	d.getJSON("/api/series?metric=procwatch&range=1m", &s)
	if !hasLabel(s, "beastied") {
		t.Errorf("procwatch series is missing the watched name: %v", s.Labels)
	}

	code, body := d.get("/metrics")
	if code != http.StatusOK || !strings.Contains(string(body), `beastie_proc_watch_count{name="beastied"}`) {
		t.Errorf("proc_watch is not exported to Prometheus")
	}
}

// The watchdog is the one failure threshold rules cannot see: it fires from a
// wall-clock tick when snapshots stop arriving, and resolves on the next one.
func TestSamplerWatchdog(t *testing.T) {
	h := newHook(t)
	cfg := strings.Replace(baseConfig(), `interval = "200ms"`, `interval = "3s"`, 1) +
		fmt.Sprintf(`
[alerts]
webhook = %q
stale_after = "1s"
`, h.URL+"/watchdog")

	d := startDaemon(t, cfg)
	d.waitSample()

	posts := h.wait(t, 2, 25*time.Second)
	var firing, resolved bool
	for _, p := range posts {
		var ev struct {
			Rule  string `json:"rule"`
			State string `json:"state"`
		}
		if json.Unmarshal([]byte(p.body), &ev) != nil || ev.Rule != "watchdog" {
			continue
		}
		switch ev.State {
		case "firing":
			firing = true
		case "resolved":
			resolved = true
		}
	}
	if !firing {
		t.Errorf("the watchdog never fired with stale_after below the interval; posts: %v", posts)
	}
	if !resolved {
		t.Errorf("the watchdog never resolved when the next sample arrived; posts: %v", posts)
	}

	var got alertsResponse
	d.getJSON("/api/alerts", &got)
	wd, ok := got.rule("watchdog")
	if !ok {
		t.Fatal("the watchdog is missing from /api/alerts")
	}
	// It is reported through the same shape as a real rule, so while it is
	// firing it has to carry when it fired rather than a zero time.
	if wd.State == "firing" && strings.HasPrefix(wd.LastFire, "0001-01-01") {
		t.Errorf("the watchdog is firing with no last_fire: %+v", wd)
	}
}

// A webhook nobody answers leaves the rule reading "firing" everywhere, so the
// only way to find out is the log. Silence here means an operator believes
// they are being paged when they are not.
func TestUndeliveredWebhooksAreLogged(t *testing.T) {
	// A listener closed straight away gives an address that refuses connections.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := l.Addr().String()
	l.Close()

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer failing.Close()

	cfg := baseConfig() + fmt.Sprintf(`
[alerts]

[[alerts.rule]]
name      = "refused"
metric    = "mem"
field     = "used_pct"
op        = ">"
threshold = 0
webhook   = %q

[[alerts.rule]]
name      = "server-error"
metric    = "load"
field     = "load1"
op        = ">="
threshold = 0
webhook   = %q
`, "http://"+dead+"/gone", failing.URL+"/boom")

	d := startDaemon(t, cfg)
	d.waitSample()

	for _, want := range []string{
		`alert "refused": webhook`,
		`alert "server-error": webhook`,
		"not delivered",
		"HTTP 500",
	} {
		if !d.waitLog(want, 20*time.Second) {
			t.Errorf("log never mentioned %q:\n%s", want, d.log())
		}
	}

	// The rules still read as firing — which is exactly why the log matters.
	var got alertsResponse
	d.getJSON("/api/alerts", &got)
	for _, name := range []string{"refused", "server-error"} {
		if r, ok := got.rule(name); !ok || r.State != "firing" {
			t.Errorf("rule %q is %+v, want firing", name, r)
		}
	}
}
