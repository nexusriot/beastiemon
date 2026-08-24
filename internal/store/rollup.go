package store

import (
	"time"

	"github.com/nexusriot/beastiemon/internal/collect"
)

// stat accumulates one numeric field across the samples in a bucket.
type stat struct {
	sum, min, max float64
	n             int
}

func (s *stat) add(v float64) {
	if s.n == 0 {
		s.min, s.max = v, v
	} else {
		if v < s.min {
			s.min = v
		}
		if v > s.max {
			s.max = v
		}
	}
	s.sum += v
	s.n++
}

func (s stat) avg() float64 {
	if s.n == 0 {
		return 0
	}
	return s.sum / float64(s.n)
}

// keyedAgg aggregates a keyed metric array (net by iface, fs by mount, …),
// keeping width numeric fields per key and preserving first-seen key order so
// the finalized snapshot is stable across buckets.
type keyedAgg struct {
	order  []string
	fields map[string][]stat
	width  int
}

func newKeyedAgg(width int) keyedAgg {
	return keyedAgg{fields: map[string][]stat{}, width: width}
}

func (k *keyedAgg) add(key string, vals ...float64) {
	fs, ok := k.fields[key]
	if !ok {
		fs = make([]stat, k.width)
		k.fields[key] = fs
		k.order = append(k.order, key)
	}
	for i := 0; i < len(fs) && i < len(vals); i++ {
		fs[i].add(vals[i])
	}
}

// bucketAgg accumulates the snapshots that fall in one resolution window and
// finalizes them into three snapshots: the element-wise average, minimum, and
// maximum. Averaging alone erases the short spikes that make wide-range history
// useful, so the min/max envelope is stored beside the average and surfaced by
// /api/series?band=1. Set-valued fields that can't be averaged (processes,
// jails, ARC) are carried from the most recent sample in the bucket.
type bucketAgg struct {
	bucket time.Time
	n      int
	last   collect.Snapshot

	cpu  [4]stat // Total, User, Sys, Idle
	core []stat  // per-core Total
	mem  [8]stat // Total, Used, Free, Available, UsedPct, SwapTotal, SwapUsed, SwapPct
	load [3]stat // Load1, Load5, Load15
	net  keyedAgg
	disk keyedAgg
	fs   keyedAgg
	temp keyedAgg
	zfs  keyedAgg
}

func newBucketAgg(bucket time.Time) *bucketAgg {
	return &bucketAgg{
		bucket: bucket,
		net:    newKeyedAgg(4),
		disk:   newKeyedAgg(4),
		fs:     newKeyedAgg(4),
		temp:   newKeyedAgg(1),
		zfs:    newKeyedAgg(4),
	}
}

func (a *bucketAgg) add(s collect.Snapshot) {
	a.n++
	a.last = s

	a.cpu[0].add(s.CPU.Total)
	a.cpu[1].add(s.CPU.User)
	a.cpu[2].add(s.CPU.Sys)
	a.cpu[3].add(s.CPU.Idle)
	for i, v := range s.CPU.PerCore {
		if i >= len(a.core) {
			a.core = append(a.core, stat{})
		}
		a.core[i].add(v)
	}

	a.mem[0].add(float64(s.Mem.Total))
	a.mem[1].add(float64(s.Mem.Used))
	a.mem[2].add(float64(s.Mem.Free))
	a.mem[3].add(float64(s.Mem.Available))
	a.mem[4].add(s.Mem.UsedPct)
	a.mem[5].add(float64(s.Mem.SwapTotal))
	a.mem[6].add(float64(s.Mem.SwapUsed))
	a.mem[7].add(s.Mem.SwapPct)

	a.load[0].add(s.Load.Load1)
	a.load[1].add(s.Load.Load5)
	a.load[2].add(s.Load.Load15)

	for _, n := range s.Net {
		a.net.add(n.Interface, n.RxBps, n.TxBps, n.RxPps, n.TxPps)
	}
	for _, d := range s.Disk {
		a.disk.add(d.Device, d.ReadBps, d.WriteBps, d.ReadIOPS, d.WriteIOPS)
	}
	for _, f := range s.FS {
		a.fs.add(f.Mount, float64(f.Total), float64(f.Used), float64(f.Free), f.UsedPct)
	}
	for _, t := range s.Temps {
		a.temp.add(t.Name, t.Celsius)
	}
	for _, z := range s.ZFS {
		a.zfs.add(z.Pool, float64(z.Size), float64(z.Alloc), float64(z.Free), z.UsedPct)
	}
}

