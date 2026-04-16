package server

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
)

// AdminDeps is the set of dependencies needed by the /admin/* endpoints.
type AdminDeps struct {
	Token      string      // static Bearer token
	Store      *Store      // Reload() target
	Manifester *Manifester // Invalidate() cache after reload
	Logging    *Logging    // SetLevel() for /admin/loglevel
	Logger     *slog.Logger
}

// RegisterAdminHandlers adds:
//
//	POST /admin/reload     — re-reads target binary, invalidates manifest cache
//	POST /admin/loglevel   — JSON {"level":"debug|info|warn|error"}
//
// Both endpoints are protected by Authorization: Bearer <token> using
// constant-time comparison. Mismatches return 401.
func RegisterAdminHandlers(mux *http.ServeMux, d AdminDeps) {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	auth := bearerTokenMiddleware(d.Token, logger)

	mux.Handle("POST /admin/reload", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oldHash := d.Store.TargetHash()
		if err := d.Store.Reload(r.Context()); err != nil {
			logger.Error("admin reload failed",
				"op", "admin_reload", "err", err, "remote", r.RemoteAddr)
			http.Error(w, "reload failed", http.StatusInternalServerError)
			return
		}
		if d.Manifester != nil {
			d.Manifester.Invalidate()
		}
		newHash := d.Store.TargetHash()
		logger.Info("admin reload applied",
			"op", "admin_reload",
			"previous_hash", oldHash,
			"target_hash", newHash,
			"remote", r.RemoteAddr,
		)
		writeJSON(w, http.StatusOK, map[string]any{
			"target_hash":   newHash,
			"previous_hash": oldHash,
		})
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
// comparison is constant-time to prevent timing side channels.
func bearerTokenMiddleware(token string, logger *slog.Logger) func(http.Handler) http.Handler {
	want := []byte(token)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			const prefix = "Bearer "
			if !strings.HasPrefix(auth, prefix) {
				logger.Warn("admin auth missing bearer",
					"op", "admin_auth", "path", r.URL.Path, "remote", r.RemoteAddr)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			got := []byte(auth[len(prefix):])
			if subtle.ConstantTimeCompare(got, want) != 1 {
				logger.Warn("admin auth failed",
					"op", "admin_auth", "path", r.URL.Path, "remote", r.RemoteAddr)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
