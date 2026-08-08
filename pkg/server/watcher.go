package server

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// DefaultWatcherDebounce coalesces bursts of filesystem events (e.g. a `cp`
// emitting CREATE+WRITE+CLOSE_WRITE in rapid succession) into a single
// onChange invocation.
const DefaultWatcherDebounce = 500 * time.Millisecond

// Watcher observes a single target file and fires onChange after a debounce
// window once any of WRITE, CREATE or RENAME events land on its path. It
// watches the enclosing directory, not the file itself, so atomic `mv` over
// the target (which replaces the inode) still fires.
//
// Lifecycle guarantee: once Run returns, onChange will NOT be invoked
// again. The debounce is implemented inside Run's goroutine (no
// time.AfterFunc that could fire after cancellation), so a cancelled ctx
// deterministically stops the callback chain.
type Watcher struct {
	path     string
	debounce time.Duration
	onChange func()
	logger   *slog.Logger
}

// NewWatcher returns a Watcher on the given path. onChange is invoked from
// the Watcher's own goroutine (Run's goroutine), so onChange does not need
// to be reentrant with itself — the Watcher serializes.
func NewWatcher(path string, debounce time.Duration, onChange func(), logger *slog.Logger) *Watcher {
	if debounce == 0 {
		debounce = DefaultWatcherDebounce
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Watcher{
		path:     path,
		debounce: debounce,
		onChange: onChange,
		logger:   logger,
	}
}

// Run blocks until ctx is cancelled. Returns nil on clean exit. Errors only
// on initialization (fsnotify not available, dir missing, etc.). Once Run
// returns, onChange is guaranteed NOT to be called again — any pending
// debounce timer is stopped.
func (w *Watcher) Run(ctx context.Context) error {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("fsnotify: %w", err)
	}
	defer fw.Close()

	dir := filepath.Dir(w.path)
	base := filepath.Base(w.path)
	if err := fw.Add(dir); err != nil {
		return fmt.Errorf("watch dir %q: %w", dir, err)
	}
	w.logger.Info("target watcher started",
		"op", "watcher", "dir", dir, "file", base, "debounce", w.debounce,
	)

	// Debounce state lives here in the Run goroutine. timer is nil when
	// idle; a pending event arms it; timer.C firing in this same select
	// invokes onChange in this goroutine, so no callback outlives Run.
	var timer *time.Timer
	var pending bool

	for {
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			w.logger.Info("target watcher stopping", "op", "watcher")
			return nil

		case ev, ok := <-fw.Events:
			if !ok {
				if timer != nil {
					timer.Stop()
				}
				return nil
			}
			if filepath.Base(ev.Name) != base {
				continue
			}
			// Write covers in-place edits, Create covers atomic mv-over-target,
			// Rename fires when the target file is renamed away.
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			w.logger.Debug("target event",
				"op", "watcher", "event", ev.Op.String(), "name", ev.Name,
			)
			pending = true
			if timer == nil {
				timer = time.NewTimer(w.debounce)
			} else {
				if !timer.Stop() {
					// Drain in case the timer already fired and we haven't
					// consumed timer.C yet. Non-blocking so it's safe either
					// way.
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(w.debounce)
			}

		case <-timerChan(timer):
			// The debounce window elapsed and we stayed idle. Fire onChange
			// inside this goroutine so the invocation is strictly bounded by
			// ctx — no time.AfterFunc escape hatch.
			if pending {
				pending = false
				w.onChange()
			}
			timer = nil

		case err, ok := <-fw.Errors:
			if !ok {
				if timer != nil {
					timer.Stop()
				}
				return nil
			}
			w.logger.Warn("watcher error", "op", "watcher", "err", err)
		}
	}
}

// timerChan returns t.C when t is non-nil, or a nil channel when it is nil.
// Receiving from a nil channel blocks forever, which is exactly what we
// want in the select: a disabled timer must never win the select race.
func timerChan(t *time.Timer) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}
