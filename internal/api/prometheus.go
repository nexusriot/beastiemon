package api

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/nexusriot/beastiemon/internal/collect"
)

// handleMetricsProm serves the latest snapshot in Prometheus text exposition
// format at /metrics, so BeastieMon can be a scrape target. Like /api/metrics
// it returns 503 until the first sample lands. When [auth] is on, scrapers
// authenticate with the bearer token (header or ?token=), same as any endpoint.
func (s *Server) handleMetricsProm(w http.ResponseWriter, r *http.Request) {
	snap, ok := s.ring.Last()
	if !ok {
		http.Error(w, "no data yet", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	writePrometheus(w, snap)
}

// writePrometheus renders one snapshot as Prometheus metrics. Kept separate
// from the HTTP handler so it is unit-testable against a plain buffer.
func writePrometheus(w io.Writer, snap collect.Snapshot) {
	g := func(name, help string) { fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name) }

	g("beastie_cpu_percent", "CPU utilisation by mode.")
	fmt.Fprintf(w, "beastie_cpu_percent{mode=\"total\"} %g\n", snap.CPU.Total)
	fmt.Fprintf(w, "beastie_cpu_percent{mode=\"user\"} %g\n", snap.CPU.User)
	fmt.Fprintf(w, "beastie_cpu_percent{mode=\"sys\"} %g\n", snap.CPU.Sys)
	fmt.Fprintf(w, "beastie_cpu_percent{mode=\"idle\"} %g\n", snap.CPU.Idle)

	if len(snap.CPU.PerCore) > 0 {
		g("beastie_cpu_core_percent", "Per-core CPU utilisation.")
		for i, v := range snap.CPU.PerCore {
			fmt.Fprintf(w, "beastie_cpu_core_percent{core=\"%d\"} %g\n", i, v)
		}
	}

	g("beastie_mem_bytes", "Memory by kind.")
	fmt.Fprintf(w, "beastie_mem_bytes{kind=\"total\"} %d\n", snap.Mem.Total)
	fmt.Fprintf(w, "beastie_mem_bytes{kind=\"used\"} %d\n", snap.Mem.Used)
	fmt.Fprintf(w, "beastie_mem_bytes{kind=\"free\"} %d\n", snap.Mem.Free)
	fmt.Fprintf(w, "beastie_mem_bytes{kind=\"available\"} %d\n", snap.Mem.Available)
	g("beastie_mem_used_percent", "Memory used percentage.")
	fmt.Fprintf(w, "beastie_mem_used_percent %g\n", snap.Mem.UsedPct)
	g("beastie_swap_bytes", "Swap by kind.")
	fmt.Fprintf(w, "beastie_swap_bytes{kind=\"total\"} %d\n", snap.Mem.SwapTotal)
	fmt.Fprintf(w, "beastie_swap_bytes{kind=\"used\"} %d\n", snap.Mem.SwapUsed)

	g("beastie_load", "System load average.")
	fmt.Fprintf(w, "beastie_load{period=\"1\"} %g\n", snap.Load.Load1)
	fmt.Fprintf(w, "beastie_load{period=\"5\"} %g\n", snap.Load.Load5)
	fmt.Fprintf(w, "beastie_load{period=\"15\"} %g\n", snap.Load.Load15)

	g("beastie_uptime_seconds", "Host uptime in seconds.")
	fmt.Fprintf(w, "beastie_uptime_seconds %d\n", snap.Uptime)

	if len(snap.Net) > 0 {
		g("beastie_net_bps", "Network throughput in bytes/sec.")
		for _, n := range snap.Net {
			fmt.Fprintf(w, "beastie_net_bps{iface=\"%s\",dir=\"rx\"} %g\n", promLabel(n.Interface), n.RxBps)
			fmt.Fprintf(w, "beastie_net_bps{iface=\"%s\",dir=\"tx\"} %g\n", promLabel(n.Interface), n.TxBps)
		}
	}

	if len(snap.Disk) > 0 {
		g("beastie_disk_bps", "Disk throughput in bytes/sec.")
		for _, d := range snap.Disk {
			fmt.Fprintf(w, "beastie_disk_bps{dev=\"%s\",dir=\"read\"} %g\n", promLabel(d.Device), d.ReadBps)
			fmt.Fprintf(w, "beastie_disk_bps{dev=\"%s\",dir=\"write\"} %g\n", promLabel(d.Device), d.WriteBps)
		}
	}

	if len(snap.FS) > 0 {
		g("beastie_fs_used_percent", "Filesystem usage percentage.")
		for _, f := range snap.FS {
			fmt.Fprintf(w, "beastie_fs_used_percent{mount=\"%s\"} %g\n", promLabel(f.Mount), f.UsedPct)
		}
	}

	if len(snap.Temps) > 0 {
		g("beastie_temp_celsius", "Sensor temperature in Celsius.")
		for _, t := range snap.Temps {
			fmt.Fprintf(w, "beastie_temp_celsius{sensor=\"%s\"} %g\n", promLabel(t.Name), t.Celsius)
		}
	}

	if len(snap.Procs) > 0 {
		g("beastie_proc_cpu_percent", "Top process CPU percentage.")
		for _, p := range snap.Procs {
			fmt.Fprintf(w, "beastie_proc_cpu_percent{pid=\"%d\",name=\"%s\"} %g\n", p.PID, promLabel(p.Name), p.CPUPct)
		}
	}

	if len(snap.ZFS) > 0 {
		g("beastie_zfs_used_percent", "ZFS pool usage percentage.")
		for _, z := range snap.ZFS {
			fmt.Fprintf(w, "beastie_zfs_used_percent{pool=\"%s\"} %g\n", promLabel(z.Pool), z.UsedPct)
		}
	}

	if snap.ARC != nil {
		g("beastie_arc_bytes", "ZFS ARC size by kind.")
		fmt.Fprintf(w, "beastie_arc_bytes{kind=\"size\"} %d\n", snap.ARC.Size)
		fmt.Fprintf(w, "beastie_arc_bytes{kind=\"target\"} %d\n", snap.ARC.Target)
		g("beastie_arc_hit_percent", "ZFS ARC lifetime hit rate.")
		fmt.Fprintf(w, "beastie_arc_hit_percent %g\n", snap.ARC.HitRate)
	}

	if len(snap.Jails) > 0 {
		g("beastie_jail_procs", "Process count per jail.")
		for _, j := range snap.Jails {
			fmt.Fprintf(w, "beastie_jail_procs{jid=\"%d\",name=\"%s\"} %d\n", j.JID, promLabel(j.Name), j.Procs)
		}
	}
}

// promLabel escapes a Prometheus label value (backslash, double-quote, newline).
func promLabel(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}
