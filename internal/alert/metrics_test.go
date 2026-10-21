package alert

import (
	"testing"

	"github.com/nexusriot/beastiemon/internal/collect"
	"github.com/nexusriot/beastiemon/internal/config"
)

func bsdSnapshot() collect.Snapshot {
	return collect.Snapshot{
		Net: []collect.NetStats{
			{Interface: "em0", RxBps: 100, TxBps: 50, RxErr: 2, TxErr: 1, RxDrop: 4},
			{Interface: "em1", RxBps: 25, TxBps: 25, RxErr: 3},
		},
		Disk: []collect.DiskStats{
			{Device: "ada0", ReadBps: 1000, WriteBps: 500, ReadIOPS: 10, WriteIOPS: 5},
		},
		ZFS: []collect.ZFSStats{
			{Pool: "zroot", UsedPct: 40, Health: "ONLINE", Scan: "scrub_done", ScanAgeHours: 10, ScanKnown: true},
			{Pool: "tank", UsedPct: 91, Health: "DEGRADED", Scan: "none"},
		},
		ARC:   &collect.ARCStats{Size: 512, Target: 1024, HitRate: 97.5},
		Jails: []collect.JailStat{{JID: 1, Name: "web", Procs: 12}, {JID: 2, Name: "db", Procs: 4}},
		Watch: []collect.ProcWatch{{Name: "sshd", Count: 0}, {Name: "nginx", Count: 3}},
		PF:    &collect.PFStats{Enabled: true, States: 500, StateLimit: 10000, StatePct: 5, DropRate: 2, MatchRate: 54},
	}
}

func TestMetricValueNewMetrics(t *testing.T) {
	snap := bsdSnapshot()
	cases := []struct {
		metric, field string
		want          float64
	}{
		// Aggregates that `beastie check` and rules both wanted.
		{"net", "total_bps", 200},
		{"disk", "total_bps", 1500},
		{"disk", "total_iops", 15},
		// Error/drop rates now that the collector surfaces them.
		{"net", "rx_err", 5},
		{"net", "err", 6},
		{"net", "drop", 4},
		// ZFS reduces to the worst case across pools.
		{"zfs", "used_pct", 91},
		{"zfs", "health", 1}, // DEGRADED
		{"zfs", "", 91},      // default field is used_pct
		{"arc", "hit_rate", 97.5},
		{"arc", "", 97.5},
		{"arc", "size", 512},
		{"jail", "count", 2},
		{"jail", "web", 12},
		{"proc", "sshd", 0},
		{"proc", "nginx", 3},
		{"pf", "state_pct", 5},
		{"pf", "states", 500},
		{"pf", "drop_rate", 2},
	}
	for _, c := range cases {
		got, ok := metricValue(snap, c.metric, c.field)
		if !ok {
			t.Errorf("%s.%q: not readable", c.metric, c.field)
			continue
		}
		if got != c.want {
			t.Errorf("%s.%q = %v, want %v", c.metric, c.field, got, c.want)
		}
	}
}

// scan_age_hours must ignore pools that have never been scrubbed rather than
// treating their zero as "scrubbed just now" — that zero would hold the
// worst-case maximum down and mask a genuinely overdue pool.
func TestZFSScanAgeIgnoresNeverScrubbed(t *testing.T) {
	snap := bsdSnapshot() // zroot known at 10h, tank never scrubbed
	got, ok := metricValue(snap, "zfs", "scan_age_hours")
	if !ok || got != 10 {
		t.Fatalf("scan_age_hours = %v, %v; want 10, true", got, ok)
	}

	// With no pool carrying a known age there is nothing to compare, so the
	// rule reads "unknown" instead of firing or reading green.
	snap.ZFS = []collect.ZFSStats{{Pool: "tank", Scan: "none"}}
	if _, ok := metricValue(snap, "zfs", "scan_age_hours"); ok {
		t.Error("want unknown when no pool has ever been scrubbed")
	}
}

func TestZFSHealthOrdinal(t *testing.T) {
	for _, c := range []struct {
		health string
		want   float64
	}{
		{"ONLINE", 0},
		{"online", 0},
		{"", 0}, // no health column reported: don't invent a verdict
		{"DEGRADED", 1},
		{"FAULTED", 2},
		{"UNAVAIL", 2},
		{"REMOVED", 2},
		{"OFFLINE", 2},
	} {
		if got := zfsHealthOrdinal(c.health); got != c.want {
			t.Errorf("zfsHealthOrdinal(%q) = %v, want %v", c.health, got, c.want)
		}
	}
}

