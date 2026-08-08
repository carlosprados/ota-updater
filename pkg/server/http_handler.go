package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/carlosprados/ota-updater/pkg/protocol"
)

// HTTPConfig bundles dependencies for the HTTP handler set.
type HTTPConfig struct {
	Store      *Store
	Registry   *Registry
	Manifester *Manifester
	Logger     *slog.Logger
	Metrics    *Metrics // optional; nil disables per-request metric emission
}

// NewHTTPHandler wires the OTA endpoints onto a fresh ServeMux, wrapped in
// panic-recovery middleware so a handler crash never brings down the process:
//
//	POST /heartbeat          → ManifestResponse
//	GET  /delta/{from}/{to}  → compressed delta with Range support
//	GET  /binary/{hash}      → whole compressed binary with Range support
//	POST /report             → update report sink
//	GET  /health             → server health probe
func NewHTTPHandler(cfg HTTPConfig) http.Handler {
	h := &httpHandler{
		store:      cfg.Store,
		registry:   cfg.Registry,
		manifester: cfg.Manifester,
		logger:     cfg.Logger,
		metrics:    cfg.Metrics,
	}
	if h.logger == nil {
		h.logger = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+protocol.PathHeartbeat, h.heartbeat)
	mux.HandleFunc("POST "+protocol.PathReport, h.report)
	mux.HandleFunc("GET "+protocol.PathHealth, h.health)
	mux.HandleFunc("GET "+protocol.PathDelta+"/{from}/{to}", h.delta)
	mux.HandleFunc("GET "+protocol.PathBinary+"/{hash}", h.binary)
	return recoverHTTP(mux, h.logger)
}

type httpHandler struct {
	store      *Store
	registry   *Registry
	manifester *Manifester
	logger     *slog.Logger
	metrics    *Metrics
}

func (h *httpHandler) heartbeat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	result := "none"
	defer func() {
		h.metrics.ObserveHeartbeat("http", result, time.Since(start).Seconds())
	}()

	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, maxHeartbeatBody)

	var hb protocol.Heartbeat
	if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
		h.logger.Warn("invalid heartbeat payload",
			"op", "heartbeat", "err", err, "remote", r.RemoteAddr,
		)
		result = "bad_request"
		http.Error(w, "invalid heartbeat", http.StatusBadRequest)
		return
	}
	resp, err := h.manifester.Build(r.Context(), &hb)
	if err != nil {
		// An unknown artifact is a client-side mistake (typo, misconfigured
		// device), not a server fault — answering 500 would make it look like
		// an outage in every dashboard.
		if errors.Is(err, protocol.ErrInvalidMessage) {
			h.logger.Warn("malformed heartbeat rejected",
				"op", "heartbeat", "err", err, "remote", r.RemoteAddr,
			)
			result = "bad_request"
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if IsArtifactNotFound(err) {
			h.logger.Warn("heartbeat for unknown artifact",
				"op", "heartbeat", "device_id", hb.DeviceID,
				"artifact", hb.Artifact, "err", err, "remote", r.RemoteAddr,
			)
			result = "unknown_artifact"
			http.Error(w, "unknown artifact", http.StatusNotFound)
			return
		}
		h.logger.Error("manifest build",
			"op", "heartbeat", "device_id", hb.DeviceID,
			"artifact", hb.Artifact, "err", err,
		)
		result = "error"
		http.Error(w, "manifest build failed", http.StatusInternalServerError)
		return
	}
	result = manifestResult(resp)
	h.logger.Info("heartbeat served",
		"op", "heartbeat",
		"device_id", hb.DeviceID,
		"artifact", resp.Artifact,
		"from", hb.VersionHash,
		"to", resp.TargetHash,
		"update_available", resp.UpdateAvailable,
		"mode", transferMode(resp),
		"retry_after", resp.RetryAfter,
		"remote", r.RemoteAddr,
	)
	writeJSON(w, http.StatusOK, resp)
}

// manifestResult maps a response to the heartbeats_total result label.
func manifestResult(resp *protocol.ManifestResponse) string {
	switch {
	case !resp.UpdateAvailable:
		return "none"
	case resp.RetryAfter > 0:
		return "retry"
	case resp.BinaryEndpoint != "":
		return "full"
	default:
		return "update"
	}
}

// transferMode names which transfer the manifest points at, for logs.
func transferMode(resp *protocol.ManifestResponse) string {
	switch {
	case resp.BinaryEndpoint != "":
		return "full"
	case resp.DeltaEndpoint != "":
		return "delta"
	default:
		return "none"
	}
}

