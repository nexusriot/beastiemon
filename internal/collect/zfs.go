//go:build freebsd

package collect

import (
	"encoding/binary"
	"os"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
)

// collectZFS reports per-pool capacity via `zpool list`, the scrub/resilver
// state parsed out of `zpool status`, and — when datasetLimit is positive —
// the largest datasets from `zfs list`. Returns nil when ZFS is absent or
// there are no pools; the parsing is in bsdextra_parse.go.
func collectZFS(datasetLimit int) ([]ZFSStats, []ZFSDataset) {
	out, err := exec.Command("zpool", "list", "-Hp", "-o",
		"name,size,alloc,free,capacity,health").Output()
	if err != nil {
		return nil, nil
	}
	pools := parseZpoolList(string(out))
	if len(pools) == 0 {
		return nil, nil
	}
	attachScanState(pools)
	return pools, collectDatasets(datasetLimit)
}

// attachScanState folds `zpool status` into the pools already read from
// `zpool list`. A failure here is not fatal: capacity is still worth
// reporting without the scrub state.
func attachScanState(pools []ZFSStats) {
	out, err := cLocale(exec.Command("zpool", "status")).Output()
	if err != nil {
		return
	}
	scans := parseZpoolStatusScans(string(out), time.Now())
	for i := range pools {
		scan, ok := scans[pools[i].Pool]
		if !ok {
			continue
		}
		pools[i].Scan = scan.state
		pools[i].ScanPct = scan.pct
		pools[i].ScanAgeHours = scan.ageHours
		pools[i].ScanKnown = scan.known
	}
}

func collectDatasets(limit int) []ZFSDataset {
	if limit <= 0 {
		return nil
	}
	out, err := exec.Command("zfs", "list", "-Hp", "-o",
		"name,used,avail,refer,mountpoint").Output()
	if err != nil {
		return nil
	}
	return parseZfsList(string(out), limit)
}

// cLocale pins a command to the C locale. `zpool status` renders its scan
// completion timestamp through the locale's date format, and the parser knows
// only the C-locale shapes.
func cLocale(cmd *exec.Cmd) *exec.Cmd {
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	return cmd
}

// collectARC reads ARC counters from the kstat.zfs.misc.arcstats sysctl tree.
// Returns nil when ZFS isn't loaded (the first sysctl errors).
func collectARC() *ARCStats {
	size, ok := sysctlU64("kstat.zfs.misc.arcstats.size")
	if !ok {
		return nil
	}
	target, _ := sysctlU64("kstat.zfs.misc.arcstats.c")
	max, _ := sysctlU64("kstat.zfs.misc.arcstats.c_max")
	hits, _ := sysctlU64("kstat.zfs.misc.arcstats.hits")
	misses, _ := sysctlU64("kstat.zfs.misc.arcstats.misses")
	return arcFromKstats(size, target, max, hits, misses)
}

// sysctlU64 reads a 64-bit sysctl. These arcstats are u64 on amd64/arm64.
func sysctlU64(name string) (uint64, bool) {
	raw, err := unix.SysctlRaw(name)
	if err != nil || len(raw) < 8 {
		return 0, false
	}
	return binary.LittleEndian.Uint64(raw), true
}
