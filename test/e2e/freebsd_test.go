//go:build e2e && freebsd

// The FreeBSD half of the suite: the collectors that are stubs everywhere
// else, driven against the real base-system tools. Both bugs the first live
// runs found — a ps column list FreeBSD reads differently, and ps escaping in
// guest names — passed every unit test and the container suite, because only
// a FreeBSD ps produces that output.
//
// It needs root (to create a ZFS pool, a jail and the guest stand-ins), and
// runs the daemon as _beastie when that user exists, so it sees the
// permissions an installed daemon has. Run it with `make e2e-freebsd`. Each
// test creates what it uses and removes it again; a test whose tooling is
// missing (no zfs.ko, no cc) skips rather than fails.
package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// requireRoot skips unless the suite runs as root, and returns the
// credential to run the daemon under: _beastie if installed, else root.
func requireRoot(t *testing.T) *syscall.Credential {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("the FreeBSD e2e tests need root")
	}
	u, err := user.Lookup("_beastie")
	if err != nil {
		t.Log("no _beastie user; running the daemon as root")
		return nil
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	// operator is what the package adds _beastie to, for devstat(3).
	groups := []uint32{5}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: groups}
}

func requireTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not available", tool)
		}
	}
}

// must executes a setup command and fails the test on error.
func must(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// startAs is startDaemon run as cred.
func startAs(t *testing.T, cred *syscall.Credential, cfg string) *daemon {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	return startDaemonOpts(t, addr, strings.ReplaceAll(cfg, "%ADDR%", addr), daemonOpts{cred: cred})
}

// bsdSnap is the part of /api/metrics these tests read.
type bsdSnap struct {
	Health *struct {
		CheckedAt       time.Time `json:"checked_at"`
		VulnKnown       bool      `json:"vuln_known"`
		Vulnerable      int       `json:"vulnerable"`
		VulnPkgs        []string  `json:"vuln_pkgs"`
		VulnDBAgeHours  float64   `json:"vulndb_age_hours"`
		KernelKnown     bool      `json:"kernel_known"`
		RunningKernel   string    `json:"running_kernel"`
		InstalledKernel string    `json:"installed_kernel"`
		RebootRequired  bool      `json:"reboot_required"`
		BECount         int       `json:"be_count"`
	} `json:"health"`
	VMs []struct {
		Name   string  `json:"name"`
		State  string  `json:"state"`
		PID    int     `json:"pid"`
		CPUPct float64 `json:"cpu_pct"`
	} `json:"vms"`
	ZFS []struct {
		Pool   string `json:"pool"`
		Health string `json:"health"`
		Scan   string `json:"scan"`
	} `json:"zfs"`
	Datasets []struct {
		Name string `json:"name"`
	} `json:"datasets"`
	ARC   *struct{ Size uint64 } `json:"arc"`
	Jails []struct {
		Name  string `json:"name"`
		Procs int    `json:"procs"`
	} `json:"jails"`
}

func (d *daemon) bsdSnap() bsdSnap {
	d.t.Helper()
	var s bsdSnap
	d.getJSON("/api/metrics", &s)
	return s
}

func (d *daemon) ruleState(name string) string {
	d.t.Helper()
	var a alertsResponse
	d.getJSON("/api/alerts", &a)
	r, _ := a.rule(name)
	return r.State
}

// promLine matches one sample line of the text exposition format, with label
// values limited to the three escapes the format defines.
var promLine = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*` +
	`(\{[a-zA-Z_][a-zA-Z0-9_]*="(?:[^"\\\n]|\\[\\"n])*"(,[a-zA-Z_][a-zA-Z0-9_]*="(?:[^"\\\n]|\\[\\"n])*")*\})?` +
	` -?[0-9.eE+]+$`)

func checkExposition(t *testing.T, d *daemon) string {
	t.Helper()
	_, body := d.get("/metrics")
	for _, l := range strings.Split(string(body), "\n") {
		if l != "" && !strings.HasPrefix(l, "#") && !promLine.MatchString(l) {
			t.Errorf("unparseable exposition line: %q", l)
		}
	}
	return string(body)
}

// vuxml writes a vulnerability database. The one entry marks the pkg package
// itself — installed on every host this can run on — so the expected result
// does not depend on what else the host has installed.
func vuxml(t *testing.T, path string, withEntry bool, age time.Duration) {
	t.Helper()
	body := `<?xml version="1.0" encoding="utf-8"?>
<vuxml xmlns="http://www.vuxml.org/apps/vuxml-1">
`
	if withEntry {
		body += `  <vuln vid="beeee000-0000-4000-8000-000000000001">
    <topic>beastiemon e2e: marks pkg vulnerable</topic>
    <affects><package><name>pkg</name><range><ge>0</ge></range></package></affects>
    <description><body xmlns="http://www.w3.org/1999/xhtml"><p>test entry</p></body></description>
    <references><url>https://example.invalid/</url></references>
    <dates><discovery>2026-01-01</discovery><entry>2026-01-01</entry></dates>
  </vuln>
`
	}
	body += "</vuxml>\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-age)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func TestFreeBSDHostHealth(t *testing.T) {
	cred := requireRoot(t)
	requireTools(t, "pkg", "freebsd-version")

	dir := sharedTempDir(t)
	vuln := filepath.Join(dir, "vuln.xml")
	clean := filepath.Join(dir, "clean.xml")
	vuxml(t, vuln, true, 100*time.Hour)
	vuxml(t, clean, false, time.Hour)

	cfgFor := func(db string) string {
		return strings.Replace(baseConfig(), "top_procs = 5",
			"top_procs = 5\nhost_health = true\nhost_health_interval = \"1h\"\n"+
				fmt.Sprintf("host_health_vulnxml = %q", db), 1) + `
[alerts]
[[alerts.rule]]
name      = "vulnerable-packages"
metric    = "host"
op        = ">"
threshold = 0

[[alerts.rule]]
name      = "vuln-db-stale"
metric    = "host"
field     = "vulndb_age_hours"
op        = ">"
threshold = 72
`
	}
	d := startAs(t, cred, cfgFor(vuln))
	d.waitSample()

	// switchDB points the check at another database and reloads; the new
	// sampler runs the check at once, so CheckedAt moves.
	switchDB := func(db string) {
		t.Helper()
		before := d.bsdSnap().Health.CheckedAt
		if err := os.WriteFile(d.cfgPath, []byte(strings.ReplaceAll(cfgFor(db), "%ADDR%", d.addr)), 0o644); err != nil {
			t.Fatal(err)
		}
		d.hup()
		if !waitFor(15*time.Second, func() bool {
			h := d.bsdSnap().Health
			return h != nil && h.CheckedAt.After(before)
		}) {
			t.Fatalf("no fresh health check after switching to %s:\n%s", db, d.log())
		}
	}

	t.Run("vulnerable package found", func(t *testing.T) {
		if !waitFor(15*time.Second, func() bool { return d.bsdSnap().Health != nil }) {
			t.Fatalf("no health result:\n%s", d.log())
		}
		h := d.bsdSnap().Health
		if !h.VulnKnown || h.Vulnerable < 1 {
			t.Fatalf("pkg audit as the daemon user should flag pkg: %+v", h)
		}
		found := false
		for _, p := range h.VulnPkgs {
			found = found || strings.HasPrefix(p, "pkg-")
		}
		if !found {
			t.Errorf("vulnerable list should name pkg: %v", h.VulnPkgs)
		}
		if h.VulnDBAgeHours < 99 || h.VulnDBAgeHours > 102 {
			t.Errorf("vuln.xml age = %vh, want ~100h", h.VulnDBAgeHours)
		}
		for _, r := range []string{"vulnerable-packages", "vuln-db-stale"} {
			if !waitFor(5*time.Second, func() bool { return d.ruleState(r) == "firing" }) {
				t.Errorf("rule %s should fire, is %s", r, d.ruleState(r))
			}
		}
		body := checkExposition(t, d)
		if !strings.Contains(body, "beastie_pkg_vulnerable ") {
			t.Error("/metrics is missing beastie_pkg_vulnerable")
		}
	})

	t.Run("kernel and boot environments match the host", func(t *testing.T) {
		h := d.bsdSnap().Health
		running := strings.TrimSpace(must(t, "freebsd-version", "-r"))
		installed := strings.TrimSpace(must(t, "freebsd-version", "-k"))
		if !h.KernelKnown || h.RunningKernel != running || h.InstalledKernel != installed ||
			h.RebootRequired != (running != installed) {
			t.Errorf("kernel: got %+v, host says running %s installed %s", h, running, installed)
		}
		// bectl only works on a ZFS-root host; whatever it says for the
		// daemon's user is what the daemon must report.
		want := 0
		cmd := exec.Command("bectl", "list", "-H")
		if cred != nil {
			cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
		}
		if out, err := cmd.Output(); err == nil {
			want = len(strings.Split(strings.TrimSpace(string(out)), "\n"))
		}
		if h.BECount != want {
			t.Errorf("be_count = %d, bectl lists %d", h.BECount, want)
		}
	})

	t.Run("clean database", func(t *testing.T) {
		switchDB(clean)
		h := d.bsdSnap().Health
		if !h.VulnKnown || h.Vulnerable != 0 {
			t.Errorf("an empty vuln.xml should be a known 0: %+v", h)
		}
		if !waitFor(5*time.Second, func() bool { return d.ruleState("vulnerable-packages") == "ok" }) {
			t.Errorf("rule should resolve, is %s", d.ruleState("vulnerable-packages"))
		}
	})

	t.Run("missing database reads unknown", func(t *testing.T) {
		switchDB(filepath.Join(dir, "does-not-exist.xml"))
		if h := d.bsdSnap().Health; h.VulnKnown || !h.KernelKnown {
			t.Errorf("only the pkg part should be unknown: %+v", h)
		}
		if s := d.ruleState("vulnerable-packages"); s != "unknown" {
			t.Errorf("rule should be unknown, is %s", s)
		}
		if body := checkExposition(t, d); strings.Contains(body, "beastie_pkg_vulnerable ") {
			t.Error("an unknown vulnerable count must not be exported as a number")
		}
	})

	t.Run("standalone CLI as the daemon user", func(t *testing.T) {
		switchDB(vuln)
		out, errOut, code := runCLIAs(t, cred, "-config", d.cfgPath, "health")
		if code != 0 || !strings.Contains(out, "vulnerable") || !strings.Contains(out, "KERNEL") {
			t.Errorf("beastie health: exit %d\n%s%s", code, out, errOut)
		}
	})
}

// fakeBhyve builds a stand-in that titles itself the way bhyve does
// (setproctitle(vmname)), so ps shows it exactly as a real guest. bhyve itself
// needs VT-x, which CI and most test VMs do not have.
func fakeBhyve(t *testing.T) string {
	t.Helper()
	requireTools(t, "cc")
	dir := sharedTempDir(t)
	src := filepath.Join(dir, "bhyve.c")
	err := os.WriteFile(src, []byte(`#include <sys/types.h>
#include <unistd.h>
#include <string.h>
int main(int argc, char **argv) {
	if (argc < 3) return 2;
	setproctitle("%s", argv[2]);
	if (strcmp(argv[1], "busy") == 0) { volatile unsigned long x = 0; for (;;) x++; }
	for (;;) sleep(60);
}
`), 0o644)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bhyve")
	must(t, "cc", "-O0", "-o", bin, src)
	return bin
}

func startGuest(t *testing.T, bin, mode, name string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, mode, name)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting guest %q: %v", name, err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return cmd
}

func TestFreeBSDBhyveGuests(t *testing.T) {
	cred := requireRoot(t)
	bin := fakeBhyve(t)

	// Names that ps(1) escapes or pads, which must come back exactly as the
	// guest was created or they stop matching their /dev/vmm entries.
	odd := []string{`x\y`, "t\tab", "ctl\x01x", "a  b", `we"b`, "<img src=x>", "vm-ünï"}
	startGuest(t, bin, "busy", "web")
	db := startGuest(t, bin, "idle", "db")
	for _, n := range odd {
		startGuest(t, bin, "idle", n)
	}
	total := 2 + len(odd)

	cfg := strings.Replace(baseConfig(), "top_procs = 5", "top_procs = 5\nbhyve = true", 1) +
		fmt.Sprintf(`
[alerts]
[[alerts.rule]]
name      = "web-busy"
metric    = "vm"
field     = "web"
op        = ">"
threshold = 20

[[alerts.rule]]
name      = "vms-down"
metric    = "vm"
field     = "count"
op        = "<"
threshold = %d
`, total)
	d := startAs(t, cred, cfg)
	d.waitSample()

	running := func() map[string]int {
		got := map[string]int{}
		for _, v := range d.bsdSnap().VMs {
			if v.State == "running" {
				got[v.Name] = v.PID
			}
		}
		return got
	}

	t.Run("every guest by its exact name", func(t *testing.T) {
		if !waitFor(10*time.Second, func() bool { return len(running()) == total }) {
			t.Fatalf("want %d guests, got %q", total, running())
		}
		got := running()
		for _, n := range append([]string{"web", "db"}, odd...) {
			if _, ok := got[n]; !ok {
				t.Errorf("guest %q missing or renamed; have %q", n, got)
			}
		}
		if s := d.ruleState("vms-down"); s != "ok" {
			t.Errorf("vms-down with every guest up: %s", s)
		}
	})

	t.Run("CPU of a busy guest", func(t *testing.T) {
		if !waitFor(15*time.Second, func() bool { return d.ruleState("web-busy") == "firing" }) {
			t.Errorf("web-busy never fired:\n%+v", d.bsdSnap().VMs)
		}
		var s series
		d.getJSON("/api/series?metric=vm&range=1m&name=web", &s)
		if !hasLabel(s, "web") {
			t.Errorf("vm series is missing web: %v", s.Labels)
		}
	})

	t.Run("escaped names in Prometheus", func(t *testing.T) {
		body := checkExposition(t, d)
		for _, want := range []string{`beastie_vm_up{vm="x\\y"} 1`, `beastie_vm_up{vm="we\"b"} 1`} {
			if !strings.Contains(body, want) {
				t.Errorf("/metrics is missing %s", want)
			}
		}
	})

	t.Run("a guest that exits and comes back", func(t *testing.T) {
		oldPID := running()["db"]
		db.Process.Kill()
		db.Wait()
		if !waitFor(10*time.Second, func() bool { return d.ruleState("vms-down") == "firing" }) {
			t.Errorf("vms-down should fire with db gone; guests %q", running())
		}
		startGuest(t, bin, "idle", "db")
		if !waitFor(10*time.Second, func() bool { return d.ruleState("vms-down") == "ok" }) {
			t.Errorf("vms-down should resolve once db is back; guests %q", running())
		}
		if pid := running()["db"]; pid == oldPID || pid == 0 {
			t.Errorf("db should be tracked under its new pid, have %d (old %d)", pid, oldPID)
		}
		for _, v := range d.bsdSnap().VMs {
			if v.CPUPct < 0 || v.CPUPct > 100*float64(numCPU(t))+1 {
				t.Errorf("guest %q CPU %v is out of range after the restart", v.Name, v.CPUPct)
			}
		}
	})

	t.Run("orphaned vmm instance", func(t *testing.T) {
		// Needs vmm.ko working, which needs VT-x/AMD-V.
		if _, err := os.Stat("/dev/vmm"); err != nil {
			t.Skip("no /dev/vmm (vmm.ko not loaded or no hardware virtualisation)")
		}
		name := fmt.Sprintf("bme2e-orphan-%d", os.Getpid())
		if out, err := exec.Command("bhyvectl", "--create", "--vm="+name).CombinedOutput(); err != nil {
			t.Skipf("bhyvectl --create: %v %s", err, out)
		}
		t.Cleanup(func() { exec.Command("bhyvectl", "--destroy", "--vm="+name).Run() })
		if !waitFor(10*time.Second, func() bool {
			for _, v := range d.bsdSnap().VMs {
				if v.Name == name && v.State == "orphaned" {
					return true
				}
			}
			return false
		}) {
			t.Errorf("a vmm instance with no process should show as orphaned: %+v", d.bsdSnap().VMs)
		}
	})
}

