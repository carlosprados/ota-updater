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

// The legacy single-target shape must keep working verbatim: it is folded
// into one artifact named "default", which also becomes the default track.
func TestLoadConfig_LegacyTargetFoldsIntoArtifact(t *testing.T) {
	p := writeYAML(t, `
crypto:
  private_key: "./keys/server.key"
target:
  version: "1.0.0"
  binary: "./store/binaries/latest"
admin:
  token: "a-strong-enough-token-of-32-chars"
`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Artifacts) != 1 {
		t.Fatalf("legacy target produced %d artifacts, want 1", len(cfg.Artifacts))
	}
	a := cfg.Artifacts[0]
	if a.Name != LegacyTargetArtifactName || a.Binary != "./store/binaries/latest" || a.Version != "1.0.0" {
		t.Fatalf("folded artifact = %+v", a)
	}
	if a.OS != "" || a.Arch != "" {
		t.Fatalf("legacy artifact must stay platform-independent, got os=%q arch=%q", a.OS, a.Arch)
	}
	if cfg.DefaultArtifact != LegacyTargetArtifactName {
		t.Fatalf("default_artifact = %q, want %q", cfg.DefaultArtifact, LegacyTargetArtifactName)
	}
	if !cfg.AllowFullDownload() {
		t.Fatalf("allow_full_download should default to true")
	}
}

func TestLoadConfig_MultiArtifact(t *testing.T) {
	p := writeYAML(t, `
crypto:
  private_key: "./keys/server.key"
admin:
  token: "a-strong-enough-token-of-32-chars"
artifacts:
  - name: "agent"
    os: "linux"
    arch: "arm64"
    version: "1.0.0"
    binary: "/srv/agent-arm64"
  - name: "agent"
    os: "linux"
    arch: "amd64"
    version: "1.0.0"
    binary: "/srv/agent-amd64"
  - name: "tariff-table"
    version: "2024-06"
    binary: "/srv/tariff.bin"
default_artifact: "agent/linux/arm64"
`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Artifacts) != 3 {
		t.Fatalf("got %d artifacts, want 3", len(cfg.Artifacts))
	}
	if cfg.DefaultArtifact != "agent/linux/arm64" {
		t.Fatalf("default_artifact = %q", cfg.DefaultArtifact)
	}
}

func TestLoadConfig_SingleArtifactInfersDefault(t *testing.T) {
	p := writeYAML(t, `
crypto:
  private_key: "k"
admin:
  token: "a-strong-enough-token-of-32-chars"
artifacts:
  - name: "agent"
    os: "linux"
    arch: "arm64"
    binary: "/srv/agent"
`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DefaultArtifact != "agent/linux/arm64" {
		t.Fatalf("a single artifact should become the default, got %q", cfg.DefaultArtifact)
	}
}

func TestLoadConfig_ArtifactValidation(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "no artifacts at all",
			yaml: "crypto:\n  private_key: k\nadmin:\n  token: \"a-strong-enough-token-of-32-chars\"\n",
			want: "no artifacts configured",
		},
		{
			name: "missing binary",
			yaml: "crypto:\n  private_key: k\nadmin:\n  token: \"a-strong-enough-token-of-32-chars\"\nartifacts:\n  - name: agent\n",
			want: "binary is required",
		},
		{
			name: "duplicate key",
			yaml: "crypto:\n  private_key: k\nadmin:\n  token: \"a-strong-enough-token-of-32-chars\"\nartifacts:\n  - name: agent\n    binary: /a\n  - name: agent\n    binary: /b\ndefault_artifact: agent\n",
			want: "duplicate artifact key",
		},
		{
			name: "half-specified platform",
			yaml: "crypto:\n  private_key: k\nadmin:\n  token: \"a-strong-enough-token-of-32-chars\"\nartifacts:\n  - name: agent\n    os: linux\n    binary: /a\n",
			want: "must both be set",
		},
		{
			name: "default not among artifacts",
			yaml: "crypto:\n  private_key: k\nadmin:\n  token: \"a-strong-enough-token-of-32-chars\"\nartifacts:\n  - name: a\n    binary: /a\n  - name: b\n    binary: /b\ndefault_artifact: ghost\n",
			want: "not among the declared artifacts",
		},
		{
			name: "multiple artifacts without a default",
			yaml: "crypto:\n  private_key: k\nadmin:\n  token: \"a-strong-enough-token-of-32-chars\"\nartifacts:\n  - name: a\n    binary: /a\n  - name: b\n    binary: /b\n",
			want: "default_artifact is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(writeYAML(t, tc.yaml))
			if err == nil {
				t.Fatalf("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want mention of %q", err, tc.want)
			}
		})
	}
}

func TestLoadConfig_AllowFullDownloadExplicitFalse(t *testing.T) {
	p := writeYAML(t, `
crypto:
  private_key: "k"
admin:
  token: "a-strong-enough-token-of-32-chars"
artifacts:
  - name: "agent"
    binary: "/srv/agent"
manifest:
  allow_full_download: false
`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.AllowFullDownload() {
		t.Fatalf("an explicit false must be honored, not overwritten by the default")
	}
}

func TestLoadConfig_RetentionDefaults(t *testing.T) {
	p := writeYAML(t, `
crypto:
  private_key: "k"
admin:
  token: "a-strong-enough-token-of-32-chars"
artifacts:
  - name: "agent"
    binary: "/srv/agent"
`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Retention.Enabled {
		t.Fatalf("retention must default to disabled")
	}
	if cfg.Retention.CollectOrphanBinaries {
		t.Fatalf("orphan binary collection must default to disabled")
	}
	if cfg.Retention.Interval != 6*time.Hour {
		t.Fatalf("retention.interval = %v, want 6h", cfg.Retention.Interval)
	}
	if cfg.Retention.HistoryDepth != DefaultHistoryDepth {
		t.Fatalf("retention.history_depth = %d", cfg.Retention.HistoryDepth)
	}
	if cfg.Retention.OrphanBinaryMinAge != 24*time.Hour {
		t.Fatalf("retention.orphan_binary_min_age = %v, want 24h", cfg.Retention.OrphanBinaryMinAge)
	}
	if cfg.Store.StateFile == "" {
		t.Fatalf("store.state_file must have a default; without it API publishes vanish on restart")
	}
}

// The shipped example config must actually load. Without this, configs/
// silently drifts from the code and the first person to copy it hits an
// error that looks like a bug in the server.
func TestLoadConfig_ShippedExampleIsValid(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "configs", "server.yaml"))
	if err != nil {
		t.Fatalf("read shipped config: %v", err)
	}
	// The example ships a placeholder token that is intentionally not a real
	// secret but is long enough to pass validation.
	p := writeYAML(t, string(raw))
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("configs/server.yaml does not load: %v", err)
	}
	if len(cfg.Artifacts) == 0 {
		t.Fatalf("shipped config declares no artifacts")
	}
	if cfg.DefaultArtifact == "" {
		t.Fatalf("shipped config has no default artifact")
	}
}
