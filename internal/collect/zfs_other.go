//go:build !freebsd

package collect

// ZFS is a FreeBSD-only feature; these stubs let the package build elsewhere.
func collectZFS() []ZFSStats { return nil }
func collectARC() *ARCStats  { return nil }
