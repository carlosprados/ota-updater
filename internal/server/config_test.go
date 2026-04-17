package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "server.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return p
}

func TestLoadConfig_DefaultsAndValidation(t *testing.T) {
	p := writeYAML(t, `
crypto:
  private_key: "./keys/server.key"
target:
  binary: "./store/binaries/latest"
admin:
  token: "a-strong-enough-token-of-32-chars"
`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.HTTP.Addr != ":8080" {
		t.Errorf("default http.addr = %q", cfg.HTTP.Addr)
	}
	if cfg.CoAP.Addr != ":5683" {
		t.Errorf("default coap.addr = %q", cfg.CoAP.Addr)
	}
	if cfg.Logging.Level != "info" || cfg.Logging.Format != "text" {
		t.Errorf("default logging = %+v", cfg.Logging)
	}
	if cfg.HTTP.ShutdownTimeout != 15*time.Second {
		t.Errorf("default shutdown timeout = %v", cfg.HTTP.ShutdownTimeout)
	}
}

func TestLoadConfig_MissingAdminToken(t *testing.T) {
	p := writeYAML(t, `
crypto:
  private_key: "./keys/server.key"
target:
  binary: "./bin/latest"
`)
	_, err := LoadConfig(p)
	if err == nil {
		t.Fatalf("expected error for missing admin.token")
	}
}

func TestLoadConfig_ShortAdminTokenRejected(t *testing.T) {
	p := writeYAML(t, `
crypto:
  private_key: "./keys/server.key"
target:
  binary: "./bin/latest"
admin:
  token: "short"
`)
	_, err := LoadConfig(p)
	if err == nil {
		t.Fatalf("expected error for short admin.token")
	}
	if !strings.Contains(err.Error(), "at least 32") {
		t.Fatalf("err = %v, want mention of min length", err)
	}
}

func TestLoadConfig_UnknownLogLevel(t *testing.T) {
	p := writeYAML(t, `
crypto:
  private_key: "k"
target:
  binary: "b"
admin:
  token: "a-strong-enough-token-of-32-chars"
logging:
  level: "trace"
`)
	_, err := LoadConfig(p)
	if err == nil {
		t.Fatalf("expected error for unknown level")
	}
}
