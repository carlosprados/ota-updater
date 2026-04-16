// Package agent implements the edge-agent side of the OTA system:
// heartbeats, manifest verification, chunked delta downloads, A/B slot
// management, watchdog health checks, and rollback.
//
// This package is organized to be usable as a Go library: all public types
// are exported and callers can construct Config values in code, bypassing
// the YAML loader entirely. At step 18 these packages move to pkg/agent/
// for external import.
package agent

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Transport selects the wire protocol used to reach the update-server.
// Embedders can set it programmatically with the typed constants instead
// of stringly-typed YAML values.
type Transport string

const (
	TransportCoAP Transport = "coap"
	TransportHTTP Transport = "http"
)

// Config is the top-level agent configuration. Load from YAML with LoadConfig
// or construct directly in code (library use). Call Validate() before wiring.
type Config struct {
	Server  ServerConfig  `yaml:"server"`
	Device  DeviceConfig  `yaml:"device"`
	Update  UpdateConfig  `yaml:"update"`
	Crypto  CryptoConfig  `yaml:"crypto"`
	Logging LoggingConfig `yaml:"logging"`
}

// ServerConfig describes how to talk to the update-server.
type ServerConfig struct {
	HTTPURL   string            `yaml:"http_url"`
	CoAPURL   string            `yaml:"coap_url"`
	Transport Transport         `yaml:"transport"`
	Fallback  bool              `yaml:"fallback"` // try the other transport once per cycle on failure
	CoAP      CoAPClientConfig  `yaml:"coap"`
}

// CoAPClientConfig tunes the CoAP client for NB-IoT links. Defaults are
// conservative (generous ACK timeouts, 512-byte blocks).
type CoAPClientConfig struct {
	BlockSize      int           `yaml:"block_size"`      // 16..1024, power of 2
	ACKTimeout     time.Duration `yaml:"ack_timeout"`     // per-CON ACK window
	MaxRetransmits int           `yaml:"max_retransmits"` // RFC 7252 §4.8 default = 4
	Keepalive      time.Duration `yaml:"keepalive"`       // 0 disables
}

// DeviceConfig identifies the device and locates its slot layout on disk.
type DeviceConfig struct {
	ID            string `yaml:"id"`
	SlotsDir      string `yaml:"slots_dir"`
	ActiveSymlink string `yaml:"active_symlink"`
}

// UpdateConfig controls the update loop cadence and download behavior.
type UpdateConfig struct {
	CheckInterval   time.Duration `yaml:"check_interval"`
	ChunkSize       int           `yaml:"chunk_size"`
	MaxRetries      int           `yaml:"max_retries"`
	RetryBackoff    time.Duration `yaml:"retry_backoff"`
	WatchdogTimeout time.Duration `yaml:"watchdog_timeout"`
}

// CryptoConfig locates the Ed25519 public key used to verify manifests.
type CryptoConfig struct {
	PublicKey string `yaml:"public_key"`
}

// LoggingConfig mirrors the server's logging config. Handled at runtime via
// a slog.LevelVar so library consumers can flip the level without restart.
type LoggingConfig struct {
	Level  string `yaml:"level"`  // debug|info|warn|error
	Format string `yaml:"format"` // text|json
}

// LoadConfig reads, parses, defaults and validates a YAML config file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read agent config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse agent config: %w", err)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ApplyDefaults fills in zero-valued fields with project-wide defaults. Safe
// to call multiple times; idempotent. Library consumers can compose a Config
// in code, call ApplyDefaults to avoid hardcoding every field, then Validate.
func (c *Config) ApplyDefaults() {
	if c.Server.Transport == "" {
		c.Server.Transport = TransportCoAP
	}
	if c.Server.CoAP.BlockSize == 0 {
		c.Server.CoAP.BlockSize = 512
	}
	if c.Server.CoAP.ACKTimeout == 0 {
		c.Server.CoAP.ACKTimeout = 60 * time.Second
	}
	if c.Server.CoAP.MaxRetransmits == 0 {
		c.Server.CoAP.MaxRetransmits = 4
	}
	if c.Server.CoAP.Keepalive == 0 {
		c.Server.CoAP.Keepalive = 2 * time.Minute
	}
	if c.Update.CheckInterval == 0 {
		c.Update.CheckInterval = time.Hour
	}
	if c.Update.ChunkSize == 0 {
		c.Update.ChunkSize = 4096
	}
	if c.Update.MaxRetries == 0 {
		c.Update.MaxRetries = 10
	}
	if c.Update.RetryBackoff == 0 {
		c.Update.RetryBackoff = 30 * time.Second
	}
	if c.Update.WatchdogTimeout == 0 {
		c.Update.WatchdogTimeout = 60 * time.Second
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "text"
	}
}

