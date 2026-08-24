package alert

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/beastiemon/internal/collect"
	"github.com/nexusriot/beastiemon/internal/config"
)

func cpuSnap(t time.Time, total float64) collect.Snapshot {
	return collect.Snapshot{Time: t, CPU: collect.CPUStats{Total: total}}
}

func TestSustainedFiringAndResolve(t *testing.T) {
	cfg := config.AlertsConfig{
		Webhook: "http://example/hook",
		Rules: []config.AlertRule{{
			Name: "cpu-hot", Metric: "cpu", Field: "total",
			Op: ">", Threshold: 90, For: config.Duration{Duration: 2 * time.Second},
		}},
	}
	e := New(cfg)
	var events []Event
	e.post = func(_, _ string, ev Event) { events = append(events, ev) }

	base := time.Now()
	e.Eval(cpuSnap(base, 95)) // tripped; since=base, For not yet elapsed
	if len(events) != 0 {
		t.Fatalf("should not fire before For elapses, got %+v", events)
	}
	e.Eval(cpuSnap(base.Add(1*time.Second), 95)) // still inside For
	if len(events) != 0 {
		t.Fatalf("should not fire at 1s")
	}
	e.Eval(cpuSnap(base.Add(2*time.Second), 95)) // For elapsed → firing
	if len(events) != 1 || events[0].State != "firing" {
		t.Fatalf("want one firing event at 2s, got %+v", events)
	}
	e.Eval(cpuSnap(base.Add(3*time.Second), 50)) // recovered → resolved
	if len(events) != 2 || events[1].State != "resolved" {
		t.Fatalf("want resolved event, got %+v", events)
	}
	if events[0].Value != 95 || events[1].Value != 50 {
		t.Fatalf("event values wrong: %+v", events)
	}
}

func TestFireImmediateForZero(t *testing.T) {
	cfg := config.AlertsConfig{Webhook: "http://h", Rules: []config.AlertRule{{
		Name: "load", Metric: "load", Field: "load1", Op: ">=", Threshold: 1,
	}}}
	e := New(cfg)
	var n int
	e.post = func(string, string, Event) { n++ }

	e.Eval(collect.Snapshot{Time: time.Now(), Load: collect.LoadStats{Load1: 2}})
	if n != 1 {
		t.Fatalf("For=0 should fire immediately, got %d", n)
	}
	// Still tripped on the next tick — must not re-fire.
	e.Eval(collect.Snapshot{Time: time.Now(), Load: collect.LoadStats{Load1: 2}})
	if n != 1 {
		t.Fatalf("should not re-fire while already firing, got %d", n)
	}
}

func TestFSMaxSelector(t *testing.T) {
	cfg := config.AlertsConfig{Webhook: "http://h", Rules: []config.AlertRule{{
		Metric: "fs", Field: "max", Op: ">", Threshold: 80,
	}}}
	e := New(cfg)
	var ev *Event
	e.post = func(_, _ string, got Event) { ev = &got }

	e.Eval(collect.Snapshot{Time: time.Now(), FS: []collect.FSStats{
		{Mount: "/", UsedPct: 50}, {Mount: "/var", UsedPct: 92},
	}})
	if ev == nil || ev.Value != 92 {
		t.Fatalf("want fire on max used_pct=92, got %+v", ev)
	}
}

func TestWebhookHTTPPost(t *testing.T) {
	got := make(chan Event, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var ev Event
		json.Unmarshal(body, &ev)
		got <- ev
	}))
	defer srv.Close()

	cfg := config.AlertsConfig{Rules: []config.AlertRule{{
		Name: "mem", Metric: "mem", Field: "used_pct", Op: ">", Threshold: 50,
		Webhook: srv.URL,
	}}}
	e := New(cfg) // exercises the real async httpPost path
	e.Eval(collect.Snapshot{Time: time.Now(), Mem: collect.MemStats{UsedPct: 75}})

	select {
	case ev := <-got:
		if ev.State != "firing" || ev.Value != 75 || ev.Rule != "mem" {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("webhook POST not received")
	}
}

