//go:build !freebsd

package collect

func collectTemps() []TempStat { return nil }
