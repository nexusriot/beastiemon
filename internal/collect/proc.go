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

	// watch is the [collect] proc_watch set, in config order. Counting
	// matches needs a name for every process, not just the top-N, so the
	// extra syscall per process is paid only when this list is non-empty.
	watch []string
}

func NewProcCollector(topN int, watch []string) *ProcCollector {
	if topN <= 0 {
		topN = 5
	}
	return &ProcCollector{topN: topN, watch: watch}
}

// Collect returns the top-N processes by CPU and, when [collect] proc_watch
// is set, a count of running processes per watched name. The counts exist
// because the top-N list cannot answer "is sshd running?" — an idle service
// never ranks, so its absence is indistinguishable from it being quiet.
func (c *ProcCollector) Collect() ([]ProcStat, []ProcWatch) {
	procs, err := psutil.Processes()
	if err != nil {
		return nil, nil
	}
	now := time.Now()
	dt := now.Sub(c.prevAt).Seconds()
	if dt <= 0 {
		dt = 1
	}

	type entry struct {
		p      *psutil.Process
		name   string // resolved up-front only while watching; "" otherwise
		cpuPct float64
	}

	cur := make(map[int32]float64, len(procs))
	first := c.prev == nil
	var entries []entry
	counts := make(map[string]int, len(c.watch))
	watching := len(c.watch) > 0

	for _, p := range procs {
		times, err := p.Times()
		if err != nil {
			continue
		}
		cpuSec := times.User + times.System
		cur[p.Pid] = cpuSec

		var name string
		if watching {
			// Resolve once and reuse below, so watching costs one extra
			// syscall per process rather than two.
			name, _ = p.Name()
			if name != "" {
				counts[name]++
			}
		}
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
		entries = append(entries, entry{p: p, name: name, cpuPct: pct})
	}

	c.prev = cur
	c.prevAt = now

	// Report every watched name, including the ones at zero — a missing key
	// and a count of 0 mean the same thing to an operator, but only the
	// explicit 0 lets a rule fire on it.
	var watch []ProcWatch
	for _, name := range c.watch {
		watch = append(watch, ProcWatch{Name: name, Count: counts[name]})
	}

	// Rank by CPU first; only enrich the survivors with name/memory so we
	// avoid an extra syscall per process for the long tail we won't show.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].cpuPct > entries[j].cpuPct
	})
	// Enrich in rank order and stop at topN. Doing it here rather than by
	// pre-truncating lets skipped entries (see below) fall through to the next
	// candidate instead of shortening the list.
	out := make([]ProcStat, 0, c.topN)
	for _, e := range entries {
		if len(out) == c.topN {
			break
		}
		name := e.name
		if name == "" {
			name, _ = e.p.Name()
		}
		// FreeBSD's idle process accrues one core-second per core per second,
		// so it pins ~100%×ncpu and would permanently own the top slot while
		// telling the operator nothing. top(1) hides it by default too.
		if name == "idle" {
			continue
		}
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
	return out, watch
}
