package collect

import (
	"sort"
	"time"

	psutil "github.com/shirou/gopsutil/v3/process"
)

// ProcCollector reports the top-N processes by CPU usage over the sample
// interval. CPU% is derived from the delta in per-process CPU time, the same
// way CPUCollector works for the system total, so it needs a prior sample
// before it can report anything.
type ProcCollector struct {
	topN   int
	prev   map[int32]float64 // pid -> cumulative cpu seconds (user+system)
	prevAt time.Time
}

func NewProcCollector(topN int) *ProcCollector {
	if topN <= 0 {
		topN = 5
	}
	return &ProcCollector{topN: topN}
}

func (c *ProcCollector) Collect() []ProcStat {
	procs, err := psutil.Processes()
	if err != nil {
		return nil
	}
	now := time.Now()
	dt := now.Sub(c.prevAt).Seconds()
	if dt <= 0 {
		dt = 1
	}

	type entry struct {
		p      *psutil.Process
		cpuPct float64
	}

	cur := make(map[int32]float64, len(procs))
	first := c.prev == nil
	var entries []entry

	for _, p := range procs {
		times, err := p.Times()
		if err != nil {
			continue
		}
		cpuSec := times.User + times.System
		cur[p.Pid] = cpuSec
		if first {
			continue
		}
		prevSec, ok := c.prev[p.Pid]
		if !ok {
			continue
		}
		pct := (cpuSec - prevSec) / dt * 100
		if pct < 0 {
			pct = 0
		}
		entries = append(entries, entry{p: p, cpuPct: pct})
	}

	c.prev = cur
	c.prevAt = now

	// Rank by CPU first; only enrich the survivors with name/memory so we
	// avoid an extra syscall per process for the long tail we won't show.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].cpuPct > entries[j].cpuPct
	})
	if len(entries) > c.topN {
		entries = entries[:c.topN]
	}

	out := make([]ProcStat, 0, len(entries))
	for _, e := range entries {
		name, _ := e.p.Name()
		var rss uint64
		if mi, err := e.p.MemoryInfo(); err == nil && mi != nil {
			rss = mi.RSS
		}
		var memPct float64
		if mp, err := e.p.MemoryPercent(); err == nil {
			memPct = float64(mp)
		}
		out = append(out, ProcStat{
			PID:    e.p.Pid,
			Name:   name,
			CPUPct: round2(e.cpuPct),
			MemPct: round2(memPct),
			RSS:    rss,
		})
	}
	return out
}
