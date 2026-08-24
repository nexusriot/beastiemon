//go:build !freebsd

package collect

// Jails are a FreeBSD-only feature; this stub lets the package build elsewhere.
func collectJails() []JailStat { return nil }
