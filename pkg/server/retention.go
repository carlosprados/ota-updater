package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/carlosprados/ota-updater/pkg/protocol"
)

// RetentionOptions parameterizes the sweeper. A zero value is inert: with
// every limit disabled, Sweep only collects deltas that can provably never
// be requested again.
type RetentionOptions struct {
	// Interval between automatic sweeps in Run. 0 → 6h.
	Interval time.Duration
	// DeltaMaxAge deletes derived transfer artifacts older than this,
	// measured by mtime (creation, since these files are never rewritten).
	// 0 disables.
	DeltaMaxAge time.Duration
	// DeltasMaxTotalBytes caps the total size of the deltas dir. Oldest
	// first are deleted until the total fits. 0 disables.
	DeltasMaxTotalBytes int64
	// CollectOrphanBinaries deletes binaries no artifact references.
	CollectOrphanBinaries bool
	// OrphanBinaryMinAge is the grace period protecting a freshly-written
	// binary from collection before the Publish that references it lands.
	// 0 → 24h.
	OrphanBinaryMinAge time.Duration
	Metrics            *Metrics
}

// RetentionStats reports what one sweep did.
type RetentionStats struct {
	DeltasDeleted   int
	FullsDeleted    int
	BinariesDeleted int
	BytesReclaimed  int64
	Scanned         int
	Duration        time.Duration
}

// Retention reclaims disk space in the store.
//
// It is the counterweight to content-addressed storage: nothing in the store
// is ever overwritten, so without a sweeper every release adds a binary plus
// one delta per source version still in the field, forever. At N components ×
// M versions × K architectures that growth is multiplicative.
//
// The sweeper distinguishes two classes of file, and treats them very
// differently:
//
//   - Derived transfer artifacts (deltas, compressed binaries) are pure
//     cache. Deleting one costs CPU to regenerate and nothing else, so they
//     are collected aggressively.
//   - Binaries are the only copy of something an operator produced. They are
//     collected only when explicitly enabled, only when no artifact
//     references them as a current target or in its history, and only after
//     a grace period.
//
// A binary that is collected while a device in the field is still running it
// does not strand that device: the manifester falls back to a full download.
// The cost is downlink, not availability — which is exactly why history depth
// is a tuning knob and not a correctness requirement.
type Retention struct {
	store    *Store
	registry *Registry
	opts     RetentionOptions
	logger   *slog.Logger
}

// NewRetention builds a sweeper over store and registry.
func NewRetention(store *Store, registry *Registry, opts RetentionOptions, logger *slog.Logger) *Retention {
	if logger == nil {
		logger = slog.Default()
	}
	if opts.Interval <= 0 {
		opts.Interval = 6 * time.Hour
	}
	if opts.OrphanBinaryMinAge <= 0 {
		opts.OrphanBinaryMinAge = 24 * time.Hour
	}
	return &Retention{store: store, registry: registry, opts: opts, logger: logger}
}

// Run sweeps on a ticker until ctx is cancelled. The first sweep happens one
// interval in, not at boot: a restart loop must never turn into a delete
// loop, and the store is at its emptiest right after start anyway.
func (r *Retention) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.opts.Interval)
	defer ticker.Stop()
	r.logger.Info("retention sweeper started",
		"op", "retention", "interval", r.opts.Interval,
		"delta_max_age", r.opts.DeltaMaxAge,
		"deltas_max_total_mb", r.opts.DeltasMaxTotalBytes>>20,
		"collect_orphan_binaries", r.opts.CollectOrphanBinaries,
	)
	for {
		select {
		case <-ctx.Done():
			r.logger.Info("retention sweeper stopped", "op", "retention")
			return ctx.Err()
		case <-ticker.C:
			if _, err := r.Sweep(ctx); err != nil {
				r.logger.Error("retention sweep failed", "op", "retention", "err", err)
			}
		}
	}
}

