// Package alert evaluates threshold rules against each snapshot and fires a
// webhook when a rule stays tripped for its configured duration (and again,
// resolved, when it recovers). Beyond per-rule firing status it keeps a
// capped in-memory event history and last-value per rule (served by
// /api/alerts via States/Events, optionally persisted through SetSink), and
// an optional sampler watchdog (CheckStale) that fires when snapshots stop
// arriving. Eval still costs O(rules) per tick.
package alert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nexusriot/beastiemon/internal/collect"
	"github.com/nexusriot/beastiemon/internal/config"
)

// Event is the JSON payload POSTed to a rule's webhook.
type Event struct {
	Rule      string    `json:"rule"`
	Metric    string    `json:"metric"`
	Field     string    `json:"field"`
	Op        string    `json:"op"`
	Threshold float64   `json:"threshold"`
	Value     float64   `json:"value"`
	State     string    `json:"state"` // "firing" | "resolved"
	Time      time.Time `json:"ts"`
}

type ruleState struct {
	rule      config.AlertRule
	webhook   string
	format    string
	since     time.Time // when the condition first became true (zero when false)
	firing    bool      // whether a "firing" event has been sent and not yet resolved
	lastFire  time.Time // when the most recent "firing" event was sent (for Repeat)
	lastValue float64   // most recently evaluated value (for /api/alerts)
	// lastOK records whether the metric was readable on the last tick. It
	// starts true so a rule reads "ok" in the window before the first
	// snapshot arrives, rather than flashing "unknown" at every startup.
	lastOK bool
}

// RuleStatus is one rule's current state as reported by States (and served at
// /api/alerts). State is "ok", "pending" (condition true, For not yet
// elapsed), "firing", or "unknown" (the metric could not be read on the last
// tick — a missing mount, absent sensors, or a typo in metric/field — so the
// rule is not actually watching anything).
type RuleStatus struct {
	Name      string    `json:"name"`
	Metric    string    `json:"metric"`
	Field     string    `json:"field,omitempty"`
	Op        string    `json:"op"`
	Threshold float64   `json:"threshold"`
	State     string    `json:"state"`
	Value     float64   `json:"value"`
	Since     time.Time `json:"since"`     // zero unless pending/firing
	LastFire  time.Time `json:"last_fire"` // zero until the rule has fired
}

// historyCap bounds the in-memory event log. It only backs /api/alerts when
// SQLite persistence is off; with a store attached events also flow to disk
// through the sink.
const historyCap = 200

// Engine evaluates a fixed set of rules. Construct with New and call Eval once
// per snapshot. Eval/CheckStale run on the daemon's main loop; States and
// Events are safe to call concurrently from HTTP handlers.
type Engine struct {
	mu     sync.Mutex
	rules  []ruleState
	client *http.Client
	// post sends an event to a webhook, rendering it in the given format.
	// Swappable in tests; the default (httpPost) fires asynchronously so a slow
	// endpoint never stalls Eval.
	post func(url, format string, ev Event)

	// Section-level webhook/format, used by the watchdog (it has no rule).
	webhook string
	format  string

	// Sampler watchdog: fires when no snapshot has been evaluated for
	// staleAfter — the one failure threshold rules can't see, because Eval
	// stops being called at all.
	staleAfter  time.Duration
	lastSnap    time.Time
	staleFiring bool

	history []Event // recent events, oldest first, capped at historyCap
	sink    func(Event)
}

// New builds an Engine from config. Per-rule webhook and format fall back to
// the section-wide defaults.
func New(cfg config.AlertsConfig) *Engine {
	e := &Engine{
		client:     &http.Client{Timeout: 10 * time.Second},
		webhook:    cfg.Webhook,
		format:     cfg.Format,
		staleAfter: cfg.StaleAfter.Duration,
		lastSnap:   time.Now(), // watchdog baseline: engine start
	}
	for _, r := range cfg.Rules {
		wh := r.Webhook
		if wh == "" {
			wh = cfg.Webhook
		}
		fm := r.Format
		if fm == "" {
			fm = cfg.Format
		}
		e.rules = append(e.rules, ruleState{rule: r, webhook: wh, format: fm, lastOK: true})
	}
	e.post = e.httpPost
	return e
}

