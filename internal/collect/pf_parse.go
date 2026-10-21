package collect

import (
	"strconv"
	"strings"
)

// pfctl(8) output parsing, free of build tags and of any exec, so it is
// unit-testable on any host. The FreeBSD collector (pf.go) feeds real command
// output through these; the non-FreeBSD stub never calls them.

// pfRaw is the cumulative state pfctl reports. The collector turns the
// counters into per-second rates itself rather than using pfctl's own Rate
// column, which is a lifetime average since pf was enabled and so says
// nothing about what the firewall is doing right now.
type pfRaw struct {
	enabled  bool
	states   uint64
	searches uint64
	inserts  uint64
	matches  uint64
	drops    uint64
}

// dropCounters are the pfctl "Counters" entries that mean traffic was
// discarded for want of resources, as opposed to by a rule. Summed into
// pfRaw.drops because any of them being non-zero is the same incident.
var dropCounters = map[string]bool{
	"memory":      true,
	"state-limit": true,
	"congestion":  true,
}

// parsePfctlInfo parses `pfctl -si`. The layout is section headers followed by
// "  <label>   <total>   <rate>/s" rows, where the label itself contains
// spaces ("current entries"), so rows are matched on their trailing numeric
// columns rather than by splitting on whitespace alone.
func parsePfctlInfo(out string) pfRaw {
	var pf pfRaw
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "Status:") {
			// "Status: Enabled for 3 days 02:11:33    Debug: Urgent"
			pf.enabled = strings.Contains(trimmed, "Enabled")
			continue
		}
		label, total, ok := pfCounterRow(trimmed)
		if !ok {
			continue
		}
		switch {
		case label == "current entries":
			pf.states = total
		case label == "searches":
			pf.searches = total
		case label == "inserts":
			pf.inserts = total
		case label == "match":
			pf.matches = total
		case dropCounters[label]:
			pf.drops += total
		}
	}
	return pf
}

// pfCounterRow splits a counter row into its label and total. A row carries
// either one numeric column (the state-table "current entries") or two (a
// total plus pfctl's own "N/s" rate), and the label is everything before them.
func pfCounterRow(line string) (label string, total uint64, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", 0, false
	}
	// Drop a trailing rate column so the total is always the last field left.
	if strings.HasSuffix(fields[len(fields)-1], "/s") {
		fields = fields[:len(fields)-1]
	}
	if len(fields) < 2 {
		return "", 0, false
	}
	total, err := strconv.ParseUint(fields[len(fields)-1], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return strings.Join(fields[:len(fields)-1], " "), total, true
}

// parsePfctlMemory parses the states hard limit out of `pfctl -sm`:
//
//	states        hard limit    10000
//
// Returns 0 when the limit isn't reported, which leaves PFStats.StatePct at 0
// rather than inventing a denominator.
func parsePfctlMemory(out string) uint64 {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "states" {
			continue
		}
		if n, err := strconv.ParseUint(f[len(f)-1], 10, 64); err == nil {
			return n
		}
	}
	return 0
}
