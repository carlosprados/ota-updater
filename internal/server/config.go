package server

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level YAML config consumed by cmd/update-server.
type Config struct {
	HTTP     HTTPYAMLConfig    `yaml:"http"`
	CoAP     CoAPYAMLConfig    `yaml:"coap"`
	Store    StoreYAMLConfig   `yaml:"store"`
	Crypto   CryptoYAMLConfig  `yaml:"crypto"`
	Target   TargetYAMLConfig  `yaml:"target"`
	Admin    AdminYAMLConfig   `yaml:"admin"`
	Logging  LoggingConfig     `yaml:"logging"`
	Manifest ManifestYAMLConfig `yaml:"manifest"`
}

type HTTPYAMLConfig struct {
	Addr              string        `yaml:"addr"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	ReadTimeout       time.Duration `yaml:"read_timeout"`
	WriteTimeout      time.Duration `yaml:"write_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
}

type CoAPYAMLConfig struct {
	Addr string `yaml:"addr"`
}

type StoreYAMLConfig struct {
	BinariesDir       string `yaml:"binaries_dir"`
	DeltasDir         string `yaml:"deltas_dir"`
	TargetMaxMemoryMB int    `yaml:"target_max_memory_mb"` // cap on keeping the active target in RAM
	HotDeltaCacheMB   int    `yaml:"hot_delta_cache_mb"`   // byte budget of the hot delta LRU
	DeltaConcurrency  int    `yaml:"delta_concurrency"`    // max concurrent bsdiff runs
}

type CryptoYAMLConfig struct {
	PrivateKey string `yaml:"private_key"`
}

type TargetYAMLConfig struct {
	Version string `yaml:"version"`
	Binary  string `yaml:"binary"`
}

type AdminYAMLConfig struct {
	Token string `yaml:"token"` // static Bearer token for /admin/* endpoints
}

// LoggingConfig selects verbosity and output format. Both values are
// case-insensitive.
type LoggingConfig struct {
	Level  string `yaml:"level"`  // debug|info|warn|error
	Format string `yaml:"format"` // text|json
}

type ManifestYAMLConfig struct {
	ChunkSize  int `yaml:"chunk_size"`
	RetryAfter int `yaml:"retry_after"`
	CacheSize  int `yaml:"cache_size"` // signed-manifest LRU entry count
}

// LoadConfig reads, parses, defaults and validates a YAML config.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.HTTP.Addr == "" {
		c.HTTP.Addr = ":8080"
	}
	if c.HTTP.ReadHeaderTimeout == 0 {
		c.HTTP.ReadHeaderTimeout = 5 * time.Second
	}
	if c.HTTP.ReadTimeout == 0 {
		c.HTTP.ReadTimeout = 30 * time.Second
	}
	if c.HTTP.WriteTimeout == 0 {
		c.HTTP.WriteTimeout = 60 * time.Second
	}
	if c.HTTP.IdleTimeout == 0 {
		c.HTTP.IdleTimeout = 120 * time.Second
	}
	if c.HTTP.ShutdownTimeout == 0 {
		c.HTTP.ShutdownTimeout = 15 * time.Second
	}
	if c.CoAP.Addr == "" {
		c.CoAP.Addr = ":5683"
	}
	if c.Store.BinariesDir == "" {
		c.Store.BinariesDir = "./store/binaries"
	}
	if c.Store.DeltasDir == "" {
		c.Store.DeltasDir = "./store/deltas"
	}
	if c.Store.TargetMaxMemoryMB == 0 {
		c.Store.TargetMaxMemoryMB = 200
	}
	if c.Store.HotDeltaCacheMB == 0 {
		c.Store.HotDeltaCacheMB = 512
	}
	if c.Store.DeltaConcurrency == 0 {
		c.Store.DeltaConcurrency = 2
	}
	if c.Manifest.CacheSize == 0 {
		c.Manifest.CacheSize = 4096
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "text"
	}
}

func (c *Config) validate() error {
	if c.Crypto.PrivateKey == "" {
		return errors.New("crypto.private_key is required")
	}
	if c.Target.Binary == "" {
		return errors.New("target.binary is required")
	}
	if c.Admin.Token == "" {
		return errors.New("admin.token is required (bearer token for /admin/* endpoints)")
	}
	if _, ok := parseLogLevel(c.Logging.Level); !ok {
		return fmt.Errorf("logging.level: unknown %q (use debug|info|warn|error)", c.Logging.Level)
	}
	return nil
}
