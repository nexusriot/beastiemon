package collect

import (
	"sort"
	"testing"
)

// psutil returns per-interface and per-device counters in a map, so ranging
// over it reshuffles the emitted array on every tick. That churned
// /api/metrics and every SSE frame, made `beastie --json net` output
// nondeterministic, and randomised the first-seen key order that the rollup
// aggregator uses to keep stored buckets stable. Both collectors now sort.
func TestNetCollectorOutputIsSorted(t *testing.T) {
	c := NewNetCollector(nil)
	c.Collect() // prime: the first pass has no previous sample to diff against
	stats := c.Collect()

	names := make([]string, len(stats))
	for i, s := range stats {
		names[i] = s.Interface
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("net stats not sorted by interface: %v", names)
	}
}

func TestDiskCollectorOutputIsSorted(t *testing.T) {
	var c DiskCollector
	c.Collect() // prime
	stats := c.Collect()

	names := make([]string, len(stats))
	for i, s := range stats {
		names[i] = s.Device
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("disk stats not sorted by device: %v", names)
	}
}
