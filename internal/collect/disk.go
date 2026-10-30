package collect

import (
	"math"
	"sort"
	"time"

	psutil "github.com/shirou/gopsutil/v3/disk"
)

type DiskCollector struct {
	prev   map[string]psutil.IOCountersStat
	prevAt time.Time
}

func (d *DiskCollector) Collect() []DiskStats {
	counters, err := psutil.IOCounters()
	if err != nil || len(counters) == 0 {
		return nil
	}
	now := time.Now()

	var stats []DiskStats
	if d.prev != nil {
		dt := now.Sub(d.prevAt).Seconds()
		if dt <= 0 {
			dt = 1
		}
		for name, cur := range counters {
			p, ok := d.prev[name]
			if !ok {
				continue
			}
			stats = append(stats, diskDelta(name, cur, p, dt))
		}
	}

	// Same reason as NetCollector: psutil hands back a map, so sort by device
	// to keep the emitted array order stable from tick to tick.
	sort.Slice(stats, func(i, j int) bool { return stats[i].Device < stats[j].Device })

	d.prev = counters
	d.prevAt = now
	return stats
}

// diskDelta turns two counter readings dt seconds apart into rates. gopsutil
// reports ReadTime, WriteTime and IoTime in milliseconds on both FreeBSD
// (devstat durations and busy_time) and Linux (/proc/diskstats).
func diskDelta(name string, cur, prev psutil.IOCountersStat, dt float64) DiskStats {
	reads := deltaU64(cur.ReadCount, prev.ReadCount)
	writes := deltaU64(cur.WriteCount, prev.WriteCount)
	return DiskStats{
		Device:    name,
		ReadBps:   round2(deltaU64(cur.ReadBytes, prev.ReadBytes) / dt),
		WriteBps:  round2(deltaU64(cur.WriteBytes, prev.WriteBytes) / dt),
		ReadIOPS:  round2(reads / dt),
		WriteIOPS: round2(writes / dt),
		// Busy time can run a hair past wall time when the two readings
		// straddle the tick unevenly, so clamp to what gstat would show.
		BusyPct: round2(math.Min(100, deltaU64(cur.IoTime, prev.IoTime)/(dt*1000)*100)),
		ReadMs:  round2(perOp(deltaU64(cur.ReadTime, prev.ReadTime), reads)),
		WriteMs: round2(perOp(deltaU64(cur.WriteTime, prev.WriteTime), writes)),
	}
}

// perOp is total/ops, or 0 for an idle interval rather than NaN.
func perOp(total, ops float64) float64 {
	if ops == 0 {
		return 0
	}
	return total / ops
}
