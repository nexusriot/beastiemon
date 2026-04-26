package collect

import (
	"context"
	"time"

	psutil_host "github.com/shirou/gopsutil/v3/host"
	psutil_load "github.com/shirou/gopsutil/v3/load"

	"github.com/nexusriot/beastiemon/internal/config"
)

// Sampler runs all collectors on a fixed interval and sends snapshots on C.
type Sampler struct {
	C    chan Snapshot
	cfg  config.Config
	cpu  CPUCollector
	mem  MemCollector
	disk DiskCollector
	net  *NetCollector
	fs   *FSCollector
}

func NewSampler(cfg config.Config) *Sampler {
	return &Sampler{
		C:   make(chan Snapshot, 4),
		cfg: cfg,
		net: NewNetCollector(cfg.Collect.NetExclude),
		fs:  NewFSCollector(cfg.Collect.FSInclude),
	}
}

func (s *Sampler) Run(ctx context.Context) {
	tick := time.NewTicker(s.cfg.Collect.Interval.Duration)
	defer tick.Stop()

	// Prime delta-based collectors before first publish.
	s.cpu.Collect()
	s.disk.Collect()
	s.net.Collect()
	time.Sleep(s.cfg.Collect.Interval.Duration)

	for {
		select {
		case <-ctx.Done():
			return
		case t := <-tick.C:
			snap := s.collect(t)
			select {
			case s.C <- snap:
			default:
				// Drop if consumer is slow.
			}
		}
	}
}

func (s *Sampler) collect(t time.Time) Snapshot {
	uptime, _ := psutil_host.Uptime()
	load, _ := psutil_load.Avg()

	snap := Snapshot{
		Time:   t,
		CPU:    s.cpu.Collect(),
		Mem:    s.mem.Collect(),
		Disk:   s.disk.Collect(),
		Net:    s.net.Collect(),
		FS:     s.fs.Collect(),
		Temps:  collectTemps(),
		Uptime: uptime,
	}
	if load != nil {
		snap.Load = LoadStats{
			Load1:  round2(load.Load1),
			Load5:  round2(load.Load5),
			Load15: round2(load.Load15),
		}
	}
	return snap
}
