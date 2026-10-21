package collect

import (
	"math"

	psutil "github.com/shirou/gopsutil/v3/cpu"
)

type CPUCollector struct {
	prev []psutil.TimesStat
}

func (c *CPUCollector) Collect() CPUStats {
	times, err := psutil.Times(true)
	if err != nil || len(times) == 0 {
		return CPUStats{}
	}

	stats := CPUStats{}

	if len(c.prev) == len(times) {
		perCore := make([]float64, len(times))
		var sumUser, sumSys, sumIdle, sumAll float64

		for i, cur := range times {
			p := c.prev[i]
			dUser := cur.User - p.User
			dSys := cur.System - p.System
			dNice := cur.Nice - p.Nice
			dIrq := cur.Irq - p.Irq
			dSoft := cur.Softirq - p.Softirq
			dIdle := cur.Idle - p.Idle
			dUsed := dUser + dSys + dNice + dIrq + dSoft
			dTotal := dUsed + dIdle
			if dTotal > 0 {
				perCore[i] = round2(dUsed / dTotal * 100)
			}
			sumUser += dUser
			sumSys += dSys
			sumIdle += dIdle
			sumAll += dTotal
		}

		if sumAll > 0 {
			stats.Total = round2((sumAll - sumIdle) / sumAll * 100)
			stats.User = round2(sumUser / sumAll * 100)
			stats.Sys = round2(sumSys / sumAll * 100)
			stats.Idle = round2(sumIdle / sumAll * 100)
		}
		stats.PerCore = perCore
	}

	c.prev = times
	return stats
}

// round2 rounds to two decimal places. It goes through math.Round rather than
// an int conversion: `int` is 32 bits on the 386/arm FreeBSD targets the
// Makefile can build, where v*100 overflows for any rate past ~21 MB/s and the
// out-of-range float-to-int conversion yields garbage (a large negative number)
// that then poisons the API, Prometheus, rollups, and alert thresholds.
// math.Round is width-independent and rounds negatives half-away-from-zero
// instead of truncating them toward zero.
func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

// deltaU64 returns cur-prev as a float64, clamped to 0 when the counter has
// reset (interface or device recreated under the same name), so the unsigned
// subtraction can't wrap to ~1.8e19 and poison rates, rollups, and alerts.
func deltaU64(cur, prev uint64) float64 {
	if cur < prev {
		return 0
	}
	return float64(cur - prev)
}
