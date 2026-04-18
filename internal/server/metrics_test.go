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
	m.ObserveDeltaServe("http", "hit", 0.05)
	m.ObserveDeltaGeneration("ok", 1.5)
	m.IncAdminRateLimited()
	m.IncSignatureFailure()
	m.SetManifestCacheEntries(17)
	m.SetHotDeltaCacheBytes(1 << 20)
	m.SetHotDeltaCacheEntries(3)
	m.SetTargetBinarySize(8_000_000)
	m.SetTargetInMemory(true)

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
		`updater_target_in_memory 1`,
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
	m.ObserveDeltaServe("http", "hit", 0)
	m.ObserveDeltaGeneration("ok", 0)
	m.ObserveAdminRequest("/admin/reload", 200)
	m.IncAdminRateLimited()
	m.IncSignatureFailure()
	m.IncAsyncGenerationInflight()
	m.DecAsyncGenerationInflight()
	m.SetManifestCacheEntries(0)
	m.SetHotDeltaCacheBytes(0)
	m.SetHotDeltaCacheEntries(0)
	m.SetTargetBinarySize(0)
	m.SetTargetInMemory(false)
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
