package collect

import (
	"sort"
	"strconv"
	"strings"
	"time"
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
// Tokens are matched by key name and order-independent, so extra parameters
// are ignored rather than mistaken for a field. Splitting on the *first* '='
// means a value may itself contain '=' (a path like /jails/a=b survives); a
// space inside a value would still split the token, and the unkeyed remainder
// is dropped — exceedingly rare in a jail path, and a documented limitation.
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

// scanInfo is one pool's scrub/resilver state, parsed out of `zpool status`.
type scanInfo struct {
	state    string  // none | scrub | resilver | scrub_done | resilver_done
	pct      float64 // progress while running
	ageHours float64 // hours since the last completed scan
	known    bool    // whether ageHours is meaningful
}

// scanDateLayouts are the shapes `zpool status` prints its completion
// timestamp in. The collector runs zpool under LC_ALL=C so the C-locale form
// comes first; the zero-padded variant covers the days-of-month where some
// builds pad instead of space-aligning.
var scanDateLayouts = []string{
	"Mon Jan _2 15:04:05 2006",
	"Mon Jan 02 15:04:05 2006",
	"Mon Jan _2 15:04 2006",
}

// parseZpoolStatusScans parses `zpool status` into a per-pool scan state. The
// relevant line is the "scan:" entry under each "pool:" header, which reads
// one of:
//
//	scan: none requested
//	scan: scrub in progress since Sun Oct 19 03:00:01 2025
//	      ... 9.12% done, 02:34:56 to go
//	scan: scrub repaired 0B in 00:05:23 with 0 errors on Sun Oct 12 03:05:24 2025
//	scan: resilvered 1.20G in 00:01:23 with 0 errors on Sun Oct 12 03:05:24 2025
//
// now is passed in rather than read from the clock so the age arithmetic is
// testable. Pools with no "scan:" line are absent from the result.
func parseZpoolStatusScans(out string, now time.Time) map[string]scanInfo {
	scans := map[string]scanInfo{}
	var pool string
	var cur *scanInfo

	commit := func() {
		if pool != "" && cur != nil {
			scans[pool] = *cur
		}
		cur = nil
	}

	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "pool:"):
			commit()
			pool = strings.TrimSpace(strings.TrimPrefix(trimmed, "pool:"))
		case strings.HasPrefix(trimmed, "scan:"):
			info := parseScanLine(strings.TrimSpace(strings.TrimPrefix(trimmed, "scan:")), now)
			cur = &info
		case cur != nil && cur.pct == 0 && strings.Contains(trimmed, "% done"):
			// Progress lands on a continuation line under "scan:".
			cur.pct = parseScanPercent(trimmed)
		case strings.HasPrefix(trimmed, "config:") || strings.HasPrefix(trimmed, "errors:"):
			commit()
		}
	}
	commit()
	return scans
}

func parseScanLine(line string, now time.Time) scanInfo {
	switch {
	case strings.Contains(line, "scrub in progress"):
		return scanInfo{state: "scrub", pct: parseScanPercent(line)}
	case strings.Contains(line, "resilver in progress"):
		return scanInfo{state: "resilver", pct: parseScanPercent(line)}
	case strings.HasPrefix(line, "scrub repaired"), strings.HasPrefix(line, "scrub canceled"):
		return completedScan("scrub_done", line, now)
	case strings.HasPrefix(line, "resilvered"):
		return completedScan("resilver_done", line, now)
	}
	return scanInfo{state: "none"}
}

// completedScan pulls the trailing " on <date>" off a finished-scan line.
func completedScan(state, line string, now time.Time) scanInfo {
	info := scanInfo{state: state}
	i := strings.LastIndex(line, " on ")
	if i < 0 {
		return info
	}
	stamp := strings.TrimSpace(line[i+len(" on "):])
	for _, layout := range scanDateLayouts {
		t, err := time.ParseInLocation(layout, stamp, time.Local)
		if err != nil {
			continue
		}
		// A timestamp in the future means a clock skew, not a negative age.
		if age := now.Sub(t).Hours(); age >= 0 {
			info.ageHours, info.known = round2(age), true
		} else {
			info.known = true
		}
		break
	}
	return info
}

// parseScanPercent extracts "9.12% done" from wherever it sits on the line.
func parseScanPercent(line string) float64 {
	for _, f := range strings.Fields(line) {
		if !strings.HasSuffix(f, "%") {
			continue
		}
		if v, err := strconv.ParseFloat(strings.TrimSuffix(f, "%"), 64); err == nil {
			return v
		}
	}
	return 0
}

// parseZfsList parses `zfs list -Hp -o name,used,avail,refer,mountpoint`,
// keeping the largest limit datasets by used space. limit <= 0 returns nil:
// a host with jails can carry hundreds of datasets and they would otherwise
// all ride every snapshot.
func parseZfsList(out string, limit int) []ZFSDataset {
	if limit <= 0 {
		return nil
	}
	var sets []ZFSDataset
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) < 4 {
			continue
		}
		used, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		avail, _ := strconv.ParseUint(f[2], 10, 64)
		refer, _ := strconv.ParseUint(f[3], 10, 64)
		d := ZFSDataset{Name: f[0], Used: used, Avail: avail, Refer: refer}
		// "-" is zfs's placeholder for an unset mountpoint.
		if len(f) >= 5 && f[4] != "-" && f[4] != "none" {
			d.Mount = f[4]
		}
		sets = append(sets, d)
	}
	sortDatasetsByUsed(sets)
	if len(sets) > limit {
		sets = sets[:limit]
	}
	return sets
}

// sortDatasetsByUsed orders datasets largest first, breaking ties by name so
// the emitted array is stable from tick to tick.
func sortDatasetsByUsed(sets []ZFSDataset) {
	sort.Slice(sets, func(i, j int) bool {
		if sets[i].Used != sets[j].Used {
			return sets[i].Used > sets[j].Used
		}
		return sets[i].Name < sets[j].Name
	})
}
