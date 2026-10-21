//go:build !freebsd

package collect

// pf is a FreeBSD-only feature; these stubs let the package build elsewhere.
func pfctlInfo() (pfRaw, bool) { return pfRaw{}, false }
func pfctlMemoryLimit() uint64 { return 0 }
