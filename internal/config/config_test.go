package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadMissingFileGivesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.conf"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	def := Default()
	if cfg.Collect.RingSize != def.Collect.RingSize || cfg.Collect.Interval != def.Collect.Interval {
		t.Errorf("missing file should yield defaults, got %+v", cfg.Collect)
	}
}

func TestLoadNormalizesBadValues(t *testing.T) {
	p := filepath.Join(t.TempDir(), "beastiemon.conf")
	conf := `[collect]
interval = "0s"
ring_size = 0
top_procs = -1

[store]
path = "/tmp/history.db"
retention = "0s"
resolution = "0s"
`
	if err := os.WriteFile(p, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	def := Default()
	if cfg.Collect.Interval.Duration <= 0 || cfg.Collect.Interval != def.Collect.Interval {
		t.Errorf("interval not normalized: %v", cfg.Collect.Interval.Duration)
	}
	if cfg.Collect.RingSize != def.Collect.RingSize {
		t.Errorf("ring_size not normalized: %d", cfg.Collect.RingSize)
	}
	if cfg.Collect.TopProcs != def.Collect.TopProcs {
		t.Errorf("top_procs not normalized: %d", cfg.Collect.TopProcs)
	}
	if cfg.Store.Retention.Duration != 30*24*time.Hour {
		t.Errorf("retention not normalized: %v", cfg.Store.Retention.Duration)
	}
	if cfg.Store.Resolution.Duration != time.Minute {
		t.Errorf("resolution not normalized: %v", cfg.Store.Resolution.Duration)
	}
	if cfg.Store.Path != "/tmp/history.db" {
		t.Errorf("valid setting clobbered: path = %q", cfg.Store.Path)
	}
}

func TestLoadKeepsValidValues(t *testing.T) {
	p := filepath.Join(t.TempDir(), "beastiemon.conf")
	conf := `[collect]
interval = "5s"
ring_size = 100
`
	if err := os.WriteFile(p, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Collect.Interval.Duration != 5*time.Second || cfg.Collect.RingSize != 100 {
		t.Errorf("valid values changed by normalize: %+v", cfg.Collect)
	}
}

func TestAlertsEnabledByWatchdogAlone(t *testing.T) {
	a := AlertsConfig{}
	if a.Enabled() {
		t.Error("empty alerts config should be disabled")
	}
	a.StaleAfter = Duration{30 * time.Second}
	if !a.Enabled() {
		t.Error("stale_after alone should enable the alert engine")
	}
}

func TestCoarseTierDefaults(t *testing.T) {
	def := Default()
	if def.Store.CoarseAfter.Duration != 7*24*time.Hour {
		t.Errorf("coarse_after default: %v", def.Store.CoarseAfter.Duration)
	}
	if def.Store.CoarseResolution.Duration != time.Hour {
		t.Errorf("coarse_resolution default: %v", def.Store.CoarseResolution.Duration)
	}

	// An explicit "0s" disables the tier and survives normalize.
	p := filepath.Join(t.TempDir(), "beastiemon.conf")
	if err := os.WriteFile(p, []byte("[store]\npath = \"/tmp/h.db\"\ncoarse_after = \"0s\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Store.CoarseAfter.Duration != 0 {
		t.Errorf("explicit 0s must disable the coarse tier, got %v", cfg.Store.CoarseAfter.Duration)
	}
}
