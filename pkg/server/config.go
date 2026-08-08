package server

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/carlosprados/ota-updater/pkg/protocol"
)

// Config is the top-level YAML config consumed by cmd/update-server.
type Config struct {
	HTTP  HTTPYAMLConfig  `yaml:"http"`
	CoAP  CoAPYAMLConfig  `yaml:"coap"`
	Store StoreYAMLConfig `yaml:"store"`
	// Artifacts declares the publication tracks served by this instance.
	// Config is authoritative for the artifacts it lists: on every boot they
	// are re-published from their `binary` path, overriding whatever the
	// admin API left in the persisted registry.
	Artifacts []ArtifactYAMLConfig `yaml:"artifacts"`
	// DefaultArtifact is the canonical key ("name" or "name/os/arch") that
	// answers heartbeats which name no artifact. Optional when exactly one
	// artifact is configured.
	DefaultArtifact string              `yaml:"default_artifact"`
	Crypto          CryptoYAMLConfig    `yaml:"crypto"`
	Target          TargetYAMLConfig    `yaml:"target"`
	Admin           AdminYAMLConfig     `yaml:"admin"`
	Logging         LoggingConfig       `yaml:"logging"`
	Manifest        ManifestYAMLConfig  `yaml:"manifest"`
	Metrics         MetricsYAMLConfig   `yaml:"metrics"`
	Retention       RetentionYAMLConfig `yaml:"retention"`
}

// ArtifactYAMLConfig declares one publication track.
type ArtifactYAMLConfig struct {
	Name    string `yaml:"name"`    // required
	OS      string `yaml:"os"`      // optional; must be set together with arch
	Arch    string `yaml:"arch"`    // optional; must be set together with os
	Version string `yaml:"version"` // human-readable label returned to agents
	Binary  string `yaml:"binary"`  // path to the current target; watched for changes
}

// LegacyTargetArtifactName is the artifact key synthesized from a bare
// `target:` block. It exists so single-artifact configs written before
// multi-artifact support keep working verbatim.
const LegacyTargetArtifactName = "default"

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
	BinariesDir string `yaml:"binaries_dir"`
	DeltasDir   string `yaml:"deltas_dir"`
	// StateFile is where the artifact registry persists itself. Without it,
	// artifacts published through the admin API vanish on restart.
	StateFile string `yaml:"state_file"`
	// TargetCacheMB is the byte budget for uncompressed delta-target binaries
	// held in RAM, shared across all artifacts.
	TargetCacheMB    int `yaml:"target_cache_mb"`
	HotDeltaCacheMB  int `yaml:"hot_delta_cache_mb"` // byte budget of the hot transfer LRU
	DeltaConcurrency int `yaml:"delta_concurrency"`  // max concurrent bsdiff runs
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
	// AllowFullDownload lets the server offer the whole compressed target to
	// agents whose current version it cannot diff against. Pointer so an
	// omitted key means "default true" while an explicit `false` is honored.
	AllowFullDownload *bool `yaml:"allow_full_download"`
}

