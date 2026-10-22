package main

import (
	"os"
	"path/filepath"
	"strings"
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

// `beastied -t` and startup/reload must warn about exactly the same rules.
// The proc_watch mismatch used to be reported only by -t: a daemon started
// (or reloaded) with such a rule logged nothing, leaving a rule stuck at
// "unknown" with no explanation anywhere in the log.
func TestConfigWarningsCoversBothKinds(t *testing.T) {
	cfg := config.Default()
	cfg.Collect.ProcWatch = []string{"sshd"}
	cfg.Alerts.Rules = []config.AlertRule{
		{Name: "field-typo", Metric: "cpu", Field: "totl", Op: ">", Threshold: 90},
		{Name: "unwatched", Metric: "proc", Field: "nginx", Op: "<", Threshold: 1},
		{Name: "fine", Metric: "proc", Field: "sshd", Op: "<", Threshold: 1},
	}

	warns := configWarnings(cfg)
	if len(warns) != 2 {
		t.Fatalf("want 2 warnings (field typo + proc_watch mismatch), got %d: %v",
			len(warns), warns)
	}
	var sawTypo, sawUnwatched bool
	for _, w := range warns {
		if contains(w, "field-typo") {
			sawTypo = true
		}
		if contains(w, "unwatched") && contains(w, "proc_watch") {
			sawUnwatched = true
		}
	}
	if !sawTypo {
		t.Errorf("missing the alert.Validate field-typo warning: %v", warns)
	}
	if !sawUnwatched {
		t.Errorf("missing the cross-section proc_watch warning: %v", warns)
	}
}

// A misspelled setting is dropped silently by the TOML decoder, so the host
// monitors with a default nobody chose. -t and startup share configWarnings,
// so both now say which key was ignored.
func TestConfigWarningsReportsUnknownKeys(t *testing.T) {
	cfg := config.Default()
	cfg.Unknown = []string{"collect.intervall", "collekt"}

	warns := configWarnings(cfg)
	if len(warns) != 2 {
		t.Fatalf("want one warning per unknown key, got %d: %v", len(warns), warns)
	}
	for _, key := range cfg.Unknown {
		var found bool
		for _, w := range warns {
			if contains(w, key) {
				found = true
			}
		}
		if !found {
			t.Errorf("no warning names %q: %v", key, warns)
		}
	}
	// And they make -t exit non-zero, like any other config problem.
	if code := testConfig("test.conf", cfg); code != 1 {
		t.Errorf("a config with unknown keys should exit 1, got %d", code)
	}
}

// A reload cannot attach a history store — the path change needs a restart —
// so the line it logs must not claim "store on" off the back of the new file.
// Reporting persistence that is not happening is worse than reporting none.
func TestStatusLineReportsEffectiveStore(t *testing.T) {
	cfg := config.Default()
	cfg.Store.Path = "/var/db/beastiemon/history.db"

	if got := statusLine(cfg, false); !contains(got, "store off") {
		t.Errorf("a configured-but-not-open store must read off, got %q", got)
	}
	if got := statusLine(cfg, true); !contains(got, "store on") {
		t.Errorf("an open store must read on, got %q", got)
	}
}

// Packaging contract: the shipped rc.d script and the newsyslog rule have to
// agree, and the sample config's socket example has to be somewhere the
// unprivileged daemon can actually bind.
func TestRCScriptPackagingContract(t *testing.T) {
	rc, err := os.ReadFile(filepath.Join("..", "..", "freebsd", "beastied.in"))
	if err != nil {
		t.Fatalf("read rc.d template: %v", err)
	}
	script := string(rc)

	// newsyslog SIGHUPs the supervisor to rotate. Without daemon(8) -H the
	// supervisor never reopens -o, so it keeps writing to the rotated inode
	// and every line after the first rotation is lost.
	args := grepLine(script, "command_args=")
	if args == "" {
		t.Fatal("rc.d template has no command_args line")
	}
	if !contains(args, " -H ") {
		t.Errorf("daemon(8) needs -H to reopen its log on rotation: %q", args)
	}

	// A Unix-socket listen path needs a directory the daemon user owns:
	// /var/run is root-owned and beastied binds after dropping privileges.
	if !contains(script, "beastied_rundir") {
		t.Error("rc.d template must define beastied_rundir for socket listens")
	}
	if !contains(script, `install -d -o "${beastied_runas}"`) {
		t.Error("start_precmd must create the run directory owned by the daemon user")
	}

	conf, err := os.ReadFile(filepath.Join("..", "..", "freebsd", "beastiemon.conf"))
	if err != nil {
		t.Fatalf("read sample config: %v", err)
	}
	// The socket example must sit inside that directory, not directly in
	// /var/run, where binding fails with EACCES and daemon(8) -r then
	// respawns the daemon about once a second, forever.
	if !contains(string(conf), "/var/run/beastied/beastied.sock") {
		t.Error("sample config's socket example must point inside /var/run/beastied")
	}
	if contains(string(conf), `"/var/run/beastied.sock"`) {
		t.Error("sample config still suggests /var/run/beastied.sock, which _beastie cannot bind")
	}
}

// grepLine returns the first line of s containing sub, or "".
func grepLine(s, sub string) string {
	for _, line := range strings.Split(s, "\n") {
		if contains(line, sub) {
			return line
		}
	}
	return ""
}