// SetSink registers a callback invoked with every emitted event (in addition
// to the webhook POST and the in-memory history). The daemon uses it to
// persist events to the SQLite store.
func (e *Engine) SetSink(fn func(Event)) {
	e.mu.Lock()
	e.sink = fn
	e.mu.Unlock()
}

// Eval checks every rule against snap, firing/resolving webhooks as needed.
// While a rule is firing it re-notifies every rule.Repeat (if set), and it only
// resolves once the value clears the threshold by rule.Hysteresis (if set).
func (e *Engine) Eval(snap collect.Snapshot) {
	now := snap.Time
	if now.IsZero() {
		now = time.Now()
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	// A snapshot arrived: resolve the sampler watchdog if it was firing.
	if e.staleFiring {
		e.staleFiring = false
		e.emitWatchdog(now.Sub(e.lastSnap).Seconds(), "resolved", now)
	}
	e.lastSnap = now

	for i := range e.rules {
		rs := &e.rules[i]
		val, ok := metricValue(snap, rs.rule.Metric, rs.rule.Field)
		rs.lastValue, rs.lastOK = val, ok
		tripped := ok && compare(val, rs.rule.Op, rs.rule.Threshold)

		if !rs.firing {
			if !tripped {
				rs.since = time.Time{}
				continue
			}
			if rs.since.IsZero() {
				rs.since = now
			}
			if now.Sub(rs.since) >= rs.rule.For.Duration {
				rs.firing = true
				rs.lastFire = now
				e.emit(rs, val, "firing", now)
			}
			continue
		}

		// Firing: resolve once the value clears (respecting hysteresis), else
		// re-notify on the Repeat cadence.
		if !ok || rs.cleared(val) {
			rs.firing = false
			rs.since = time.Time{}
			rs.lastFire = time.Time{}
			e.emit(rs, val, "resolved", now)
			continue
		}
		if d := rs.rule.Repeat.Duration; d > 0 && now.Sub(rs.lastFire) >= d {
			rs.lastFire = now
			e.emit(rs, val, "firing", now)
		}
	}
}

// cleared reports whether a firing rule's value has recovered enough to
// resolve. Without hysteresis this is simply "no longer tripped"; with
// hysteresis the value must recede past the threshold by that margin, which
// prevents flapping when it hovers around the boundary.
func (rs *ruleState) cleared(val float64) bool {
	h := rs.rule.Hysteresis
	if h <= 0 {
		return !compare(val, rs.rule.Op, rs.rule.Threshold)
	}
	thr := rs.rule.Threshold
	switch rs.rule.Op {
	case ">", ">=":
		return val <= thr-h
	case "<", "<=":
		return val >= thr+h
	}
	return !compare(val, rs.rule.Op, rs.rule.Threshold)
}

// CheckStale fires the sampler watchdog when no snapshot has been evaluated
// for the configured stale_after window (sampler wedged, collector hung). The
// episode resolves on the next Eval. Call it on a timer; it no-ops when the
// watchdog is disabled or already firing.
func (e *Engine) CheckStale(now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.staleAfter <= 0 || e.staleFiring {
		return
	}
	age := now.Sub(e.lastSnap)
	if age < e.staleAfter {
		return
	}
	e.staleFiring = true
	e.emitWatchdog(age.Seconds(), "firing", now)
}

// emitWatchdog records a watchdog event and posts it to the section-level
// webhook. Caller holds e.mu.
func (e *Engine) emitWatchdog(ageSecs float64, state string, now time.Time) {
	ev := Event{
		Rule:      "watchdog",
		Metric:    "sampler",
		Field:     "age_seconds",
		Op:        ">",
		Threshold: e.staleAfter.Seconds(),
		Value:     ageSecs,
		State:     state,
		Time:      now,
	}
	e.record(ev)
	if e.webhook != "" {
		e.post(e.webhook, e.format, ev)
	}
}

// States reports every rule's current state, plus the watchdog pseudo-rule
// when stale_after is configured. Safe for concurrent use.
func (e *Engine) States() []RuleStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]RuleStatus, 0, len(e.rules)+1)
	for i := range e.rules {
		rs := &e.rules[i]
		// "unknown" outranks "ok": a rule whose metric is unreadable has never
		// tested its threshold, and reporting it green hides the failure.
		state := "ok"
		switch {
		case rs.firing:
			state = "firing"
		case !rs.lastOK:
			state = "unknown"
		case !rs.since.IsZero():
			state = "pending"
		}
		out = append(out, RuleStatus{
			Name:      rs.rule.Name,
			Metric:    rs.rule.Metric,
			Field:     rs.rule.Field,
			Op:        rs.rule.Op,
			Threshold: rs.rule.Threshold,
			State:     state,
			Value:     rs.lastValue,
			Since:     rs.since,
			LastFire:  rs.lastFire,
		})
	}
	if e.staleAfter > 0 {
		st := RuleStatus{
			Name:      "watchdog",
			Metric:    "sampler",
			Field:     "age_seconds",
			Op:        ">",
			Threshold: e.staleAfter.Seconds(),
			State:     "ok",
			Value:     time.Since(e.lastSnap).Seconds(),
		}
		if e.staleFiring {
			st.State = "firing"
			st.Since = e.lastSnap
		}
		out = append(out, st)
	}
	return out
}

