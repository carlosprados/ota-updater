package server

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestLogging_LevelVarRuntimeChange(t *testing.T) {
	var buf bytes.Buffer
	l, err := NewLoggingTo(LoggingConfig{Level: "info", Format: "text"}, &buf)
	if err != nil {
		t.Fatalf("NewLoggingTo: %v", err)
	}
	log := l.Logger()

	log.Debug("d1")
	log.Info("i1")
	if strings.Contains(buf.String(), "d1") {
		t.Fatalf("debug message leaked at info level")
	}
	if !strings.Contains(buf.String(), "i1") {
		t.Fatalf("info message missing")
	}

	buf.Reset()
	l.SetLevel(slog.LevelDebug)
	log.Debug("d2")
	if !strings.Contains(buf.String(), "d2") {
		t.Fatalf("debug message missing after SetLevel(Debug)")
	}

	buf.Reset()
	l.SetLevel(slog.LevelError)
	log.Warn("w1")
	if strings.Contains(buf.String(), "w1") {
		t.Fatalf("warn leaked at error level")
	}
}

func TestLogging_JSONFormat(t *testing.T) {
	var buf bytes.Buffer
	l, err := NewLoggingTo(LoggingConfig{Level: "info", Format: "json"}, &buf)
	if err != nil {
		t.Fatalf("NewLoggingTo: %v", err)
	}
	l.Logger().Info("hello", "k", 42)
	out := buf.String()
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Fatalf("expected JSON output, got %q", out)
	}
}

func TestLogging_UnknownLevel(t *testing.T) {
	_, err := NewLoggingTo(LoggingConfig{Level: "trace", Format: "text"}, nil)
	if err == nil {
		t.Fatalf("expected error")
	}
}
