package server

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetrics_HandlerEmitsPrometheusFormat(t *testing.T) {
	m := NewMetrics()
	// Exercise a handful of paths so the output has non-zero lines.
	m.ObserveHeartbeat("http", "none", 0.01)
	m.ObserveHeartbeat("coap", "update", 0.2)
	m.ObserveDeltaServe("http", "hit", "delta", 0.05)
	m.ObserveDeltaServe("http", "miss", "full", 0.20)
	m.ObserveDeltaGeneration("ok", 1.5)
	m.IncAdminRateLimited()
	m.IncSignatureFailure()
	m.SetManifestCacheEntries(17)
	m.SetHotDeltaCacheBytes(1 << 20)
	m.SetHotDeltaCacheEntries(3)
	m.SetTargetCacheBytes(8_000_000)
	m.SetTargetCacheEntries(1)
	m.SetArtifactCount(2)
	m.SetArtifactTargetSize("app/linux/amd64", 8_000_000)
	m.ObserveRetentionSweep("ok")
	m.ObserveRetentionDeleted("delta", 1234)

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	out := string(body)

	// Spot-check a handful of series so we catch naming drift early.
	required := []string{
		`updater_heartbeats_total{`,
		`updater_heartbeat_duration_seconds_bucket{`,
		`updater_deltas_served_total{`,
		`updater_delta_generations_total{result="ok"} 1`,
		`updater_admin_rate_limited_total 1`,
		`updater_signature_failures_total 1`,
		`updater_manifest_cache_entries 17`,
		`updater_hot_delta_cache_bytes 1.048576e+06`,
		`updater_target_cache_bytes 8e+06`,
		`updater_target_cache_entries 1`,
		`updater_artifacts 2`,
		`updater_artifact_target_size_bytes{artifact="app/linux/amd64"} 8e+06`,
		`updater_retention_sweeps_total{result="ok"} 1`,
		`updater_retention_deleted_files_total{kind="delta"} 1`,
		`updater_retention_reclaimed_bytes_total 1234`,
		// mode distinguishes patch transfers from full-download fallbacks;
		// a rising "full" share is the signal that retention is evicting
		// source binaries the fleet still needs.
		`updater_deltas_served_total{hot_hit="miss",mode="full",transport="http"} 1`,
		`# HELP go_goroutines`, // confirms the Go collector is registered
	}
	for _, want := range required {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in /metrics output", want)
		}
	}
}

func TestMetrics_NilSafe(t *testing.T) {
	// Every accessor must be a no-op on a nil receiver. Non-nil Metrics is
	// opt-in; tests that don't want the overhead pass nil.
	var m *Metrics
	m.ObserveHeartbeat("http", "none", 0)
	m.ObserveDeltaServe("http", "hit", "delta", 0)
	m.ObserveDeltaGeneration("ok", 0)
	m.ObserveAdminRequest("/admin/reload", 200)
	m.IncAdminRateLimited()
	m.IncSignatureFailure()
	m.IncAsyncGenerationInflight()
	m.DecAsyncGenerationInflight()
	m.SetManifestCacheEntries(0)
	m.SetHotDeltaCacheBytes(0)
	m.SetHotDeltaCacheEntries(0)
	m.SetTargetCacheBytes(0)
	m.SetTargetCacheEntries(0)
	m.SetArtifactCount(0)
	m.SetArtifactTargetSize("app/linux/amd64", 0)
	m.DeleteArtifactTargetSize("app/linux/amd64")
	m.ObserveRetentionSweep("error")
	m.ObserveRetentionDeleted("binary", 0)
}

func TestCodeLabel(t *testing.T) {
	cases := map[int]string{
		200: "2xx", 204: "2xx",
		301: "3xx",
		400: "4xx", 401: "Unauthorized", 403: "Forbidden", 429: "Too Many Requests",
		500: "5xx", 503: "5xx",
		999: "unknown",
	}
	for code, want := range cases {
		if got := codeLabel(code); got != want {
			t.Errorf("codeLabel(%d) = %q, want %q", code, got, want)
		}
	}
}
