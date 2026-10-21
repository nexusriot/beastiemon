package collect

import "testing"

func TestDeltaU64(t *testing.T) {
	cases := []struct {
		name      string
		cur, prev uint64
		want      float64
	}{
		{"normal increase", 1500, 1000, 500},
		{"no change", 1000, 1000, 0},
		{"counter reset clamps to zero", 100, 1000, 0},
		{"reset from large counter", 42, 1 << 60, 0},
	}
	for _, c := range cases {
		if got := deltaU64(c.cur, c.prev); got != c.want {
			t.Errorf("%s: deltaU64(%d, %d) = %v, want %v", c.name, c.cur, c.prev, got, c.want)
		}
	}
}

// round2 used to go through `int`, which is 32 bits on the 386/arm FreeBSD
// targets the Makefile can build. Any throughput past ~21 MB/s overflowed the
// conversion there and came back as a large negative number, which then flowed
// into /api/metrics, the Prometheus exporter, stored rollups, and net/disk
// alert thresholds. Guard the ranges a saturated link actually produces.
func TestRound2HandlesLargeAndNegativeValues(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want float64
	}{
		{"small", 42.126, 42.13},
		{"half rounds up", 0.005, 0.01},
		{"100 Mbit/s in bytes", 12_500_000, 12_500_000},
		{"1 Gbit/s in bytes", 125_000_000, 125_000_000},
		{"10 Gbit/s in bytes", 1_250_000_000, 1_250_000_000},
		{"packets per second", 1_488_095, 1_488_095},
		{"negative rounds away from zero", -0.005, -0.01},
		{"negative large", -125_000_000, -125_000_000},
		{"zero", 0, 0},
	}
	for _, c := range cases {
		if got := round2(c.in); got != c.want {
			t.Errorf("%s: round2(%v) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}
