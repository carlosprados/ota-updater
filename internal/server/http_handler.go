package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/amplia/ota-updater/pkg/protocol"
)

// HTTPConfig bundles dependencies for the HTTP handler set.
type HTTPConfig struct {
	Store      *Store
	Manifester *Manifester
	Logger     *slog.Logger
	Metrics    *Metrics // optional; nil disables per-request metric emission
	// BinariesDir is the path served by GET /binaries/{name}. When empty,
	// the binaries endpoint is not registered. This is intentionally a
	// duplicate of StoreOptions.BinariesDir so the handler can be wired
	// without coupling to the Store's private fields, and so deployments
	// that don't want plain-binary download can omit it.
	BinariesDir string
}

// NewHTTPHandler wires the OTA endpoints onto a fresh ServeMux, wrapped in
// panic-recovery middleware so a handler crash never brings down the process:
//
//	POST /heartbeat          → ManifestResponse
//	GET  /delta/{from}/{to}  → compressed delta with Range support
//	GET  /binaries/{name}    → plain binary download (when BinariesDir set)
//	POST /report             → update report sink
//	GET  /health             → server health probe
func NewHTTPHandler(cfg HTTPConfig) http.Handler {
	h := &httpHandler{
		store:       cfg.Store,
		manifester:  cfg.Manifester,
		logger:      cfg.Logger,
		metrics:     cfg.Metrics,
		binariesDir: cfg.BinariesDir,
	}
	if h.logger == nil {
		h.logger = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+protocol.PathHeartbeat, h.heartbeat)
	mux.HandleFunc("POST "+protocol.PathReport, h.report)
	mux.HandleFunc("GET "+protocol.PathHealth, h.health)
	mux.HandleFunc("GET "+protocol.PathDelta+"/{from}/{to}", h.delta)
	if h.binariesDir != "" {
		mux.HandleFunc("GET /binaries/{name}", h.binary)
	}
	return recoverHTTP(mux, h.logger)
}

type httpHandler struct {
	store       *Store
	manifester  *Manifester
	logger      *slog.Logger
	metrics     *Metrics
	binariesDir string
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
		h.logger.Error("manifest build",
			"op", "heartbeat", "device_id", hb.DeviceID, "err", err,
		)
		result = "error"
		http.Error(w, "manifest build failed", http.StatusInternalServerError)
		return
	}
	switch {
	case !resp.UpdateAvailable:
		result = "none"
	case resp.RetryAfter > 0:
		result = "retry"
	default:
		result = "update"
	}
	h.logger.Info("heartbeat served",
		"op", "heartbeat",
		"device_id", hb.DeviceID,
		"from", hb.VersionHash,
		"to", h.store.TargetHash(),
		"update_available", resp.UpdateAvailable,
		"retry_after", resp.RetryAfter,
		"remote", r.RemoteAddr,
	)
	writeJSON(w, http.StatusOK, resp)
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
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "ok",
		"target_hash": h.store.TargetHash(),
	})
}

func (h *httpHandler) delta(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	hotHit := "miss"
	served := false
	defer func() {
		if served {
			h.metrics.ObserveDeltaServe("http", hotHit, time.Since(start).Seconds())
		}
	}()

	from := r.PathValue("from")
	to := r.PathValue("to")

	if !isValidHashSegment(from) || !isValidHashSegment(to) {
		http.NotFound(w, r)
		return
	}
	// The agent only ever asks for deltas to the current target; any other
	// to-hash is either stale or crafted, and we don't persist history on
	// disk so there's nothing to serve.
	if to != h.store.TargetHash() {
		http.NotFound(w, r)
		return
	}

	// Peek the hot cache before calling GetDeltaBytes so the metric label
	// is accurate; GetDeltaBytes itself would hide the distinction.
	if _, ok := h.store.PeekHotDelta(from, to); ok {
		hotHit = "hit"
	}

	data, found, err := h.store.GetDeltaBytes(r.Context(), from)
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
	w.Header().Set("Content-Type", "application/octet-stream")
	h.logger.Info("delta served",
		"op", "delta_get", "from", from, "to", to,
		"size", len(data), "range", r.Header.Get("Range"), "remote", r.RemoteAddr,
	)
	// http.ServeContent handles Range, Accept-Ranges, Content-Length, 206.
	// Passing zero-valued time disables If-Modified-Since handling, which is
	// fine: the delta bytes are immutable for a given (from, to) pair and
	// the client validates via the SHA-256 in the signed manifest.
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}

// binary serves a plain binary from BinariesDir by name. Intended for
// fleets whose update orchestrator (e.g. an external IoT platform) sends a
// downloadUrl + sha256 instead of consuming the signed manifest + delta
// flow. The agent verifies integrity on its end via the sha256.
//
// Security model:
//   - Names are restricted to a strict allowlist (see isValidBinaryName).
//     Anything else returns 404 — no leak about whether the file exists.
//   - filepath.Rel is used as a defense in depth against any name that slips
//     past the validator and resolves outside BinariesDir.
//   - The endpoint is unauthenticated by design: agents fetching it may have
//     no shared secret with this server, only the download URL the
//     orchestrator handed them. Operators must avoid placing sensitive
//     artifacts in BinariesDir, and ideally restrict network access.
func (h *httpHandler) binary(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !isValidBinaryName(name) {
		http.NotFound(w, r)
		return
	}

	full := filepath.Join(h.binariesDir, name)
	rel, err := filepath.Rel(h.binariesDir, full)
	if err != nil || rel == "." || filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") {
		http.NotFound(w, r)
		return
	}

	f, err := os.Open(full)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		h.logger.Error("open binary",
			"op", "binary_get", "name", name, "err", err,
		)
		http.Error(w, "fetch binary", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		h.logger.Error("stat binary",
			"op", "binary_get", "name", name, "err", err,
		)
		http.Error(w, "fetch binary", http.StatusInternalServerError)
		return
	}
	if !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	h.logger.Info("binary served",
		"op", "binary_get",
		"name", name,
		"size", info.Size(),
		"range", r.Header.Get("Range"),
		"remote", r.RemoteAddr,
	)
	// http.ServeContent handles Range, Accept-Ranges, Content-Length, 206.
	http.ServeContent(w, r, name, info.ModTime(), f)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// isValidBinaryName accepts only conservative filenames: 1..255 chars, no
// path separators, no leading dot, and only [A-Za-z0-9._-]. This rules out
// "..", "/", absolute paths, hidden files and shell-quoting tricks before
// the path ever touches the filesystem.
func isValidBinaryName(s string) bool {
	if len(s) == 0 || len(s) > 255 {
		return false
	}
	if s[0] == '.' {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}

// isValidHashSegment accepts exactly 64 lowercase hex chars (SHA-256 hex).
// Rejecting everything else defends against path traversal regardless of how
// the mux resolves the template.
func isValidHashSegment(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}
