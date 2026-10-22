//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Every subcommand has to run against a live host and say something, with the
// FreeBSD-only panels explaining their absence rather than printing nothing.
func TestCLISubcommands(t *testing.T) {
	d := startDaemon(t, baseConfig())
	d.waitSample()

	commands := []string{
		"status", "cpu", "mem", "net", "disk", "fs", "temp",
		"proc", "load", "zfs", "jail", "pf", "watch",
	}

	t.Run("local sampling", func(t *testing.T) {
		for _, c := range commands {
			out, errOut, code := d.cli(c)
			if code != 0 {
				t.Errorf("beastie %s: exit %d, stderr %s", c, code, errOut)
			}
			if strings.TrimSpace(out) == "" {
				t.Errorf("beastie %s printed nothing", c)
			}
		}
	})

	t.Run("no command is the full status", func(t *testing.T) {
		// Plain `beastie` passes no positional argument at all, which is the
		// most common invocation there is.
		out, errOut, code := d.cli()
		if code != 0 {
			t.Fatalf("plain beastie: exit %d, stderr %s", code, errOut)
		}
		for _, want := range []string{"CPU", "MEM", "LOAD"} {
			if !strings.Contains(out, want) {
				t.Errorf("plain beastie is missing the %s line:\n%s", want, out)
			}
		}
	})

	t.Run("absent subsystems explain themselves", func(t *testing.T) {
		// None of these collectors exist off FreeBSD, so each one is a chance
		// to print a confusing blank.
		for cmd, want := range map[string]string{
			"zfs":   "no pools",
			"jail":  "no jails",
			"pf":    "unavailable",
			"watch": "nothing watched",
			"temp":  "unavailable",
		} {
			out, _, _ := d.cli(cmd)
			if !strings.Contains(strings.ToLower(out), want) {
				t.Errorf("beastie %s should say %q, got:\n%s", cmd, want, out)
			}
		}
	})

	t.Run("remote mode", func(t *testing.T) {
		for _, c := range commands {
			if _, errOut, code := d.cli("--remote", "auto", c); code != 0 {
				t.Errorf("beastie --remote auto %s: exit %d, stderr %s", c, code, errOut)
			}
		}
	})
}

func TestCLIJSON(t *testing.T) {
	d := startDaemon(t, baseConfig())
	d.waitSample()

	for _, c := range []string{
		"", "cpu", "mem", "net", "disk", "fs", "temp", "proc",
		"load", "zfs", "jail", "pf", "watch",
	} {
		args := []string{"--json"}
		name := c
		if c != "" {
			args = append(args, c)
		} else {
			name = "(full snapshot)"
		}
		out, errOut, code := d.cli(args...)
		if code != 0 {
			t.Errorf("beastie --json %s: exit %d, stderr %s", name, code, errOut)
			continue
		}
		var any any
		if err := json.Unmarshal([]byte(out), &any); err != nil {
			t.Errorf("beastie --json %s did not emit JSON: %v\n%s", name, err, out)
		}
		if strings.Contains(out, "Host:") {
			t.Errorf("beastie --json %s leaked the host banner into stdout", name)
		}
	}
}

// Flags belong before the command. The flag package stops parsing at the
// first non-flag argument, so one written afterwards used to be dropped in
// silence: `beastie cpu --json` printed coloured text and exited 0, which a
// script piping into jq reads as valid output.
func TestCLIFlagOrdering(t *testing.T) {
	d := startDaemon(t, baseConfig())
	d.waitSample()

	for _, args := range [][]string{
		{"cpu", "--json"},
		{"status", "--json"},
		{"mem", "--no-color"},
	} {
		out, errOut, code := d.cli(args...)
		if code == 0 {
			t.Errorf("beastie %v should be refused, got exit 0:\n%s", args, out)
		}
		if !strings.Contains(errOut, "must come before the command") {
			t.Errorf("beastie %v should explain the ordering, stderr was: %s", args, errOut)
		}
	}

	// check is the exception: it takes its own flags after the subcommand.
	if _, errOut, code := d.cli("check", "--warn", "80", "--crit", "90", "cpu"); code != 0 {
		t.Errorf("check must keep its own flags: exit %d, stderr %s", code, errOut)
	}
}

