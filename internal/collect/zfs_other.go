//go:build !freebsd

package collect

// ZFS is a FreeBSD-only feature; these stubs let the package build elsewhere.
func collectZFS(int) ([]ZFSStats, []ZFSDataset) { return nil, nil }
func collectARC() *ARCStats                     { return nil }
