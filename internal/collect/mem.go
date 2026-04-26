package collect

import (
	psutil "github.com/shirou/gopsutil/v3/mem"
)

type MemCollector struct{}

func (m *MemCollector) Collect() MemStats {
	vm, err := psutil.VirtualMemory()
	if err != nil {
		return MemStats{}
	}
	sw, _ := psutil.SwapMemory()

	s := MemStats{
		Total:     vm.Total,
		Used:      vm.Used,
		Free:      vm.Free,
		Available: vm.Available,
		UsedPct:   round2(vm.UsedPercent),
	}
	if sw != nil {
		s.SwapTotal = sw.Total
		s.SwapUsed = sw.Used
		if sw.Total > 0 {
			s.SwapPct = round2(float64(sw.Used) / float64(sw.Total) * 100)
		}
	}
	return s
}
