package collect

import (
	"strconv"
	"strings"
)

// Parsing for the ZFS and jail collectors lives here — free of build tags and
// of any syscall/exec — so it is unit-testable on any platform. The FreeBSD
// collectors (zfs.go, jail.go) feed real `zpool`/`jls`/`ps` output through
// these; the non-FreeBSD stubs never call them.

// parseZpoolList parses `zpool list -Hp -o name,size,alloc,free,capacity,health`.
// -H drops the header and tab-separates columns; -p prints exact byte counts.
func parseZpoolList(out string) []ZFSStats {
	var pools []ZFSStats
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 6 {
			continue
		}
		size, _ := strconv.ParseUint(f[1], 10, 64)
		alloc, _ := strconv.ParseUint(f[2], 10, 64)
		free, _ := strconv.ParseUint(f[3], 10, 64)
		cap := strings.TrimSuffix(f[4], "%")
		usedPct, _ := strconv.ParseFloat(cap, 64)
		if usedPct == 0 && size > 0 {
			usedPct = round2(float64(alloc) / float64(size) * 100)
		}
		pools = append(pools, ZFSStats{
			Pool:    f[0],
			Size:    size,
			Alloc:   alloc,
			Free:    free,
			UsedPct: usedPct,
			Health:  f[5],
		})
	}
	return pools
}

// arcFromKstats builds an ARCStats from raw kstat.zfs.misc.arcstats values.
// hits/misses are cumulative counters, so the hit rate is lifetime.
func arcFromKstats(size, target, max, hits, misses uint64) *ARCStats {
	a := &ARCStats{Size: size, Target: target, Max: max}
	if total := hits + misses; total > 0 {
		a.HitRate = round2(float64(hits) / float64(total) * 100)
	}
	return a
}

// parseJlsN parses `jls -n <params...>` output. Each line is space-separated
// key=value pairs, e.g. "jid=1 name=web host.hostname=web.local path=/jails/web".
// Keys after the first three are treated as the path remainder so a path may
// contain '=' (a space in a jail path, which is exceedingly rare, would still
// split — documented limitation).
func parseJlsN(out string) []JailStat {
	var jails []JailStat
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var j JailStat
		for _, tok := range strings.Fields(line) {
			k, v, ok := strings.Cut(tok, "=")
			if !ok {
				continue
			}
			switch k {
			case "jid":
				n, _ := strconv.ParseInt(v, 10, 32)
				j.JID = int32(n)
			case "name":
				j.Name = v
			case "host.hostname":
				j.Host = v
			case "path":
				j.Path = v
			}
		}
		if j.JID != 0 || j.Name != "" {
			jails = append(jails, j)
		}
	}
	return jails
}

// parsePsJIDCounts parses `ps -axo jid=` output (one JID per line) into a
// per-JID process count. JID 0 is the host and is included in the map but
// callers only look up real jail JIDs.
func parsePsJIDCounts(out string) map[int32]int {
	counts := make(map[int32]int)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		n, err := strconv.ParseInt(line, 10, 32)
		if err != nil {
			continue
		}
		counts[int32(n)]++
	}
	return counts
}
