package agent

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Logging bundles a slog.Logger with a slog.LevelVar so library consumers
// (and cmd/edge-agent) can mutate the level at runtime without rebuilding
// the logger or reopening its sink. Mirrors the server-side helper but
// lives here so the agent package is self-contained when embedded.
type Logging struct {
	levelVar *slog.LevelVar
	logger   *slog.Logger
}

// NewLogger returns a Logging bundle that writes to stderr in the format
// requested by cfg. Returns an error for unknown levels or formats.
func NewLogger(cfg LoggingConfig) (*Logging, error) {
	return NewLoggerTo(cfg, os.Stderr)
}

// NewLoggerTo is the test-friendly variant that lets the caller supply the
// output sink (e.g. a bytes.Buffer or io.Discard).
func NewLoggerTo(cfg LoggingConfig, out io.Writer) (*Logging, error) {
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

// Logger returns the underlying *slog.Logger.
func (l *Logging) Logger() *slog.Logger { return l.logger }

// Level returns the current level.
func (l *Logging) Level() slog.Level { return l.levelVar.Level() }

// SetLevel atomically changes the level. Safe from any goroutine.
func (l *Logging) SetLevel(level slog.Level) { l.levelVar.Set(level) }

// parseLogLevel maps canonical strings to slog.Level. The empty string is
// treated as "info" so a zero-value LoggingConfig boots cleanly.
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
