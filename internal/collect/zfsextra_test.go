package collect

import (
	"testing"
	"time"
)

// Real `zpool status` output covering every scan state: a finished scrub, one
// in progress (whose percentage sits on a continuation line), and a pool that
// has never been scrubbed.
const zpoolStatusSample = `  pool: zroot
 state: ONLINE
  scan: scrub repaired 0B in 00:05:23 with 0 errors on Sun Oct 12 03:05:24 2025
config:

	NAME        STATE     READ WRITE CKSUM
	zroot       ONLINE       0     0     0

errors: No known data errors

  pool: tank
 state: DEGRADED
  scan: scrub in progress since Sun Oct 19 03:00:01 2025
	1.23T scanned at 1.20G/s, 456G issued at 445M/s, 5.00T total
	0B repaired, 9.12% done, 02:34:56 to go
config:

	NAME        STATE     READ WRITE CKSUM
	tank        DEGRADED     0     0     0

errors: No known data errors

  pool: cold
 state: ONLINE
  scan: none requested
config:

	NAME        STATE     READ WRITE CKSUM
	cold        ONLINE       0     0     0

errors: No known data errors
`

func TestParseZpoolStatusScans(t *testing.T) {
	// Two days after the zroot scrub finished.
	now := time.Date(2025, 10, 14, 3, 5, 24, 0, time.Local)
	scans := parseZpoolStatusScans(zpoolStatusSample, now)

	if len(scans) != 3 {
		t.Fatalf("want 3 pools, got %d: %+v", len(scans), scans)
	}
	z := scans["zroot"]
	if z.state != "scrub_done" || !z.known {
		t.Fatalf("zroot: want a known completed scrub, got %+v", z)
	}
	if z.ageHours != 48 {
		t.Errorf("zroot scrub age = %v hours, want 48", z.ageHours)
	}
	tank := scans["tank"]
	if tank.state != "scrub" {
		t.Errorf("tank state = %q, want scrub", tank.state)
	}
	// The percentage lives two lines below "scan:".
	if tank.pct != 9.12 {
		t.Errorf("tank progress = %v, want 9.12", tank.pct)
	}
	cold := scans["cold"]
	if cold.state != "none" {
		t.Errorf("cold state = %q, want none", cold.state)
	}
	// "Never scrubbed" must not masquerade as "scrubbed 0 hours ago": a rule
	// on scan_age_hours would read that as freshly clean.
	if cold.known {
		t.Error("a pool that has never been scrubbed has no known scan age")
	}
}

func TestParseZpoolStatusResilver(t *testing.T) {
	now := time.Date(2025, 10, 12, 4, 5, 24, 0, time.Local)
	out := "  pool: tank\n  scan: resilvered 1.20G in 00:01:23 with 0 errors on Sun Oct 12 03:05:24 2025\nconfig:\n"
	s := parseZpoolStatusScans(out, now)["tank"]
	if s.state != "resilver_done" || !s.known || s.ageHours != 1 {
		t.Fatalf("want a resilver finished 1h ago, got %+v", s)
	}
}

// A completion timestamp in the future means the clock moved, not that the
// scrub happens later; reporting a negative age would read as "overdue by
// minus a week" to any rule comparing it.
func TestParseZpoolStatusClockSkew(t *testing.T) {
	now := time.Date(2025, 10, 1, 0, 0, 0, 0, time.Local)
	out := "  pool: tank\n  scan: scrub repaired 0B in 00:05:23 with 0 errors on Sun Oct 12 03:05:24 2025\nconfig:\n"
	s := parseZpoolStatusScans(out, now)["tank"]
	if s.ageHours != 0 {
		t.Errorf("want age clamped to 0 under clock skew, got %v", s.ageHours)
	}
}

func TestParseZfsList(t *testing.T) {
	out := "zroot\t1000\t5000\t900\t/\n" +
		"zroot/var\t9000\t5000\t8000\t/var\n" +
		"zroot/snap\t50\t5000\t50\t-\n"

	// Largest first, capped at the limit.
	sets := parseZfsList(out, 2)
	if len(sets) != 2 {
		t.Fatalf("want 2 datasets, got %d", len(sets))
	}
	if sets[0].Name != "zroot/var" || sets[0].Used != 9000 {
		t.Fatalf("want the largest dataset first, got %+v", sets[0])
	}
	if sets[0].Mount != "/var" {
		t.Errorf("mount = %q, want /var", sets[0].Mount)
	}

	// "-" is zfs's placeholder for an unset mountpoint, not a path.
	all := parseZfsList(out, 10)
	if len(all) != 3 {
		t.Fatalf("want all 3 datasets, got %d", len(all))
	}
	if all[2].Name != "zroot/snap" || all[2].Mount != "" {
		t.Errorf("unmounted dataset: %+v", all[2])
	}

	// 0 means "collect none", which is the default.
	if got := parseZfsList(out, 0); got != nil {
		t.Errorf("limit 0 should collect nothing, got %v", got)
	}
}
