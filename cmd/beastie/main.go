package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	psutil_host "github.com/shirou/gopsutil/v3/host"

	"github.com/nexusriot/beastiemon/internal/collect"
	"github.com/nexusriot/beastiemon/internal/config"
)

var version = "0.1.0"

// ANSI colour helpers
const (
	reset  = "\033[0m"
	bold   = "\033[1m"
	red    = "\033[31m"
	green  = "\033[32m"
	yellow = "\033[33m"
	cyan   = "\033[36m"
	white  = "\033[37m"
	gray   = "\033[90m"
)

// FreeBSD Beastie — trident rendered in red, body in default.
var beastieLines = []string{
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

func printBanner() {
	for _, l := range beastieLines {
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
		fmt.Printf(gray + "        cores: " + reset)
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
}

// collectOnce uses the sampler to get a single snapshot with deltas warmed up.
func collectOnce(cfg config.Config) collect.Snapshot {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sampler := collect.NewSampler(cfg)
	go sampler.Run(ctx)

	select {
	case snap := <-sampler.C:
		return snap
	case <-ctx.Done():
		return collect.Snapshot{Time: time.Now()}
	}
}

func usage() {
	printBanner()
	fmt.Println(bold + "Usage:" + reset + "  beastie [command]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Printf("  %-10s  %s\n", "status", "show all metrics (default)")
	fmt.Printf("  %-10s  %s\n", "cpu", "CPU usage per-core breakdown")
	fmt.Printf("  %-10s  %s\n", "mem", "memory and swap")
	fmt.Printf("  %-10s  %s\n", "net", "network interface throughput")
	fmt.Printf("  %-10s  %s\n", "disk", "disk I/O throughput")
	fmt.Printf("  %-10s  %s\n", "fs", "filesystem usage")
	fmt.Printf("  %-10s  %s\n", "temp", "sensor temperatures")
	fmt.Printf("  %-10s  %s\n", "top", "continuous refresh (Ctrl-C to quit)")
	fmt.Printf("  %-10s  %s\n", "version", "print version")
	fmt.Println()
	fmt.Printf(gray + "Web UI runs via beastied on http://127.0.0.1:8088/" + reset + "\n")
}

func main() {
	cfgPath := flag.String("config", "/usr/local/etc/beastiemon.conf", "config file")
	flag.Usage = usage
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	cmd := flag.Arg(0)
	if cmd == "" {
		cmd = "status"
	}

	switch cmd {
	case "version":
		fmt.Printf("beastie %s\n", version)
		return

	case "help", "-h", "--help":
		usage()
		return

	case "top":
		printBanner()
		host, _ := psutil_host.Info()
		if host != nil {
			fmt.Printf(bold+"Host:"+reset+" %s  "+bold+"OS:"+reset+" %s %s\n\n",
				host.Hostname, host.OS, host.PlatformVersion)
		}
		for {
			snap := collectOnce(cfg)
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
		printBanner()

		host, _ := psutil_host.Info()
		if host != nil {
			fmt.Printf(bold+"Host:"+reset+" %s  "+bold+"OS:"+reset+" %s %s\n\n",
				host.Hostname, host.OS, host.PlatformVersion)
		}

		snap := collectOnce(cfg)

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
		case "load":
			printLoad(snap)
		default: // status
			printAll(snap)
		}
	}
}
