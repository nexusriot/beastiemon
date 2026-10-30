package collect

import (
	"strconv"
	"strings"
)

// Parsing for the host-health and bhyve collectors. Like bsdextra_parse.go it
// has no build tags and no exec, so it is tested on any platform.

// maxVulnPkgs caps the names carried in a snapshot: the count is what alerts
// use, and a long-neglected host can list hundreds.
const maxVulnPkgs = 20

// parsePkgAuditQ parses `pkg audit -q`: one "name-version" per vulnerable
// package, with nothing printed when the host is clean.
func parsePkgAuditQ(out string) (count int, names []string) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		count++
		if len(names) < maxVulnPkgs {
			names = append(names, line)
		}
	}
	return count, names
}

// parseBectlList parses `bectl list -H` (tab-separated: name, active,
// mountpoint, space, created). The active column holds N for the running
// environment, R for the one the next boot selects, NR when they coincide.
func parseBectlList(out string) (count int, active, next string) {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), "\t")
		if len(f) < 2 || f[0] == "" {
			continue
		}
		count++
		if strings.Contains(f[1], "N") {
			active = f[0]
		}
		if strings.Contains(f[1], "R") {
			next = f[0]
		}
	}
	return count, active, next
}

// bhyveProc is one bhyve process as listed by ps.
type bhyveProc struct {
	pid    int32
	name   string
	rssKB  uint64
	cpuSec float64
	etime  uint64
}

// parseBhyvePs parses `ps -axww -o pid= -o rss= -o cputime= -o etimes= -o comm= -o args=`
// and keeps the bhyve processes. bhyve sets its title to the VM name, which ps
// shows as "bhyve: <vm> (bhyve)"; without a title the VM name is bhyve's last
// argument. args is taken verbatim rather than re-joined from fields, so a
// name with runs of spaces survives, and ps's vis(3) escaping is undone so the
// name matches its /dev/vmm entry.
func parseBhyvePs(out string) []bhyveProc {
	var procs []bhyveProc
	for _, line := range strings.Split(out, "\n") {
		f, rest := fieldsN(line, 5)
		if len(f) < 5 || f[4] != "bhyve" || rest == "" {
			continue
		}
		pid, err := strconv.ParseInt(f[0], 10, 32)
		if err != nil {
			continue
		}
		rss, _ := strconv.ParseUint(f[1], 10, 64)
		etime, _ := strconv.ParseUint(f[3], 10, 64)
		var name string
		if title, ok := strings.CutPrefix(rest, "bhyve: "); ok {
			name = strings.TrimSuffix(title, " (bhyve)")
		} else {
			argv := strings.Fields(rest)
			name = argv[len(argv)-1]
		}
		name = unvis(name)
		procs = append(procs, bhyveProc{
			pid: int32(pid), name: name, rssKB: rss, cpuSec: parsePsCPUTime(f[2]), etime: etime,
		})
	}
	return procs
}

// fieldsN splits off the first n whitespace-separated fields of line and
// returns them with the remainder, which keeps its inner whitespace.
func fieldsN(line string, n int) ([]string, string) {
	var f []string
	rest := line
	for len(f) < n {
		rest = strings.TrimLeft(rest, " \t")
		if rest == "" {
			return f, ""
		}
		end := strings.IndexAny(rest, " \t")
		if end < 0 {
			end = len(rest)
		}
		f = append(f, rest[:end])
		rest = rest[end:]
	}
	return f, strings.TrimLeft(rest, " \t")
}

// unvis undoes the encodings FreeBSD's ps applies to args with vis(3):
// "\\" for a backslash, "\ooo" octal, "\^X" for control characters and
// "\M-X" / "\M^X" for bytes with the high bit set. Anything else is kept
// as is.
func unvis(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		r := s[i+1:]
		switch {
		case r[0] == '\\':
			b.WriteByte('\\')
			i++
		case len(r) >= 3 && isOctal(r[0]) && isOctal(r[1]) && isOctal(r[2]):
			b.WriteByte((r[0]-'0')<<6 | (r[1]-'0')<<3 | (r[2] - '0'))
			i += 3
		case len(r) >= 2 && r[0] == '^':
			b.WriteByte(caret(r[1]))
			i += 2
		case len(r) >= 3 && r[0] == 'M' && r[1] == '-':
			b.WriteByte(r[2] | 0x80)
			i += 3
		case len(r) >= 3 && r[0] == 'M' && r[1] == '^':
			b.WriteByte(caret(r[2]) | 0x80)
			i += 3
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

// caret decodes the X of "^X": "^?" is DEL, anything else is X with bit 6 cleared.
func caret(c byte) byte {
	if c == '?' {
		return 0x7f
	}
	return c &^ 0x40
}

// parsePsCPUTime parses ps's cputime column, "M:SS.hh" with unbounded
// minutes on FreeBSD; an "H:MM:SS.hh" or "D-H:MM:SS" form is accepted too so a
// format change degrades to a wrong-looking number rather than zero.
func parsePsCPUTime(s string) float64 {
	var days float64
	if d, rest, ok := strings.Cut(s, "-"); ok {
		days, _ = strconv.ParseFloat(d, 64)
		s = rest
	}
	var total float64
	for _, part := range strings.Split(s, ":") {
		v, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return 0
		}
		total = total*60 + v
	}
	return days*86400 + total
}

// buildVMStats turns bhyve processes into VMStats, computing CPU% from the
// previous cputime per PID, and appends an "orphaned" entry for every
// /dev/vmm instance with no process. Sorted by name for a stable order.
func buildVMStats(procs []bhyveProc, vmmNames []string, prev map[int32]float64, dt float64) ([]VMStat, map[int32]float64) {
	cur := make(map[int32]float64, len(procs))
	running := make(map[string]bool, len(procs))
	var vms []VMStat
	for _, p := range procs {
		cur[p.pid] = p.cpuSec
		running[p.name] = true
		var pct float64
		if before, ok := prev[p.pid]; ok && dt > 0 && p.cpuSec >= before {
			pct = round2((p.cpuSec - before) / dt * 100)
		}
		vms = append(vms, VMStat{
			Name: p.name, State: "running", PID: p.pid,
			CPUPct: pct, RSS: p.rssKB * 1024, UptimeSec: p.etime,
		})
	}
	for _, n := range vmmNames {
		if !running[n] {
			vms = append(vms, VMStat{Name: n, State: "orphaned"})
		}
	}
	sortVMs(vms)
	return vms, cur
}
