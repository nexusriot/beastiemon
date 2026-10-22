package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nexusriot/beastiemon/internal/alert"
	"github.com/nexusriot/beastiemon/internal/api"
	"github.com/nexusriot/beastiemon/internal/collect"
	"github.com/nexusriot/beastiemon/internal/config"
	"github.com/nexusriot/beastiemon/internal/store"
	bweb "github.com/nexusriot/beastiemon/web"
)

var version = "0.4.0"

func main() {
	cfgPath := flag.String("config", "/usr/local/etc/beastiemon.conf", "config file path")
	vFlag := flag.Bool("version", false, "print version and exit")
	tFlag := flag.Bool("t", false, "test the configuration and exit (0 ok, 1 problems)")
	flag.Parse()

	if *vFlag {
		fmt.Printf("beastied %s\n", version)
		os.Exit(0)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		if *tFlag {
			// Report through the same channel as the other -t output rather
			// than the log, so `beastied -t` is greppable from a script.
			fmt.Printf("%s: %v\n", *cfgPath, err)
			os.Exit(1)
		}
		log.Fatalf("config: %v", err)
	}

	if *tFlag {
		os.Exit(testConfig(*cfgPath, cfg))
	}

	ring := store.NewRing(cfg.Collect.RingSize)
	srv := api.New(ring, bweb.FS, cfg.Auth)

	// Optional on-disk history.
	var hist *store.SQLite
	if cfg.Store.Enabled() {
		hist, err = store.OpenSQLite(cfg.Store.Path, store.Options{
			Retention:        cfg.Store.Retention.Duration,
			Resolution:       cfg.Store.Resolution.Duration,
			CoarseAfter:      cfg.Store.CoarseAfter.Duration,
			CoarseResolution: cfg.Store.CoarseResolution.Duration,
		})
		if err != nil {
			log.Fatalf("store: %v", err)
		}
		// A store that cannot write is invisible otherwise: the startup line
		// still says "store on" and /api/series still answers from the ring.
		hist.SetErrorFunc(func(op string, err error) {
			if err == nil {
				log.Printf("store: %s is working again", op)
				return
			}
			log.Printf("store: %s failed: %v — history is not being saved", op, err)
		})
		srv.SetStore(hist)
		defer hist.Close()
	}

	// Optional alert engine (rebuilt on reload). buildAlerts wires the engine
	// into the API (/api/alerts) and, when the store is on, persists events.
	var alerts *alert.Engine
	buildAlerts := func(c config.Config) {
		if !c.Alerts.Enabled() {
			alerts = nil
			srv.SetAlerts(nil)
			return
		}
		// A typo in metric/field/op — or a proc rule naming something
		// proc_watch never counts — makes a rule silently never fire, which
		// looks exactly like a rule that is simply never tripping. Say so,
		// with the same list `beastied -t` prints.
		for _, w := range configWarnings(c) {
			log.Printf("config: %s", w)
		}
		alerts = alert.New(c.Alerts)
		// A webhook nobody answers is invisible from inside the daemon: the
		// rule still reads "firing" everywhere. Say so, naming the rule and
		// the endpoint. Volume follows the operator's own `repeat` (0, the
		// default, is one POST per episode).
		alerts.SetDeliveryErrorFunc(func(ev alert.Event, url string, err error) {
			log.Printf("alert %q: webhook %s not delivered: %v", ev.Rule, url, err)
		})
		if hist != nil {
			alerts.SetSink(persistEvent(hist))
		}
		srv.SetAlerts(alerts)
	}
	buildAlerts(cfg)

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// HTTP server with graceful shutdown. listen() picks TCP vs Unix socket.
	ln, err := listen(cfg.Server.Listen)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	httpSrv := &http.Server{Handler: srv}
	go func() {
		log.Printf("beastied %s listening on %s (%s)", version, cfg.Server.Listen,
			statusLine(cfg, hist != nil))
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	// Sampler runs under a cancelable child context so SIGHUP can swap it.
	sampCtx, sampCancel := context.WithCancel(rootCtx)
	sampler := collect.NewSampler(cfg)
	go sampler.Run(sampCtx)

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)

	// The sampler watchdog can't ride on snapshots (a wedged sampler stops
	// producing them), so a coarse wall-clock tick drives it.
	staleTick := time.NewTicker(time.Second)
	defer staleTick.Stop()

	for {
		select {
		case <-rootCtx.Done():
			log.Println("shutting down")
			sampCancel()
			srv.Close() // unblock SSE/WS handlers so Shutdown can finish promptly
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			httpSrv.Shutdown(shutCtx)
			cancel()
			return

		case <-hup:
			newCfg, err := config.Load(*cfgPath)
			if err != nil {
				log.Printf("reload: %v (keeping running config)", err)
				continue
			}
			// Warn about settings that only take effect on restart.
			if newCfg.Server.Listen != cfg.Server.Listen {
				log.Printf("reload: listen address change needs a restart")
			}
			if newCfg.Store.Path != cfg.Store.Path {
				log.Printf("reload: store path change needs a restart")
			}
			if newCfg.Collect.RingSize != cfg.Collect.RingSize {
				log.Printf("reload: ring_size change needs a restart")
			}
			// Apply hot-reloadable settings.
			srv.SetAuth(newCfg.Auth)
			buildAlerts(newCfg)
			// Rebuild the sampler (interval, fs_include, net_exclude, top_procs, zfs, jails).
			sampCancel()
			sampCtx, sampCancel = context.WithCancel(rootCtx)
			sampler = collect.NewSampler(newCfg)
			go sampler.Run(sampCtx)
			cfg = newCfg
			log.Printf("reloaded configuration (%s)", statusLine(cfg, hist != nil))

		case now := <-staleTick.C:
			if alerts != nil {
				alerts.CheckStale(now)
			}

		case snap := <-sampler.C:
			srv.Ingest(snap)
			if alerts != nil {
				alerts.Eval(snap)
			}
		}
	}
}