// Events returns up to limit recent events from the in-memory history, newest
// first. Safe for concurrent use.
func (e *Engine) Events(limit int) []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := len(e.history)
	if limit > 0 && limit < n {
		n = limit
	}
	out := make([]Event, 0, n)
	for i := len(e.history) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, e.history[i])
	}
	return out
}

// emit records an event and, when the rule has a webhook, posts it there.
// Caller holds e.mu.
func (e *Engine) emit(rs *ruleState, val float64, state string, now time.Time) {
	ev := Event{
		Rule:      rs.rule.Name,
		Metric:    rs.rule.Metric,
		Field:     rs.rule.Field,
		Op:        rs.rule.Op,
		Threshold: rs.rule.Threshold,
		Value:     val,
		State:     state,
		Time:      now,
	}
	e.record(ev)
	if rs.webhook != "" {
		e.post(rs.webhook, rs.format, ev)
	}
}

// record appends an event to the capped in-memory history and forwards it to
// the sink (SQLite persistence) when one is set. Caller holds e.mu.
func (e *Engine) record(ev Event) {
	e.history = append(e.history, ev)
	if len(e.history) > historyCap {
		e.history = e.history[len(e.history)-historyCap:]
	}
	if e.sink != nil {
		e.sink(ev)
	}
}

func (e *Engine) httpPost(url, format string, ev Event) {
	contentType, body := renderPayload(format, ev)
	go func() {
		resp, err := e.client.Post(url, contentType, bytes.NewReader(body))
		if err == nil {
			resp.Body.Close()
		}
	}()
}

// renderPayload serialises an event for a webhook. "slack" and "discord" wrap a
// human-readable line in the one-field shape each service's incoming webhooks
// expect, so a rule can POST straight to a Slack/Discord webhook URL with no
// translating proxy. Any other value (including "" and "raw") sends the Event
// JSON verbatim.
func renderPayload(format string, ev Event) (contentType string, body []byte) {
	switch strings.ToLower(format) {
	case "slack":
		body, _ = json.Marshal(map[string]string{"text": alertMessage(ev)})
	case "discord":
		body, _ = json.Marshal(map[string]string{"content": alertMessage(ev)})
	default:
		body, _ = json.Marshal(ev)
	}
	return "application/json", body
}

// alertMessage is the one-line human summary used by the chat formats, e.g.
// "🔥 FIRING cpu-hot: cpu.total = 95.00 (> 90)".
func alertMessage(ev Event) string {
	icon, verb := "🔥", "FIRING"
	if ev.State == "resolved" {
		icon, verb = "✅", "RESOLVED"
	}
	name := ev.Rule
	if name == "" {
		name = ev.Metric
	}
	field := ev.Field
	if field == "" {
		field = "value"
	}
	return fmt.Sprintf("%s %s %s: %s.%s = %.2f (%s %g)",
		icon, verb, name, ev.Metric, field, ev.Value, ev.Op, ev.Threshold)
}

