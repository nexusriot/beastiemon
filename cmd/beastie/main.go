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
	"slices"
	"sort"
	"strings"
	"time"

	psutil_host "github.com/shirou/gopsutil/v3/host"

	"github.com/nexusriot/beastiemon/internal/alert"
	"github.com/nexusriot/beastiemon/internal/collect"
	"github.com/nexusriot/beastiemon/internal/config"
)

var version = "0.2.0"

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
	// Clamp both ends: strings.Repeat panics on a negative count, and
	// int(math.Round(NaN)) is an unspecified (typically very negative) value,
	// so an unreadable percentage would take the whole command down.
	filled := int(math.Round(pct / 100 * float64(width)))
	if filled > width {
		filled = width
	}
	if filled < 0 || math.IsNaN(pct) {
		filled = 0
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
		if t := netTrouble(n); t != "" {
			fmt.Println(t)
		}
	}
}

// netTrouble renders an error/drop line, and only when there is something to
// say — a healthy interface shouldn't carry four zeroes across the output.
func netTrouble(n collect.NetStats) string {
	if n.RxErr == 0 && n.TxErr == 0 && n.RxDrop == 0 && n.TxDrop == 0 {
		return ""
	}
	return fmt.Sprintf("        "+red+"err rx:%.0f/s tx:%.0f/s  drop rx:%.0f/s tx:%.0f/s"+reset,
		n.RxErr, n.TxErr, n.RxDrop, n.TxDrop)
}

func printZFS(snap collect.Snapshot) {
	for _, z := range snap.ZFS {
		hcol := green
		if z.Health != "" && z.Health != "ONLINE" {
			hcol = red
		}
		fmt.Printf(bold+"ZFS"+reset+"     %-12s %s %s%.1f%%%s  %s%s%s  %s\n",
			z.Pool, bar(z.UsedPct, 16), bold, z.UsedPct, reset,
			hcol, z.Health, reset, scanSummary(z))
	}
	for _, d := range snap.Datasets {
		fmt.Printf("        "+gray+"%-28s used:%-9s avail:%s"+reset+"\n",
			d.Name, humanBytes(d.Used), humanBytes(d.Avail))
	}
	if arc := snap.ARC; arc != nil {
		fmt.Printf(bold+"ARC"+reset+"     %s  %shit %.0f%%%s\n",
			humanBytes(arc.Size), cyan, arc.HitRate, reset)
	}
}

// scanSummary describes a pool's scrub state in one phrase.
func scanSummary(z collect.ZFSStats) string {
	switch z.Scan {
	case "scrub", "resilver":
		return fmt.Sprintf(gray+"%s %.1f%%"+reset, z.Scan, z.ScanPct)
	case "scrub_done", "resilver_done":
		if !z.ScanKnown {
			return ""
		}
		return fmt.Sprintf(gray+"scrubbed %s ago"+reset, humanHours(z.ScanAgeHours))
	case "none":
		return yellow + "never scrubbed" + reset
	}
	return ""
}

// humanHours renders a scrub age at the resolution an operator cares about.
func humanHours(h float64) string {
	if h >= 48 {
		return fmt.Sprintf("%.0fd", h/24)
	}
	return fmt.Sprintf("%.0fh", h)
}

func printJails(snap collect.Snapshot) {
	for _, j := range snap.Jails {
		fmt.Printf(bold+"JAIL"+reset+"    %-4d %-16s %-24s "+gray+"%d procs"+reset+"\n",
			j.JID, j.Name, j.Host, j.Procs)
	}
}

