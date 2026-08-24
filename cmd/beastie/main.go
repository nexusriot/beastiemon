package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	psutil_host "github.com/shirou/gopsutil/v3/host"

	"github.com/nexusriot/beastiemon/internal/collect"
	"github.com/nexusriot/beastiemon/internal/config"
)

var version = "0.1.0"

// ANSI colour helpers. These are vars, not consts, so applyColor() can blank
// them when output isn't a terminal (or NO_COLOR / --no-color is set).
var (
	reset  = "\033[0m"
	bold   = "\033[1m"
	red    = "\033[31m"
	green  = "\033[32m"
	yellow = "\033[33m"
	cyan   = "\033[36m"
	white  = "\033[37m"
	gray   = "\033[90m"
)

// showBanner gates the mascot; false when stdout isn't a terminal.
var showBanner = true

// bannerLines builds the Beastie mascot using the current colour vars, so it
// reflects applyColor()'s decision (trident in red, body default/grey).
func bannerLines() []string {
	return []string{
		red + `    ,        ,` + reset,
		red + `   /(        )` + "`" + reset,
		red + `   \ \___   / |` + reset,
		red + `   /- _  ` + "`" + `-/  '` + reset,
		red + `  (/\/ \ \   /\` + reset,
		red + `  / /   | ` + "`" + `   /` + reset,
		`  ` + red + `O` + reset + ` ` + red + `O` + reset + `   ) /   |`,
		red + "  `-^--'" + "`" + `<     '` + reset,
		gray + ` (_.)  _  )   /` + reset,
		gray + `  ` + "`" + `.___/` + "`" + `    /` + reset,
		gray + `    ` + "`" + `-----' /` + reset,
		red + `<----.     '__\` + reset,
		red + `<----|====O)))==)` + reset,
		red + `<----'    ` + "`" + `--'` + reset,
	}
}

func printBanner() {
	if !showBanner {
		return
	}
	for _, l := range bannerLines() {
		fmt.Println(l)
	}
	fmt.Printf(bold+cyan+"    BeastieMon v%s"+reset+"  — FreeBSD system monitor\n\n", version)
}

// bar renders a coloured progress bar of given width.
func bar(pct float64, width int) string {
	filled := int(math.Round(pct / 100 * float64(width)))
	if filled > width {
		filled = width
	}
	colour := green
	switch {
	case pct >= 90:
		colour = red
	case pct >= 70:
		colour = yellow
	}
	return colour + strings.Repeat("█", filled) + gray + strings.Repeat("░", width-filled) + reset
}

// humanBytes formats bytes as human-readable.
func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func humanDuration(secs uint64) string {
	d := secs / 86400
	h := (secs % 86400) / 3600
	m := (secs % 3600) / 60
	s := secs % 60
	if d > 0 {
		return fmt.Sprintf("%dd %02d:%02d:%02d", d, h, m, s)
	}
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

func printCPU(snap collect.Snapshot) {
	c := snap.CPU
	fmt.Printf(bold+"CPU"+reset+"     %s %s%.1f%%%s",
		bar(c.Total, 20), bold, c.Total, reset)
	fmt.Printf("  user:%s%.1f%%%s  sys:%s%.1f%%%s  idle:%s%.1f%%%s\n",
		cyan, c.User, reset, yellow, c.Sys, reset, gray, c.Idle, reset)

	if len(c.PerCore) > 0 {
		fmt.Print(gray + "        cores: " + reset)
		for i, v := range c.PerCore {
			col := green
			if v >= 90 {
				col = red
			} else if v >= 70 {
				col = yellow
			}
			fmt.Printf("%scpu%d:%s%.0f%%%s ", col, i, reset, v, reset)
		}
		fmt.Println()
	}
}

func printMem(snap collect.Snapshot) {
	m := snap.Mem
	fmt.Printf(bold+"MEM"+reset+"     %s %s%.1f%%%s",
		bar(m.UsedPct, 20), bold, m.UsedPct, reset)
	fmt.Printf("  used:%s%s%s  free:%s  total:%s\n",
		cyan, humanBytes(m.Used), reset, humanBytes(m.Free), humanBytes(m.Total))

	if m.SwapTotal > 0 {
		fmt.Printf(bold+"SWAP"+reset+"    %s %s%.1f%%%s",
			bar(m.SwapPct, 20), bold, m.SwapPct, reset)
		fmt.Printf("  used:%s%s%s  total:%s\n",
			cyan, humanBytes(m.SwapUsed), reset, humanBytes(m.SwapTotal))
	}
}

func printNet(snap collect.Snapshot) {
	if len(snap.Net) == 0 {
		fmt.Println(gray + "NET     (no data)" + reset)
		return
	}
	// Sort by interface name for stable output.
	nets := append([]collect.NetStats(nil), snap.Net...)
	sort.Slice(nets, func(i, j int) bool { return nets[i].Interface < nets[j].Interface })
	for _, n := range nets {
		fmt.Printf(bold+"NET"+reset+"     %-8s  "+cyan+"↓"+reset+" %-12s  "+yellow+"↑"+reset+" %-12s  "+gray+"rx:%.0fpps tx:%.0fpps"+reset+"\n",
			n.Interface,
			humanBytes(uint64(n.RxBps))+"/s",
			humanBytes(uint64(n.TxBps))+"/s",
			n.RxPps, n.TxPps)
	}
}

func printDisk(snap collect.Snapshot) {
	if len(snap.Disk) == 0 {
		fmt.Println(gray + "DISK    (no data — may need operator group)" + reset)
		return
	}
	disks := append([]collect.DiskStats(nil), snap.Disk...)
	sort.Slice(disks, func(i, j int) bool { return disks[i].Device < disks[j].Device })
	for _, d := range disks {
		fmt.Printf(bold+"DISK"+reset+"    %-8s  R:"+cyan+"%-12s"+reset+" W:"+yellow+"%-12s"+reset+" "+gray+"riops:%.0f wiops:%.0f"+reset+"\n",
			d.Device,
			humanBytes(uint64(d.ReadBps))+"/s",
			humanBytes(uint64(d.WriteBps))+"/s",
			d.ReadIOPS, d.WriteIOPS)
	}
}

func printFS(snap collect.Snapshot) {
	for _, f := range snap.FS {
		fmt.Printf(bold+"FS"+reset+"      %-12s %s %s%.1f%%%s  used:%s  free:%s  total:%s\n",
			f.Mount, bar(f.UsedPct, 16), bold, f.UsedPct, reset,
			humanBytes(f.Used), humanBytes(f.Free), humanBytes(f.Total))
	}
}

func printTemps(snap collect.Snapshot) {
	if len(snap.Temps) == 0 {
		fmt.Println(gray + "TEMP    (unavailable — load coretemp or amdtemp kmod)" + reset)
		return
	}
	for _, t := range snap.Temps {
		col := green
		if t.Celsius >= 80 {
			col = red
		} else if t.Celsius >= 65 {
			col = yellow
		}
		fmt.Printf(bold+"TEMP"+reset+"    %-8s  %s%.1f°C%s\n", t.Name, col, t.Celsius, reset)
	}
}

func printProcs(snap collect.Snapshot) {
	if len(snap.Procs) == 0 {
		fmt.Println(gray + "PROC    (no data — needs two samples)" + reset)
		return
	}
	fmt.Printf(bold+"PROC"+reset+gray+"    %-7s %6s %6s %10s  %s\n"+reset,
		"PID", "CPU%", "MEM%", "RSS", "COMMAND")
	for _, p := range snap.Procs {
		col := green
		if p.CPUPct >= 90 {
			col = red
		} else if p.CPUPct >= 50 {
			col = yellow
		}
		fmt.Printf("        %-7d %s%6.1f%s %6.1f %10s  %s\n",
			p.PID, col, p.CPUPct, reset, p.MemPct, humanBytes(p.RSS), p.Name)
	}
}

func printLoad(snap collect.Snapshot) {
	l := snap.Load
	col1 := green
	if l.Load1 >= 2 {
		col1 = red
	} else if l.Load1 >= 1 {
		col1 = yellow
	}
	fmt.Printf(bold+"LOAD"+reset+"    %s%.2f%s  %.2f  %.2f\n",
		col1, l.Load1, reset, l.Load5, l.Load15)
}

func printUptime(snap collect.Snapshot) {
	fmt.Printf(bold+"UPTIME"+reset+"  %s\n", humanDuration(snap.Uptime))
}

func printAll(snap collect.Snapshot) {
	printCPU(snap)
	printMem(snap)
	printNet(snap)
	printDisk(snap)
	printFS(snap)
	printTemps(snap)
	printLoad(snap)
	printUptime(snap)
	printProcs(snap)
}

// emitJSON prints the part of the snapshot relevant to cmd as indented JSON.
// Sub-objects reuse the same struct tags as the daemon's /api/metrics, so
// CLI and API output are byte-identical for a given metric.
func emitJSON(cmd string, snap collect.Snapshot) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	switch cmd {
	case "cpu":
		enc.Encode(snap.CPU)
	case "mem":
		enc.Encode(snap.Mem)
	case "net":
		enc.Encode(snap.Net)
	case "disk":
		enc.Encode(snap.Disk)
	case "fs":
		enc.Encode(snap.FS)
	case "temp":
		enc.Encode(snap.Temps)
	case "proc":
		enc.Encode(snap.Procs)
	case "load":
		enc.Encode(snap.Load)
	default: // status
		enc.Encode(snap)
	}
}

// collectTimeout bounds collectOnce. The sampler sleeps one full interval to
// warm its delta-based collectors before the first snapshot, so the timeout
// must scale with the configured interval — a fixed bound shorter than the
// interval would make every command silently report zeros.
func collectTimeout(interval time.Duration) time.Duration {
	t := 2*interval + 2*time.Second
	if t < 5*time.Second {
		t = 5 * time.Second
	}
	return t
}

// collectOnce uses the sampler to get a single snapshot with deltas warmed up.
// ok is false when collection timed out and the snapshot is empty.
func collectOnce(cfg config.Config) (collect.Snapshot, bool) {
	ctx, cancel := context.WithTimeout(context.Background(),
		collectTimeout(cfg.Collect.Interval.Duration))
	defer cancel()

	sampler := collect.NewSampler(cfg)
	go sampler.Run(ctx)

	select {
	case snap := <-sampler.C:
		return snap, true
	case <-ctx.Done():
		return collect.Snapshot{Time: time.Now()}, false
	}
}

// snapshotFn produces one snapshot: local sampling or a remote daemon fetch.
type snapshotFn func() (collect.Snapshot, error)

// mustSnap runs get for the human-facing commands: on failure it still
// returns the (empty) snapshot but warns on stderr instead of printing
// all-zero metrics as if they were real.
func mustSnap(get snapshotFn) collect.Snapshot {
	snap, err := get()
	if err != nil {
		fmt.Fprintf(os.Stderr, "beastie: %v; output may be empty\n", err)
	}
	return snap
}

// remoteBase resolves the -remote flag value to an HTTP client and base URL.
// Accepted forms: "auto" (use server.listen from the config), an absolute
// path (beastied's Unix socket), host:port, or a full http(s) URL.
func remoteBase(cfg config.Config, target string) (*http.Client, string, error) {
	if target == "auto" {
		target = cfg.Server.Listen
		if target == "" {
			return nil, "", errors.New("remote auto: no server.listen in config")
		}
	}
	client := &http.Client{Timeout: 5 * time.Second}
	switch {
	case strings.HasPrefix(target, "/"):
		sock := target
		client.Transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
		}
		// The host part is a placeholder; the transport dials the socket.
		return client, "http://beastied", nil
	case strings.HasPrefix(target, "http://"), strings.HasPrefix(target, "https://"):
		return client, strings.TrimSuffix(target, "/"), nil
	default:
		return client, "http://" + target, nil
	}
}

// fetchRemote reads the latest snapshot from a running beastied's
// /api/metrics, authenticating with the config's token (or basic
// credentials). Unlike local sampling it returns instantly — the daemon's
// deltas are already warm.
func fetchRemote(cfg config.Config, target string) (collect.Snapshot, error) {
	client, base, err := remoteBase(cfg, target)
	if err != nil {
		return collect.Snapshot{}, err
	}
	req, err := http.NewRequest(http.MethodGet, base+"/api/metrics", nil)
	if err != nil {
		return collect.Snapshot{}, err
	}
	if cfg.Auth.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Auth.Token)
	} else if cfg.Auth.BasicEnabled() {
		req.SetBasicAuth(cfg.Auth.Username, cfg.Auth.Password)
	}
	resp, err := client.Do(req)
	if err != nil {
		return collect.Snapshot{}, fmt.Errorf("remote %s: %w", target, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusServiceUnavailable:
		return collect.Snapshot{}, errors.New("remote: daemon has no data yet")
	case http.StatusUnauthorized:
		return collect.Snapshot{}, errors.New("remote: unauthorized (set [auth] token or username/password in the config)")
	default:
		return collect.Snapshot{}, fmt.Errorf("remote: HTTP %d", resp.StatusCode)
	}
	var snap collect.Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return collect.Snapshot{}, fmt.Errorf("remote: bad response: %w", err)
	}
	return snap, nil
}

// isTerminal reports whether f is a character device (a TTY). Used to decide
// colour/banner; works on FreeBSD/Linux with no extra dependency.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// wantColor decides whether ANSI colour should be emitted: only on a terminal,
// and never when NO_COLOR (see no-color.org) or --no-color is set.
func wantColor(isTTY bool, noColorEnv string, noColorFlag bool) bool {
	if noColorFlag || noColorEnv != "" {
		return false
	}
	return isTTY
}

// applyColor(false) blanks every colour escape so output is plain text.
func applyColor(on bool) {
	if on {
		return
	}
	reset, bold, red, green, yellow, cyan, white, gray = "", "", "", "", "", "", "", ""
}

// checkValue returns the scalar `beastie check` compares against thresholds,
// plus a short label. Higher is "worse" for every supported metric.
func checkValue(snap collect.Snapshot, metric string) (float64, string, bool) {
	switch metric {
	case "cpu":
		return snap.CPU.Total, "cpu.total%", true
	case "mem":
		return snap.Mem.UsedPct, "mem.used%", true
	case "swap":
		return snap.Mem.SwapPct, "swap.used%", true
	case "load":
		return snap.Load.Load1, "load1", true
	case "fs":
		var max float64
		for _, f := range snap.FS {
			if f.UsedPct > max {
				max = f.UsedPct
			}
		}
		return max, "fs.max_used%", true
	case "temp":
		var max float64
		for _, t := range snap.Temps {
			if t.Celsius > max {
				max = t.Celsius
			}
		}
		return max, "temp.max_c", true
	case "net":
		var sum float64
		for _, n := range snap.Net {
			sum += n.RxBps + n.TxBps
		}
		return sum, "net.total_bps", true
	case "disk":
		var sum float64
		for _, d := range snap.Disk {
			sum += d.ReadBps + d.WriteBps
		}
		return sum, "disk.total_bps", true
	}
	return 0, "", false
}

// evalCheck applies nagios threshold semantics; thresholds <= 0 are "unset".
func evalCheck(val, warn, crit float64) (string, int) {
	if crit > 0 && val >= crit {
		return "CRITICAL", 2
	}
	if warn > 0 && val >= warn {
		return "WARNING", 1
	}
	return "OK", 0
}

// runCheck implements `beastie check [--warn N] [--crit N] <metric>`: it prints
// one nagios plugin line with perfdata and exits 0/1/2 (3 = UNKNOWN). Output
// is always plain text (no banner/colour) so it drops into monitoring cleanly.
func runCheck(getSnap snapshotFn, args []string) {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	warn := fs.Float64("warn", 0, "warning threshold")
	crit := fs.Float64("crit", 0, "critical threshold")
	if err := fs.Parse(args); err != nil {
		os.Exit(3)
	}
	metric := fs.Arg(0)
	if metric == "" {
		fmt.Println("UNKNOWN: check needs a metric (cpu|mem|swap|load|fs|temp|net|disk)")
		os.Exit(3)
	}
	snap, err := getSnap()
	if err != nil {
		fmt.Printf("UNKNOWN: %v\n", err)
		os.Exit(3)
	}
	val, label, ok := checkValue(snap, metric)
	if !ok {
		fmt.Printf("UNKNOWN: unknown metric %q\n", metric)
		os.Exit(3)
	}
	status, code := evalCheck(val, *warn, *crit)
	fmt.Printf("%s: %s = %.2f | value=%.2f;%.0f;%.0f\n", status, label, val, val, *warn, *crit)
	os.Exit(code)
}

func usage() {
	printBanner()
	fmt.Println(bold + "Usage:" + reset + "  beastie [flags] [command]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Printf("  %-10s  %s\n", "status", "show all metrics (default)")
	fmt.Printf("  %-10s  %s\n", "cpu", "CPU usage per-core breakdown")
	fmt.Printf("  %-10s  %s\n", "mem", "memory and swap")
	fmt.Printf("  %-10s  %s\n", "net", "network interface throughput")
	fmt.Printf("  %-10s  %s\n", "disk", "disk I/O throughput")
	fmt.Printf("  %-10s  %s\n", "fs", "filesystem usage")
	fmt.Printf("  %-10s  %s\n", "temp", "sensor temperatures")
	fmt.Printf("  %-10s  %s\n", "proc", "top processes by CPU")
	fmt.Printf("  %-10s  %s\n", "load", "load average")
	fmt.Printf("  %-10s  %s\n", "top", "continuous refresh (Ctrl-C to quit)")
	fmt.Printf("  %-10s  %s\n", "check", "nagios-style threshold check (exit 0/1/2/3)")
	fmt.Printf("  %-10s  %s\n", "version", "print version")
	fmt.Println()
	fmt.Println(bold + "Flags:" + reset)
	fmt.Printf("  %-16s  %s\n", "-config <path>", "config file (default /usr/local/etc/beastiemon.conf)")
	fmt.Printf("  %-16s  %s\n", "--json", "emit JSON instead of coloured text (NDJSON for top)")
	fmt.Printf("  %-16s  %s\n", "--no-color", "disable ANSI colour and the banner")
	fmt.Printf("  %-16s  %s\n", "--remote <addr>", "read from a running beastied: \"auto\", host:port, URL, or socket path")
	fmt.Println(gray + "  (flags must precede the command; colour also auto-off when piped)" + reset)
	fmt.Println()
	fmt.Println(bold + "Check:" + reset + "  beastie check [--warn N] [--crit N] <cpu|mem|swap|load|fs|temp|net|disk>")
	fmt.Println()
	fmt.Print(gray + "Web UI runs via beastied on http://127.0.0.1:8088/" + reset + "\n")
}

func main() {
	cfgPath := flag.String("config", "/usr/local/etc/beastiemon.conf", "config file")
	jsonOut := flag.Bool("json", false, "emit JSON instead of coloured text")
	noColor := flag.Bool("no-color", false, "disable ANSI colour and the banner")
	remote := flag.String("remote", "",
		`read metrics from a running beastied instead of sampling locally: "auto" (use server.listen from config), host:port, URL, or a Unix socket path`)
	flag.Usage = usage
	flag.Parse()

	// Colour + banner: on only for an interactive terminal, and never when
	// NO_COLOR or --no-color is set. Keeps pipes/files/less free of escapes.
	tty := isTerminal(os.Stdout)
	if !wantColor(tty, os.Getenv("NO_COLOR"), *noColor) {
		applyColor(false)
	}
	showBanner = tty && !*noColor

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	// getSnap is the single snapshot source every command shares: remote
	// fetch when -remote is set (instant — the daemon's deltas are warm),
	// else one local sampling pass.
	getSnap := func() (collect.Snapshot, error) {
		if *remote != "" {
			return fetchRemote(cfg, *remote)
		}
		snap, ok := collectOnce(cfg)
		if !ok {
			return snap, errors.New("metric collection timed out")
		}
		return snap, nil
	}

	cmd := flag.Arg(0)
	if cmd == "" {
		cmd = "status"
	}

	switch cmd {
	case "version":
		if *jsonOut {
			json.NewEncoder(os.Stdout).Encode(map[string]string{"version": version})
		} else {
			fmt.Printf("beastie %s\n", version)
		}
		return

	case "help", "-h", "--help":
		usage()
		return

	case "check":
		runCheck(getSnap, flag.Args()[1:])
		return

	case "top":
		if *jsonOut {
			// Stream one compact JSON snapshot per interval (NDJSON).
			enc := json.NewEncoder(os.Stdout)
			for {
				if snap, err := getSnap(); err == nil {
					enc.Encode(snap)
				}
				time.Sleep(cfg.Collect.Interval.Duration)
			}
		}
		printBanner()
		host, _ := psutil_host.Info()
		if host != nil {
			fmt.Printf(bold+"Host:"+reset+" %s  "+bold+"OS:"+reset+" %s %s\n\n",
				host.Hostname, host.OS, host.PlatformVersion)
		}
		for {
			snap := mustSnap(getSnap)
			// Move cursor up to overwrite previous output (14 lines max).
			fmt.Print("\033[2J\033[H") // clear screen
			printBanner()
			if host != nil {
				fmt.Printf(bold+"Host:"+reset+" %s  "+bold+"OS:"+reset+" %s %s\n\n",
					host.Hostname, host.OS, host.PlatformVersion)
			}
			printAll(snap)
			fmt.Printf(gray+"\nRefreshing every %s — Ctrl-C to quit\n"+reset,
				cfg.Collect.Interval.Duration)
			time.Sleep(cfg.Collect.Interval.Duration)
		}

	default:
		if *jsonOut {
			emitJSON(cmd, mustSnap(getSnap))
			return
		}

		printBanner()

		host, _ := psutil_host.Info()
		if host != nil {
			fmt.Printf(bold+"Host:"+reset+" %s  "+bold+"OS:"+reset+" %s %s\n\n",
				host.Hostname, host.OS, host.PlatformVersion)
		}

		snap := mustSnap(getSnap)

		switch cmd {
		case "cpu":
			printCPU(snap)
		case "mem":
			printMem(snap)
		case "net":
			printNet(snap)
		case "disk":
			printDisk(snap)
		case "fs":
			printFS(snap)
		case "temp":
			printTemps(snap)
		case "proc":
			printProcs(snap)
		case "load":
			printLoad(snap)
		default: // status
			printAll(snap)
		}
	}
}
