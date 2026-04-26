package collect

import (
	"time"

	psutil "github.com/shirou/gopsutil/v3/net"
)

type NetCollector struct {
	exclude map[string]bool
	prev    map[string]psutil.IOCountersStat
	prevAt  time.Time
}

func NewNetCollector(exclude []string) *NetCollector {
	ex := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		ex[e] = true
	}
	return &NetCollector{exclude: ex}
}

func (n *NetCollector) Collect() []NetStats {
	ifaces, err := psutil.IOCounters(true)
	if err != nil {
		return nil
	}
	now := time.Now()

	cur := make(map[string]psutil.IOCountersStat, len(ifaces))
	for _, iface := range ifaces {
		if !n.exclude[iface.Name] {
			cur[iface.Name] = iface
		}
	}

	var stats []NetStats
	if n.prev != nil {
		dt := now.Sub(n.prevAt).Seconds()
		if dt <= 0 {
			dt = 1
		}
		for name, c := range cur {
			p, ok := n.prev[name]
			if !ok {
				continue
			}
			stats = append(stats, NetStats{
				Interface: name,
				RxBps:     round2(float64(c.BytesRecv-p.BytesRecv) / dt),
				TxBps:     round2(float64(c.BytesSent-p.BytesSent) / dt),
				RxPps:     round2(float64(c.PacketsRecv-p.PacketsRecv) / dt),
				TxPps:     round2(float64(c.PacketsSent-p.PacketsSent) / dt),
			})
		}
	}

	n.prev = cur
	n.prevAt = now
	return stats
}
