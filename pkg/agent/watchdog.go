package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Default tuning for the post-swap health verification window. These match
// the values committed to in the project notes (CLAUDE.md, 2026-04-17 entry):
// three heartbeat attempts inside the window, and a version that boots more
// than twice without a confirmed-healthy run is declared bad.
const (
	DefaultWatchdogRetries  = 3
	DefaultWatchdogMaxBoots = 2
)

// BootCountFileName is the basename of the persistent boot-count file inside
// SlotManager's slotsDir. Exported so tooling and operators can locate it.
const BootCountFileName = ".boot_count"

// ErrBootCountExceeded indicates that the same version has been booted more
// than MaxBoots times without a Confirm() call. The caller (updater) must
// treat the active slot as bad and trigger a permanent rollback.
var ErrBootCountExceeded = errors.New("boot count exceeded for current version")

// HealthChecker decides whether the agent is "healthy" after a swap+restart.
// Implementations must honor ctx cancellation. The default implementation
// equates "healthy" with "the update server answered a heartbeat", but
// embedders are encouraged to compose application-specific checks (e.g.
// device peripherals, internal services) on top.
type HealthChecker interface {
	Check(ctx context.Context) error
}

// HeartbeatFunc lets the default health checker invoke a heartbeat without
// taking a hard dependency on a particular transport package. The updater
// (step 14) wires a real heartbeat call here.
type HeartbeatFunc func(ctx context.Context) error

// DefaultHealthChecker passes when Heartbeat returns nil. It is intentionally
// minimal — embedders that need richer checks should implement HealthChecker
// directly or wrap this one.
type DefaultHealthChecker struct {
	Heartbeat HeartbeatFunc
}

// Check implements HealthChecker.
func (d *DefaultHealthChecker) Check(ctx context.Context) error {
	if d == nil || d.Heartbeat == nil {
		return errors.New("default health checker: heartbeat func not configured")
	}
	return d.Heartbeat(ctx)
}

// bootCountState is the on-disk JSON shape of the boot-count file. Kept
// human-readable so an operator can `cat .boot_count` on a stuck device.
type bootCountState struct {
	VersionHash  string `json:"version_hash"`
	Count        int    `json:"count"`
	LastBootUnix int64  `json:"last_boot_unix"`
}

// BootCounter persists how many times the current version has been booted
// without a healthy confirmation. Stored as a small JSON file inside the
// slots directory so it survives reboots and process crashes.
//
// Concurrency: safe for use from multiple goroutines via an internal mutex,
// but writes are atomic at the filesystem level (temp + rename).
type BootCounter struct {
	path string
	mu   sync.Mutex
}

// NewBootCounter returns a counter persisting at path. The file is created
// lazily on the first Increment.
func NewBootCounter(path string) (*BootCounter, error) {
	if path == "" {
		return nil, errors.New("boot counter path is required")
	}
	if dir := filepath.Dir(path); dir != "" {
		if info, err := os.Stat(dir); err != nil {
			return nil, fmt.Errorf("stat boot counter dir %q: %w", dir, err)
		} else if !info.IsDir() {
			return nil, fmt.Errorf("boot counter dir %q is not a directory", dir)
		}
	}
	return &BootCounter{path: path}, nil
}

// Current reads the persisted state without modifying it. A missing file
// is reported as zero count and empty hash, not as an error.
func (b *BootCounter) Current() (versionHash string, count int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st, err := b.readLocked()
	if err != nil {
		return "", 0, err
	}
	return st.VersionHash, st.Count, nil
}

// Increment bumps the boot counter for versionHash. If the persisted hash
// matches, the count grows by one; otherwise the file is reset to count=1
// (a different version is now booting). Returns the post-increment count.
func (b *BootCounter) Increment(versionHash string) (int, error) {
	if versionHash == "" {
		return 0, errors.New("version hash is required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	st, err := b.readLocked()
	if err != nil {
		return 0, err
	}
	if st.VersionHash == versionHash {
		st.Count++
	} else {
		st.VersionHash = versionHash
		st.Count = 1
	}
	st.LastBootUnix = time.Now().Unix()
	if err := b.writeLocked(st); err != nil {
		return 0, err
	}
	return st.Count, nil
}

// Reset removes the persisted file, signalling that the current version has
// been confirmed healthy and future boots start counting from scratch.
func (b *BootCounter) Reset() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := os.Remove(b.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reset boot counter: %w", err)
	}
	return nil
}

func (b *BootCounter) readLocked() (bootCountState, error) {
	data, err := os.ReadFile(b.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return bootCountState{}, nil
		}
		return bootCountState{}, fmt.Errorf("read boot counter: %w", err)
	}
	var st bootCountState
	if err := json.Unmarshal(data, &st); err != nil {
		// Corrupt file: treat as fresh state rather than blocking the boot.
		// The next write will overwrite it atomically.
		return bootCountState{}, nil
	}
	return st, nil
}