// Validate enforces cross-field invariants and surfaces misconfigurations
// as errors instead of silent misbehavior at runtime.
func (c *Config) Validate() error {
	switch c.Server.Transport {
	case TransportCoAP, TransportHTTP:
	default:
		return fmt.Errorf("server.transport: unknown %q (use %q or %q)",
			c.Server.Transport, TransportCoAP, TransportHTTP)
	}
	if err := validateURL(c.Server.CoAPURL, "coap://"); err != nil && c.Server.Transport == TransportCoAP {
		return fmt.Errorf("server.coap_url: %w", err)
	}
	if err := validateURL(c.Server.HTTPURL, "http://", "https://"); err != nil && c.Server.Transport == TransportHTTP {
		return fmt.Errorf("server.http_url: %w", err)
	}
	if c.Server.Fallback {
		// Fallback needs the other URL to exist too.
		switch c.Server.Transport {
		case TransportCoAP:
			if err := validateURL(c.Server.HTTPURL, "http://", "https://"); err != nil {
				return fmt.Errorf("server.http_url (fallback): %w", err)
			}
		case TransportHTTP:
			if err := validateURL(c.Server.CoAPURL, "coap://"); err != nil {
				return fmt.Errorf("server.coap_url (fallback): %w", err)
			}
		}
	}
	if !validCoAPBlockSize(c.Server.CoAP.BlockSize) {
		return fmt.Errorf("server.coap.block_size: %d is not a power of two in [16, 1024]",
			c.Server.CoAP.BlockSize)
	}
	if strings.TrimSpace(c.Device.ID) == "" {
		return errors.New("device.id is required")
	}
	if c.Device.SlotsDir == "" {
		return errors.New("device.slots_dir is required")
	}
	if c.Device.ActiveSymlink == "" {
		return errors.New("device.active_symlink is required")
	}
	if c.Crypto.PublicKey == "" {
		return errors.New("crypto.public_key is required")
	}
	if c.Update.ChunkSize <= 0 {
		return errors.New("update.chunk_size must be positive")
	}
	if !isKnownLogLevel(c.Logging.Level) {
		return fmt.Errorf("logging.level: unknown %q (use debug|info|warn|error)", c.Logging.Level)
	}
	return nil
}

func validateURL(raw string, allowedSchemes ...string) error {
	if raw == "" {
		return errors.New("required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse %q: %w", raw, err)
	}
	if u.Host == "" {
		return fmt.Errorf("missing host in %q", raw)
	}
	for _, prefix := range allowedSchemes {
		if strings.HasPrefix(raw, prefix) {
			return nil
		}
	}
	return fmt.Errorf("unsupported scheme in %q (want one of %v)", raw, allowedSchemes)
}

// validCoAPBlockSize accepts 16, 32, 64, 128, 256, 512, 1024 (per RFC 7959).
func validCoAPBlockSize(n int) bool {
	if n < 16 || n > 1024 {
		return false
	}
	return n&(n-1) == 0
}

// isKnownLogLevel is duplicated with server.parseLogLevel on purpose: the
// agent package is designed to be importable standalone as a library without
// pulling in the server package.
func isKnownLogLevel(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug", "info", "warn", "warning", "error":
		return true
	}
	return false
}
