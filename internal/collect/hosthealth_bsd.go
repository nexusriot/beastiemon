//go:build freebsd

package collect

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

// checkHostHealth runs the three independent checks. Each one that fails
// leaves its Known flag false (or its fields empty) rather than failing the
// whole result.
//
// vulnXML is passed to pkg audit with -f rather than letting pkg find its own,
// so the age reported and the database checked are always the same file.
func checkHostHealth(ctx context.Context, vulnXML string) *HostHealth {
	now := time.Now()
	h := &HostHealth{CheckedAt: now}

	if st, err := os.Stat(vulnXML); err == nil {
		h.VulnDBAgeHours = round2(now.Sub(st.ModTime()).Hours())
		// pkg audit exits 1 when it finds vulnerable packages, so a non-zero
		// status with output is a result, not a failure.
		out, err := cLocale(exec.CommandContext(ctx, "pkg", "audit", "-q", "-f", vulnXML)).Output()
		var exit *exec.ExitError
		if err == nil || (errors.As(err, &exit) && len(strings.TrimSpace(string(out))) > 0) {
			h.VulnKnown = true
			h.Vulnerable, h.VulnPkgs = parsePkgAuditQ(string(out))
		}
	}

	running := freebsdVersion(ctx, "-r")
	installed := freebsdVersion(ctx, "-k")
	if running != "" && installed != "" {
		h.KernelKnown = true
		h.RunningKernel, h.InstalledKernel = running, installed
		h.RebootRequired = running != installed
	}
	h.Userland = freebsdVersion(ctx, "-u")

	if out, err := exec.CommandContext(ctx, "bectl", "list", "-H").Output(); err == nil {
		h.BECount, h.BEActive, h.BENext = parseBectlList(string(out))
	}
	return h
}

func freebsdVersion(ctx context.Context, flag string) string {
	out, err := exec.CommandContext(ctx, "freebsd-version", flag).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