func TestRepeatRenotify(t *testing.T) {
	cfg := config.AlertsConfig{Webhook: "http://h", Rules: []config.AlertRule{{
		Name: "cpu", Metric: "cpu", Field: "total", Op: ">", Threshold: 90,
		Repeat: config.Duration{Duration: 5 * time.Second},
	}}}
	e := New(cfg)
	var states []string
	e.post = func(_, _ string, ev Event) { states = append(states, ev.State) }

	base := time.Now()
	e.Eval(cpuSnap(base, 95))                    // fire (For=0)
	e.Eval(cpuSnap(base.Add(2*time.Second), 95)) // still firing, before Repeat → silent
	if len(states) != 1 {
		t.Fatalf("want 1 event before Repeat elapses, got %v", states)
	}
	e.Eval(cpuSnap(base.Add(5*time.Second), 95)) // Repeat elapsed → renotify
	if len(states) != 2 || states[1] != "firing" {
		t.Fatalf("want a second firing event at Repeat, got %v", states)
	}
	e.Eval(cpuSnap(base.Add(6*time.Second), 10)) // recovered → resolved
	if len(states) != 3 || states[2] != "resolved" {
		t.Fatalf("want resolved after recovery, got %v", states)
	}
}

func TestHysteresisSuppressesFlapping(t *testing.T) {
	cfg := config.AlertsConfig{Webhook: "http://h", Rules: []config.AlertRule{{
		Name: "cpu", Metric: "cpu", Field: "total", Op: ">", Threshold: 90,
		Hysteresis: 5,
	}}}
	e := New(cfg)
	var states []string
	e.post = func(_, _ string, ev Event) { states = append(states, ev.State) }

	base := time.Now()
	e.Eval(cpuSnap(base, 95))                    // fire
	e.Eval(cpuSnap(base.Add(1*time.Second), 88)) // under threshold but inside the 5-wide margin → stay firing
	if len(states) != 1 {
		t.Fatalf("hysteresis should keep it firing at 88, got %v", states)
	}
	e.Eval(cpuSnap(base.Add(2*time.Second), 84)) // past threshold-hysteresis (85) → resolve
	if len(states) != 2 || states[1] != "resolved" {
		t.Fatalf("want resolve once past the hysteresis margin, got %v", states)
	}
}

func TestPayloadFormats(t *testing.T) {
	ev := Event{Rule: "cpu-hot", Metric: "cpu", Field: "total", Op: ">", Threshold: 90, Value: 95, State: "firing"}

	ct, body := renderPayload("slack", ev)
	if ct != "application/json" {
		t.Fatalf("content-type: %s", ct)
	}
	var slack map[string]string
	if err := json.Unmarshal(body, &slack); err != nil {
		t.Fatalf("slack payload: %v", err)
	}
	if !strings.Contains(slack["text"], "cpu-hot") {
		t.Fatalf("slack text missing rule name: %v", slack)
	}

	_, body = renderPayload("discord", ev)
	var discord map[string]string
	if json.Unmarshal(body, &discord); discord["content"] == "" {
		t.Fatalf("discord payload missing content: %s", body)
	}

	// Default/empty emits the Event verbatim.
	_, body = renderPayload("", ev)
	var raw Event
	if err := json.Unmarshal(body, &raw); err != nil || raw.Value != 95 || raw.State != "firing" {
		t.Fatalf("raw payload should be the Event JSON: %s", body)
	}
}

func TestStatesReporting(t *testing.T) {
	cfg := config.AlertsConfig{Webhook: "http://h", Rules: []config.AlertRule{{
		Name: "cpu-hot", Metric: "cpu", Field: "total", Op: ">", Threshold: 90,
		For: config.Duration{Duration: 10 * time.Second},
	}}}
	e := New(cfg)
	e.post = func(string, string, Event) {}

	base := time.Now()
	if st := e.States(); len(st) != 1 || st[0].State != "ok" {
		t.Fatalf("initial state: want ok, got %+v", st)
	}
	e.Eval(cpuSnap(base, 95)) // tripped, inside For → pending
	st := e.States()
	if st[0].State != "pending" || st[0].Value != 95 || st[0].Since.IsZero() {
		t.Fatalf("want pending value=95 with since set, got %+v", st[0])
	}
	e.Eval(cpuSnap(base.Add(10*time.Second), 96)) // For elapsed → firing
	if st = e.States(); st[0].State != "firing" || st[0].LastFire.IsZero() {
		t.Fatalf("want firing with last_fire set, got %+v", st[0])
	}
	e.Eval(cpuSnap(base.Add(11*time.Second), 20)) // recovered
	if st = e.States(); st[0].State != "ok" || st[0].Value != 20 {
		t.Fatalf("want ok value=20, got %+v", st[0])
	}
}

