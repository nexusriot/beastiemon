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

	// Datasets is per-dataset ZFS usage, capped at [collect] zfs_datasets
	// (0 = don't collect). Separate from ZFS because a host with jails can
	// have hundreds of datasets and every one would ride every snapshot.
	Datasets []ZFSDataset `json:"datasets,omitempty"`
	// Watch counts processes named in [collect] proc_watch. Empty unless
	// that list is set, because filling it costs a name lookup per process.
	Watch []ProcWatch `json:"watch,omitempty"`
	// PF is packet-filter state, present only with [collect] pf and a
	// readable /dev/pf.
	PF *PFStats `json:"pf,omitempty"`
	// Health is the slow host-health check ([collect] host_health): pkg
	// audit, pending kernel reboot, boot environments. It refreshes on its own
	// interval, so consecutive snapshots carry the same value until then.
	Health *HostHealth `json:"health,omitempty"`
	// VMs are bhyve guests, present only with [collect] bhyve.
	VMs []VMStat `json:"vms,omitempty"`
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

// NetStats is per-interface throughput. The error and drop rates come from the
// same IOCounters call as the byte/packet counters, so they are free to
// collect; they carry omitempty because they are zero on a healthy host and
// would otherwise add ~56 bytes per interface to every snapshot on the wire.
// Note that FreeBSD always reports 0 for outbound drops.
type NetStats struct {
	Interface string  `json:"iface"`
	RxBps     float64 `json:"rx_bps"`
	TxBps     float64 `json:"tx_bps"`
	RxPps     float64 `json:"rx_pps"`
	TxPps     float64 `json:"tx_pps"`
	RxErr     float64 `json:"rx_err,omitempty"`
	TxErr     float64 `json:"tx_err,omitempty"`
	RxDrop    float64 `json:"rx_drop,omitempty"`
	TxDrop    float64 `json:"tx_drop,omitempty"`
}

// DiskStats is per-device I/O. BusyPct and the latencies come from the same
// devstat(3) counters as throughput, so they cost nothing extra: BusyPct is
// the share of the interval the device had at least one request outstanding
// (gstat's %busy), and ReadMs/WriteMs are the mean service time per completed
// request (gstat's ms/r, ms/w). Latency is 0 when no requests of that kind
// completed during the interval.
type DiskStats struct {
	Device    string  `json:"dev"`
	ReadBps   float64 `json:"read_bps"`
	WriteBps  float64 `json:"write_bps"`
	ReadIOPS  float64 `json:"read_iops"`
	WriteIOPS float64 `json:"write_iops"`
	BusyPct   float64 `json:"busy_pct"`
	ReadMs    float64 `json:"read_ms"`
	WriteMs   float64 `json:"write_ms"`
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

// ZFSStats is per-pool capacity from `zpool list`, plus the scan state parsed
// out of `zpool status`. FreeBSD-only.
type ZFSStats struct {
	Pool    string  `json:"pool"`
	Size    uint64  `json:"size"`
	Alloc   uint64  `json:"alloc"`
	Free    uint64  `json:"free"`
	UsedPct float64 `json:"used_pct"`
	Health  string  `json:"health,omitempty"`

	// Scan is "none", "scrub", "resilver", "scrub_done" or "resilver_done".
	Scan string `json:"scan,omitempty"`
	// ScanPct is progress while a scrub or resilver is running.
	ScanPct float64 `json:"scan_pct,omitempty"`
	// ScanAgeHours is hours since the last completed scrub. 0 when unknown
	// or while one is in progress; a pool that has never been scrubbed is
	// the case a "scrub overdue" rule wants to catch, so see ScanKnown.
	ScanAgeHours float64 `json:"scan_age_hours,omitempty"`
	// ScanKnown distinguishes "no scrub has ever completed" from "age 0".
	ScanKnown bool `json:"scan_known,omitempty"`
}

// ZFSDataset is one dataset's space usage, from `zfs list`. FreeBSD-only and
// collected only when [collect] zfs_datasets is set.
type ZFSDataset struct {
	Name  string `json:"name"`
	Used  uint64 `json:"used"`
	Avail uint64 `json:"avail"`
	Refer uint64 `json:"refer"`
	Mount string `json:"mount,omitempty"`
}

// ProcWatch is the number of running processes matching a name from
// [collect] proc_watch. It exists so a rule can say "sshd must be running":
// the Procs list is only the top-N by CPU, so a service that is idle (or
// stopped) never appears there.
type ProcWatch struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// PFStats summarises pf(4) from pfctl. The state table is the interesting
// part: exhausting it drops connections with nothing in the logs to explain
// it. Rates are per-second deltas; StatePct is current/limit. FreeBSD-only.
type PFStats struct {
	Enabled    bool    `json:"enabled"`
	States     uint64  `json:"states"`
	StateLimit uint64  `json:"state_limit,omitempty"`
	StatePct   float64 `json:"state_pct,omitempty"`
	SearchRate float64 `json:"search_rate"`
	InsertRate float64 `json:"insert_rate"`
	MatchRate  float64 `json:"match_rate"`
	// DropRate sums the pfctl counters that mean pf discarded traffic
	// because it ran out of room — memory, state-limit and congestion.
	// Anything above zero here is a real incident, which is why it is
	// broken out rather than left among the twenty other counters.
	DropRate float64 `json:"drop_rate"`
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

// HostHealth is the result of one host-health check. Each part has its own
// Known flag because they fail independently: a host may have no vuln.xml
// yet, or not boot from ZFS, and that must read "unknown", not "healthy".
type HostHealth struct {
	CheckedAt time.Time `json:"checked_at"`

	// From `pkg audit -q` against the local vuln.xml. The daemon never
	// fetches the database itself (no outbound network); periodic(8)'s
	// daily security run refreshes it.
	VulnKnown      bool     `json:"vuln_known"`
	Vulnerable     int      `json:"vulnerable"`
	VulnPkgs       []string `json:"vuln_pkgs,omitempty"`
	VulnDBAgeHours float64  `json:"vulndb_age_hours,omitempty"`

	// From freebsd-version(1). A newer installed kernel than the running
	// one means an update was installed and the host was never rebooted.
	KernelKnown     bool   `json:"kernel_known"`
	RunningKernel   string `json:"running_kernel,omitempty"`
	InstalledKernel string `json:"installed_kernel,omitempty"`
	Userland        string `json:"userland,omitempty"`
	RebootRequired  bool   `json:"reboot_required"`

	// From `bectl list -H`; zero on a host that doesn't boot from ZFS.
	BECount  int    `json:"be_count,omitempty"`
	BEActive string `json:"be_active,omitempty"`
	BENext   string `json:"be_next,omitempty"`
}

// BEPending reports whether the next boot selects a different boot
// environment than the running one — an upgrade staged with bectl activate.
func (h *HostHealth) BEPending() bool {
	return h.BECount > 0 && h.BENext != "" && h.BENext != h.BEActive
}

// VMStat is one bhyve guest. State is "running" for a live bhyve process and
// "orphaned" for a /dev/vmm instance with no process behind it: a guest that
// exited without `bhyvectl --destroy` and still holds its wired memory.
type VMStat struct {
	Name      string  `json:"name"`
	State     string  `json:"state"`
	PID       int32   `json:"pid,omitempty"`
	CPUPct    float64 `json:"cpu_pct"`
	RSS       uint64  `json:"rss"`
	UptimeSec uint64  `json:"uptime_sec,omitempty"`
}
