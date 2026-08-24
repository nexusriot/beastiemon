//go:build freebsd

package collect

import "os/exec"

// collectJails enumerates running jails via jls(8) and attaches a per-jail
// process count derived from a single `ps -axo jid=` call. Returns nil when
// there are no jails. Parsing lives in bsdextra_parse.go.
func collectJails() []JailStat {
	out, err := exec.Command("jls", "-n", "jid", "name", "host.hostname", "path").Output()
	if err != nil {
		return nil
	}
	jails := parseJlsN(string(out))
	if len(jails) == 0 {
		return nil
	}
	if ps, err := exec.Command("ps", "-axo", "jid=").Output(); err == nil {
		counts := parsePsJIDCounts(string(ps))
		for i := range jails {
			jails[i].Procs = counts[jails[i].JID]
		}
	}
	return jails
}
