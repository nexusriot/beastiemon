package config

import (
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Server  ServerConfig  `toml:"server"`
	Collect CollectConfig `toml:"collect"`
	Auth    AuthConfig    `toml:"auth"`
	Store   StoreConfig   `toml:"store"`
	Alerts  AlertsConfig  `toml:"alerts"`
}

type ServerConfig struct {
	// Listen is a "host:port" TCP address, or an absolute path (starting
	// with "/") for a Unix-domain socket.
	Listen string `toml:"listen"`
}

// AuthConfig controls optional HTTP authentication. Auth is disabled when
// all fields are empty (the default). Basic auth (username/password) makes
// the browser dashboard prompt for credentials natively; the bearer token
// is intended for API/CLI clients that can set an Authorization header.
type AuthConfig struct {
	Username string `toml:"username"`
	Password string `toml:"password"`
	Token    string `toml:"token"`
}

// Enabled reports whether any credential is configured.
func (a AuthConfig) Enabled() bool {
	return a.Username != "" || a.Password != "" || a.Token != ""
}

// BasicEnabled reports whether HTTP Basic auth is configured.
func (a AuthConfig) BasicEnabled() bool {
	return a.Username != "" || a.Password != ""
}

// StoreConfig controls optional on-disk persistence (SQLite). Disabled when
// Path is empty (the default): only the in-memory ring is used. When enabled,
// snapshots are downsampled to at most one row per Resolution and rows older
// than Retention are pruned, so /api/series can answer ranges beyond the ring.
type StoreConfig struct {
	Path       string   `toml:"path"`       // e.g. /var/db/beastiemon/history.db
	Retention  Duration `toml:"retention"`  // prune rows older than this (default 720h = 30d)
	Resolution Duration `toml:"resolution"` // keep at most one row per interval (default 1m)

	// CoarseAfter/CoarseResolution add a second downsampling tier: rows older
	// than CoarseAfter are re-aggregated into one row per CoarseResolution
	// (min/max envelopes merge exactly; averages become means of bucket
	// means). Defaults: 168h (7d) and 1h. Set coarse_after = "0s" to keep
	// full fine-resolution history for the whole retention window.
	CoarseAfter      Duration `toml:"coarse_after"`
	CoarseResolution Duration `toml:"coarse_resolution"`
}

// Enabled reports whether persistence is configured.
func (s StoreConfig) Enabled() bool { return s.Path != "" }

// AlertsConfig holds optional threshold alert rules. When a rule's metric
// field stays beyond its threshold for at least For, a webhook POST fires;
// another fires (with resolved=true) when it recovers.
type AlertsConfig struct {
	Webhook string      `toml:"webhook"` // default endpoint for rules that don't set their own
	Format  string      `toml:"format"`  // default payload format: raw|slack|discord
	Rules   []AlertRule `toml:"rule"`    // [[alerts.rule]] tables

	// StaleAfter arms a sampler watchdog: a synthetic "watchdog" alert fires
	// (to the section-level webhook) when no snapshot has been collected for
	// this long — the one failure threshold rules can't see, because they are
	// only evaluated when snapshots arrive. 0 (default) disables it.
	StaleAfter Duration `toml:"stale_after"`
}

// AlertRule watches one scalar drawn from a snapshot. Array metrics (fs, temp,
// disk, net) use Field as a selector: an exact name/mount, or "max" (default)
// for the worst-case value across entries.
type AlertRule struct {
	Name      string   `toml:"name"`
	Metric    string   `toml:"metric"`    // cpu|mem|swap|load|fs|temp|disk|net
	Field     string   `toml:"field"`     // metric-specific, e.g. total, used_pct, load1, max
	Op        string   `toml:"op"`        // > | >= | < | <=
	Threshold float64  `toml:"threshold"` // comparison value
	For       Duration `toml:"for"`       // sustain Duration before firing (default 0)
	Webhook   string   `toml:"webhook"`   // per-rule override of AlertsConfig.Webhook

	// Repeat re-notifies while a rule stays firing: another "firing" event is
	// sent every Repeat until it resolves. 0 (default) notifies once per episode.
	Repeat Duration `toml:"repeat"`
	// Hysteresis is a recovery margin that suppresses flapping around the
	// threshold: a firing rule only resolves once the value has receded past
	// the threshold by this much (e.g. a ">90" rule with hysteresis 5 resolves
	// at <=85). 0 (default) resolves as soon as the condition is no longer met.
	Hysteresis float64 `toml:"hysteresis"`
	// Format overrides AlertsConfig.Format for this rule's webhook payload.
	Format string `toml:"format"`
}

// Enabled reports whether any alert rule or the sampler watchdog is configured.
func (a AlertsConfig) Enabled() bool { return len(a.Rules) > 0 || a.StaleAfter.Duration > 0 }

type CollectConfig struct {
	Interval   Duration `toml:"interval"`
	FSInclude  []string `toml:"fs_include"`
	NetExclude []string `toml:"net_exclude"`
	RingSize   int      `toml:"ring_size"`
	TopProcs   int      `toml:"top_procs"`
	// ZFS and Jails are off by default: each shells out to zpool/jls/ps every
	// tick, so they only run where the host actually uses those features.
	ZFS   bool `toml:"zfs"`
	Jails bool `toml:"jails"`
}

// Duration wraps time.Duration for TOML unmarshaling.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func Default() Config {
	return Config{
		Server: ServerConfig{
			Listen: "127.0.0.1:8088",
		},
		Collect: CollectConfig{
			Interval:   Duration{time.Second},
			FSInclude:  []string{"/", "/var", "/usr", "/tmp"},
			NetExclude: []string{"lo0"},
			RingSize:   3600,
			TopProcs:   5,
		},
		Store: StoreConfig{
			// Path empty => disabled. The rest apply once a path is set.
			Retention:        Duration{30 * 24 * time.Hour},
			Resolution:       Duration{time.Minute},
			CoarseAfter:      Duration{7 * 24 * time.Hour},
			CoarseResolution: Duration{time.Hour},
		},
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return cfg, nil
	}
	_, err := toml.DecodeFile(path, &cfg)
	if err == nil {
		cfg.normalize()
	}
	return cfg, err
}

// normalize clamps nonsensical values back to their defaults so a bad config
// can't panic the daemon: time.NewTicker requires interval > 0, and a
// zero-capacity ring would panic on the first Push.
func (c *Config) normalize() {
	def := Default()
	if c.Collect.Interval.Duration <= 0 {
		c.Collect.Interval = def.Collect.Interval
	}
	if c.Collect.RingSize <= 0 {
		c.Collect.RingSize = def.Collect.RingSize
	}
	if c.Collect.TopProcs <= 0 {
		c.Collect.TopProcs = def.Collect.TopProcs
	}
	if c.Store.Retention.Duration <= 0 {
		c.Store.Retention = def.Store.Retention
	}
	if c.Store.Resolution.Duration <= 0 {
		c.Store.Resolution = def.Store.Resolution
	}
}
