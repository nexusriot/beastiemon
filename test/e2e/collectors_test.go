//go:build e2e

package e2e

import (
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// gatedRules are rules on the two opt-in collectors that only produce data on
// FreeBSD. They exercise both halves of the "nothing collected" contract: a
// host rule must read unknown rather than 0, and with bhyve off the config
// check must say so — vm.count reads 0, so "fewer than 1 guest" would fire.
const gatedRules = `
[alerts]
[[alerts.rule]]
name      = "vulnerable-packages"
metric    = "host"
op        = ">"
threshold = 0

[[alerts.rule]]
name      = "vms-down"
metric    = "vm"
field     = "count"
op        = "<"
threshold = 1
`

func collectorsOn(cfg string) string {
	return strings.Replace(cfg, "top_procs = 5",
		"top_procs = 5\nbhyve = true\nhost_health = true\nhost_health_interval = \"1h\"", 1)
}

// Off FreeBSD the host-health and bhyve collectors are stubs. Turning them on
// must cost nothing visible: no delay to the first sample (the sampler waits
// up to a few seconds for the first health check), no fabricated health
// section, and a host rule that reads unknown rather than "0 vulnerable".
func TestOptionalCollectorsWithoutFreeBSD(t *testing.T) {
	if runtime.GOOS == "freebsd" {
		t.Skip("the collectors are real on FreeBSD; see freebsd_test.go")
	}
	start := time.Now()
	d := startDaemon(t, collectorsOn(baseConfig())+gatedRules)
	d.waitSample()
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("first sample took %v with host_health on; the stub should not hold it back", took)
	}

	var snap map[string]any
	d.getJSON("/api/metrics", &snap)
	for _, key := range []string{"health", "vms"} {
		if _, ok := snap[key]; ok {
			t.Errorf("/api/metrics carries %q off FreeBSD: %v", key, snap[key])
		}
	}

	var alerts alertsResponse
	d.getJSON("/api/alerts", &alerts)
	if r, ok := alerts.rule("vulnerable-packages"); !ok || r.State != "unknown" {
		t.Errorf("host rule should be unknown with no check result, got %+v", r)
	}

	_, body := d.get("/metrics")
	for _, absent := range []string{"beastie_pkg_vulnerable", "beastie_reboot_required", "beastie_vm_up"} {
		if strings.Contains(string(body), absent) {
			t.Errorf("/metrics exports %s with nothing collected", absent)
		}
	}
	if strings.Contains(d.log(), "needs [collect]") {
		t.Errorf("collectors are on, so no rule should be flagged:\n%s", d.log())
	}
}

// The gated-rule warning is part of the reload path too: switching a
// collector off under a live rule must be logged, and switching it back on
// must clear the complaint.
func TestReloadTogglesOptionalCollectors(t *testing.T) {
	d := startDaemon(t, collectorsOn(baseConfig())+gatedRules)
	d.waitSample()
	on, err := os.ReadFile(d.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	off := strings.Replace(string(on), "bhyve = true\nhost_health = true\n", "", 1)
	if off == string(on) {
		t.Fatal("test bug: could not strip the collector switches")
	}

	if err := os.WriteFile(d.cfgPath, []byte(off), 0o644); err != nil {
		t.Fatal(err)
	}
	d.hup()
	for _, want := range []string{
		`"vms-down": metric "vm" needs [collect] bhyve = true`,
		`"vulnerable-packages": metric "host" needs [collect] host_health = true`,
	} {
		if !d.waitLog(want, 10*time.Second) {
			t.Errorf("reload with the collector off should log %q:\n%s", want, d.log())
		}
	}
	reloads := strings.Count(d.log(), "reloaded configuration")

	if err := os.WriteFile(d.cfgPath, on, 0o644); err != nil {
		t.Fatal(err)
	}
	before := strings.Count(d.log(), "needs [collect]")
	d.hup()
	deadline := time.Now().Add(10 * time.Second)
	for strings.Count(d.log(), "reloaded configuration") == reloads && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if strings.Count(d.log(), "reloaded configuration") == reloads {
		t.Fatalf("second reload never happened:\n%s", d.log())
	}
	if after := strings.Count(d.log(), "needs [collect]"); after != before {
		t.Errorf("reload with the collectors back on still warned:\n%s", d.log())
	}
	if code, _ := d.get("/api/metrics"); code != 200 {
		t.Errorf("daemon stopped serving across reloads: %d", code)
	}
}
