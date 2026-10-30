package collect

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParsePkgAuditQ(t *testing.T) {
	n, names := parsePkgAuditQ("curl-8.4.0\nopenssl-3.0.11_1\n\n")
	if n != 2 || len(names) != 2 || names[1] != "openssl-3.0.11_1" {
		t.Fatalf("got %d %v", n, names)
	}
	if n, names := parsePkgAuditQ(""); n != 0 || names != nil {
		t.Fatalf("clean host: got %d %v", n, names)
	}

	// The count stays exact past the cap on names.
	var b strings.Builder
	for i := 0; i < maxVulnPkgs+5; i++ {
		fmt.Fprintf(&b, "pkg%d-1.0\n", i)
	}
	if n, names := parsePkgAuditQ(b.String()); n != maxVulnPkgs+5 || len(names) != maxVulnPkgs {
		t.Fatalf("cap: got %d names for %d", len(names), n)
	}
}

func TestParseBectlList(t *testing.T) {
	out := "default\tNR\t/\t2.1G\t2024-03-01 10:00\n" +
		"14.1-backup\t-\t-\t512M\t2024-06-01 09:00\n"
	n, active, next := parseBectlList(out)
	if n != 2 || active != "default" || next != "default" {
		t.Fatalf("got %d %q %q", n, active, next)
	}

	// After `bectl activate`, N and R sit on different rows.
	out = "default\tN\t/\t2.1G\t2024-03-01 10:00\n" +
		"14.2-upgrade\tR\t-\t3.0G\t2024-12-01 09:00\n"
	n, active, next = parseBectlList(out)
	if n != 2 || active != "default" || next != "14.2-upgrade" {
		t.Fatalf("got %d %q %q", n, active, next)
	}
	h := &HostHealth{BECount: n, BEActive: active, BENext: next}
	if !h.BEPending() {
		t.Error("BEPending should be true when next boot differs")
	}
}

func TestParseBhyvePs(t *testing.T) {
	out := `    1   1024     0:00.10    86400 init     /sbin/init
  812 4194304  125:30.50     3600 bhyve    bhyve: web (bhyve)
  900 2097152     1:02.00      120 bhyve    /usr/sbin/bhyve -c 2 -m 2G -s 0,hostbridge db
  950   2048     0:00.01       10 sh       sh -c echo bhyve: fake
`
	procs := parseBhyvePs(out)
	if len(procs) != 2 {
		t.Fatalf("want 2 bhyve procs, got %+v", procs)
	}
	if procs[0].name != "web" || procs[0].pid != 812 || procs[0].rssKB != 4194304 ||
		procs[0].cpuSec != 125*60+30.5 || procs[0].etime != 3600 {
		t.Errorf("titled guest: %+v", procs[0])
	}
	// Without a title the VM name is bhyve's last argument.
	if procs[1].name != "db" || procs[1].cpuSec != 62 {
		t.Errorf("untitled guest: %+v", procs[1])
	}
}

// ps vis-encodes args and pads columns; the name must come back exactly as
// the guest was created, or it won't match its /dev/vmm entry.
func TestParseBhyvePsOddNames(t *testing.T) {
	out := "  10 2048 0:00.00 5 bhyve    bhyve: x\\\\y (bhyve)\n" +
		"  11 2048 0:00.00 5 bhyve    bhyve: t\\011ab (bhyve)\n" +
		"  12 2048 0:00.00 5 bhyve    bhyve: ctl\\^Ax (bhyve)\n" +
		"  13 2048 0:00.00 5 bhyve    bhyve: a  b (bhyve)\n" +
		"  14 2048 0:00.00 5 bhyve    bhyve: vm-ünï (bhyve)\n"
	want := []string{`x\y`, "t\tab", "ctl\x01x", "a  b", "vm-ünï"}
	procs := parseBhyvePs(out)
	if len(procs) != len(want) {
		t.Fatalf("want %d procs, got %+v", len(want), procs)
	}
	for i, p := range procs {
		if p.name != want[i] {
			t.Errorf("proc %d: name %q, want %q", i, p.name, want[i])
		}
	}
}

func TestUnvis(t *testing.T) {
	cases := map[string]string{
		`plain`:     "plain",
		`a\\b`:      `a\b`,
		`\101\102`:  "AB",
		`\^?`:       "\x7f",
		`\M-a`:      "\xe1",
		`\M^A`:      "\x81",
		`trailing\`: `trailing\`,
		`\q`:        `\q`,
	}
	for in, want := range cases {
		if got := unvis(in); got != want {
			t.Errorf("unvis(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParsePsCPUTime(t *testing.T) {
	cases := map[string]float64{
		"0:01.50":    1.5,
		"125:30.50":  7530.5,
		"1:02:03.00": 3723,
		"2-01:00:00": 2*86400 + 3600,
		"garbage":    0,
	}
	for in, want := range cases {
		if got := parsePsCPUTime(in); got != want {
			t.Errorf("parsePsCPUTime(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestBuildVMStats(t *testing.T) {
	procs := []bhyveProc{
		{pid: 812, name: "web", rssKB: 1024, cpuSec: 103, etime: 60},
		{pid: 900, name: "db", rssKB: 2048, cpuSec: 50},
	}
	prev := map[int32]float64{812: 100}
	vms, cur := buildVMStats(procs, []string{"web", "db", "stale"}, prev, 2)

	if len(vms) != 3 || vms[0].Name != "db" || vms[1].Name != "stale" || vms[2].Name != "web" {
		t.Fatalf("want sorted db/stale/web, got %+v", vms)
	}
	// 3 CPU-seconds over 2 s is 1.5 cores.
	if vms[2].CPUPct != 150 || vms[2].RSS != 1024*1024 || vms[2].State != "running" {
		t.Errorf("web: %+v", vms[2])
	}
	// No previous sample for db, so no rate yet rather than a huge one.
	if vms[0].CPUPct != 0 {
		t.Errorf("db first sample: %+v", vms[0])
	}
	if vms[1].State != "orphaned" || vms[1].PID != 0 {
		t.Errorf("stale vmm instance: %+v", vms[1])
	}
	if cur[812] != 103 || cur[900] != 50 {
		t.Errorf("cur: %v", cur)
	}
}

func TestHostHealthCollectorRefreshes(t *testing.T) {
	calls := 0
	h := &HostHealthCollector{
		every: 10 * time.Millisecond,
		ready: make(chan struct{}),
		check: func(context.Context) *HostHealth {
			calls++
			return &HostHealth{Vulnerable: calls, VulnKnown: true}
		},
	}
	if h.Latest() != nil {
		t.Fatal("Latest before the first check should be nil")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx)

	select {
	case <-h.Ready():
	case <-time.After(time.Second):
		t.Fatal("first check never finished")
	}
	deadline := time.Now().Add(time.Second)
	for h.Latest().Vulnerable < 2 {
		if time.Now().After(deadline) {
			t.Fatal("collector did not refresh on its interval")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