// Sweep runs one collection pass. Errors on individual files are logged and
// skipped rather than aborting the sweep — a single unreadable file must not
// stop the server from reclaiming everything else.
func (r *Retention) Sweep(ctx context.Context) (RetentionStats, error) {
	start := time.Now()
	var stats RetentionStats

	live := r.registry.LiveHashes()
	current := r.registry.CurrentTargets()

	if err := r.sweepDeltas(ctx, live, current, &stats); err != nil {
		r.opts.Metrics.ObserveRetentionSweep("error")
		return stats, err
	}
	if r.opts.CollectOrphanBinaries {
		if err := r.sweepBinaries(ctx, live, &stats); err != nil {
			r.opts.Metrics.ObserveRetentionSweep("error")
			return stats, err
		}
	}
	if stats.DeltasDeleted+stats.FullsDeleted+stats.BinariesDeleted > 0 {
		// Drop cached bytes whose backing file is gone. The bytes themselves
		// would still be valid (content-addressed and immutable), but holding
		// them wastes the RAM budget on artifacts nobody will ask for again.
		r.store.InvalidateHot()
	}
	stats.Duration = time.Since(start)
	r.opts.Metrics.ObserveRetentionSweep("ok")
	r.logger.Info("retention sweep complete",
		"op", "retention",
		"scanned", stats.Scanned,
		"deltas_deleted", stats.DeltasDeleted,
		"fulls_deleted", stats.FullsDeleted,
		"binaries_deleted", stats.BinariesDeleted,
		"reclaimed_mb", stats.BytesReclaimed>>20,
		"duration_ms", stats.Duration.Milliseconds(),
	)
	return stats, nil
}

// candidate is a file considered for deletion, with the metadata needed to
// order the byte-cap pass.
type candidate struct {
	path  string
	size  int64
	mtime time.Time
	kind  string // "delta" | "full"
}

func (r *Retention) sweepDeltas(ctx context.Context, live, current map[string]struct{}, stats *RetentionStats) error {
	entries, err := os.ReadDir(r.store.opts.DeltasDir)
	if err != nil {
		return fmt.Errorf("read deltas dir: %w", err)
	}
	now := time.Now()
	var keep []candidate

	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.IsDir() {
			continue
		}
		name := e.Name()
		info, err := e.Info()
		if err != nil {
			r.logger.Warn("retention: stat failed, skipping",
				"op", "retention", "file", name, "err", err)
			continue
		}
		stats.Scanned++

		kind, from, to, ok := parseTransferName(name)
		if !ok {
			// Not a file this sweeper owns (stray temp, operator's note,
			// future format). Leaving it alone is always the safe choice.
			r.logger.Debug("retention: unrecognized file, leaving in place",
				"op", "retention", "file", name)
			continue
		}

		var reason string
		switch kind {
		case "delta":
			// A delta is only ever requested toward an artifact's CURRENT
			// target. Once that target moves, every delta pointing at the
			// old one is unreachable by construction.
			if _, ok := current[to]; !ok {
				reason = "destination is no longer a current target"
			} else if _, ok := live[from]; !ok {
				// Source aged out of every artifact's history: any device
				// still on it now takes the full-download path.
				reason = "source no longer in artifact history"
			}
		case "full":
			if _, ok := live[from]; !ok {
				reason = "binary no longer referenced by any artifact"
			}
		}
		if reason == "" && r.opts.DeltaMaxAge > 0 && now.Sub(info.ModTime()) > r.opts.DeltaMaxAge {
			reason = "older than delta_max_age"
		}
		if reason != "" {
			r.deleteFile(filepath.Join(r.store.opts.DeltasDir, name), info.Size(), kind, reason, stats)
			continue
		}
		keep = append(keep, candidate{
			path:  filepath.Join(r.store.opts.DeltasDir, name),
			size:  info.Size(),
			mtime: info.ModTime(),
			kind:  kind,
		})
	}

	return r.enforceByteCap(ctx, keep, stats)
}

