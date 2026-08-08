package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"golang.org/x/time/rate"

	"github.com/carlosprados/ota-updater/pkg/protocol"
)

// maxAdminBody bounds every /admin/* request body. These are tiny JSON
// control messages; anything larger is a mistake or an attack.
const maxAdminBody = 8 << 10

// decodeOptionalJSON decodes r's body into v, treating an empty body as "no
// fields set" rather than an error. Used by endpoints where every field is
// optional and a bare POST is a meaningful request.
func decodeOptionalJSON(r *http.Request, v any) error {
	err := json.NewDecoder(r.Body).Decode(v)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// AdminDeps is the set of dependencies needed by the /admin/* endpoints.
type AdminDeps struct {
	Token      string      // static Bearer token
	Store      *Store      // content-addressed byte store
	Registry   *Registry   // artifact publication state
	Manifester *Manifester // Invalidate() cache after a change
	Retention  *Retention  // optional; enables POST /admin/gc
	Logging    *Logging    // SetLevel() for /admin/loglevel
	Logger     *slog.Logger
	Metrics    *Metrics // optional; nil disables per-request metric emission

	// RateLimitPerSec is the refill rate of the token bucket that throttles
	// authentication FAILURES (401s). Legitimate requests with a correct
	// token are never counted. 0 disables the limiter entirely.
	RateLimitPerSec float64
	// RateLimitBurst is the bucket size at steady state. Combined with
	// RateLimitPerSec it caps the 401 request rate before the middleware
	// starts returning 429 Too Many Requests.
	RateLimitBurst int
}

// RegisterAdminHandlers adds:
//
//	POST   /admin/reload     — re-read file-backed artifacts from disk
//	GET    /admin/artifacts  — list every publication track
//	POST   /admin/artifacts  — publish/update one track from a file path
//	DELETE /admin/artifacts?artifact=<key> — unregister a track
//	POST   /admin/default    — choose the track answering unnamed heartbeats
//	POST   /admin/gc         — run a retention sweep now
//	POST   /admin/loglevel   — JSON {"level":"debug|info|warn|error"}
//
// Every endpoint is protected by Authorization: Bearer <token> using
// constant-time comparison. Mismatches return 401.
func RegisterAdminHandlers(mux *http.ServeMux, d AdminDeps) {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	var limiter *rate.Limiter
	if d.RateLimitPerSec > 0 && d.RateLimitBurst > 0 {
		limiter = rate.NewLimiter(rate.Limit(d.RateLimitPerSec), d.RateLimitBurst)
	}
	auth := bearerTokenMiddleware(d.Token, limiter, d.Metrics, logger)

	// POST /admin/reload re-reads artifacts from their source files. With no
	// body it reloads every file-backed artifact — the behavior a
	// single-artifact deployment already relied on. With {"artifact":"key"}
	// it reloads just that one, which is what a per-component CI pipeline
	// wants so a deploy of one service never disturbs the others.
	mux.Handle("POST /admin/reload", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		r.Body = http.MaxBytesReader(w, r.Body, maxAdminBody)

		var req struct {
			Artifact string `json:"artifact"`
		}
		// An empty body is valid and means "all".
		if err := decodeOptionalJSON(r, &req); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}

		var targets []protocol.ArtifactKey
		if req.Artifact != "" {
			key, err := protocol.ParseArtifactKey(req.Artifact)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			targets = []protocol.ArtifactKey{key}
		} else {
			for key := range d.Registry.WatchedSources() {
				targets = append(targets, key)
			}
		}
		if len(targets) == 0 {
			http.Error(w, "no file-backed artifacts to reload", http.StatusNotFound)
			return
		}

		reloaded := make(map[string]string, len(targets))
		for _, key := range targets {
			art, err := d.Registry.Republish(key)
			if err != nil {
				logger.Error("admin reload failed",
					"op", "admin_reload", "artifact", key.String(),
					"err", err, "remote", r.RemoteAddr)
				status := http.StatusInternalServerError
				if errors.Is(err, ErrArtifactNotFound) {
					status = http.StatusNotFound
				}
				http.Error(w, "reload failed: "+err.Error(), status)
				return
			}
			reloaded[key.String()] = art.TargetHash
		}
		logger.Info("admin reload applied",
			"op", "admin_reload", "artifacts", len(reloaded), "remote", r.RemoteAddr)
		writeJSON(w, http.StatusOK, map[string]any{"reloaded": reloaded})
	})))

	mux.Handle("GET /admin/artifacts", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"artifacts": d.Registry.List(),
			"default":   d.Registry.Default(),
		})
	})))

	mux.Handle("POST /admin/artifacts", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		r.Body = http.MaxBytesReader(w, r.Body, maxAdminBody)

		var req struct {
			Name    string `json:"name"`
			OS      string `json:"os"`
			Arch    string `json:"arch"`
			Version string `json:"version"`
			Binary  string `json:"binary"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if req.Binary == "" {
			http.Error(w, "binary (path) is required", http.StatusBadRequest)
			return
		}
		key := protocol.ArtifactKey{Name: req.Name, OS: req.OS, Arch: req.Arch}
		if err := key.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		art, err := d.Registry.PublishFile(key, req.Version, req.Binary)
		if err != nil {
			logger.Error("admin publish failed",
				"op", "admin_publish", "artifact", key.String(),
				"err", err, "remote", r.RemoteAddr)
			// A bad path is the operator's mistake, not a server fault.
			status := http.StatusInternalServerError
			if errors.Is(err, os.ErrNotExist) {
				status = http.StatusBadRequest
			}
			http.Error(w, "publish failed: "+err.Error(), status)
			return
		}
		logger.Info("admin publish applied",
			"op", "admin_publish", "artifact", key.String(),
			"version", art.Version, "target_hash", art.TargetHash,
			"remote", r.RemoteAddr)
		writeJSON(w, http.StatusOK, art)
	})))

	mux.Handle("DELETE /admin/artifacts", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, err := protocol.ParseArtifactKey(r.URL.Query().Get("artifact"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := d.Registry.Remove(key); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, ErrArtifactNotFound) {
				status = http.StatusNotFound
			}
			http.Error(w, err.Error(), status)
			return
		}
		logger.Info("admin artifact removed",
			"op", "admin_remove", "artifact", key.String(), "remote", r.RemoteAddr)
		w.WriteHeader(http.StatusNoContent)
	})))

	mux.Handle("POST /admin/default", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		r.Body = http.MaxBytesReader(w, r.Body, maxAdminBody)

		var req struct {
			Artifact string `json:"artifact"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		key, err := protocol.ParseArtifactKey(req.Artifact)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := d.Registry.SetDefault(key); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, ErrArtifactNotFound) {
				status = http.StatusNotFound
			}
			http.Error(w, err.Error(), status)
			return
		}
		if d.Manifester != nil {
			d.Manifester.Invalidate()
		}
		logger.Info("admin default artifact changed",
			"op", "admin_default", "artifact", key.String(), "remote", r.RemoteAddr)
		writeJSON(w, http.StatusOK, map[string]any{"default": key.String()})
	})))

	// POST /admin/gc forces a retention sweep. Useful to reclaim space
	// immediately after retiring an artifact instead of waiting for the
	// next tick, and to exercise the policy on a staging server before
	// enabling the background sweeper in production.
	mux.Handle("POST /admin/gc", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d.Retention == nil {
			http.Error(w, "retention is not configured", http.StatusNotFound)
			return
		}
		stats, err := d.Retention.Sweep(r.Context())
		if err != nil {
			logger.Error("admin gc failed",
				"op", "admin_gc", "err", err, "remote", r.RemoteAddr)
			http.Error(w, "gc failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		logger.Info("admin gc applied", "op", "admin_gc", "remote", r.RemoteAddr)
		writeJSON(w, http.StatusOK, stats)
	})))

	mux.Handle("POST /admin/loglevel", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		r.Body = http.MaxBytesReader(w, r.Body, 256)

		var req struct {
			Level string `json:"level"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		lvl, ok := parseLogLevel(req.Level)
		if !ok {
			http.Error(w, "unknown level", http.StatusBadRequest)
			return
		}
		if d.Logging != nil {
			d.Logging.SetLevel(lvl)
		}
		logger.Info("admin loglevel changed",
			"op", "admin_loglevel", "level", req.Level, "remote", r.RemoteAddr)
		writeJSON(w, http.StatusOK, map[string]any{"level": req.Level})
	})))
}

// bearerTokenMiddleware enforces Authorization: Bearer <token>. The token
// comparison is constant-time to prevent timing side channels. Requests
// that would result in 401 (missing/wrong token) consume a token from
// the provided rate limiter; when the bucket is empty, 429 is returned
// with Retry-After: 1 instead. Legitimate requests with the correct
// token never touch the limiter — CI/CD tooling that calls /admin/reload
// hundreds of times in a row never sees a 429.
func bearerTokenMiddleware(token string, limiter *rate.Limiter, metrics *Metrics, logger *slog.Logger) func(http.Handler) http.Handler {
	want := []byte(token)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			const prefix = "Bearer "
			if !strings.HasPrefix(auth, prefix) {
				if !allow(limiter) {
					metrics.IncAdminRateLimited()
					metrics.ObserveAdminRequest(r.URL.Path, http.StatusTooManyRequests)
					w.Header().Set("Retry-After", "1")
					http.Error(w, "too many requests", http.StatusTooManyRequests)
					return
				}
				metrics.ObserveAdminRequest(r.URL.Path, http.StatusUnauthorized)
				logger.Warn("admin auth missing bearer",
					"op", "admin_auth", "path", r.URL.Path, "remote", r.RemoteAddr)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			got := []byte(auth[len(prefix):])
			if subtle.ConstantTimeCompare(got, want) != 1 {
				if !allow(limiter) {
					metrics.IncAdminRateLimited()
					metrics.ObserveAdminRequest(r.URL.Path, http.StatusTooManyRequests)
					w.Header().Set("Retry-After", "1")
					http.Error(w, "too many requests", http.StatusTooManyRequests)
					return
				}
				metrics.ObserveAdminRequest(r.URL.Path, http.StatusUnauthorized)
				logger.Warn("admin auth failed",
					"op", "admin_auth", "path", r.URL.Path, "remote", r.RemoteAddr)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// allow consumes one token from the limiter. A nil limiter always allows
// (rate limiting disabled).
func allow(l *rate.Limiter) bool {
	if l == nil {
		return true
	}
	return l.Allow()
}
