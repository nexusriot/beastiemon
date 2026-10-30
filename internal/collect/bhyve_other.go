//go:build !freebsd

package collect

// bhyve is FreeBSD-only; this stub lets the package build elsewhere.
func listBhyve() ([]bhyveProc, []string, bool) { return nil, nil, false }
