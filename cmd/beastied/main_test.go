package main

import (
	"testing"

	"github.com/nexusriot/beastiemon/internal/config"
)

// A proc rule can only resolve if the name it watches is also in
// [collect] proc_watch. That pairing spans two config sections, so
// alert.Validate — which only sees [alerts] — cannot catch it, and the rule
// would otherwise sit silently at "unknown" forever.
func TestUnwatchedProcRules(t *testing.T) {
	cfg := config.Config{}
	cfg.Collect.ProcWatch = []string{"sshd"}
	cfg.Alerts.Rules = []config.AlertRule{
		{Name: "ok", Metric: "proc", Field: "sshd", Op: "<", Threshold: 1},
		{Name: "typo", Metric: "proc", Field: "sshdd", Op: "<", Threshold: 1},
		// Not a proc rule, so proc_watch is irrelevant to it.
		{Name: "cpu", Metric: "cpu", Field: "total", Op: ">", Threshold: 90},
		// An empty field is alert.Validate's problem, not this check's.
		{Name: "nofield", Metric: "proc", Op: "<", Threshold: 1},
	}

	warns := unwatchedProcRules(cfg)
	if len(warns) != 1 {
		t.Fatalf("want exactly one warning, got %d: %v", len(warns), warns)
	}
	if !contains(warns[0], "sshdd") || !contains(warns[0], "proc_watch") {
		t.Errorf("warning should name the process and the fix: %q", warns[0])
	}
}

func TestTestConfigExitCodes(t *testing.T) {
	good := config.Default()
	if code := testConfig("test.conf", good); code != 0 {
		t.Errorf("a clean config should exit 0, got %d", code)
	}

	bad := config.Default()
	bad.Alerts.Rules = []config.AlertRule{
		{Name: "typo", Metric: "cpu", Field: "total", Op: "=~", Threshold: 90},
	}
	if code := testConfig("test.conf", bad); code != 1 {
		t.Errorf("a config with a dead rule should exit 1, got %d", code)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
