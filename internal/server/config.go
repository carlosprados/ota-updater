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
	HTTP     HTTPYAMLConfig     `yaml:"http"`
	CoAP     CoAPYAMLConfig     `yaml:"coap"`
	Store    StoreYAMLConfig    `yaml:"store"`
	Crypto   CryptoYAMLConfig   `yaml:"crypto"`
	Target   TargetYAMLConfig   `yaml:"target"`
	Admin    AdminYAMLConfig    `yaml:"admin"`
	Logging  LoggingConfig      `yaml:"logging"`
	Manifest ManifestYAMLConfig `yaml:"manifest"`
	Metrics  MetricsYAMLConfig  `yaml:"metrics"`
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
	// DiskSpaceMinFreePct raises a startup warning when the filesystem
	// containing BinariesDir/DeltasDir has less than this percentage of
	// free space. 0 disables the check. Default 10.
	DiskSpaceMinFreePct int `yaml:"disk_space_min_free_pct"`
	// DiskSpaceMinFreeMB is the absolute-bytes equivalent; the warning
	// fires when EITHER the percent OR the MB threshold is breached.
	// Default 100 MiB.
	DiskSpaceMinFreeMB int `yaml:"disk_space_min_free_mb"`
}

// MetricsYAMLConfig toggles the separate observability HTTP listener. When
// Addr is empty the listener is not started and no metrics nor pprof are
// exposed. The listener is expected to be bound to a loopback or
// private-net address — /metrics has no auth and /debug/pprof exposes
// process internals.
type MetricsYAMLConfig struct {
	Addr         string `yaml:"addr"`          // e.g. "127.0.0.1:9100"; "" disables.
	PprofEnabled bool   `yaml:"pprof_enabled"` // mount /debug/pprof/* on the observability listener
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
	// RateLimitPerSec is the refill rate of the token bucket that throttles
	// authentication FAILURES only. 0 disables (not recommended in prod).
	// Legitimate requests with the right token are never counted.
	RateLimitPerSec float64 `yaml:"rate_limit_per_sec"`
	// RateLimitBurst is the size of the token bucket. Combined with
	// RateLimitPerSec, an attacker who floods with wrong tokens sees 429
	// after this many 401s until tokens refill.
	RateLimitBurst int `yaml:"rate_limit_burst"`
}

// adminTokenMinLen is the minimum accepted length for admin.token. Aimed at
// ~128 bits of entropy when the token is random hex (32 chars = 128 bits)
// or random base64 (22+ chars). The /admin/* endpoints have no rate limit
// yet, so short tokens are trivially brute-forceable by anyone who can
// reach the admin port.
const adminTokenMinLen = 32

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
		// NB-IoT math: a 2 MiB delta at 20 kbps takes ~13 minutes. A 60 s
		// default would cut such downloads mid-stream, forcing the agent
		// to reconnect and Range-resume — avoidable latency. 10 min
		// covers deltas up to ~1.5 MiB comfortably at 20 kbps and the
		// operator can raise it further if their fleet ships larger
		// payloads. See README "Memory bounds" for the bsdiff tradeoffs.
		c.HTTP.WriteTimeout = 10 * time.Minute
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
	if c.Admin.RateLimitPerSec == 0 {
		c.Admin.RateLimitPerSec = 5
	}
	if c.Admin.RateLimitBurst == 0 {
		c.Admin.RateLimitBurst = 20
	}
	if c.Store.DiskSpaceMinFreePct == 0 {
		c.Store.DiskSpaceMinFreePct = 10
	}
	if c.Store.DiskSpaceMinFreeMB == 0 {
		c.Store.DiskSpaceMinFreeMB = 100
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
	// Minimum length of 32 chars — roughly 128 bits of entropy when the
	// token is random hex or base64. Short tokens are trivially brute-forced
	// because /admin/* has no rate limit yet. Generate with e.g.
	// `openssl rand -hex 16` (32 hex chars) or `openssl rand -base64 24`.
	if n := len(c.Admin.Token); n < adminTokenMinLen {
		return fmt.Errorf("admin.token too short: %d chars, need at least %d "+
			"(generate with `openssl rand -hex 16`)", n, adminTokenMinLen)
	}
	if _, ok := parseLogLevel(c.Logging.Level); !ok {
		return fmt.Errorf("logging.level: unknown %q (use debug|info|warn|error)", c.Logging.Level)
	}
	return nil
}