// testConfig implements `beastied -t`: load the config, report anything that
// would misbehave, and exit without starting anything. The rule warnings are
// the same ones startup logs — but a rule typo discovered at startup has
// already taken the reload with it, so being able to check first is the
// difference between a safe `service beastied reload` and a silent gap in
// monitoring. Returns the process exit code.
func testConfig(path string, cfg config.Config) int {
	warns := configWarnings(cfg)
	for _, w := range warns {
		fmt.Printf("%s: warning: %s\n", path, w)
	}
	if len(warns) > 0 {
		fmt.Printf("%s: configuration has %d problem(s)\n", path, len(warns))
		return 1
	}
	// Nothing is running yet, so the store is reported as the file asks for it.
	fmt.Printf("%s: syntax ok (%s)\n", path, statusLine(cfg, cfg.Store.Enabled()))
	return 0
}

// configWarnings is every "this rule can never fire" problem in cfg: the ones
// alert.Validate finds inside [alerts], plus the proc_watch mismatch that
// spans two sections. `beastied -t` prints them and startup/reload log them,
// from this one list, so the two can't drift apart — a rule that only -t
// warned about would sit at "unknown" with nothing in the log to explain it.
func configWarnings(cfg config.Config) []string {
	var warns []string
	for _, key := range cfg.Unknown {
		warns = append(warns, fmt.Sprintf(
			"unknown setting %q — check the spelling; it is being ignored", key))
	}
	warns = append(warns, alert.Validate(cfg.Alerts)...)
	return append(warns, unwatchedProcRules(cfg)...)
}

// unwatchedProcRules reports proc rules whose field is not in proc_watch.
func unwatchedProcRules(cfg config.Config) []string {
	watched := make(map[string]bool, len(cfg.Collect.ProcWatch))
	for _, n := range cfg.Collect.ProcWatch {
		watched[n] = true
	}
	var out []string
	for _, r := range cfg.Alerts.Rules {
		if r.Metric != "proc" || r.Field == "" || watched[r.Field] {
			continue
		}
		name := r.Name
		if name == "" {
			name = "(unnamed)"
		}
		out = append(out, fmt.Sprintf(
			"alert rule %q: watches process %q, but it is not in [collect] proc_watch — it will never fire",
			name, r.Field))
	}
	return out
}

// persistEvent adapts the SQLite store to the alert engine's event sink,
// storing each event as its wire JSON.
func persistEvent(hist *store.SQLite) func(alert.Event) {
	return func(ev alert.Event) {
		b, err := json.Marshal(ev)
		if err != nil {
			return
		}
		hist.SaveAlertEvent(ev.Time, ev.Rule, ev.State, b)
	}
}

// listen opens a TCP listener, or a Unix-domain socket when addr is an
// absolute path (starts with "/"). Stale sockets are removed first.
func listen(addr string) (net.Listener, error) {
	if strings.HasPrefix(addr, "/") {
		_ = os.Remove(addr)
		ln, err := net.Listen("unix", addr)
		if err != nil {
			return nil, err
		}
		_ = os.Chmod(addr, 0o660)
		return ln, nil
	}
	return net.Listen("tcp", addr)
}

// statusLine summarises the enabled optional subsystems for the startup log.
// storeActive is whether a history store is really open, which is not the same
// as cfg asking for one: a reload cannot attach a store (the path change needs
// a restart), and logging "store on" there would promise persistence that is
// not happening.
func statusLine(cfg config.Config, storeActive bool) string {
	auth := "auth off"
	if cfg.Auth.Enabled() {
		auth = "auth on"
	}
	store := "store off"
	if storeActive {
		store = "store on"
	}
	return fmt.Sprintf("%s, %s, %d alert rule(s)", auth, store, len(cfg.Alerts.Rules))
}
