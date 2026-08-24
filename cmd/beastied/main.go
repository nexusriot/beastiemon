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

var version = "0.1.0"

func main() {
	cfgPath := flag.String("config", "/usr/local/etc/beastiemon.conf", "config file path")
	vFlag := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *vFlag {
		fmt.Printf("beastied %s\n", version)
		os.Exit(0)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
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
		srv.SetStore(hist)
		defer hist.Close()
	}

	// Optional alert engine (rebuilt on reload). buildAlerts wires the engine
	// into the API (/api/alerts) and, when the store is on, persists events.
	var alerts *alert.Engine
	buildAlerts := func(ac config.AlertsConfig) {
		if !ac.Enabled() {
			alerts = nil
			srv.SetAlerts(nil)
			return
		}
		alerts = alert.New(ac)
		if hist != nil {
			alerts.SetSink(persistEvent(hist))
		}
		srv.SetAlerts(alerts)
	}
	buildAlerts(cfg.Alerts)

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// HTTP server with graceful shutdown. listen() picks TCP vs Unix socket.
	ln, err := listen(cfg.Server.Listen)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	httpSrv := &http.Server{Handler: srv}
	go func() {
		log.Printf("beastied %s listening on %s (%s)", version, cfg.Server.Listen, statusLine(cfg))
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
			buildAlerts(newCfg.Alerts)
			// Rebuild the sampler (interval, fs_include, net_exclude, top_procs, zfs, jails).
			sampCancel()
			sampCtx, sampCancel = context.WithCancel(rootCtx)
			sampler = collect.NewSampler(newCfg)
			go sampler.Run(sampCtx)
			cfg = newCfg
			log.Printf("reloaded configuration (%s)", statusLine(cfg))

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
func statusLine(cfg config.Config) string {
	auth := "auth off"
	if cfg.Auth.Enabled() {
		auth = "auth on"
	}
	store := "store off"
	if cfg.Store.Enabled() {
		store = "store on"
	}
	return fmt.Sprintf("%s, %s, %d alert rule(s)", auth, store, len(cfg.Alerts.Rules))
}
