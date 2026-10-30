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
	proc *ProcCollector
	pf   *PFCollector
	vm   *VMCollector
	// health is nil unless [collect] host_health is on.
	health *HostHealthCollector
}

func NewSampler(cfg config.Config) *Sampler {
	return &Sampler{
		C:    make(chan Snapshot, 4),
		cfg:  cfg,
		net:  NewNetCollector(cfg.Collect.NetExclude),
		fs:   NewFSCollector(cfg.Collect.FSInclude),
		proc: NewProcCollector(cfg.Collect.TopProcs, cfg.Collect.ProcWatch),
		pf:   &PFCollector{},
		vm:   &VMCollector{},
		health: func() *HostHealthCollector {
			if !cfg.Collect.HostHealth {
				return nil
			}
			return NewHostHealthCollector(cfg.Collect.HostHealthInterval.Duration, cfg.Collect.HostHealthVulnXML)
		}(),
	}
}

// hostHealthWait is how long the first snapshot waits for the first
// host-health check beyond the usual warm-up. Without it a one-shot CLI run
// would always print the section empty; a check slower than this simply lands
// in a later snapshot.
const hostHealthWait = 3 * time.Second

func (s *Sampler) Run(ctx context.Context) {
	tick := time.NewTicker(s.cfg.Collect.Interval.Duration)
	defer tick.Stop()

	// Prime delta-based collectors before first publish.
	s.cpu.Collect()
	s.disk.Collect()
	s.net.Collect()
	s.proc.Collect()
	if s.cfg.Collect.PF {
		s.pf.Collect()
	}
	if s.cfg.Collect.Bhyve {
		s.vm.Collect()
	}
	if s.health != nil {
		go s.health.Run(ctx)
	}
	time.Sleep(s.cfg.Collect.Interval.Duration)
	if s.health != nil {
		select {
		case <-s.health.Ready():
		case <-time.After(hostHealthWait):
		case <-ctx.Done():
			return
		}
	}

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

	procs, watch := s.proc.Collect()
	snap := Snapshot{
		Time:   t,
		CPU:    s.cpu.Collect(),
		Mem:    s.mem.Collect(),
		Disk:   s.disk.Collect(),
		Net:    s.net.Collect(),
		FS:     s.fs.Collect(),
		Temps:  collectTemps(),
		Procs:  procs,
		Watch:  watch,
		Uptime: uptime,
	}
	// Opt-in FreeBSD extras (no-ops / stubs elsewhere).
	if s.cfg.Collect.ZFS {
		snap.ZFS, snap.Datasets = collectZFS(s.cfg.Collect.ZFSDatasets)
		snap.ARC = collectARC()
	}
	if s.cfg.Collect.Jails {
		snap.Jails = collectJails()
	}
	if s.cfg.Collect.PF {
		snap.PF = s.pf.Collect()
	}
	if s.cfg.Collect.Bhyve {
		snap.VMs = s.vm.Collect()
	}
	if s.health != nil {
		snap.Health = s.health.Latest()
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
