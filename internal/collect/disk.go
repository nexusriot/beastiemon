package collect

import (
	"time"

	psutil "github.com/shirou/gopsutil/v3/disk"
)

type DiskCollector struct {
	prev    map[string]psutil.IOCountersStat
	prevAt  time.Time
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
			stats = append(stats, DiskStats{
				Device:    name,
				ReadBps:   round2(float64(cur.ReadBytes-p.ReadBytes) / dt),
				WriteBps:  round2(float64(cur.WriteBytes-p.WriteBytes) / dt),
				ReadIOPS:  round2(float64(cur.ReadCount-p.ReadCount) / dt),
				WriteIOPS: round2(float64(cur.WriteCount-p.WriteCount) / dt),
			})
		}
	}

	d.prev = counters
	d.prevAt = now
	return stats
}
