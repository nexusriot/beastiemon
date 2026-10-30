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
		// Unlike the JSON, these are always emitted: a scrape target that
		// drops a series when it is zero makes rate() and alerting awkward.
		g("beastie_net_errors", "Network error rate in packets/sec.")
		for _, n := range snap.Net {
			fmt.Fprintf(w, "beastie_net_errors{iface=\"%s\",dir=\"rx\"} %g\n", promLabel(n.Interface), n.RxErr)
			fmt.Fprintf(w, "beastie_net_errors{iface=\"%s\",dir=\"tx\"} %g\n", promLabel(n.Interface), n.TxErr)
		}
		g("beastie_net_drops", "Network drop rate in packets/sec (tx is always 0 on FreeBSD).")
		for _, n := range snap.Net {
			fmt.Fprintf(w, "beastie_net_drops{iface=\"%s\",dir=\"rx\"} %g\n", promLabel(n.Interface), n.RxDrop)
			fmt.Fprintf(w, "beastie_net_drops{iface=\"%s\",dir=\"tx\"} %g\n", promLabel(n.Interface), n.TxDrop)
		}
	}

	if len(snap.Disk) > 0 {
		g("beastie_disk_bps", "Disk throughput in bytes/sec.")
		for _, d := range snap.Disk {
			fmt.Fprintf(w, "beastie_disk_bps{dev=\"%s\",dir=\"read\"} %g\n", promLabel(d.Device), d.ReadBps)
			fmt.Fprintf(w, "beastie_disk_bps{dev=\"%s\",dir=\"write\"} %g\n", promLabel(d.Device), d.WriteBps)
		}
		g("beastie_disk_iops", "Disk operations per second.")
		for _, d := range snap.Disk {
			fmt.Fprintf(w, "beastie_disk_iops{dev=\"%s\",dir=\"read\"} %g\n", promLabel(d.Device), d.ReadIOPS)
			fmt.Fprintf(w, "beastie_disk_iops{dev=\"%s\",dir=\"write\"} %g\n", promLabel(d.Device), d.WriteIOPS)
		}
		g("beastie_disk_busy_percent", "Share of the interval the disk had requests outstanding (gstat %busy).")
		for _, d := range snap.Disk {
			fmt.Fprintf(w, "beastie_disk_busy_percent{dev=\"%s\"} %g\n", promLabel(d.Device), d.BusyPct)
		}
		g("beastie_disk_latency_ms", "Mean disk service time per request in milliseconds.")
		for _, d := range snap.Disk {
			fmt.Fprintf(w, "beastie_disk_latency_ms{dev=\"%s\",dir=\"read\"} %g\n", promLabel(d.Device), d.ReadMs)
			fmt.Fprintf(w, "beastie_disk_latency_ms{dev=\"%s\",dir=\"write\"} %g\n", promLabel(d.Device), d.WriteMs)
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
		// Health as an ordinal so it can be alerted on numerically:
		// 0 ONLINE, 1 DEGRADED, 2 anything worse. Mirrors the alert engine.
		g("beastie_zfs_health", "ZFS pool health (0 online, 1 degraded, 2 worse).")
		for _, z := range snap.ZFS {
			fmt.Fprintf(w, "beastie_zfs_health{pool=\"%s\",state=\"%s\"} %g\n",
				promLabel(z.Pool), promLabel(z.Health), promZFSHealth(z.Health))
		}
		g("beastie_zfs_scan_age_hours", "Hours since the pool's last completed scrub.")
		for _, z := range snap.ZFS {
			if !z.ScanKnown {
				continue // never scrubbed: no age to report, and 0 would lie
			}
			fmt.Fprintf(w, "beastie_zfs_scan_age_hours{pool=\"%s\"} %g\n", promLabel(z.Pool), z.ScanAgeHours)
		}
	}

	if len(snap.Datasets) > 0 {
		g("beastie_zfs_dataset_bytes", "ZFS dataset space by kind.")
		for _, d := range snap.Datasets {
			fmt.Fprintf(w, "beastie_zfs_dataset_bytes{dataset=\"%s\",kind=\"used\"} %d\n", promLabel(d.Name), d.Used)
			fmt.Fprintf(w, "beastie_zfs_dataset_bytes{dataset=\"%s\",kind=\"avail\"} %d\n", promLabel(d.Name), d.Avail)
		}
	}

	if len(snap.Watch) > 0 {
		g("beastie_proc_watch_count", "Running processes matching a watched name.")
		for _, x := range snap.Watch {
			fmt.Fprintf(w, "beastie_proc_watch_count{name=\"%s\"} %d\n", promLabel(x.Name), x.Count)
		}
	}

	if pf := snap.PF; pf != nil {
		g("beastie_pf_enabled", "Whether pf is enabled (1) or disabled (0).")
		fmt.Fprintf(w, "beastie_pf_enabled %d\n", boolGauge(pf.Enabled))
		g("beastie_pf_states", "Current pf state-table entries.")
		fmt.Fprintf(w, "beastie_pf_states %d\n", pf.States)
		if pf.StateLimit > 0 {
			g("beastie_pf_state_limit", "pf state-table hard limit.")
			fmt.Fprintf(w, "beastie_pf_state_limit %d\n", pf.StateLimit)
			g("beastie_pf_state_percent", "pf state-table utilisation percentage.")
			fmt.Fprintf(w, "beastie_pf_state_percent %g\n", pf.StatePct)
		}
		g("beastie_pf_rate", "pf counter rates per second.")
		fmt.Fprintf(w, "beastie_pf_rate{kind=\"search\"} %g\n", pf.SearchRate)
		fmt.Fprintf(w, "beastie_pf_rate{kind=\"insert\"} %g\n", pf.InsertRate)
		fmt.Fprintf(w, "beastie_pf_rate{kind=\"match\"} %g\n", pf.MatchRate)
		fmt.Fprintf(w, "beastie_pf_rate{kind=\"drop\"} %g\n", pf.DropRate)
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

	if len(snap.VMs) > 0 {
		g("beastie_vm_up", "bhyve guest state: 1 running, 0 orphaned (vmm instance without a process).")
		for _, v := range snap.VMs {
			fmt.Fprintf(w, "beastie_vm_up{vm=\"%s\"} %d\n", promLabel(v.Name), boolGauge(v.State == "running"))
		}
		g("beastie_vm_cpu_percent", "bhyve guest CPU usage (100 = one core).")
		for _, v := range snap.VMs {
			fmt.Fprintf(w, "beastie_vm_cpu_percent{vm=\"%s\"} %g\n", promLabel(v.Name), v.CPUPct)
		}
		g("beastie_vm_rss_bytes", "bhyve guest resident memory.")
		for _, v := range snap.VMs {
			fmt.Fprintf(w, "beastie_vm_rss_bytes{vm=\"%s\"} %d\n", promLabel(v.Name), v.RSS)
		}
	}

	if h := snap.Health; h != nil {
		// Unknown parts are omitted rather than reported as 0, which would
		// read as "no vulnerable packages" on a host that never fetched vuln.xml.
		if h.VulnKnown {
			g("beastie_pkg_vulnerable", "Installed packages with known vulnerabilities (pkg audit).")
			fmt.Fprintf(w, "beastie_pkg_vulnerable %d\n", h.Vulnerable)
			g("beastie_pkg_vulndb_age_hours", "Age of the local pkg vuln.xml database.")
			fmt.Fprintf(w, "beastie_pkg_vulndb_age_hours %g\n", h.VulnDBAgeHours)
		}
		if h.KernelKnown {
			g("beastie_reboot_required", "1 when the installed kernel differs from the running one.")
			fmt.Fprintf(w, "beastie_reboot_required %d\n", boolGauge(h.RebootRequired))
		}
		if h.BECount > 0 {
			g("beastie_be_pending", "1 when the next boot selects a different boot environment.")
			fmt.Fprintf(w, "beastie_be_pending %d\n", boolGauge(h.BEPending()))
		}
		g("beastie_health_checked_timestamp_seconds", "When the host-health check last ran.")
		fmt.Fprintf(w, "beastie_health_checked_timestamp_seconds %d\n", h.CheckedAt.Unix())
	}
}

func boolGauge(b bool) int {
	if b {
		return 1
	}
	return 0
}

// promZFSHealth grades pool health the same way alert.zfsHealthOrdinal does,
// so a Prometheus rule and a [[alerts.rule]] agree on what "1" means.
func promZFSHealth(health string) float64 {
	switch strings.ToUpper(strings.TrimSpace(health)) {
	case "ONLINE", "":
		return 0
	case "DEGRADED":
		return 1
	default:
		return 2
	}
}

// promLabel escapes a Prometheus label value (backslash, double-quote, newline).
func promLabel(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}
