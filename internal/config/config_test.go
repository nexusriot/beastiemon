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

func TestLoadNewCollectKnobs(t *testing.T) {
	p := filepath.Join(t.TempDir(), "beastiemon.conf")
	conf := `[collect]
pf = true
zfs = true
zfs_datasets = 10
proc_watch = ["sshd", "nginx"]
`
	if err := os.WriteFile(p, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Collect.PF || !cfg.Collect.ZFS {
		t.Errorf("opt-in collectors not read: %+v", cfg.Collect)
	}
	if cfg.Collect.ZFSDatasets != 10 {
		t.Errorf("zfs_datasets = %d, want 10", cfg.Collect.ZFSDatasets)
	}
	if len(cfg.Collect.ProcWatch) != 2 || cfg.Collect.ProcWatch[0] != "sshd" {
		t.Errorf("proc_watch = %v", cfg.Collect.ProcWatch)
	}
}

// Unlike the other count knobs, 0 is meaningful for zfs_datasets (collect
// none) and is the default, so only a negative value gets clamped.
func TestZFSDatasetsZeroIsMeaningful(t *testing.T) {
	if def := Default(); def.Collect.ZFSDatasets != 0 {
		t.Errorf("default zfs_datasets = %d, want 0", def.Collect.ZFSDatasets)
	}

	p := filepath.Join(t.TempDir(), "beastiemon.conf")
	if err := os.WriteFile(p, []byte("[collect]\nzfs_datasets = -5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Collect.ZFSDatasets != 0 {
		t.Errorf("negative zfs_datasets should clamp to 0, got %d", cfg.Collect.ZFSDatasets)
	}
}

// The opt-in collectors must stay off unless a config asks for them: each
// shells out every tick, and a default install should not pay for that.
func TestOptInCollectorsDefaultOff(t *testing.T) {
	def := Default()
	if def.Collect.ZFS || def.Collect.Jails || def.Collect.PF {
		t.Errorf("opt-in collectors should default off: %+v", def.Collect)
	}
	if len(def.Collect.ProcWatch) != 0 {
		t.Errorf("proc_watch should default empty, got %v", def.Collect.ProcWatch)
	}
}

// A misspelled key decodes into nothing and the setting is silently not
// applied, so the host monitors with a default nobody chose. Load records
// them; the daemon reports them from -t and at startup.
func TestLoadRecordsUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "typo.conf")
	os.WriteFile(path, []byte(`
[server]
listen = "127.0.0.1:8088"

[collect]
intervall = "5s"
ring_size = 100

[collekt]
top_procs = 9
`), 0o644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"collect.intervall", "collekt", "collekt.top_procs"}
	if len(cfg.Unknown) != len(want) {
		t.Fatalf("Unknown = %v, want %v", cfg.Unknown, want)
	}
	for i, w := range want {
		if cfg.Unknown[i] != w {
			t.Errorf("Unknown[%d] = %q, want %q", i, cfg.Unknown[i], w)
		}
	}
	// The keys that *were* spelled correctly still apply.
	if cfg.Collect.RingSize != 100 {
		t.Errorf("ring_size = %d, want the file's 100", cfg.Collect.RingSize)
	}
	// And the misspelled one did not, which is exactly why it is worth saying.
	if cfg.Collect.Interval.Duration != time.Second {
		t.Errorf("interval = %v, want the 1s default", cfg.Collect.Interval.Duration)
	}
}

// Every key the README documents must land in a field: if one is renamed or
// dropped, the unknown-key check above would start rejecting valid configs,
// which is worse than the silence it replaced.
func TestEveryDocumentedKeyDecodes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "full.conf")
	os.WriteFile(path, []byte(`
[server]
listen = "127.0.0.1:8088"

[collect]
interval = "1s"
ring_size = 3600
fs_include = ["/", "/var"]
net_exclude = ["lo0"]
top_procs = 5
zfs = true
jails = true
pf = true
zfs_datasets = 10
proc_watch = ["sshd"]

[auth]
username = "admin"
password = "pw"
token = "tok"

[store]
path = "/tmp/history.db"
retention = "720h"
resolution = "1m"
coarse_after = "168h"
coarse_resolution = "1h"

[alerts]
webhook = "https://example.com/hook"
format = "slack"
stale_after = "30s"

[[alerts.rule]]
name = "cpu-sustained"
metric = "cpu"
field = "total"
op = ">"
threshold = 90
for = "30s"
repeat = "5m"
hysteresis = 5
webhook = "https://example.com/rule"
format = "discord"
`), 0o644)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Unknown) != 0 {
		t.Errorf("documented keys reported as unknown: %v", cfg.Unknown)
	}
}