// check is a nagios plugin: the exit code is the whole interface.
func TestCLICheckMode(t *testing.T) {
	d := startDaemon(t, baseConfig())
	d.waitSample()

	cases := []struct {
		args   []string
		want   int
		expect string
	}{
		// Thresholds are optional; omitting both is an informational OK.
		{[]string{"check", "cpu"}, 0, "OK"},
		{[]string{"check", "--warn", "99", "--crit", "100", "mem"}, 0, "OK"},
		// Memory is never at 1%, so these two straddle the real value.
		{[]string{"check", "--warn", "1", "--crit", "100", "mem"}, 1, "WARNING"},
		{[]string{"check", "--warn", "1", "--crit", "2", "mem"}, 2, "CRITICAL"},
		// A field the metric does not have is a typo, and says so.
		{[]string{"check", "--field", "totl", "cpu"}, 3, `has no field "totl"`},
		// A field that exists but has nothing behind it is a different
		// problem: no readings, rather than a green zero.
		{[]string{"check", "--field", "/nonexistent", "fs"}, 3, "nothing to measure"},
		{[]string{"check", "temp"}, 3, "nothing to measure"},
		{[]string{"check", "nonsense"}, 3, "unknown metric"},
		{[]string{"check"}, 3, "needs a metric"},
		// --below flips the comparison for the metrics where small is bad.
		{[]string{"check", "--crit", "1", "--below", "--field", "beastied", "proc"}, 3, "nothing to measure"},
	}
	for _, c := range cases {
		out, errOut, code := d.cli(c.args...)
		if code != c.want {
			t.Errorf("beastie %v: exit %d, want %d (%s%s)", c.args, code, c.want, out, errOut)
		}
		if !strings.Contains(out+errOut, c.expect) {
			t.Errorf("beastie %v should mention %q, got: %s%s", c.args, c.expect, out, errOut)
		}
	}
}

// A snapshot that could not be taken has nothing honest to print. Reporting
// success with a screenful of zeros is how a dead daemon passes for an idle
// host.
func TestCLIUnreachableRemote(t *testing.T) {
	d := startDaemon(t, baseConfig())
	d.waitSample()

	dead := fmt.Sprintf("127.0.0.1:%d", freePort(t))

	t.Run("display commands fail", func(t *testing.T) {
		for _, c := range []string{"cpu", "status", "mem"} {
			out, errOut, code := d.cli("--remote", dead, c)
			if code == 0 {
				t.Errorf("beastie --remote <dead> %s exited 0", c)
			}
			if strings.Contains(out, "0.0%") {
				t.Errorf("beastie --remote <dead> %s printed fabricated zeros:\n%s", c, out)
			}
			if !strings.Contains(errOut, "connection refused") {
				t.Errorf("beastie --remote <dead> %s should say why: %s", c, errOut)
			}
		}
	})

	t.Run("json emits nothing", func(t *testing.T) {
		out, _, code := d.cli("--json", "--remote", dead, "cpu")
		if code == 0 {
			t.Error("--json against a dead daemon exited 0")
		}
		if strings.TrimSpace(out) != "" {
			t.Errorf("--json against a dead daemon wrote to stdout: %s", out)
		}
	})

	t.Run("check and alerts stay nagios-shaped", func(t *testing.T) {
		out, _, code := d.cli("--remote", dead, "check", "--warn", "1", "--crit", "2", "cpu")
		if code != 3 {
			t.Errorf("check against a dead daemon: exit %d, want 3", code)
		}
		if !strings.Contains(out, "UNKNOWN") {
			t.Errorf("check against a dead daemon should print UNKNOWN: %s", out)
		}
		if _, _, code := d.cli("--remote", dead, "alerts"); code != 3 {
			t.Errorf("alerts against a dead daemon: exit %d, want 3", code)
		}
	})
}

// "auto" means "the daemon whose config I just read", and the README tells LAN
// installs to bind 0.0.0.0 — a bind address, not one a client can connect to.
//
// This exercises the flow end to end, but it cannot be the guard for that
// rewrite: Linux quietly routes a connection to 0.0.0.0 to loopback, so the
// case passes here either way. FreeBSD refuses it outright with "network is
// unreachable", which is where the bug showed up. The guard is
// TestRemoteAutoRewritesWildcardBind in cmd/beastie, which asserts the
// resolved base URL and so holds on every platform.
func TestCLIRemoteAutoWithWildcardBind(t *testing.T) {
	port := freePort(t)
	addr := fmt.Sprintf("0.0.0.0:%d", port)
	d := startDaemonAt(t, addr, strings.ReplaceAll(baseConfig(), "%ADDR%", addr))
	d.waitSample()

	for _, c := range []string{"cpu", "mem", "status"} {
		if _, errOut, code := d.cli("--remote", "auto", c); code != 0 {
			t.Errorf("beastie --remote auto %s against a wildcard bind: exit %d, stderr %s",
				c, code, errOut)
		}
	}
	if _, _, code := d.cli("--remote", "auto", "check", "--warn", "99", "--crit", "100", "cpu"); code != 0 {
		t.Errorf("check --remote auto against a wildcard bind: exit %d", code)
	}
}

