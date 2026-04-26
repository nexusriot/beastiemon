package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

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
	srv := api.New(ring, bweb.FS)
	sampler := collect.NewSampler(cfg)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go func() {
		log.Printf("beastied %s listening on %s", version, cfg.Server.Listen)
		if err := http.ListenAndServe(cfg.Server.Listen, srv); err != nil {
			log.Fatalf("http: %v", err)
		}
	}()

	go sampler.Run(ctx)

	for {
		select {
		case <-ctx.Done():
			log.Println("shutting down")
			return
		case snap := <-sampler.C:
			srv.Ingest(snap)
		}
	}
}
