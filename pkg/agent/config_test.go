package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return p
}

func TestLoadConfig_Defaults(t *testing.T) {
	p := writeYAML(t, `
server:
  coap_url: "coap://update.example.com:5683"
  http_url: "http://update.example.com:8080"
device:
  id: "edge-001"
  slots_dir: "/opt/agent/slots"
  active_symlink: "/opt/agent/current"
crypto:
  public_key: "/opt/agent/keys/agent.pub"
`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Server.Transport != TransportCoAP {
		t.Errorf("default transport = %q", cfg.Server.Transport)
	}
	if cfg.Server.CoAP.BlockSize != 512 {
		t.Errorf("default block_size = %d", cfg.Server.CoAP.BlockSize)
	}
	if cfg.Update.CheckInterval != time.Hour {
		t.Errorf("default check_interval = %v", cfg.Update.CheckInterval)
	}
	if cfg.Logging.Level != "info" {
		t.Errorf("default logging.level = %q", cfg.Logging.Level)
	}
}

func TestConfig_Validate_UnknownTransport(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			Transport: "xyz",
			CoAPURL:   "coap://h:5683",
			HTTPURL:   "http://h:8080",
		},
		Device:  DeviceConfig{ID: "d", SlotsDir: "/s", ActiveSymlink: "/c"},
		Crypto:  CryptoConfig{PublicKey: "k"},
		Logging: LoggingConfig{Level: "info", Format: "text"},
	}
	cfg.ApplyDefaults()
	cfg.Server.Transport = "xyz" // defaults set it to coap; force xyz back
	cfg.Update.ChunkSize = 4096  // defaults set it
	cfg.Server.CoAP.BlockSize = 512
	if err := cfg.Validate(); err == nil {
		t.Fatalf("expected error for unknown transport")
	}
}

func TestConfig_Validate_FallbackRequiresBothURLs(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			Transport: TransportCoAP,
			CoAPURL:   "coap://h:5683",
			Fallback:  true, // but no http_url set
		},
		Device:  DeviceConfig{ID: "d", SlotsDir: "/s", ActiveSymlink: "/c"},
		Crypto:  CryptoConfig{PublicKey: "k"},
		Logging: LoggingConfig{Level: "info"},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err == nil {
		t.Fatalf("expected error for fallback without http_url")
	}
}

func TestConfig_Validate_BadBlockSize(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			Transport: TransportCoAP,
			CoAPURL:   "coap://h:5683",
			CoAP:      CoAPClientConfig{BlockSize: 500}, // not a power of two
		},
		Device:  DeviceConfig{ID: "d", SlotsDir: "/s", ActiveSymlink: "/c"},
		Crypto:  CryptoConfig{PublicKey: "k"},
		Logging: LoggingConfig{Level: "info"},
	}
	// Only defaults we skip: block_size (to preserve 500).
	if cfg.Server.CoAP.ACKTimeout == 0 {
		cfg.Server.CoAP.ACKTimeout = time.Second
	}
	if cfg.Update.ChunkSize == 0 {
		cfg.Update.ChunkSize = 4096
	}
	if err := cfg.Validate(); err == nil {
		t.Fatalf("expected error for non-power-of-two block size")
	}
}

func TestConfig_Validate_MissingDeviceID(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			Transport: TransportCoAP,
			CoAPURL:   "coap://h:5683",
		},
		Device:  DeviceConfig{SlotsDir: "/s", ActiveSymlink: "/c"},
		Crypto:  CryptoConfig{PublicKey: "k"},
		Logging: LoggingConfig{Level: "info"},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err == nil {
		t.Fatalf("expected error for missing device.id")
	}
}

func TestConfig_Validate_HTTPSAccepted(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			Transport: TransportHTTP,
			HTTPURL:   "https://update.example.com",
		},
		Device:  DeviceConfig{ID: "d", SlotsDir: "/s", ActiveSymlink: "/c"},
		Crypto:  CryptoConfig{PublicKey: "k"},
		Logging: LoggingConfig{Level: "info"},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("https should validate: %v", err)
	}
}

func TestConfig_LibraryUse_NoYAML(t *testing.T) {
	// The common library flow: construct Config in code, defaults, validate.
	cfg := &Config{
		Server: ServerConfig{
			Transport: TransportCoAP,
			CoAPURL:   "coap://server.local:5683",
			HTTPURL:   "http://server.local:8080",
			Fallback:  true,
		},
		Device:  DeviceConfig{ID: "embedded-123", SlotsDir: "/var/lib/myapp/slots", ActiveSymlink: "/var/lib/myapp/current"},
		Crypto:  CryptoConfig{PublicKey: "/etc/myapp/agent.pub"},
		Logging: LoggingConfig{Level: "info"},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("library-built config must validate: %v", err)
	}
	if cfg.Update.CheckInterval == 0 {
		t.Fatalf("ApplyDefaults did not fill CheckInterval")
	}
}