// RetentionYAMLConfig bounds how much disk the store may consume over time.
//
// Nothing here is required for correctness — with retention disabled the
// server works exactly as before and grows without limit. It matters at the
// project's target scale: N components × M versions × K architectures means
// every release multiplies both binaries and (from,to) delta pairs, and a
// server that never collects will eventually fill its filesystem and start
// failing writes mid-campaign.
type RetentionYAMLConfig struct {
	// Enabled turns the background sweeper on. Default false: silently
	// deleting an operator's binaries is not something to opt out of.
	Enabled bool `yaml:"enabled"`
	// Interval between sweeps. Default 6h.
	Interval time.Duration `yaml:"interval"`
	// HistoryDepth is how many superseded targets each artifact keeps as
	// valid delta sources. Devices further behind than this still update,
	// via full download. Default 10.
	HistoryDepth int `yaml:"history_depth"`
	// DeltaMaxAge deletes cached transfer artifacts (deltas and compressed
	// binaries) untouched for longer than this. They are pure cache: deleting
	// one costs a regeneration, never data. 0 disables. Default 720h (30d).
	DeltaMaxAge time.Duration `yaml:"delta_max_age"`
	// DeltasMaxTotalMB caps the total size of the deltas dir; oldest first
	// are deleted until it fits. 0 disables. Default 0.
	DeltasMaxTotalMB int `yaml:"deltas_max_total_mb"`
	// CollectOrphanBinaries deletes binaries that no artifact references as
	// a current target or in its history. This is the only setting that can
	// destroy something not reconstructible, so it defaults to false.
	CollectOrphanBinaries bool `yaml:"collect_orphan_binaries"`
	// OrphanBinaryMinAge protects freshly-written binaries from being
	// collected in the window between RegisterBinary and the Publish that
	// references them. Default 24h.
	OrphanBinaryMinAge time.Duration `yaml:"orphan_binary_min_age"`
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
	if c.Store.TargetCacheMB == 0 {
		c.Store.TargetCacheMB = 200
	}
	if c.Store.StateFile == "" {
		c.Store.StateFile = "./store/artifacts.json"
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
	if c.Manifest.AllowFullDownload == nil {
		// Default on. A device the server cannot diff against is otherwise
		// stranded forever, which is a far worse outcome than one expensive
		// transfer.
		enabled := true
		c.Manifest.AllowFullDownload = &enabled
	}
	if c.Retention.Interval == 0 {
		c.Retention.Interval = 6 * time.Hour
	}
	if c.Retention.HistoryDepth == 0 {
		c.Retention.HistoryDepth = DefaultHistoryDepth
	}
	if c.Retention.DeltaMaxAge == 0 {
		c.Retention.DeltaMaxAge = 720 * time.Hour
	}
	if c.Retention.OrphanBinaryMinAge == 0 {
		c.Retention.OrphanBinaryMinAge = 24 * time.Hour
	}

	// Fold a legacy single-target config into the artifact list. Doing it
	// here means everything downstream — registry, watcher, admin, retention —
	// only ever deals with artifacts and never special-cases the old shape.
	if len(c.Artifacts) == 0 && c.Target.Binary != "" {
		c.Artifacts = []ArtifactYAMLConfig{{
			Name:    LegacyTargetArtifactName,
			Version: c.Target.Version,
			Binary:  c.Target.Binary,
		}}
		if c.DefaultArtifact == "" {
			c.DefaultArtifact = LegacyTargetArtifactName
		}
	}
	// A single declared artifact is unambiguously the default.
	if c.DefaultArtifact == "" && len(c.Artifacts) == 1 {
		c.DefaultArtifact = artifactKeyOf(c.Artifacts[0]).String()
	}
}

// artifactKeyOf builds the protocol key for a declared artifact.
func artifactKeyOf(a ArtifactYAMLConfig) protocol.ArtifactKey {
	return protocol.ArtifactKey{Name: a.Name, OS: a.OS, Arch: a.Arch}
}

// AllowFullDownload reports the effective setting after defaults.
func (c *Config) AllowFullDownload() bool {
	return c.Manifest.AllowFullDownload == nil || *c.Manifest.AllowFullDownload
}

func (c *Config) validate() error {
	if c.Crypto.PrivateKey == "" {
		return errors.New("crypto.private_key is required")
	}
	// After applyDefaults a legacy `target:` block has already been folded
	// into Artifacts, so this one check covers both config shapes.
	if len(c.Artifacts) == 0 {
		return errors.New("no artifacts configured: declare `artifacts:` (or a legacy `target.binary`)")
	}
	seen := make(map[string]struct{}, len(c.Artifacts))
	for i, a := range c.Artifacts {
		if a.Binary == "" {
			return fmt.Errorf("artifacts[%d] (%q): binary is required", i, a.Name)
		}
		key := artifactKeyOf(a)
		if err := key.Validate(); err != nil {
			return fmt.Errorf("artifacts[%d]: %w", i, err)
		}
		ks := key.String()
		if _, dup := seen[ks]; dup {
			return fmt.Errorf("artifacts[%d]: duplicate artifact key %q", i, ks)
		}
		seen[ks] = struct{}{}
	}
	if c.DefaultArtifact == "" {
		return errors.New("default_artifact is required when more than one artifact is declared")
	}
	if _, ok := seen[c.DefaultArtifact]; !ok {
		return fmt.Errorf("default_artifact %q is not among the declared artifacts", c.DefaultArtifact)
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
