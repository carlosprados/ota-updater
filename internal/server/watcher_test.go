package server

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatcher_DetectsWriteAndDebounces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.bin")
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatalf("write initial: %v", err)
	}

	var fired atomic.Int32
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := NewWatcher(path, 100*time.Millisecond, func() { fired.Add(1) }, logger)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()

	// give fsnotify a moment to install
	time.Sleep(50 * time.Millisecond)

	// burst of writes within the debounce window → single fire
	for i := range 5 {
		if err := os.WriteFile(path, []byte{'v', byte('2' + i)}, 0o644); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// wait out the debounce
	time.Sleep(300 * time.Millisecond)

	if got := fired.Load(); got != 1 {
		t.Fatalf("fired=%d, want 1 after burst", got)
	}

	// a second, separated write triggers another fire
	time.Sleep(200 * time.Millisecond)
	if err := os.WriteFile(path, []byte("v9"), 0o644); err != nil {
		t.Fatalf("second write: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := fired.Load(); got != 2 {
		t.Fatalf("fired=%d, want 2 after second write", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("watcher did not stop on ctx cancel")
	}
}

func TestWatcher_IgnoresUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.bin")
	other := filepath.Join(dir, "other.bin")
	if err := os.WriteFile(target, []byte("t"), 0o644); err != nil {
		t.Fatal(err)
	}

	var fired atomic.Int32
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := NewWatcher(target, 80*time.Millisecond, func() { fired.Add(1) }, logger)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go w.Run(ctx)
	time.Sleep(50 * time.Millisecond)

	// write to an unrelated file — must not trigger
	if err := os.WriteFile(other, []byte("noise"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := fired.Load(); got != 0 {
		t.Fatalf("fired=%d on unrelated write, want 0", got)
	}
}

func TestWatcher_CancelStopsPendingCallback(t *testing.T) {
	// Regression: an event fires a debounce timer, then ctx is cancelled
	// BEFORE the debounce window elapses. onChange must NOT be called.
	// Previously (time.AfterFunc) this race existed.
	dir := t.TempDir()
	path := filepath.Join(dir, "target.bin")
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	var fired atomic.Int32
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Long debounce so we can cancel before it elapses.
	w := NewWatcher(path, 500*time.Millisecond, func() { fired.Add(1) }, logger)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)

	// Trigger an event (arms the debounce timer).
	if err := os.WriteFile(path, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Give the event a beat to reach the watcher and arm the timer.
	time.Sleep(100 * time.Millisecond)

	// Now cancel well before the 500 ms debounce window.
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	// Wait past the would-be fire time to ensure a leaked AfterFunc would
	// have had its chance.
	time.Sleep(600 * time.Millisecond)

	if got := fired.Load(); got != 0 {
		t.Fatalf("onChange fired %d times after cancel; want 0 (timer must have been stopped)", got)
	}
}