func compare(v float64, op string, thr float64) bool {
	switch op {
	case ">":
		return v > thr
	case ">=":
		return v >= thr
	case "<":
		return v < thr
	case "<=":
		return v <= thr
	case "==":
		return v == thr
	}
	return false
}

// zfsHealthOrdinal grades a pool's health string so the ordinary numeric
// comparator can alert on it, rather than this needing a string-matching rule
// language (DESIGN.md §1: no complex rule engine). ONLINE is 0, DEGRADED is 1,
// and everything that means "this pool is not serving data properly" is 2, so
// `op = ">=", threshold = 1` catches any departure from healthy.
func zfsHealthOrdinal(health string) float64 {
	switch strings.ToUpper(strings.TrimSpace(health)) {
	case "ONLINE":
		return 0
	case "DEGRADED":
		return 1
	case "":
		// zpool list reported no health column; don't invent a verdict.
		return 0
	default: // FAULTED, OFFLINE, UNAVAIL, REMOVED, …
		return 2
	}
}

// metricValue extracts the scalar a rule watches. Array metrics (fs, temp)
// use field as an exact selector or "max"/"" for the worst-case value; disk
// and net sum the chosen component across devices/interfaces; zfs reduces to
// the worst case across pools, because "is any pool unhealthy" is the
// question an operator actually has.
func metricValue(snap collect.Snapshot, metric, field string) (float64, bool) {
	switch metric {
	case "cpu":
		switch field {
		case "", "total":
			return snap.CPU.Total, true
		case "user":
			return snap.CPU.User, true
		case "sys":
			return snap.CPU.Sys, true
		case "idle":
			return snap.CPU.Idle, true
		}
	case "mem":
		switch field {
		case "", "used_pct":
			return snap.Mem.UsedPct, true
		case "used":
			return float64(snap.Mem.Used), true
		case "free":
			return float64(snap.Mem.Free), true
		}
	case "swap":
		switch field {
		case "", "pct":
			return snap.Mem.SwapPct, true
		case "used":
			return float64(snap.Mem.SwapUsed), true
		}
	case "load":
		switch field {
		case "", "load1":
			return snap.Load.Load1, true
		case "load5":
			return snap.Load.Load5, true
		case "load15":
			return snap.Load.Load15, true
		}
	case "fs":
		return maxOrNamed(field, snap.FS, func(f collect.FSStats) (string, float64) {
			return f.Mount, f.UsedPct
		})
	case "temp":
		return maxOrNamed(field, snap.Temps, func(t collect.TempStat) (string, float64) {
			return t.Name, t.Celsius
		})
	case "disk":
		get, ok := diskField(field)
		if !ok || len(snap.Disk) == 0 {
			return 0, false
		}
		var sum float64
		for _, d := range snap.Disk {
			sum += get(d)
		}
		return sum, true
	case "net":
		get, ok := netField(field)
		if !ok || len(snap.Net) == 0 {
			return 0, false
		}
		var sum float64
		for _, n := range snap.Net {
			sum += get(n)
		}
		return sum, true
	case "zfs":
		return zfsValue(snap, field)
	case "arc":
		if snap.ARC == nil {
			return 0, false
		}
		switch field {
		case "", "hit_rate":
			return snap.ARC.HitRate, true
		case "size":
			return float64(snap.ARC.Size), true
		case "target":
			return float64(snap.ARC.Target), true
		}
	case "jail":
		switch field {
		case "", "count":
			// Always readable: zero running jails is a fact, not a gap, and
			// is exactly what a "my jails died" rule needs to see.
			return float64(len(snap.Jails)), true
		}
		for _, j := range snap.Jails {
			if j.Name == field {
				return float64(j.Procs), true
			}
		}
		// A named jail that isn't running reads "unknown", matching how an
		// unmounted fs behaves. Use jail.count to alert on one disappearing.
		return 0, false
	case "proc":
		// Counts come from [collect] proc_watch, so 0 genuinely means "not
		// running" rather than "wasn't looked for" — which is what makes
		// `op = "<", threshold = 1` a working service-down rule. A name that
		// isn't in proc_watch reads unknown, flagging the config mistake.
		for _, w := range snap.Watch {
			if w.Name == field {
				return float64(w.Count), true
			}
		}
		return 0, false
	case "pf":
		if snap.PF == nil {
			return 0, false
		}
		switch field {
		case "", "state_pct":
			return snap.PF.StatePct, true
		case "states":
			return float64(snap.PF.States), true
		case "drop_rate":
			return snap.PF.DropRate, true
		case "match_rate":
			return snap.PF.MatchRate, true
		case "search_rate":
			return snap.PF.SearchRate, true
		case "insert_rate":
			return snap.PF.InsertRate, true
		}
	}
	return 0, false
}