func (h *httpHandler) report(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, maxReportBody)

	var rep protocol.UpdateReport
	if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
		h.logger.Warn("invalid report", "op", "report", "err", err, "remote", r.RemoteAddr)
		http.Error(w, "invalid report", http.StatusBadRequest)
		return
	}
	// Reports are a pure sink, but every field below is logged: bound them
	// before they reach the log, not after.
	if err := rep.Validate(); err != nil {
		h.logger.Warn("malformed report rejected",
			"op", "report", "err", err, "remote", r.RemoteAddr)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.logger.Info("update report",
		"op", "report",
		"device_id", rep.DeviceID,
		"previous_hash", rep.PreviousHash,
		"new_hash", rep.NewHash,
		"success", rep.Success,
		"rollback_reason", rep.RollbackReason,
		"remote", r.RemoteAddr,
	)
	w.WriteHeader(http.StatusAccepted)
}

func (h *httpHandler) health(w http.ResponseWriter, _ *http.Request) {
	arts := h.registry.List()
	targets := make(map[string]string, len(arts))
	for _, a := range arts {
		targets[a.Key.String()] = a.TargetHash
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"artifacts": targets,
		"default":   h.registry.Default(),
	})
}

func (h *httpHandler) delta(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	hotHit := "miss"
	served := false
	defer func() {
		if served {
			h.metrics.ObserveDeltaServe("http", hotHit, "delta", time.Since(start).Seconds())
		}
	}()

	from := r.PathValue("from")
	to := r.PathValue("to")

	if !protocol.IsValidHash(from) || !protocol.IsValidHash(to) {
		http.NotFound(w, r)
		return
	}
	// Agents only ever ask for deltas toward a current target. Restricting
	// the destination also bounds the work an unauthenticated request can
	// trigger: a miss here dispatches a bsdiff, the most expensive thing
	// this process does.
	if !h.registry.IsCurrentTarget(to) {
		http.NotFound(w, r)
		return
	}

	// Peek the hot cache before calling GetDeltaBytes so the metric label
	// is accurate; GetDeltaBytes itself would hide the distinction.
	if _, ok := h.store.PeekHotDelta(from, to); ok {
		hotHit = "hit"
	}

	data, found, err := h.store.GetDeltaBytes(r.Context(), from, to)
	if err != nil {
		h.logger.Error("fetch delta bytes",
			"op", "delta_get", "from", from, "to", to, "err", err)
		http.Error(w, "fetch delta", http.StatusInternalServerError)
		return
	}
	if !found {
		h.logger.Info("delta not cached",
			"op", "delta_get", "from", from, "to", to, "remote", r.RemoteAddr,
		)
		http.NotFound(w, r)
		return
	}
	served = true
	h.logger.Info("delta served",
		"op", "delta_get", "from", from, "to", to,
		"size", len(data), "range", r.Header.Get("Range"), "remote", r.RemoteAddr,
	)
	serveBytes(w, r, data)
}

// binary serves the whole compressed target for the full-download fallback.
func (h *httpHandler) binary(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	hotHit := "miss"
	served := false
	defer func() {
		if served {
			h.metrics.ObserveDeltaServe("http", hotHit, "full", time.Since(start).Seconds())
		}
	}()

	hash := r.PathValue("hash")
	if !protocol.IsValidHash(hash) {
		http.NotFound(w, r)
		return
	}
	// Only bytes the registry still vouches for. Without this, the endpoint
	// would happily hand out any binary ever registered, turning the store
	// into an open mirror of every build the operator ever published.
	if !h.registry.IsLive(hash) {
		http.NotFound(w, r)
		return
	}
	if _, ok := h.store.PeekHotBinary(hash); ok {
		hotHit = "hit"
	}

	data, found, err := h.store.GetBinaryBytes(r.Context(), hash)
	if err != nil {
		h.logger.Error("fetch binary bytes",
			"op", "binary_get", "hash", hash, "err", err)
		http.Error(w, "fetch binary", http.StatusInternalServerError)
		return
	}
	if !found {
		h.logger.Warn("binary not in store",
			"op", "binary_get", "hash", hash, "remote", r.RemoteAddr)
		http.NotFound(w, r)
		return
	}
	served = true
	h.logger.Info("binary served",
		"op", "binary_get", "hash", hash,
		"size", len(data), "range", r.Header.Get("Range"), "remote", r.RemoteAddr,
	)
	serveBytes(w, r, data)
}

// serveBytes writes an immutable payload with Range support.
//
// http.ServeContent handles Range, Accept-Ranges, Content-Length and 206.
// Passing a zero time disables If-Modified-Since handling, which is correct
// here: the bytes are immutable for a given content address, and the agent
// validates them against the SHA-256 in the signed manifest anyway.
func serveBytes(w http.ResponseWriter, r *http.Request, data []byte) {
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
