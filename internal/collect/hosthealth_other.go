//go:build !freebsd

package collect

import "context"

// Host health checks FreeBSD tooling (pkg, freebsd-version, bectl); this stub
// lets the package build elsewhere.
func checkHostHealth(context.Context, string) *HostHealth { return nil }
