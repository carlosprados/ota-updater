package server

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics bundles every Prometheus collector the server exports. Instances
// are created per-server via NewMetrics so unit tests can build isolated
// registries — the global prometheus.DefaultRegisterer is not used. Handlers
// receive a *Metrics pointer via HTTPConfig/CoAPConfig/AdminDeps.
//
// Naming: every series is prefixed "updater_" so a fleet running multiple
// related services (e.g. updater + some companion) stays disambiguated.
type Metrics struct {
	registry *prometheus.Registry

	// Counters
	heartbeatsTotal         *prometheus.CounterVec // labels: transport, result
	deltasServedTotal       *prometheus.CounterVec // labels: transport, hot_hit, mode
	deltaGenerationsTotal   *prometheus.CounterVec // labels: result
	adminRequestsTotal      *prometheus.CounterVec // labels: endpoint, code
	adminRateLimitedTotal   prometheus.Counter
	signatureFailuresTotal  prometheus.Counter
	retentionSweepsTotal    *prometheus.CounterVec // labels: result
	retentionDeletedTotal   *prometheus.CounterVec // labels: kind
	retentionReclaimedBytes prometheus.Counter

	// Histograms
	heartbeatDuration     *prometheus.HistogramVec // labels: transport
	deltaGenerateDuration prometheus.Histogram
	deltaServeDuration    *prometheus.HistogramVec // labels: transport, mode

	// Gauges
	manifestCacheEntries     prometheus.Gauge
	hotDeltaCacheBytes       prometheus.Gauge
	hotDeltaCacheEntries     prometheus.Gauge
	asyncGenerationsInflight prometheus.Gauge
	targetCacheBytes         prometheus.Gauge
	targetCacheEntries       prometheus.Gauge
	artifactCount            prometheus.Gauge
	artifactTargetSizeBytes  *prometheus.GaugeVec // labels: artifact
}

// NewMetrics builds the collector set and registers them on a fresh registry.
// Pass the returned Registry to promhttp.HandlerFor to wire /metrics. The
// process/go collectors are included automatically so runtime stats are
// exposed out of the box.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewGoCollector(),
	)
	m := &Metrics{
		registry: reg,

		heartbeatsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "updater_heartbeats_total",
			Help: "Total heartbeats received, labelled by transport (http|coap) and result (update|none|retry).",
		}, []string{"transport", "result"}),
		deltasServedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "updater_deltas_served_total",
			Help: "Total transfer responses served, labelled by transport, whether the hot cache served the bytes (hit|miss), and mode (delta|full).",
		}, []string{"transport", "hot_hit", "mode"}),
		deltaGenerationsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "updater_delta_generations_total",
			Help: "Total bsdiff delta generations, labelled by result (ok|error).",
		}, []string{"result"}),
		adminRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "updater_admin_requests_total",
			Help: "Admin endpoint requests, labelled by endpoint and HTTP code.",
		}, []string{"endpoint", "code"}),
		adminRateLimitedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "updater_admin_rate_limited_total",
			Help: "Admin authentication attempts that were rejected with 429 by the token bucket.",
		}),
		signatureFailuresTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "updater_signature_failures_total",
			Help: "Manifest signing errors (server-side).",
		}),
		retentionSweepsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "updater_retention_sweeps_total",
			Help: "Retention sweeps run, labelled by result (ok|error).",
		}, []string{"result"}),
		retentionDeletedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "updater_retention_deleted_files_total",
			Help: "Files deleted by the retention sweeper, labelled by kind (binary|delta|full).",
		}, []string{"kind"}),
		retentionReclaimedBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "updater_retention_reclaimed_bytes_total",
			Help: "Disk bytes reclaimed by the retention sweeper.",
		}),

		heartbeatDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "updater_heartbeat_duration_seconds",
			Help:    "Wall time spent serving a heartbeat, labelled by transport.",
			Buckets: prometheus.DefBuckets,
		}, []string{"transport"}),
		deltaGenerateDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "updater_delta_generate_duration_seconds",
			Help:    "Wall time of bsdiff generation (bounded by delta_concurrency).",
			Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300},
		}),
		deltaServeDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "updater_delta_serve_duration_seconds",
			Help:    "Wall time to serve a transfer (may include a hot-cache miss + disk read).",
			Buckets: prometheus.DefBuckets,
		}, []string{"transport", "mode"}),

		manifestCacheEntries: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "updater_manifest_cache_entries",
			Help: "Current number of entries in the signed-manifest LRU.",
		}),
		hotDeltaCacheBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "updater_hot_delta_cache_bytes",
			Help: "Current bytes held in the hot delta LRU.",
		}),
		hotDeltaCacheEntries: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "updater_hot_delta_cache_entries",
			Help: "Current number of entries in the hot delta LRU.",
		}),
		asyncGenerationsInflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "updater_async_generations_inflight",
			Help: "Async bsdiff generations currently running (bounded by delta_concurrency).",
		}),
		targetCacheBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "updater_target_cache_bytes",
			Help: "Bytes of uncompressed delta-target binaries currently held in RAM.",
		}),
		targetCacheEntries: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "updater_target_cache_entries",
			Help: "Number of delta-target binaries currently held in RAM.",
		}),
		artifactCount: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "updater_artifacts",
			Help: "Number of artifacts (publication tracks) registered.",
		}),
		artifactTargetSizeBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "updater_artifact_target_size_bytes",
			Help: "Uncompressed size of each artifact's current target binary.",
		}, []string{"artifact"}),
	}
	reg.MustRegister(
		m.heartbeatsTotal, m.deltasServedTotal, m.deltaGenerationsTotal,
		m.adminRequestsTotal, m.adminRateLimitedTotal, m.signatureFailuresTotal,
		m.retentionSweepsTotal, m.retentionDeletedTotal, m.retentionReclaimedBytes,
		m.heartbeatDuration, m.deltaGenerateDuration, m.deltaServeDuration,
		m.manifestCacheEntries, m.hotDeltaCacheBytes, m.hotDeltaCacheEntries,
		m.asyncGenerationsInflight, m.targetCacheBytes, m.targetCacheEntries,
		m.artifactCount, m.artifactTargetSizeBytes,
	)
	return m
}