func numCPU(t *testing.T) int {
	n, err := strconv.Atoi(strings.TrimSpace(must(t, "sysctl", "-n", "hw.ncpu")))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

func TestFreeBSDZFSAndJails(t *testing.T) {
	cred := requireRoot(t)
	requireTools(t, "zpool", "zfs", "jail", "jexec")
	if exec.Command("kldstat", "-q", "-m", "zfs").Run() != nil {
		t.Skip("zfs.ko is not loaded")
	}

	dir := sharedTempDir(t)
	pool := fmt.Sprintf("bme2e%d", os.Getpid())
	a, b := filepath.Join(dir, "a.img"), filepath.Join(dir, "b.img")
	for _, f := range []string{a, b} {
		must(t, "truncate", "-s", "128m", f)
	}
	must(t, "zpool", "create", "-m", filepath.Join(dir, "mnt"), pool, "mirror", a, b)
	t.Cleanup(func() { exec.Command("zpool", "destroy", "-f", pool).Run() })
	must(t, "zfs", "create", pool+"/data")

	jname := fmt.Sprintf("bme2e%d", os.Getpid())
	must(t, "jail", "-c", "name="+jname, "path=/", "host.hostname="+jname+".test", "persist")
	t.Cleanup(func() { exec.Command("jail", "-r", jname).Run() })
	sleeper := exec.Command("jexec", jname, "/bin/sleep", "600")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sleeper.Process.Kill(); sleeper.Wait() })

	cfg := strings.Replace(baseConfig(), "top_procs = 5",
		"top_procs = 5\nzfs = true\nzfs_datasets = 20\njails = true", 1) + `
[alerts]
[[alerts.rule]]
name      = "pool-degraded"
metric    = "zfs"
field     = "health"
op        = ">="
threshold = 1
`
	d := startAs(t, cred, cfg)
	d.waitSample()

	poolHealth := func() string {
		for _, z := range d.bsdSnap().ZFS {
			if z.Pool == pool {
				return z.Health
			}
		}
		return ""
	}

	t.Run("pool, datasets, ARC and jail", func(t *testing.T) {
		if !waitFor(10*time.Second, func() bool { return poolHealth() == "ONLINE" }) {
			t.Fatalf("pool %s not reported ONLINE: %+v", pool, d.bsdSnap().ZFS)
		}
		s := d.bsdSnap()
		var sawData, sawJail bool
		for _, ds := range s.Datasets {
			sawData = sawData || ds.Name == pool+"/data"
		}
		for _, j := range s.Jails {
			sawJail = sawJail || (j.Name == jname && j.Procs >= 1)
		}
		if !sawData {
			t.Errorf("dataset %s/data missing: %+v", pool, s.Datasets)
		}
		if !sawJail {
			t.Errorf("jail %s with its sleeper missing: %+v", jname, s.Jails)
		}
		if s.ARC == nil || s.ARC.Size == 0 {
			t.Errorf("ARC not reported: %+v", s.ARC)
		}
	})

	t.Run("degraded and recovered", func(t *testing.T) {
		must(t, "zpool", "offline", pool, b)
		if !waitFor(10*time.Second, func() bool {
			return poolHealth() == "DEGRADED" && d.ruleState("pool-degraded") == "firing"
		}) {
			t.Errorf("offlining a mirror side: health %s, rule %s", poolHealth(), d.ruleState("pool-degraded"))
		}
		if body := checkExposition(t, d); !strings.Contains(body,
			fmt.Sprintf(`beastie_zfs_health{pool=%q,state="DEGRADED"} 1`, pool)) {
			t.Error("/metrics does not show the pool degraded")
		}
		must(t, "zpool", "online", pool, b)
		if !waitFor(15*time.Second, func() bool {
			return poolHealth() == "ONLINE" && d.ruleState("pool-degraded") == "ok"
		}) {
			t.Errorf("onlining it again: health %s, rule %s", poolHealth(), d.ruleState("pool-degraded"))
		}
	})

	t.Run("scrub state", func(t *testing.T) {
		must(t, "zpool", "scrub", "-w", pool)
		if !waitFor(10*time.Second, func() bool {
			for _, z := range d.bsdSnap().ZFS {
				if z.Pool == pool && z.Scan == "scrub_done" {
					return true
				}
			}
			return false
		}) {
			t.Errorf("finished scrub not reported: %+v", d.bsdSnap().ZFS)
		}
	})
}