// zfsValue reduces a per-pool field to its worst case across all pools.
func zfsValue(snap collect.Snapshot, field string) (float64, bool) {
	if len(snap.ZFS) == 0 {
		return 0, false
	}
	var pick func(collect.ZFSStats) (float64, bool)
	switch field {
	case "", "used_pct":
		pick = func(z collect.ZFSStats) (float64, bool) { return z.UsedPct, true }
	case "health":
		pick = func(z collect.ZFSStats) (float64, bool) { return zfsHealthOrdinal(z.Health), true }
	case "scan_pct":
		pick = func(z collect.ZFSStats) (float64, bool) { return z.ScanPct, true }
	case "scan_age_hours":
		// Only pools with a known last-scan time take part; a pool that has
		// never been scrubbed has no age to compare, and reporting 0 would
		// read as "scrubbed just now" — the opposite of the truth.
		pick = func(z collect.ZFSStats) (float64, bool) { return z.ScanAgeHours, z.ScanKnown }
	default:
		return 0, false
	}
	var max float64
	found := false
	for _, z := range snap.ZFS {
		v, ok := pick(z)
		if !ok {
			continue
		}
		if !found || v > max {
			max, found = v, true
		}
	}
	return max, found
}

// maxOrNamed returns the value for the named entry, or the maximum across all
// entries when field is "" or "max".
func maxOrNamed[T any](field string, items []T, f func(T) (string, float64)) (float64, bool) {
	if len(items) == 0 {
		return 0, false
	}
	if field == "" || field == "max" {
		var max float64
		found := false
		for _, it := range items {
			_, v := f(it)
			if !found || v > max {
				max, found = v, true
			}
		}
		return max, found
	}
	for _, it := range items {
		if name, v := f(it); name == field {
			return v, true
		}
	}
	return 0, false
}

// metricFields lists the fields each rule metric accepts, mirroring
// metricValue's switch. A nil list means the metric is keyed by entry name
// (a mount point, a sensor) and so accepts any field, plus "max"/"".
var metricFields = map[string][]string{
	"cpu":  {"", "total", "user", "sys", "idle"},
	"mem":  {"", "used_pct", "used", "free"},
	"swap": {"", "pct", "used"},
	"load": {"", "load1", "load5", "load15"},
	"disk": {"", "read_bps", "write_bps", "read_iops", "write_iops", "total_bps", "total_iops"},
	"net": {"", "rx_bps", "tx_bps", "rx_pps", "tx_pps", "total_bps",
		"rx_err", "tx_err", "err", "rx_drop", "tx_drop", "drop"},
	"zfs":  {"", "used_pct", "health", "scan_pct", "scan_age_hours"},
	"arc":  {"", "hit_rate", "size", "target"},
	"pf":   {"", "state_pct", "states", "drop_rate", "match_rate", "search_rate", "insert_rate"},
	"fs":   nil,
	"temp": nil,
	// jail accepts "count" or a jail name, so it is keyed like fs/temp.
	"jail": nil,
	// proc is keyed too, but unlike the others an empty field is useless:
	// there is no "worst process" to fall back to.
	"proc": nil,
}

// fieldRequired lists keyed metrics where an empty field can never resolve,
// so Validate can say so instead of letting the rule sit at "unknown".
var fieldRequired = map[string]bool{"proc": true}

var validOps = []string{">", ">=", "<", "<=", "=="}