// Handler returns an http.Handler that renders the Prometheus text
// exposition of this Metrics instance.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		Registry: m.registry,
	})
}

// Nil-safe accessors. The handlers receive a *Metrics that may be nil when
// metrics are disabled at startup; every method must no-op in that case.

func (m *Metrics) ObserveHeartbeat(transport, result string, seconds float64) {
	if m == nil {
		return
	}
	m.heartbeatsTotal.WithLabelValues(transport, result).Inc()
	m.heartbeatDuration.WithLabelValues(transport).Observe(seconds)
}

// ObserveDeltaServe records one served transfer. mode is "delta" for a patch
// and "full" for a whole compressed binary, so a rising full/delta ratio —
// the signal that retention is evicting source binaries the fleet still
// needs — is visible without reading logs.
func (m *Metrics) ObserveDeltaServe(transport, hotHit, mode string, seconds float64) {
	if m == nil {
		return
	}
	m.deltasServedTotal.WithLabelValues(transport, hotHit, mode).Inc()
	m.deltaServeDuration.WithLabelValues(transport, mode).Observe(seconds)
}

func (m *Metrics) ObserveDeltaGeneration(result string, seconds float64) {
	if m == nil {
		return
	}
	m.deltaGenerationsTotal.WithLabelValues(result).Inc()
	if result == "ok" {
		m.deltaGenerateDuration.Observe(seconds)
	}
}

func (m *Metrics) ObserveAdminRequest(endpoint string, code int) {
	if m == nil {
		return
	}
	m.adminRequestsTotal.WithLabelValues(endpoint, codeLabel(code)).Inc()
}

func (m *Metrics) IncAdminRateLimited() {
	if m == nil {
		return
	}
	m.adminRateLimitedTotal.Inc()
}

func (m *Metrics) IncSignatureFailure() {
	if m == nil {
		return
	}
	m.signatureFailuresTotal.Inc()
}

func (m *Metrics) IncAsyncGenerationInflight() {
	if m != nil {
		m.asyncGenerationsInflight.Inc()
	}
}
func (m *Metrics) DecAsyncGenerationInflight() {
	if m != nil {
		m.asyncGenerationsInflight.Dec()
	}
}
func (m *Metrics) SetManifestCacheEntries(n int) {
	if m != nil {
		m.manifestCacheEntries.Set(float64(n))
	}
}
func (m *Metrics) SetHotDeltaCacheBytes(b int64) {
	if m != nil {
		m.hotDeltaCacheBytes.Set(float64(b))
	}
}
func (m *Metrics) SetHotDeltaCacheEntries(n int) {
	if m != nil {
		m.hotDeltaCacheEntries.Set(float64(n))
	}
}
func (m *Metrics) SetTargetCacheBytes(b int64) {
	if m != nil {
		m.targetCacheBytes.Set(float64(b))
	}
}
func (m *Metrics) SetTargetCacheEntries(n int) {
	if m != nil {
		m.targetCacheEntries.Set(float64(n))
	}
}
func (m *Metrics) SetArtifactCount(n int) {
	if m != nil {
		m.artifactCount.Set(float64(n))
	}
}

// SetArtifactTargetSize records the current target size for one artifact.
// The label is the canonical ArtifactKey string, whose charset and length
// are bounded by protocol.ArtifactKey.Validate — cardinality is therefore
// bounded by the number of registered artifacts, not by request traffic.
func (m *Metrics) SetArtifactTargetSize(artifact string, size int64) {
	if m == nil {
		return
	}
	m.artifactTargetSizeBytes.WithLabelValues(artifact).Set(float64(size))
}

// DeleteArtifactTargetSize drops the series for a removed artifact so a
// deleted track stops reporting a stale size forever.
func (m *Metrics) DeleteArtifactTargetSize(artifact string) {
	if m == nil {
		return
	}
	m.artifactTargetSizeBytes.DeleteLabelValues(artifact)
}

func (m *Metrics) ObserveRetentionSweep(result string) {
	if m == nil {
		return
	}
	m.retentionSweepsTotal.WithLabelValues(result).Inc()
}

func (m *Metrics) ObserveRetentionDeleted(kind string, bytes int64) {
	if m == nil {
		return
	}
	m.retentionDeletedTotal.WithLabelValues(kind).Inc()
	m.retentionReclaimedBytes.Add(float64(bytes))
}

// codeLabel collapses HTTP codes to their class (2xx/3xx/4xx/5xx) so the
// admin_requests_total cardinality stays bounded.
func codeLabel(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		// Exact 401/403/429 are interesting for auth debugging.
		switch code {
		case 401, 403, 429:
			return http.StatusText(code)
		}
		return "4xx"
	case code >= 500 && code < 600:
		return "5xx"
	}
	return "unknown"
}
