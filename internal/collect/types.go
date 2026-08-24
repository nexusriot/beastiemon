package collect

import "time"

// Snapshot holds all metrics at a point in time.
type Snapshot struct {
	Time   time.Time   `json:"ts"`
	CPU    CPUStats    `json:"cpu"`
	Mem    MemStats    `json:"mem"`
	Net    []NetStats  `json:"net"`
	Disk   []DiskStats `json:"disk"`
	FS     []FSStats   `json:"fs"`
	Temps  []TempStat  `json:"temps,omitempty"`
	Procs  []ProcStat  `json:"procs,omitempty"`
	ZFS    []ZFSStats  `json:"zfs,omitempty"`
	ARC    *ARCStats   `json:"arc,omitempty"`
	Jails  []JailStat  `json:"jails,omitempty"`
	Load   LoadStats   `json:"load"`
	Uptime uint64      `json:"uptime"`
}

type CPUStats struct {
	Total   float64   `json:"total"`
	User    float64   `json:"user"`
	Sys     float64   `json:"sys"`
	Idle    float64   `json:"idle"`
	PerCore []float64 `json:"per_core"`
}

type MemStats struct {
	Total     uint64  `json:"total"`
	Used      uint64  `json:"used"`
	Free      uint64  `json:"free"`
	Available uint64  `json:"available"`
	UsedPct   float64 `json:"used_pct"`
	SwapTotal uint64  `json:"swap_total"`
	SwapUsed  uint64  `json:"swap_used"`
	SwapPct   float64 `json:"swap_pct"`
}

type NetStats struct {
	Interface string  `json:"iface"`
	RxBps     float64 `json:"rx_bps"`
	TxBps     float64 `json:"tx_bps"`
	RxPps     float64 `json:"rx_pps"`
	TxPps     float64 `json:"tx_pps"`
}

type DiskStats struct {
	Device    string  `json:"dev"`
	ReadBps   float64 `json:"read_bps"`
	WriteBps  float64 `json:"write_bps"`
	ReadIOPS  float64 `json:"read_iops"`
	WriteIOPS float64 `json:"write_iops"`
}

type FSStats struct {
	Mount   string  `json:"mount"`
	Device  string  `json:"dev"`
	FSType  string  `json:"fstype"`
	Total   uint64  `json:"total"`
	Used    uint64  `json:"used"`
	Free    uint64  `json:"free"`
	UsedPct float64 `json:"used_pct"`
}

type TempStat struct {
	Name    string  `json:"name"`
	Celsius float64 `json:"celsius"`
}

type ProcStat struct {
	PID    int32   `json:"pid"`
	Name   string  `json:"name"`
	CPUPct float64 `json:"cpu_pct"`
	MemPct float64 `json:"mem_pct"`
	RSS    uint64  `json:"rss"`
}

type LoadStats struct {
	Load1  float64 `json:"load1"`
	Load5  float64 `json:"load5"`
	Load15 float64 `json:"load15"`
}

// ZFSStats is per-pool capacity, sourced from `zpool list`. FreeBSD-only.
type ZFSStats struct {
	Pool    string  `json:"pool"`
	Size    uint64  `json:"size"`
	Alloc   uint64  `json:"alloc"`
	Free    uint64  `json:"free"`
	UsedPct float64 `json:"used_pct"`
	Health  string  `json:"health,omitempty"`
}

// ARCStats is the ZFS adaptive replacement cache, from kstat sysctls. The
// hit rate is lifetime (cumulative hits / (hits+misses)). FreeBSD-only.
type ARCStats struct {
	Size    uint64  `json:"size"`     // current ARC size in bytes
	Target  uint64  `json:"target"`   // target size (kstat "c")
	Max     uint64  `json:"max"`      // maximum size (kstat "c_max")
	HitRate float64 `json:"hit_rate"` // lifetime hit percentage
}

// JailStat describes one running jail, from jls(8). FreeBSD-only.
type JailStat struct {
	JID   int32  `json:"jid"`
	Name  string `json:"name"`
	Host  string `json:"host,omitempty"`
	Path  string `json:"path,omitempty"`
	Procs int    `json:"procs"`
}
