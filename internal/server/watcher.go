package server

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
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
type Watcher struct {
	path     string
	debounce time.Duration
	onChange func()
	logger   *slog.Logger
}

// NewWatcher returns a Watcher on the given path. onChange is invoked from a
// goroutine owned by the Watcher (not the caller's goroutine); onChange must
// be safe to call concurrently with itself if the caller does not serialize.
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
// on initialization (fsnotify not available, dir missing, etc.).
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

	var (
		mu    sync.Mutex
		timer *time.Timer
	)
	scheduleFire := func() {
		mu.Lock()
		defer mu.Unlock()
		if timer != nil {
			timer.Stop()
		}
		timer = time.AfterFunc(w.debounce, w.onChange)
	}

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("target watcher stopping", "op", "watcher")
			return nil
		case ev, ok := <-fw.Events:
			if !ok {
				return nil
			}
			if filepath.Base(ev.Name) != base {
				continue
			}
			// We care about the moments the file's content becomes new.
			// fsnotify.Write covers in-place writes, Create covers atomic
			// `mv`-over-target, Rename fires when the target file is renamed
			// away (and something else shows up in its place).
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			w.logger.Debug("target event",
				"op", "watcher", "event", ev.Op.String(), "name", ev.Name,
			)
			scheduleFire()
		case err, ok := <-fw.Errors:
			if !ok {
				return nil
			}
			w.logger.Warn("watcher error", "op", "watcher", "err", err)
		}
	}
}
