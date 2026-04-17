package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeChecker is a HealthChecker that fails the first failUntil times then
// succeeds. Counts attempts so tests can assert how many checks happened.
type fakeChecker struct {
	failUntil int
	calls     atomic.Int32
	err       error
}

func (f *fakeChecker) Check(ctx context.Context) error {
	n := f.calls.Add(1)
	if int(n) <= f.failUntil {
		if f.err != nil {
			return f.err
		}
		return errors.New("synthetic failure")
	}
	return nil
}

// alwaysFailChecker fails on every call with the given error (or a default).
type alwaysFailChecker struct {
	calls atomic.Int32
	err   error
}

func (a *alwaysFailChecker) Check(ctx context.Context) error {
	a.calls.Add(1)
	if a.err != nil {
		return a.err
	}
	return errors.New("permanent failure")
}

func newBootCounterAt(t *testing.T, name string) (*BootCounter, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	bc, err := NewBootCounter(path)
	if err != nil {
		t.Fatalf("NewBootCounter: %v", err)
	}
	return bc, path
}

func TestBootCounter_IncrementSameVersion(t *testing.T) {
	bc, _ := newBootCounterAt(t, BootCountFileName)

	for i := 1; i <= 3; i++ {
		got, err := bc.Increment("hash-A")
		if err != nil {
			t.Fatalf("Increment %d: %v", i, err)
		}
		if got != i {
			t.Fatalf("Increment %d returned %d", i, got)
		}
	}

	hash, count, err := bc.Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if hash != "hash-A" || count != 3 {
		t.Fatalf("Current = (%q, %d), want (hash-A, 3)", hash, count)
	}
}

func TestBootCounter_DifferentVersionResets(t *testing.T) {
	bc, _ := newBootCounterAt(t, BootCountFileName)
	if _, err := bc.Increment("hash-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := bc.Increment("hash-A"); err != nil {
		t.Fatal(err)
	}
	got, err := bc.Increment("hash-B")
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("after version change, got count=%d, want 1", got)
	}
	hash, count, _ := bc.Current()
	if hash != "hash-B" || count != 1 {
		t.Fatalf("state after version change = (%q,%d)", hash, count)
	}
}

func TestBootCounter_PersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, BootCountFileName)

	first, err := NewBootCounter(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Increment("hash-X"); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Increment("hash-X"); err != nil {
		t.Fatal(err)
	}

	// New instance — simulates a process restart.
	second, err := NewBootCounter(path)
	if err != nil {
		t.Fatal(err)
	}
	hash, count, err := second.Current()
	if err != nil {
		t.Fatal(err)
	}
	if hash != "hash-X" || count != 2 {
		t.Fatalf("post-restart Current = (%q,%d), want (hash-X,2)", hash, count)
	}
	got, err := second.Increment("hash-X")
	if err != nil {
		t.Fatal(err)
	}
	if got != 3 {
		t.Fatalf("Increment after restart = %d, want 3", got)
	}
}

func TestBootCounter_ResetClearsFile(t *testing.T) {
	bc, path := newBootCounterAt(t, BootCountFileName)
	if _, err := bc.Increment("hash-Y"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file should exist: %v", err)
	}
	if err := bc.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Reset should remove file, stat err = %v", err)
	}
	// Reset on already-empty is a no-op.
	if err := bc.Reset(); err != nil {
		t.Fatalf("Reset (idempotent): %v", err)
	}
	hash, count, _ := bc.Current()
	if hash != "" || count != 0 {
		t.Fatalf("after Reset Current = (%q,%d), want empty/0", hash, count)
	}
}

func TestBootCounter_CorruptFileTreatedAsFresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, BootCountFileName)
	if err := os.WriteFile(path, []byte("not-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	bc, err := NewBootCounter(path)
	if err != nil {
		t.Fatal(err)
	}
	hash, count, err := bc.Current()
	if err != nil {
		t.Fatalf("Current on corrupt file: %v", err)
	}
	if hash != "" || count != 0 {
		t.Fatalf("corrupt file should read empty, got (%q,%d)", hash, count)
	}
	got, err := bc.Increment("hash-Z")
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("Increment after corrupt = %d, want 1", got)
	}
}