// enforceByteCap deletes the oldest surviving transfer artifacts until the
// deltas dir fits within DeltasMaxTotalBytes.
func (r *Retention) enforceByteCap(ctx context.Context, keep []candidate, stats *RetentionStats) error {
	if r.opts.DeltasMaxTotalBytes <= 0 {
		return nil
	}
	var total int64
	for _, c := range keep {
		total += c.size
	}
	if total <= r.opts.DeltasMaxTotalBytes {
		return nil
	}
	// Oldest first. mtime is creation time here: these files are written
	// once and never modified, so it is a faithful "least recently produced"
	// ordering. It is deliberately NOT least-recently-used — tracking that
	// would mean a write per served request, which at fleet scale costs more
	// than the regeneration it would save.
	sort.Slice(keep, func(i, j int) bool { return keep[i].mtime.Before(keep[j].mtime) })

	r.logger.Warn("retention: deltas dir over byte cap, evicting oldest",
		"op", "retention", "total_mb", total>>20,
		"cap_mb", r.opts.DeltasMaxTotalBytes>>20)

	for _, c := range keep {
		if total <= r.opts.DeltasMaxTotalBytes {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if r.deleteFile(c.path, c.size, c.kind, "deltas dir over byte cap", stats) {
			total -= c.size
		}
	}
	return nil
}

func (r *Retention) sweepBinaries(ctx context.Context, live map[string]struct{}, stats *RetentionStats) error {
	entries, err := os.ReadDir(r.store.opts.BinariesDir)
	if err != nil {
		return fmt.Errorf("read binaries dir: %w", err)
	}
	now := time.Now()
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.IsDir() {
			continue
		}
		name := e.Name()
		hash, ok := strings.CutSuffix(name, ".bin")
		if !ok || !protocol.IsValidHash(hash) {
			r.logger.Debug("retention: unrecognized file in binaries dir, leaving in place",
				"op", "retention", "file", name)
			continue
		}
		stats.Scanned++
		if _, alive := live[hash]; alive {
			continue
		}
		info, err := e.Info()
		if err != nil {
			r.logger.Warn("retention: stat failed, skipping",
				"op", "retention", "file", name, "err", err)
			continue
		}
		// Grace period. RegisterBinary writes the file before the Publish
		// that references it; without this window a sweep landing between
		// the two would delete a binary that is seconds away from becoming
		// an artifact's target.
		if now.Sub(info.ModTime()) < r.opts.OrphanBinaryMinAge {
			continue
		}
		r.deleteFile(filepath.Join(r.store.opts.BinariesDir, name), info.Size(),
			"binary", "not referenced by any artifact", stats)
	}
	return nil
}

// deleteFile removes one file and records it. Returns whether it was removed.
func (r *Retention) deleteFile(path string, size int64, kind, reason string, stats *RetentionStats) bool {
	if err := os.Remove(path); err != nil {
		if !os.IsNotExist(err) {
			r.logger.Warn("retention: delete failed",
				"op", "retention", "file", path, "err", err)
		}
		return false
	}
	stats.BytesReclaimed += size
	switch kind {
	case "delta":
		stats.DeltasDeleted++
	case "full":
		stats.FullsDeleted++
	case "binary":
		stats.BinariesDeleted++
	}
	r.opts.Metrics.ObserveRetentionDeleted(kind, size)
	r.logger.Info("retention: deleted",
		"op", "retention", "file", filepath.Base(path),
		"kind", kind, "size", size, "reason", reason)
	return true
}

// parseTransferName classifies a file in the deltas dir.
//
//	<from>_<to>.delta.zst → ("delta", from, to, true)
//	<hash>.full.zst       → ("full",  hash, "",  true)
//
// Anything else returns ok=false and is left untouched. Both hash segments
// are validated as SHA-256 hex, so a crafted filename can never widen what
// the sweeper deletes.
func parseTransferName(name string) (kind, from, to string, ok bool) {
	if base, found := strings.CutSuffix(name, ".delta.zst"); found {
		f, t, split := strings.Cut(base, "_")
		if !split || !protocol.IsValidHash(f) || !protocol.IsValidHash(t) {
			return "", "", "", false
		}
		return "delta", f, t, true
	}
	if base, found := strings.CutSuffix(name, ".full.zst"); found {
		if !protocol.IsValidHash(base) {
			return "", "", "", false
		}
		return "full", base, "", true
	}
	return "", "", "", false
}
