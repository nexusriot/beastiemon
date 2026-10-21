//go:build freebsd

package collect

import "os/exec"

// pfctlInfo runs `pfctl -si`. ok is false when pf is unavailable — the module
// isn't loaded, or (the common case for a daemon running as _beastie)
// /dev/pf is root-only and the open is denied. Either way the PF panel just
// stays absent; see README "pf stats are blank".
func pfctlInfo() (pfRaw, bool) {
	out, err := exec.Command("pfctl", "-si").Output()
	if err != nil {
		return pfRaw{}, false
	}
	return parsePfctlInfo(string(out)), true
}

// pfctlMemoryLimit runs `pfctl -sm` for the states hard limit. 0 when it
// can't be read, which leaves StatePct unset rather than guessed.
func pfctlMemoryLimit() uint64 {
	out, err := exec.Command("pfctl", "-sm").Output()
	if err != nil {
		return 0
	}
	return parsePfctlMemory(string(out))
}
