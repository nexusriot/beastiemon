package config

import (
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Server  ServerConfig  `toml:"server"`
	Collect CollectConfig `toml:"collect"`
}

type ServerConfig struct {
	Listen string `toml:"listen"`
}

type CollectConfig struct {
	Interval   duration `toml:"interval"`
	FSInclude  []string `toml:"fs_include"`
	NetExclude []string `toml:"net_exclude"`
	RingSize   int      `toml:"ring_size"`
}

// duration wraps time.Duration for TOML unmarshaling.
type duration struct{ time.Duration }

func (d *duration) UnmarshalText(b []byte) error {
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
			Interval:   duration{time.Second},
			FSInclude:  []string{"/", "/var", "/usr", "/tmp"},
			NetExclude: []string{"lo0"},
			RingSize:   3600,
		},
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return cfg, nil
	}
	_, err := toml.DecodeFile(path, &cfg)
	return cfg, err
}