func printPF(snap collect.Snapshot) {
	pf := snap.PF
	if pf == nil {
		fmt.Println(gray + "PF      (unavailable — needs [collect] pf and read access to /dev/pf)" + reset)
		return
	}
	if !pf.Enabled {
		fmt.Println(bold + "PF" + reset + "      " + yellow + "disabled" + reset)
		return
	}
	states := fmt.Sprintf("%d", pf.States)
	if pf.StateLimit > 0 {
		states = fmt.Sprintf("%d/%d %s%.1f%%%s", pf.States, pf.StateLimit,
			levelColour(pf.StatePct), pf.StatePct, reset)
	}
	fmt.Printf(bold+"PF"+reset+"      states:%s  "+gray+"match:%.0f/s search:%.0f/s"+reset,
		states, pf.MatchRate, pf.SearchRate)
	if pf.DropRate > 0 {
		fmt.Printf("  %sdrops:%.0f/s%s", red, pf.DropRate, reset)
	}
	fmt.Println()
}

// levelColour applies the same thresholds bar() uses, for inline percentages.
func levelColour(pct float64) string {
	switch {
	case pct >= 90:
		return red
	case pct >= 70:
		return yellow
	}
	return green
}

func printWatch(snap collect.Snapshot) {
	for _, w := range snap.Watch {
		col, state := green, "running"
		if w.Count == 0 {
			col, state = red, "NOT RUNNING"
		}
		fmt.Printf(bold+"WATCH"+reset+"   %-20s %s%s%s "+gray+"(%d)"+reset+"\n",
			w.Name, col, state, reset, w.Count)
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
	// Opt-in subsystems print only when they produced something, so the
	// default view is unchanged on a host that doesn't use them.
	if len(snap.ZFS) > 0 {
		printZFS(snap)
	}
	if len(snap.Jails) > 0 {
		printJails(snap)
	}
	if snap.PF != nil {
		printPF(snap)
	}
	if len(snap.Watch) > 0 {
		printWatch(snap)
	}
	printProcs(snap)
}

// metricCommands are the commands that render one part of a snapshot. Anything
// outside this set (and the verbs main handles explicitly) is a typo, not a
// request for the default status view.
var metricCommands = map[string]bool{
	"status": true, "cpu": true, "mem": true, "net": true, "disk": true,
	"fs": true, "temp": true, "proc": true, "load": true,
	// Opt-in subsystems. These print an explanatory line rather than
	// nothing when the matching [collect] switch is off.
	"zfs": true, "jail": true, "pf": true, "watch": true,
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
	case "zfs":
		enc.Encode(struct {
			Pools    []collect.ZFSStats   `json:"zfs"`
			Datasets []collect.ZFSDataset `json:"datasets,omitempty"`
			ARC      *collect.ARCStats    `json:"arc,omitempty"`
		}{snap.ZFS, snap.Datasets, snap.ARC})
	case "jail":
		enc.Encode(snap.Jails)
	case "pf":
		enc.Encode(snap.PF)
	case "watch":
		enc.Encode(snap.Watch)
	default: // status
		enc.Encode(snap)
	}
}

