//go:build freebsd

package collect

import (
	"os"
	"os/exec"
)

// listBhyve reads bhyve processes from one ps call and the VM instances the
// kernel holds from /dev/vmm. ok is false only when ps itself fails; an
// unreadable or absent /dev/vmm (vmm.ko not loaded) just means no orphans can
// be detected.
func listBhyve() (procs []bhyveProc, vmm []string, ok bool) {
	// One -o per column: FreeBSD's ps takes everything after "=" as the
	// header text, commas included, so "-o pid=,rss=,..." yields only a pid
	// column headed ",rss=,...".
	out, err := exec.Command("ps", "-axww", "-o", "pid=", "-o", "rss=", "-o", "cputime=",
		"-o", "etimes=", "-o", "comm=", "-o", "args=").Output()
	if err != nil {
		return nil, nil, false
	}
	procs = parseBhyvePs(string(out))
	if ents, err := os.ReadDir("/dev/vmm"); err == nil {
		for _, e := range ents {
			vmm = append(vmm, e.Name())
		}
	}
	return procs, vmm, true
}