func (b *BootCounter) writeLocked(st bootCountState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("marshal boot counter: %w", err)
	}
	dir := filepath.Dir(b.path)
	f, err := os.CreateTemp(dir, ".boot_count-*")
	if err != nil {
		return fmt.Errorf("create temp boot counter: %w", err)
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write boot counter: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("fsync boot counter: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close boot counter: %w", err)
	}
	if err := os.Rename(tmp, b.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename boot counter: %w", err)
	}
	return nil
}

// Watchdog supervises the post-swap verification window. It increments the
// boot counter on start, runs N health checks within Timeout, and either
// confirms (clears the counter) or escalates to the caller for rollback.
//
// The Watchdog never performs the rollback itself — that is the SlotManager's
// job, invoked by the updater. Keeping the responsibilities split means the
// watchdog stays trivially testable and library consumers can plug in their
// own rollback policy if needed.
type Watchdog struct {
	counter  *BootCounter
	checker  HealthChecker
	logger   *slog.Logger
	timeout  time.Duration
	retries  int
	maxBoots int
	now      func() time.Time // overridable for tests
}

// WatchdogConfig parameterizes a Watchdog. Zero values get sensible defaults
// at NewWatchdog time so library consumers only set what they care about.
type WatchdogConfig struct {
	// Timeout is the total window for health verification after a swap.
	// Defaults to 60s when zero.
	Timeout time.Duration
	// Retries is the number of health attempts inside Timeout.
	// Defaults to DefaultWatchdogRetries (3) when zero.
	Retries int
	// MaxBoots is the threshold above which the same version is declared bad.
	// Defaults to DefaultWatchdogMaxBoots (2) when zero.
	MaxBoots int
}

// NewWatchdog wires a Watchdog with a persistent boot counter and a pluggable
// health checker. The logger may be nil (slog.Default is used).
func NewWatchdog(counter *BootCounter, checker HealthChecker, cfg WatchdogConfig, logger *slog.Logger) (*Watchdog, error) {
	if counter == nil {
		return nil, errors.New("watchdog: boot counter is required")
	}
	if checker == nil {
		return nil, errors.New("watchdog: health checker is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.Retries <= 0 {
		cfg.Retries = DefaultWatchdogRetries
	}
	if cfg.MaxBoots <= 0 {
		cfg.MaxBoots = DefaultWatchdogMaxBoots
	}
	return &Watchdog{
		counter:  counter,
		checker:  checker,
		logger:   logger,
		timeout:  cfg.Timeout,
		retries:  cfg.Retries,
		maxBoots: cfg.MaxBoots,
		now:      time.Now,
	}, nil
}

// CheckBoot is called once at agent start, before the health verification
// window opens. It bumps the boot counter and returns the post-increment
// value. If the counter exceeds MaxBoots, ErrBootCountExceeded is returned
// alongside the count so the caller can roll back permanently and report.
func (w *Watchdog) CheckBoot(versionHash string) (int, error) {
	count, err := w.counter.Increment(versionHash)
	if err != nil {
		return 0, fmt.Errorf("watchdog: increment boot count: %w", err)
	}
	w.logger.Info("boot recorded",
		"op", "watchdog_boot", "version_hash", versionHash,
		"count", count, "max", w.maxBoots,
	)
	if count > w.maxBoots {
		return count, fmt.Errorf("%w: version=%s count=%d max=%d",
			ErrBootCountExceeded, versionHash, count, w.maxBoots)
	}
	return count, nil
}

// WaitForHealth runs up to Retries health checks within Timeout. Returns nil
// on the first successful check. Each attempt is given an equal time slot;
// if a check returns early, WaitForHealth still waits the remainder of its
// slot before the next attempt to avoid hammering a slow network.
func (w *Watchdog) WaitForHealth(ctx context.Context) error {
	if w.retries <= 0 {
		return errors.New("watchdog: retries must be positive")
	}
	slot := w.timeout / time.Duration(w.retries)
	if slot <= 0 {
		slot = w.timeout
	}
	deadline := w.now().Add(w.timeout)

	var lastErr error
	for i := 0; i < w.retries; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		slotDeadline := w.now().Add(slot)
		if slotDeadline.After(deadline) {
			slotDeadline = deadline
		}
		attemptCtx, cancel := context.WithDeadline(ctx, slotDeadline)
		err := w.checker.Check(attemptCtx)
		cancel()
		if err == nil {
			w.logger.Info("health check passed",
				"op", "watchdog_health", "attempt", i+1, "of", w.retries,
			)
			return nil
		}
		lastErr = err
		w.logger.Warn("health check failed",
			"op", "watchdog_health", "attempt", i+1, "of", w.retries, "err", err,
		)
		if i+1 == w.retries {
			break
		}
		remaining := time.Until(slotDeadline)
		if remaining > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(remaining):
			}
		}
	}
	return fmt.Errorf("watchdog: health check failed after %d attempts: %w", w.retries, lastErr)
}

// Confirm clears the boot counter, marking the current version as healthy.
// Call after WaitForHealth returns nil and the updater has reported success.
func (w *Watchdog) Confirm() error {
	if err := w.counter.Reset(); err != nil {
		return fmt.Errorf("watchdog: confirm: %w", err)
	}
	w.logger.Info("watchdog confirmed healthy boot", "op", "watchdog_confirm")
	return nil
}
