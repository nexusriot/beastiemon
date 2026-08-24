package collect

import "testing"

func TestParseZpoolList(t *testing.T) {
	// `zpool list -Hp -o name,size,alloc,free,capacity,health` (tab-separated).
	out := "tank\t1000000000\t400000000\t600000000\t40\tONLINE\n" +
		"backup\t2000000000\t0\t2000000000\t0\tDEGRADED\n"
	pools := parseZpoolList(out)
	if len(pools) != 2 {
		t.Fatalf("want 2 pools, got %d", len(pools))
	}
	if pools[0].Pool != "tank" || pools[0].Size != 1000000000 ||
		pools[0].Alloc != 400000000 || pools[0].Free != 600000000 ||
		pools[0].UsedPct != 40 || pools[0].Health != "ONLINE" {
		t.Fatalf("pool[0] mismatch: %+v", pools[0])
	}
	// capacity=0 with a non-zero size should fall back to alloc/size.
	if pools[1].UsedPct != 0 || pools[1].Health != "DEGRADED" {
		t.Fatalf("pool[1] mismatch: %+v", pools[1])
	}
}

func TestArcFromKstats(t *testing.T) {
	arc := arcFromKstats(100, 200, 400, 750, 250)
	if arc.Size != 100 || arc.Target != 200 || arc.Max != 400 {
		t.Fatalf("arc fields mismatch: %+v", arc)
	}
	if arc.HitRate != 75 {
		t.Fatalf("want hit rate 75, got %v", arc.HitRate)
	}
	// No traffic yet must not divide by zero.
	if got := arcFromKstats(1, 1, 1, 0, 0); got.HitRate != 0 {
		t.Fatalf("want 0 hit rate with no hits/misses, got %v", got.HitRate)
	}
}

func TestParseJlsN(t *testing.T) {
	out := "jid=1 name=web host.hostname=web.local path=/jails/web\n" +
		"jid=2 name=db host.hostname=db.local path=/jails/db\n"
	jails := parseJlsN(out)
	if len(jails) != 2 {
		t.Fatalf("want 2 jails, got %d", len(jails))
	}
	if jails[0].JID != 1 || jails[0].Name != "web" ||
		jails[0].Host != "web.local" || jails[0].Path != "/jails/web" {
		t.Fatalf("jail[0] mismatch: %+v", jails[0])
	}
}

func TestParsePsJIDCounts(t *testing.T) {
	// `ps -axo jid=` — one JID per line, 0 = host.
	counts := parsePsJIDCounts("0\n0\n1\n2\n2\n2\n")
	if counts[0] != 2 || counts[1] != 1 || counts[2] != 3 {
		t.Fatalf("counts mismatch: %+v", counts)
	}
}