func TestWatchdog_HappyPath_FirstAttemptOK(t *testing.T) {
	bc, _ := newBootCounterAt(t, BootCountFileName)
	checker := &fakeChecker{failUntil: 0}
	w, err := NewWatchdog(bc, checker, WatchdogConfig{
		Timeout: 300 * time.Millisecond,
		Retries: 3,
	}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.CheckBoot("hash-A"); err != nil {
		t.Fatalf("CheckBoot: %v", err)
	}
	if err := w.WaitForHealth(context.Background()); err != nil {
		t.Fatalf("WaitForHealth: %v", err)
	}
	if got := checker.calls.Load(); got != 1 {
		t.Fatalf("checker calls = %d, want 1", got)
	}
	if err := w.Confirm(); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	hash, count, _ := bc.Current()
	if hash != "" || count != 0 {
		t.Fatalf("after Confirm state = (%q,%d), want cleared", hash, count)
	}
}

func TestWatchdog_HealthOKOnSecondAttempt(t *testing.T) {
	bc, _ := newBootCounterAt(t, BootCountFileName)
	checker := &fakeChecker{failUntil: 1} // fails once then OK
	w, err := NewWatchdog(bc, checker, WatchdogConfig{
		Timeout: 90 * time.Millisecond,
		Retries: 3,
	}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WaitForHealth(context.Background()); err != nil {
		t.Fatalf("WaitForHealth: %v", err)
	}
	if got := checker.calls.Load(); got != 2 {
		t.Fatalf("checker calls = %d, want 2", got)
	}
}

func TestWatchdog_AllAttemptsFail(t *testing.T) {
	bc, _ := newBootCounterAt(t, BootCountFileName)
	checker := &alwaysFailChecker{}
	w, err := NewWatchdog(bc, checker, WatchdogConfig{
		Timeout: 90 * time.Millisecond,
		Retries: 3,
	}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	err = w.WaitForHealth(context.Background())
	if err == nil {
		t.Fatalf("WaitForHealth should fail")
	}
	if got := checker.calls.Load(); got != 3 {
		t.Fatalf("checker calls = %d, want 3 (one per retry)", got)
	}
}

func TestWatchdog_BootCountEscalation(t *testing.T) {
	bc, _ := newBootCounterAt(t, BootCountFileName)
	w, err := NewWatchdog(bc, &fakeChecker{}, WatchdogConfig{
		Timeout:  100 * time.Millisecond,
		Retries:  3,
		MaxBoots: 2,
	}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}

	// Boot 1 — under limit.
	count, err := w.CheckBoot("hash-bad")
	if err != nil {
		t.Fatalf("boot 1: unexpected err %v", err)
	}
	if count != 1 {
		t.Fatalf("boot 1 count = %d, want 1", count)
	}

	// Boot 2 — equal to limit, still acceptable.
	count, err = w.CheckBoot("hash-bad")
	if err != nil {
		t.Fatalf("boot 2: unexpected err %v", err)
	}
	if count != 2 {
		t.Fatalf("boot 2 count = %d, want 2", count)
	}

	// Boot 3 — exceeds the limit, must trigger ErrBootCountExceeded.
	count, err = w.CheckBoot("hash-bad")
	if !errors.Is(err, ErrBootCountExceeded) {
		t.Fatalf("boot 3 err = %v, want ErrBootCountExceeded", err)
	}
	if count != 3 {
		t.Fatalf("boot 3 count = %d, want 3 (returned even on exceed)", count)
	}
}

func TestWatchdog_BootCountResetOnNewVersion(t *testing.T) {
	bc, _ := newBootCounterAt(t, BootCountFileName)
	w, err := NewWatchdog(bc, &fakeChecker{}, WatchdogConfig{
		Timeout: 50 * time.Millisecond, Retries: 1, MaxBoots: 2,
	}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if _, err := w.CheckBoot("hash-old"); err != nil {
			t.Fatalf("boot old #%d: %v", i, err)
		}
	}
	// New version booting after 2 prior boots of old version — counter starts fresh.
	count, err := w.CheckBoot("hash-new")
	if err != nil {
		t.Fatalf("boot new: %v", err)
	}
	if count != 1 {
		t.Fatalf("new version boot count = %d, want 1", count)
	}
}

func TestWatchdog_ContextCancel(t *testing.T) {
	bc, _ := newBootCounterAt(t, BootCountFileName)
	checker := &alwaysFailChecker{}
	w, err := NewWatchdog(bc, checker, WatchdogConfig{
		Timeout: 5 * time.Second,
		Retries: 5,
	}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	if err := w.WaitForHealth(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestWatchdog_ConstructorValidation(t *testing.T) {
	bc, _ := newBootCounterAt(t, BootCountFileName)
	if _, err := NewWatchdog(nil, &fakeChecker{}, WatchdogConfig{}, nil); err == nil {
		t.Fatalf("nil counter should error")
	}
	if _, err := NewWatchdog(bc, nil, WatchdogConfig{}, nil); err == nil {
		t.Fatalf("nil checker should error")
	}
	// Defaults are applied for zero-valued cfg.
	w, err := NewWatchdog(bc, &fakeChecker{}, WatchdogConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if w.retries != DefaultWatchdogRetries || w.maxBoots != DefaultWatchdogMaxBoots {
		t.Fatalf("defaults not applied: retries=%d maxBoots=%d", w.retries, w.maxBoots)
	}
}

func TestDefaultHealthChecker_DelegatesToHeartbeat(t *testing.T) {
	called := false
	hc := &DefaultHealthChecker{
		Heartbeat: func(ctx context.Context) error {
			called = true
			return nil
		},
	}
	if err := hc.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !called {
		t.Fatalf("Heartbeat func should have been invoked")
	}
}

func TestDefaultHealthChecker_NilHeartbeatErrors(t *testing.T) {
	hc := &DefaultHealthChecker{}
	if err := hc.Check(context.Background()); err == nil {
		t.Fatalf("Check with nil Heartbeat should error")
	}
}

func TestDefaultHealthChecker_PropagatesError(t *testing.T) {
	want := errors.New("heartbeat boom")
	hc := &DefaultHealthChecker{Heartbeat: func(ctx context.Context) error { return want }}
	if err := hc.Check(context.Background()); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}
