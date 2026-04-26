package collect

import (
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

func round2(v float64) float64 {
	return float64(int(v*100+0.5)) / 100
}
