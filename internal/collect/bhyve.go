package collect

import (
	"sort"
	"time"
)

// VMCollector reports bhyve guests. CPU% is a delta of each bhyve process's
// cumulative CPU time, like ProcCollector, so it needs one prior sample.
type VMCollector struct {
	prev   map[int32]float64
	prevAt time.Time
}

func (c *VMCollector) Collect() []VMStat {
	procs, vmm, ok := listBhyve()
	if !ok {
		return nil
	}
	now := time.Now()
	dt := now.Sub(c.prevAt).Seconds()
	vms, cur := buildVMStats(procs, vmm, c.prev, dt)
	c.prev, c.prevAt = cur, now
	return vms
}

func sortVMs(vms []VMStat) {
	sort.Slice(vms, func(i, j int) bool { return vms[i].Name < vms[j].Name })
}