// topCadence is how often `top` actually repaints. Local sampling pays a full
// delta warm-up per frame on top of the sleep between them, so the real
// cadence is ~2× the interval (see DESIGN.md §10); a remote fetch returns
// from the daemon's already-warm deltas, so there it is the interval itself.
func topCadence(interval time.Duration, remote bool) time.Duration {
	if remote {
		return interval
	}
	return 2 * interval
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

// osExit is os.Exit, indirected so tests can observe the exit status of the
// paths that end the process.
var osExit = os.Exit

// mustSnap runs get for the one-shot commands. A snapshot that could not be
// taken has nothing honest to print, so it warns and exits non-zero rather
// than rendering all-zero metrics — or, under --json, a zero-filled object —
// that a script would read as a healthy host. This is the plain-command
// counterpart of check mode's UNKNOWN/3 for the same failure.
func mustSnap(get snapshotFn) collect.Snapshot {
	snap, err := get()
	if err != nil {
		fmt.Fprintf(os.Stderr, "beastie: %v\n", err)
		osExit(1)
	}
	return snap
}

// pollSnap is mustSnap's counterpart for the continuous `top` view, where one
// failed sample should cost a frame rather than the whole session: it reports
// the failure and lets the caller skip that repaint.
func pollSnap(get snapshotFn) (collect.Snapshot, bool) {
	snap, err := get()
	if err != nil {
		fmt.Fprintf(os.Stderr, "beastie: %v\n", err)
		return snap, false
	}
	return snap, true
}

// dialable turns server.listen — a *bind* address — into one a client can
// connect to. A wildcard bind ("0.0.0.0:8088", ":8088", "[::]:8088") names
// every local interface rather than a reachable peer, and connecting to it
// fails outright on FreeBSD ("network is unreachable"). Since "auto" means
// "the daemon whose config I just read", loopback is where it answers. The
// README tells LAN installs to bind 0.0.0.0, so --remote auto has to handle
// exactly that value. Socket paths and real hosts pass through untouched.
func dialable(listen string) string {
	if strings.HasPrefix(listen, "/") {
		return listen
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	switch host {
	case "", "0.0.0.0":
		return net.JoinHostPort("127.0.0.1", port)
	case "::":
		return net.JoinHostPort("::1", port)
	}
	return listen
}

// trailingArgs returns whatever follows the command. flag.Args() is empty when
// no command was given at all — plain `beastie`, which defaults to status — so
// the slice cannot be taken blindly.
func trailingArgs(args []string) []string {
	if len(args) < 2 {
		return nil
	}
	return args[1:]
}

// flagLikeArgs picks out the arguments that look like flags, used to catch
// `beastie cpu --json` — the ordering the usage text warns about, which the
// flag package drops without a word.
func flagLikeArgs(args []string) []string {
	var bad []string
	for _, a := range args {
		if len(a) > 1 && strings.HasPrefix(a, "-") {
			bad = append(bad, a)
		}
	}
	return bad
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
		target = dialable(target)
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
	authenticate(req, cfg)
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

// alertsResp mirrors the daemon's /api/alerts envelope.
type alertsResp struct {
	Enabled bool               `json:"enabled"`
	Rules   []alert.RuleStatus `json:"rules"`
	Events  []alert.Event      `json:"events"`
}

// fetchAlerts reads /api/alerts from a running daemon. The alert engine lives
// in the daemon, so unlike the metric views this has no standalone mode:
// there is no local state to read.
func fetchAlerts(cfg config.Config, target string) (alertsResp, error) {
	var out alertsResp
	client, base, err := remoteBase(cfg, target)
	if err != nil {
		return out, err
	}
	req, err := http.NewRequest(http.MethodGet, base+"/api/alerts", nil)
	if err != nil {
		return out, err
	}
	authenticate(req, cfg)
	resp, err := client.Do(req)
	if err != nil {
		return out, fmt.Errorf("remote %s: %w", target, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return out, errors.New("remote: unauthorized (set [auth] token or username/password in the config)")
	}
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("remote: HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("remote: bad response: %w", err)
	}
	return out, nil
}

// runAlerts implements `beastie --remote <addr> alerts`. Exit status follows
// the check-mode convention so it can double as a "is anything firing" probe:
// 0 all clear, 2 something firing, 3 couldn't ask.
func runAlerts(cfg config.Config, target string, jsonOut bool) {
	if target == "" {
		fmt.Fprintln(os.Stderr, "beastie: alerts needs --remote (the alert engine runs in beastied)")
		os.Exit(3)
	}
	d, err := fetchAlerts(cfg, target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "beastie: %v\n", err)
		os.Exit(3)
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(d)
	} else {
		printAlerts(d)
	}
	if firingCount(d.Rules) > 0 {
		os.Exit(2)
	}
}

func firingCount(rules []alert.RuleStatus) int {
	n := 0
	for _, r := range rules {
		if r.State == "firing" {
			n++
		}
	}
	return n
}

func printAlerts(d alertsResp) {
	if !d.Enabled && len(d.Events) == 0 {
		fmt.Println(gray + "ALERTS  (none configured)" + reset)
		return
	}
	for _, r := range d.Rules {
		field := ""
		if r.Field != "" {
			field = "." + r.Field
		}
		fmt.Printf(bold+"ALERT"+reset+"   %-20s %s%-8s%s %-24s %s%.2f%s\n",
			nameOr(r.Name, r.Metric), stateColour(r.State), r.State, reset,
			fmt.Sprintf("%s%s %s %g", r.Metric, field, r.Op, r.Threshold),
			bold, r.Value, reset)
	}
	if len(d.Events) == 0 {
		return
	}
	fmt.Println()
	fmt.Println(gray + "Recent events:" + reset)
	for i, ev := range d.Events {
		if i == 10 {
			break
		}
		fmt.Printf("        %s  %s%-8s%s %s\n",
			ev.Time.Local().Format("2006-01-02 15:04:05"),
			stateColour(ev.State), ev.State, reset,
			nameOr(ev.Rule, ev.Metric))
	}
}

func nameOr(name, fallback string) string {
	if name == "" {
		return fallback
	}
	return name
}

func stateColour(state string) string {
	switch state {
	case "firing":
		return red
	case "pending":
		return yellow
	case "unknown":
		return gray
	case "resolved", "ok":
		return green
	}
	return ""
}

// authenticate attaches the config's credentials: the bearer token if set,
// else HTTP Basic.
func authenticate(req *http.Request, cfg config.Config) {
	if cfg.Auth.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Auth.Token)
	} else if cfg.Auth.BasicEnabled() {
		req.SetBasicAuth(cfg.Auth.Username, cfg.Auth.Password)
	}
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

// checkDefault is the field `beastie check <metric>` uses when --field is not
// given, plus the perfdata label for that default. The labels are the ones
// the command has always printed and are load-bearing for anyone graphing the
// perfdata, so they are spelled out rather than derived.
type checkDefault struct{ field, label string }

var checkDefaults = map[string]checkDefault{
	"cpu":  {"total", "cpu.total%"},
	"mem":  {"used_pct", "mem.used%"},
	"swap": {"pct", "swap.used%"},
	"load": {"load1", "load1"},
	"fs":   {"max", "fs.max_used%"},
	"temp": {"max", "temp.max_c"},
	"net":  {"total_bps", "net.total_bps"},
	"disk": {"total_bps", "disk.total_bps"},
	"zfs":  {"used_pct", "zfs.max_used%"},
	"arc":  {"hit_rate", "arc.hit_rate%"},
	"jail": {"count", "jail.count"},
	"pf":   {"state_pct", "pf.state_pct%"},
	// proc has no sensible default: there is no "worst process", only the
	// one you named. An empty field falls through to the unknown-field path.
	"proc": {"", "proc"},
}

// checkValue returns the scalar `beastie check` compares against thresholds,
// plus a short label. Higher is "worse" for every supported metric.
//
// The value itself comes from alert.Value, so a check and a [[alerts.rule]]
// written the same way measure the same thing — there is one metric table in
// the project, not two that can drift.
//
// ok is false for an unknown metric/field and for a metric with nothing to
// read: no sensors, no matching mount, no interfaces, a subsystem that isn't
// enabled. Those must not collapse to a green "0.00" — a host whose coretemp
// module never loaded, or whose monitored filesystem is not mounted, is
// exactly the case an operator needs told about.
func checkValue(snap collect.Snapshot, metric, field string) (float64, string, bool) {
	def, known := checkDefaults[metric]
	if !known {
		return 0, "", false
	}
	label := def.label
	if field == "" {
		field = def.field
	} else {
		// An explicit selector names itself, so "fs./var" rather than the
		// default's "fs.max_used%".
		label = metric + "." + field
	}
	if field == "" {
		return 0, "", false // proc without --field
	}
	// For metrics with a fixed field list, reject an unknown name up front so
	// the caller can say "no such field" instead of "nothing to measure" —
	// a typo and an unreadable sensor are different problems with different
	// fixes. Keyed metrics (fs, temp, jail, proc) accept any name by design,
	// so a miss there really is missing data.
	if fields := alert.Fields(metric); fields != nil && !slices.Contains(fields, field) {
		return 0, "", false
	}
	val, ok := alert.Value(snap, metric, field)
	return val, label, ok
}

// evalCheck applies nagios threshold semantics; thresholds <= 0 are "unset".
//
// below inverts the comparison for the metrics where small is bad — a process
// count, free space, an ARC hit rate. The two directions are exact
// complements (val >= t and val < t), so "--crit 1 --below" fires on exactly
// the values "--crit 1" would not, and a service-down check reads as
// "critical below 1 running".
func evalCheck(val, warn, crit float64, below bool) (string, int) {
	breach := func(threshold float64) bool {
		if below {
			return val < threshold
		}
		return val >= threshold
	}
	if crit > 0 && breach(crit) {
		return "CRITICAL", 2
	}
	if warn > 0 && breach(warn) {
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
	field := fs.String("field", "", "metric-specific selector, e.g. a mount, sensor, or jail name")
	below := fs.Bool("below", false, "treat lower values as worse (e.g. a process count that must stay above 0)")
	if err := fs.Parse(args); err != nil {
		os.Exit(3)
	}
	metric := fs.Arg(0)
	if metric == "" {
		fmt.Printf("UNKNOWN: check needs a metric (%s)\n", strings.Join(alert.Metrics(), "|"))
		os.Exit(3)
	}
	snap, err := getSnap()
	if err != nil {
		fmt.Printf("UNKNOWN: %v\n", err)
		os.Exit(3)
	}
	val, label, ok := checkValue(snap, metric, *field)
	if !ok {
		switch {
		case label == "" && *field != "":
			fmt.Printf("UNKNOWN: metric %q has no field %q\n", metric, *field)
		case label == "":
			fmt.Printf("UNKNOWN: unknown metric %q (want one of %s)\n",
				metric, strings.Join(alert.Metrics(), "|"))
		default:
			fmt.Printf("UNKNOWN: %s has no readings (nothing to measure)\n", label)
		}
		os.Exit(3)
	}
	status, code := evalCheck(val, *warn, *crit, *below)
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
	fmt.Printf("  %-10s  %s\n", "zfs", "ZFS pools, scrub state, datasets, ARC")
	fmt.Printf("  %-10s  %s\n", "jail", "running jails")
	fmt.Printf("  %-10s  %s\n", "pf", "pf state table and counters")
	fmt.Printf("  %-10s  %s\n", "watch", "watched processes (proc_watch)")
	fmt.Printf("  %-10s  %s\n", "top", "continuous refresh (Ctrl-C to quit)")
	fmt.Printf("  %-10s  %s\n", "check", "nagios-style threshold check (exit 0/1/2/3)")
	fmt.Printf("  %-10s  %s\n", "alerts", "rule states from a running daemon (needs --remote)")
	fmt.Printf("  %-10s  %s\n", "version", "print version")
	fmt.Println()
	fmt.Println(bold + "Flags:" + reset)
	fmt.Printf("  %-16s  %s\n", "-config <path>", "config file (default /usr/local/etc/beastiemon.conf)")
	fmt.Printf("  %-16s  %s\n", "--json", "emit JSON instead of coloured text (NDJSON for top)")
	fmt.Printf("  %-16s  %s\n", "--no-color", "disable ANSI colour and the banner")
	fmt.Printf("  %-16s  %s\n", "--remote <addr>", "read from a running beastied: \"auto\", host:port, URL, or socket path")
	fmt.Println(gray + "  (flags must precede the command; colour also auto-off when piped)" + reset)
	fmt.Println()
	fmt.Println(bold + "Check:" + reset + "  beastie check [--warn N] [--crit N] [--field F] [--below] <metric>")
	fmt.Printf("  metrics: %s\n", strings.Join(alert.Metrics(), " "))
	fmt.Println(gray + "  --field selects within a metric: a mount (fs), sensor (temp), jail name," + reset)
	fmt.Println(gray + "  watched process (proc), or a named field such as tx_bps / health / scan_age_hours." + reset)
	fmt.Println(gray + "  --below flips the comparison for metrics where small is bad:" + reset)
	fmt.Println(gray + "    beastie check --crit 1 --below --field sshd proc   # critical if sshd isn't running" + reset)
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
	colour := wantColor(tty, os.Getenv("NO_COLOR"), *noColor)
	if !colour {
		applyColor(false)
	}
	// The banner rides on the same predicate as colour: it is ASCII art whose
	// whole point is the red trident, and DESIGN.md §10 specifies NO_COLOR
	// suppresses it as well as the escapes.
	showBanner = colour

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

	case "alerts":
		runAlerts(cfg, *remote, *jsonOut)
		return

	case "top":
		if *jsonOut {
			// Stream one compact JSON snapshot per interval (NDJSON).
			enc := json.NewEncoder(os.Stdout)
			for {
				if snap, ok := pollSnap(getSnap); ok {
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
			snap, ok := pollSnap(getSnap)
			if !ok {
				time.Sleep(cfg.Collect.Interval.Duration)
				continue
			}
			// Move cursor up to overwrite previous output (14 lines max).
			fmt.Print("\033[2J\033[H") // clear screen
			printBanner()
			if host != nil {
				fmt.Printf(bold+"Host:"+reset+" %s  "+bold+"OS:"+reset+" %s %s\n\n",
					host.Hostname, host.OS, host.PlatformVersion)
			}
			printAll(snap)
			fmt.Printf(gray+"\nRefreshing every %s — Ctrl-C to quit\n"+reset,
				topCadence(cfg.Collect.Interval.Duration, *remote != ""))
			time.Sleep(cfg.Collect.Interval.Duration)
		}

	default:
		if !metricCommands[cmd] {
			fmt.Fprintf(os.Stderr, "beastie: unknown command %q\n", cmd)
			fmt.Fprintln(os.Stderr, "Run 'beastie help' for usage.")
			os.Exit(2)
		}
		// flag stops parsing at the first non-flag argument, so anything
		// flag-shaped after the command was silently dropped — `beastie cpu
		// --json` printed coloured text and exited 0, which a script piping
		// into jq reads as valid output. Only `check` takes its own flags
		// after the subcommand.
		if bad := flagLikeArgs(trailingArgs(flag.Args())); len(bad) > 0 {
			fmt.Fprintf(os.Stderr, "beastie: %s must come before the command: beastie %s %s\n",
				strings.Join(bad, " "), strings.Join(bad, " "), cmd)
			os.Exit(2)
		}
		if *jsonOut {
			emitJSON(cmd, mustSnap(getSnap))
			return
		}

		// Sample first: a failure here exits, and printing the banner and
		// host line ahead of it would leave them above the error with no
		// metrics under them.
		snap := mustSnap(getSnap)

		printBanner()

		host, _ := psutil_host.Info()
		if host != nil {
			fmt.Printf(bold+"Host:"+reset+" %s  "+bold+"OS:"+reset+" %s %s\n\n",
				host.Hostname, host.OS, host.PlatformVersion)
		}

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
		case "zfs":
			if len(snap.ZFS) == 0 {
				fmt.Println(gray + "ZFS     (no pools — needs [collect] zfs = true on a ZFS host)" + reset)
			} else {
				printZFS(snap)
			}
		case "jail":
			if len(snap.Jails) == 0 {
				fmt.Println(gray + "JAIL    (no jails — needs [collect] jails = true)" + reset)
			} else {
				printJails(snap)
			}
		case "pf":
			printPF(snap)
		case "watch":
			if len(snap.Watch) == 0 {
				fmt.Println(gray + "WATCH   (nothing watched — set [collect] proc_watch)" + reset)
			} else {
				printWatch(snap)
			}
		default: // status
			printAll(snap)
		}
	}
}