// Value exposes the rule metric table outside the package. `beastie check`
// uses it so the CLI and the alert engine resolve metric/field pairs through
// one implementation — a check and a rule written the same way cannot
// disagree about what they measure.
func Value(snap collect.Snapshot, metric, field string) (float64, bool) {
	return metricValue(snap, metric, field)
}

// Metrics lists every metric a rule (or `beastie check`) can name, sorted.
func Metrics() []string {
	out := make([]string, 0, len(metricFields))
	for m := range metricFields {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// Fields lists the fixed fields a metric accepts, or nil when the metric is
// keyed by entry name (a mount, a sensor, a jail, a watched process).
func Fields(metric string) []string { return metricFields[metric] }

// Validate reports rules that can never fire because of a typo — an unknown
// metric, an unsupported operator, or a field the metric doesn't have. These
// don't stop the daemon (the rest of the config is still good), but left
// silent they look identical to a rule that is simply never tripping, so the
// daemon logs them at startup and on reload.
func Validate(cfg config.AlertsConfig) []string {
	var warns []string
	for _, r := range cfg.Rules {
		name := r.Name
		if name == "" {
			name = "(unnamed)"
		}
		if !contains(validOps, r.Op) {
			warns = append(warns, fmt.Sprintf(
				"alert rule %q: unsupported op %q (want one of %s) — it will never fire",
				name, r.Op, strings.Join(validOps, " ")))
		}
		fields, known := metricFields[r.Metric]
		if !known {
			warns = append(warns, fmt.Sprintf(
				"alert rule %q: unknown metric %q — it will never fire", name, r.Metric))
			continue
		}
		if fields != nil && !contains(fields, r.Field) {
			warns = append(warns, fmt.Sprintf(
				"alert rule %q: metric %q has no field %q (want one of %s) — it will never fire",
				name, r.Metric, r.Field, strings.Join(fields[1:], " ")))
		}
		if fields == nil && r.Field == "" && fieldRequired[r.Metric] {
			warns = append(warns, fmt.Sprintf(
				"alert rule %q: metric %q needs a field naming what to watch "+
					"(and that name must appear in [collect] proc_watch) — it will never fire",
				name, r.Metric))
		}
	}
	return warns
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func diskField(field string) (func(collect.DiskStats) float64, bool) {
	switch field {
	case "", "read_bps":
		return func(d collect.DiskStats) float64 { return d.ReadBps }, true
	case "write_bps":
		return func(d collect.DiskStats) float64 { return d.WriteBps }, true
	case "read_iops":
		return func(d collect.DiskStats) float64 { return d.ReadIOPS }, true
	case "write_iops":
		return func(d collect.DiskStats) float64 { return d.WriteIOPS }, true
	case "total_bps":
		return func(d collect.DiskStats) float64 { return d.ReadBps + d.WriteBps }, true
	case "total_iops":
		return func(d collect.DiskStats) float64 { return d.ReadIOPS + d.WriteIOPS }, true
	}
	return nil, false
}

func netField(field string) (func(collect.NetStats) float64, bool) {
	switch field {
	case "", "rx_bps":
		return func(n collect.NetStats) float64 { return n.RxBps }, true
	case "tx_bps":
		return func(n collect.NetStats) float64 { return n.TxBps }, true
	case "rx_pps":
		return func(n collect.NetStats) float64 { return n.RxPps }, true
	case "tx_pps":
		return func(n collect.NetStats) float64 { return n.TxPps }, true
	case "total_bps":
		return func(n collect.NetStats) float64 { return n.RxBps + n.TxBps }, true
	case "rx_err":
		return func(n collect.NetStats) float64 { return n.RxErr }, true
	case "tx_err":
		return func(n collect.NetStats) float64 { return n.TxErr }, true
	case "err":
		return func(n collect.NetStats) float64 { return n.RxErr + n.TxErr }, true
	case "rx_drop":
		return func(n collect.NetStats) float64 { return n.RxDrop }, true
	case "tx_drop":
		return func(n collect.NetStats) float64 { return n.TxDrop }, true
	case "drop":
		return func(n collect.NetStats) float64 { return n.RxDrop + n.TxDrop }, true
	}
	return nil, false
}
