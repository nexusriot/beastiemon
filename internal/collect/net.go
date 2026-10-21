package collect

import (
	"sort"
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
				RxBps:     round2(deltaU64(c.BytesRecv, p.BytesRecv) / dt),
				TxBps:     round2(deltaU64(c.BytesSent, p.BytesSent) / dt),
				RxPps:     round2(deltaU64(c.PacketsRecv, p.PacketsRecv) / dt),
				TxPps:     round2(deltaU64(c.PacketsSent, p.PacketsSent) / dt),
				// Already in the counters we just read; errors and drops are
				// what actually diagnoses a flaky NIC or a full queue.
				RxErr:  round2(deltaU64(c.Errin, p.Errin) / dt),
				TxErr:  round2(deltaU64(c.Errout, p.Errout) / dt),
				RxDrop: round2(deltaU64(c.Dropin, p.Dropin) / dt),
				TxDrop: round2(deltaU64(c.Dropout, p.Dropout) / dt),
			})
		}
	}

	// Rates are gathered by ranging over a map, so without this the array
	// order is reshuffled on every tick — churning /api/metrics, each SSE
	// frame, `beastie --json net`, and the first-seen key order the rollup
	// aggregator relies on to keep stored buckets stable.
	sort.Slice(stats, func(i, j int) bool { return stats[i].Interface < stats[j].Interface })

	n.prev = cur
	n.prevAt = now
	return stats
}
