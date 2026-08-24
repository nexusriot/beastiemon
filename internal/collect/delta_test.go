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