// A rule should fire on a pool leaving ONLINE — the headline reason the alert
// engine needed to see ZFS at all.
func TestZFSHealthRuleFires(t *testing.T) {
	e := New(config.AlertsConfig{
		Webhook: "http://example/hook",
		Rules: []config.AlertRule{{
			Name: "pool-unhealthy", Metric: "zfs", Field: "health", Op: ">=", Threshold: 1,
		}},
	})
	var sent []Event
	e.post = func(_, _ string, ev Event) { sent = append(sent, ev) }

	healthy := collect.Snapshot{ZFS: []collect.ZFSStats{{Pool: "zroot", Health: "ONLINE"}}}
	e.Eval(healthy)
	if len(sent) != 0 {
		t.Fatalf("healthy pool should not fire: %+v", sent)
	}
	e.Eval(collect.Snapshot{ZFS: []collect.ZFSStats{{Pool: "zroot", Health: "DEGRADED"}}})
	if len(sent) != 1 || sent[0].State != "firing" {
		t.Fatalf("degraded pool should fire: %+v", sent)
	}
	e.Eval(healthy)
	if len(sent) != 2 || sent[1].State != "resolved" {
		t.Fatalf("recovery should resolve: %+v", sent)
	}
}

// The service-down rule: proc counts come from [collect] proc_watch, so 0 is
// a fact rather than an absence of data and has to fire.
func TestProcWatchRuleFiresWhenServiceStops(t *testing.T) {
	e := New(config.AlertsConfig{
		Webhook: "http://example/hook",
		Rules: []config.AlertRule{{
			Name: "sshd-down", Metric: "proc", Field: "sshd", Op: "<", Threshold: 1,
		}},
	})
	var sent []Event
	e.post = func(_, _ string, ev Event) { sent = append(sent, ev) }

	e.Eval(collect.Snapshot{Watch: []collect.ProcWatch{{Name: "sshd", Count: 1}}})
	if len(sent) != 0 {
		t.Fatalf("running service should not fire: %+v", sent)
	}
	e.Eval(collect.Snapshot{Watch: []collect.ProcWatch{{Name: "sshd", Count: 0}}})
	if len(sent) != 1 || sent[0].State != "firing" {
		t.Fatalf("stopped service should fire: %+v", sent)
	}

	// A name that proc_watch never counts reads "unknown", not 0: otherwise a
	// typo in the rule would look like a permanently dead service.
	e2 := New(config.AlertsConfig{
		Webhook: "http://example/hook",
		Rules: []config.AlertRule{{
			Name: "typo", Metric: "proc", Field: "sshdd", Op: "<", Threshold: 1,
		}},
	})
	e2.post = func(string, string, Event) { t.Error("an unwatched name must not fire") }
	e2.Eval(collect.Snapshot{Watch: []collect.ProcWatch{{Name: "sshd", Count: 1}}})
	if st := e2.States(); st[0].State != "unknown" {
		t.Errorf("unwatched name: state = %q, want unknown", st[0].State)
	}
}

// Subsystems that aren't enabled must read "unknown" rather than 0, or a
// dashboard would show a green ZFS rule on a host with no pools.
func TestNewMetricsUnknownWhenSubsystemAbsent(t *testing.T) {
	empty := collect.Snapshot{}
	for _, c := range [][2]string{
		{"zfs", "used_pct"}, {"zfs", "health"},
		{"arc", "hit_rate"}, {"pf", "state_pct"},
		{"jail", "web"}, {"proc", "sshd"},
	} {
		if _, ok := metricValue(empty, c[0], c[1]); ok {
			t.Errorf("%s.%s should be unknown on a host without it", c[0], c[1])
		}
	}
	// jail.count is the exception: zero running jails is a readable fact,
	// and is what a "my jails died" rule needs to see.
	if v, ok := metricValue(empty, "jail", "count"); !ok || v != 0 {
		t.Errorf("jail.count = %v, %v; want 0, true", v, ok)
	}
}
