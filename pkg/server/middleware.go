package server

import (
	"bytes"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/plgd-dev/go-coap/v3/message"
	"github.com/plgd-dev/go-coap/v3/message/codes"
	"github.com/plgd-dev/go-coap/v3/mux"
)

// recoverHTTP wraps next so any panic in a handler is logged with stack and
// converted to a 500 response instead of crashing the server process.
// net/http already recovers per-connection, but having an explicit logged
// response makes post-mortem and dashboarding sane under load.
func recoverHTTP(next http.Handler, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				logger.Error("http handler panic",
					"op", "panic_recover",
					"panic", p,
					"method", r.Method,
					"path", r.URL.Path,
					"remote", r.RemoteAddr,
					"stack", string(debug.Stack()),
				)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// recoverCoAP is the equivalent middleware for go-coap's mux.
func recoverCoAP(logger *slog.Logger) mux.MiddlewareFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next mux.Handler) mux.Handler {
		return mux.HandlerFunc(func(w mux.ResponseWriter, r *mux.Message) {
			defer func() {
				if p := recover(); p != nil {
					logger.Error("coap handler panic",
						"op", "panic_recover",
						"panic", p,
						"code", r.Code().String(),
						"stack", string(debug.Stack()),
					)
					_ = w.SetResponse(codes.InternalServerError, message.TextPlain, bytes.NewReader([]byte("internal error")))
				}
			}()
			next.ServeCOAP(w, r)
		})
	}
}

// Payload size limits for JSON endpoints. Heartbeats and reports are small
// structs; anything beyond this is either broken or hostile.
const (
	maxHeartbeatBody = 4 << 10 // 4 KiB
	maxReportBody    = 4 << 10 // 4 KiB
)