func TestCLIAlerts(t *testing.T) {
	cfg := baseConfig() + `
[alerts]
webhook = "http://127.0.0.1:1/never"

[[alerts.rule]]
name      = "firing-rule"
metric    = "mem"
field     = "used_pct"
op        = ">"
threshold = 0
`
	d := startDaemon(t, cfg)
	d.waitSample()

	// Exit 2 while something is firing makes it usable as a single probe.
	out, errOut, code := d.cli("--remote", "auto", "alerts")
	if code != 2 {
		t.Errorf("beastie alerts with a firing rule: exit %d, want 2 (%s%s)", code, out, errOut)
	}
	if !strings.Contains(out, "firing-rule") {
		t.Errorf("beastie alerts did not render the rule:\n%s", out)
	}

	// The engine lives in the daemon, so this one needs --remote and says so.
	out, errOut, _ = d.cli("alerts")
	if !strings.Contains(out+errOut, "--remote") {
		t.Errorf("beastie alerts without --remote should explain: %s%s", out, errOut)
	}
}

func TestCLIUsageErrors(t *testing.T) {
	d := startDaemon(t, baseConfig())

	for _, c := range []struct {
		args []string
		want int
	}{
		{[]string{"bogus"}, 2},
		{[]string{"CPU"}, 2}, // commands are lowercase
		{[]string{"--bogus-flag", "cpu"}, 2},
	} {
		if _, _, code := d.cli(c.args...); code != c.want {
			t.Errorf("beastie %v: exit %d, want %d", c.args, code, c.want)
		}
	}

	if out, _, code := runCLI(t, "version"); code != 0 || !strings.Contains(out, "beastie") {
		t.Errorf("beastie version: exit %d, output %q", code, out)
	}
	if out, _, code := runCLI(t, "help"); code != 0 || !strings.Contains(out, "Usage") {
		t.Errorf("beastie help: exit %d, output %q", code, out)
	}
}

// `beastied -t` is the gate before a reload, so everything that would leave
// the daemon monitoring something other than what was written has to show up
// here — including a key that was simply misspelled.
func TestDaemonConfigTest(t *testing.T) {
	cases := []struct {
		name   string
		cfg    string
		want   int
		expect []string
	}{
		{
			name: "clean config",
			cfg:  "[server]\nlisten = \"127.0.0.1:8088\"\n\n[collect]\ninterval = \"1s\"\n",
			want: 0, expect: []string{"syntax ok"},
		},
		{
			name: "misspelled keys",
			cfg:  "[collect]\nintervall = \"5s\"\n\n[collekt]\ntop_procs = 2\n",
			want: 1, expect: []string{"unknown setting", "collect.intervall", "collekt"},
		},
		{
			name: "rule field typo",
			cfg: "[alerts]\n[[alerts.rule]]\nname = \"r\"\nmetric = \"cpu\"\n" +
				"field = \"totl\"\nop = \">\"\nthreshold = 1\n",
			want: 1, expect: []string{"has no field", "never fire"},
		},
		{
			name: "proc rule nothing counts",
			cfg: "[alerts]\n[[alerts.rule]]\nname = \"svc\"\nmetric = \"proc\"\n" +
				"field = \"nginx\"\nop = \"<\"\nthreshold = 1\n",
			want: 1, expect: []string{"proc_watch", "never fire"},
		},
		{
			name: "malformed toml",
			cfg:  "[server\nlisten = broken\n",
			want: 1, expect: []string{"toml"},
		},
		{
			name: "nonsense values are clamped, not rejected",
			cfg:  "[collect]\ninterval = \"0s\"\nring_size = 0\ntop_procs = -3\n",
			want: 0, expect: []string{"syntax ok"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, code := daemonTestConfig(t, c.cfg)
			if code != c.want {
				t.Errorf("exit %d, want %d; output:\n%s", code, c.want, out)
			}
			for _, want := range c.expect {
				if !strings.Contains(out, want) {
					t.Errorf("output should mention %q:\n%s", want, out)
				}
			}
		})
	}
}
