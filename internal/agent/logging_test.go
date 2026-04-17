package agent

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestNewLogger_DefaultsToInfoText(t *testing.T) {
	var buf bytes.Buffer
	l, err := NewLoggerTo(LoggingConfig{}, &buf)
	if err != nil {
		t.Fatalf("NewLoggerTo: %v", err)
	}
	if l.Level() != slog.LevelInfo {
		t.Fatalf("default level = %v, want Info", l.Level())
	}
	l.Logger().Info("hello", "k", "v")
	if !strings.Contains(buf.String(), "hello") {
		t.Fatalf("text handler did not write expected line: %q", buf.String())
	}
}

func TestNewLogger_JSONFormat(t *testing.T) {
	var buf bytes.Buffer
	l, err := NewLoggerTo(LoggingConfig{Level: "debug", Format: "json"}, &buf)
	if err != nil {
		t.Fatalf("NewLoggerTo: %v", err)
	}
	l.Logger().Debug("hi")
	out := buf.String()
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Fatalf("expected JSON output, got %q", out)
	}
}

func TestNewLogger_RejectsUnknownLevelAndFormat(t *testing.T) {
	if _, err := NewLoggerTo(LoggingConfig{Level: "bogus"}, &bytes.Buffer{}); err == nil {
		t.Fatalf("unknown level should error")
	}
	if _, err := NewLoggerTo(LoggingConfig{Format: "bogus"}, &bytes.Buffer{}); err == nil {
		t.Fatalf("unknown format should error")
	}
}

func TestLogging_SetLevel_TakesEffect(t *testing.T) {
	var buf bytes.Buffer
	l, err := NewLoggerTo(LoggingConfig{Level: "warn"}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	l.Logger().Info("first")
	if buf.Len() != 0 {
		t.Fatalf("info should be filtered at warn: %q", buf.String())
	}
	l.SetLevel(slog.LevelDebug)
	l.Logger().Info("second")
	if !strings.Contains(buf.String(), "second") {
		t.Fatalf("after SetLevel(Debug), Info should pass: %q", buf.String())
	}
}
