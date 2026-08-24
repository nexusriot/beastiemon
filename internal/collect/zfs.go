//go:build freebsd

package collect

import (
	"encoding/binary"
	"os/exec"

	"golang.org/x/sys/unix"
)

// collectZFS reports per-pool capacity via `zpool list`. Returns nil when ZFS
// is absent or there are no pools; the parsing is in bsdextra_parse.go.
func collectZFS() []ZFSStats {
	out, err := exec.Command("zpool", "list", "-Hp", "-o",
		"name,size,alloc,free,capacity,health").Output()
	if err != nil {
		return nil
	}
	return parseZpoolList(string(out))
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