func TestEventsHistoryAndSink(t *testing.T) {
	cfg := config.AlertsConfig{Webhook: "http://h", Rules: []config.AlertRule{{
		Name: "cpu", Metric: "cpu", Field: "total", Op: ">", Threshold: 90,
	}}}
	e := New(cfg)
	e.post = func(string, string, Event) {}
	var sunk []Event
	e.SetSink(func(ev Event) { sunk = append(sunk, ev) })

	base := time.Now()
	e.Eval(cpuSnap(base, 95))                    // firing
	e.Eval(cpuSnap(base.Add(time.Second), 10))   // resolved
	e.Eval(cpuSnap(base.Add(2*time.Second), 95)) // firing again

	evs := e.Events(10)
	if len(evs) != 3 {
		t.Fatalf("want 3 events, got %d", len(evs))
	}
	// Newest first.
	if evs[0].State != "firing" || evs[1].State != "resolved" || evs[2].State != "firing" {
		t.Fatalf("order wrong: %v %v %v", evs[0].State, evs[1].State, evs[2].State)
	}
	if got := e.Events(1); len(got) != 1 || got[0].State != "firing" {
		t.Fatalf("limit: want just the newest event, got %+v", got)
	}
	if len(sunk) != 3 {
		t.Fatalf("sink should see every event, got %d", len(sunk))
	}
}

func TestEventsHistoryCapped(t *testing.T) {
	cfg := config.AlertsConfig{Webhook: "http://h", Rules: []config.AlertRule{{
		Name: "cpu", Metric: "cpu", Field: "total", Op: ">", Threshold: 90,
	}}}
	e := New(cfg)
	e.post = func(string, string, Event) {}
	base := time.Now()
	for i := 0; i < 2*historyCap; i++ { // alternate fire/resolve → 2*cap events
		e.Eval(cpuSnap(base.Add(time.Duration(2*i)*time.Second), 95))
		e.Eval(cpuSnap(base.Add(time.Duration(2*i+1)*time.Second), 10))
	}
	if got := len(e.Events(0)); got != historyCap {
		t.Fatalf("history should cap at %d, got %d", historyCap, got)
	}
}

func TestWatchdog(t *testing.T) {
	cfg := config.AlertsConfig{
		Webhook:    "http://h",
		StaleAfter: config.Duration{Duration: 30 * time.Second},
	}
	e := New(cfg)
	var events []Event
	e.post = func(_, _ string, ev Event) { events = append(events, ev) }

	base := time.Now()
	e.lastSnap = base // pin the baseline for deterministic ages

	e.CheckStale(base.Add(10 * time.Second)) // inside the window → quiet
	if len(events) != 0 {
		t.Fatalf("watchdog fired too early: %+v", events)
	}
	e.CheckStale(base.Add(31 * time.Second)) // stale → fire
	if len(events) != 1 || events[0].Rule != "watchdog" || events[0].State != "firing" {
		t.Fatalf("want watchdog firing, got %+v", events)
	}
	if events[0].Value < 30 {
		t.Fatalf("watchdog value should be the age in seconds, got %v", events[0].Value)
	}
	e.CheckStale(base.Add(40 * time.Second)) // already firing → no re-fire
	if len(events) != 1 {
		t.Fatalf("watchdog must not re-fire, got %+v", events)
	}

	// The watchdog shows up in States while firing.
	var wd *RuleStatus
	for _, st := range e.States() {
		if st.Name == "watchdog" {
			wd = &st
			break
		}
	}
	if wd == nil || wd.State != "firing" || wd.Threshold != 30 {
		t.Fatalf("want firing watchdog status, got %+v", wd)
	}

	// A snapshot arriving resolves the episode.
	e.Eval(cpuSnap(base.Add(45*time.Second), 5))
	if len(events) != 2 || events[1].State != "resolved" {
		t.Fatalf("want watchdog resolved on next snapshot, got %+v", events)
	}
	e.CheckStale(base.Add(50 * time.Second)) // fresh again → quiet
	if len(events) != 2 {
		t.Fatalf("watchdog re-fired too early after resolve: %+v", events)
	}
}
