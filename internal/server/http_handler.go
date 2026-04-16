package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/amplia/ota-updater/internal/protocol"
)

// HTTPConfig bundles dependencies for the HTTP handler set.
type HTTPConfig struct {
	Store      *Store
	Manifester *Manifester
	Logger     *slog.Logger
}

// NewHTTPHandler wires the OTA endpoints onto a fresh ServeMux:
//
//	POST /heartbeat          → ManifestResponse
//	GET  /delta/{from}/{to}  → compressed delta with Range support
//	POST /report             → update report sink
//	GET  /health             → server health probe
func NewHTTPHandler(cfg HTTPConfig) http.Handler {
	h := &httpHandler{
		store:      cfg.Store,
		manifester: cfg.Manifester,
		logger:     cfg.Logger,
	}
	if h.logger == nil {
		h.logger = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+protocol.PathHeartbeat, h.heartbeat)
	mux.HandleFunc("POST "+protocol.PathReport, h.report)
	mux.HandleFunc("GET "+protocol.PathHealth, h.health)
	mux.HandleFunc("GET "+protocol.PathDelta+"/{from}/{to}", h.delta)
	return mux
}

type httpHandler struct {
	store      *Store
	manifester *Manifester
	logger     *slog.Logger
}

func (h *httpHandler) heartbeat(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var hb protocol.Heartbeat
	if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
		h.logger.Warn("invalid heartbeat payload", "err", err, "remote", r.RemoteAddr)
		http.Error(w, "invalid heartbeat", http.StatusBadRequest)
		return
	}
	resp, err := h.manifester.Build(r.Context(), &hb)
	if err != nil {
		h.logger.Error("manifest build", "device_id", hb.DeviceID, "err", err)
		http.Error(w, "manifest build failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *httpHandler) report(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var rep protocol.UpdateReport
	if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
		http.Error(w, "invalid report", http.StatusBadRequest)
		return
	}
	h.logger.Info("update report",
		"device_id", rep.DeviceID,
		"previous_hash", rep.PreviousHash,
		"new_hash", rep.NewHash,
		"success", rep.Success,
		"rollback_reason", rep.RollbackReason,
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
	from := r.PathValue("from")
	to := r.PathValue("to")

	if !isValidHashSegment(from) || !isValidHashSegment(to) {
		http.NotFound(w, r)
		return
	}

	path := h.store.DeltaPath(from, to)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		// Kick off async generation so a follow-up heartbeat+request succeeds.
		if to == h.store.TargetHash() && h.store.HasBinary(from) {
			h.store.StartDeltaGeneration(from)
		}
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.logger.Error("open delta", "from", from, "to", to, "err", err)
		http.Error(w, "open delta", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		http.Error(w, "stat delta", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	// ServeContent handles Range, Accept-Ranges, Content-Length, 206.
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), f)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
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