// finalize returns the average, minimum, and maximum snapshots for the bucket.
func (a *bucketAgg) finalize() (avg, lo, hi collect.Snapshot) {
	return a.build(func(s stat) float64 { return s.avg() }),
		a.build(func(s stat) float64 { return s.min }),
		a.build(func(s stat) float64 { return s.max })
}

// build materializes one snapshot by applying pick to every accumulated field.
func (a *bucketAgg) build(pick func(stat) float64) collect.Snapshot {
	s := collect.Snapshot{
		Time:   a.bucket,
		Uptime: a.last.Uptime,
		Procs:  a.last.Procs,
		Jails:  a.last.Jails,
		ARC:    a.last.ARC,
	}
	s.CPU = collect.CPUStats{
		Total: pick(a.cpu[0]), User: pick(a.cpu[1]), Sys: pick(a.cpu[2]), Idle: pick(a.cpu[3]),
	}
	for _, c := range a.core {
		s.CPU.PerCore = append(s.CPU.PerCore, pick(c))
	}
	s.Mem = collect.MemStats{
		Total: uint64(pick(a.mem[0])), Used: uint64(pick(a.mem[1])), Free: uint64(pick(a.mem[2])),
		Available: uint64(pick(a.mem[3])), UsedPct: pick(a.mem[4]),
		SwapTotal: uint64(pick(a.mem[5])), SwapUsed: uint64(pick(a.mem[6])), SwapPct: pick(a.mem[7]),
	}
	s.Load = collect.LoadStats{Load1: pick(a.load[0]), Load5: pick(a.load[1]), Load15: pick(a.load[2])}

	for _, key := range a.net.order {
		f := a.net.fields[key]
		s.Net = append(s.Net, collect.NetStats{
			Interface: key, RxBps: pick(f[0]), TxBps: pick(f[1]), RxPps: pick(f[2]), TxPps: pick(f[3]),
		})
	}
	for _, key := range a.disk.order {
		f := a.disk.fields[key]
		s.Disk = append(s.Disk, collect.DiskStats{
			Device: key, ReadBps: pick(f[0]), WriteBps: pick(f[1]), ReadIOPS: pick(f[2]), WriteIOPS: pick(f[3]),
		})
	}
	for _, key := range a.fs.order {
		f := a.fs.fields[key]
		dev, fstype := lastFSIdentity(a.last.FS, key)
		s.FS = append(s.FS, collect.FSStats{
			Mount: key, Device: dev, FSType: fstype,
			Total: uint64(pick(f[0])), Used: uint64(pick(f[1])), Free: uint64(pick(f[2])), UsedPct: pick(f[3]),
		})
	}
	for _, key := range a.temp.order {
		f := a.temp.fields[key]
		s.Temps = append(s.Temps, collect.TempStat{Name: key, Celsius: pick(f[0])})
	}
	for _, key := range a.zfs.order {
		f := a.zfs.fields[key]
		s.ZFS = append(s.ZFS, collect.ZFSStats{
			Pool: key, Health: lastZFSHealth(a.last.ZFS, key),
			Size: uint64(pick(f[0])), Alloc: uint64(pick(f[1])), Free: uint64(pick(f[2])), UsedPct: pick(f[3]),
		})
	}
	return s
}

func lastFSIdentity(fs []collect.FSStats, mount string) (dev, fstype string) {
	for _, x := range fs {
		if x.Mount == mount {
			return x.Device, x.FSType
		}
	}
	return "", ""
}

func lastZFSHealth(pools []collect.ZFSStats, pool string) string {
	for _, x := range pools {
		if x.Pool == pool {
			return x.Health
		}
	}
	return ""
}
