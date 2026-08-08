package server

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Logging bundles a slog.Logger with a LevelVar so the log level can be
// mutated at runtime (via POST /admin/loglevel or embedders' own controls)
// without reinitializing the logger or reopening its sink.
type Logging struct {
	levelVar *slog.LevelVar
	logger   *slog.Logger
}

// NewLogging builds a Logging bundle from a LoggingConfig. Writes to stderr
// using text or JSON formatting.
func NewLogging(cfg LoggingConfig) (*Logging, error) {
	return NewLoggingTo(cfg, os.Stderr)
}

// NewLoggingTo is the tests-friendly variant that lets the caller supply the
// output sink (e.g. a bytes.Buffer or io.Discard).
func NewLoggingTo(cfg LoggingConfig, out io.Writer) (*Logging, error) {
	lvl := new(slog.LevelVar)
	parsed, ok := parseLogLevel(cfg.Level)
	if !ok {
		return nil, fmt.Errorf("unknown log level %q", cfg.Level)
	}
	lvl.Set(parsed)

	opts := &slog.HandlerOptions{Level: lvl}
	var handler slog.Handler
	switch strings.ToLower(strings.TrimSpace(cfg.Format)) {
	case "json":
		handler = slog.NewJSONHandler(out, opts)
	case "text", "":
		handler = slog.NewTextHandler(out, opts)
	default:
		return nil, fmt.Errorf("unknown log format %q", cfg.Format)
	}
	return &Logging{levelVar: lvl, logger: slog.New(handler)}, nil
}

// Logger returns the underlying slog.Logger.
func (l *Logging) Logger() *slog.Logger { return l.logger }

// Level returns the current level.
func (l *Logging) Level() slog.Level { return l.levelVar.Level() }

// SetLevel atomically changes the level. Safe to call from any goroutine.
func (l *Logging) SetLevel(level slog.Level) { l.levelVar.Set(level) }

// parseLogLevel maps canonical strings to slog.Level. Returns false for
// anything it doesn't recognize so validation can surface a clear error.
func parseLogLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, true
	case "info", "":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return 0, false
}
