package collect

import (
	psutil "github.com/shirou/gopsutil/v3/disk"
)

type FSCollector struct {
	include map[string]bool
}

func NewFSCollector(mounts []string) *FSCollector {
	inc := make(map[string]bool, len(mounts))
	for _, m := range mounts {
		inc[m] = true
	}
	return &FSCollector{include: inc}
}

func (f *FSCollector) Collect() []FSStats {
	parts, err := psutil.Partitions(false)
	if err != nil {
		return nil
	}
	var out []FSStats
	seen := map[string]bool{}
	for _, p := range parts {
		if len(f.include) > 0 && !f.include[p.Mountpoint] {
			continue
		}
		if seen[p.Mountpoint] {
			continue
		}
		seen[p.Mountpoint] = true
		usage, err := psutil.Usage(p.Mountpoint)
		if err != nil {
			continue
		}
		out = append(out, FSStats{
			Mount:   p.Mountpoint,
			Device:  p.Device,
			FSType:  p.Fstype,
			Total:   usage.Total,
			Used:    usage.Used,
			Free:    usage.Free,
			UsedPct: round2(usage.UsedPercent),
		})
	}
	return out
}
