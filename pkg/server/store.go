// Package server implements the update-server side of the OTA system: a
// content-addressed store of binaries and derived transfer artifacts, a
// registry of publication tracks (artifacts), manifest generation, and
// HTTP/CoAP transport handlers.
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/carlosprados/ota-updater/pkg/atomicio"
	"github.com/carlosprados/ota-updater/pkg/compression"
	"github.com/carlosprados/ota-updater/pkg/delta"
)

// ErrBinaryNotFound is returned when a requested binary is not in the store,
// which typically means the server doesn't know how to build a delta from
// that version and must fall back to a full download.
var ErrBinaryNotFound = errors.New("binary not found in store")

// Default limits. Tuned so a zero-valued StoreOptions still boots and stays
// bounded under sustained 24/7 load.
const (
	DefaultDeltaGenConcurrency = 2
	DefaultTargetCacheBytes    = int64(200 << 20) // 200 MiB
	DefaultHotDeltaCacheBytes  = int64(512 << 20) // 512 MiB
)

// Hot-cache key namespaces. Deltas and whole compressed binaries share one
// byte budget — they compete for the same scarce resource (RAM) and serve the
// same purpose (absorb a campaign burst without hitting the disk) — so a
// single LRU with prefixed keys is both simpler and better behaved than two
// independently-sized caches.
const (
	hotKeyDelta  = "d:"
	hotKeyBinary = "f:"
)

// StoreOptions parameterizes Open. Zero values get sensible defaults.
//
// Note there is no TargetPath: the Store is a pure content-addressed store
// and has no notion of "the current version". That state belongs to the
// Registry, which maps artifact keys to target hashes.
type StoreOptions struct {
	// BinariesDir holds content-addressed binaries as <hash>.bin.
	BinariesDir string
	// DeltasDir holds derived transfer artifacts: deltas as
	// <from>_<to>.delta.zst and whole compressed binaries as <hash>.full.zst.
	// Everything in here is a cache — deleting it costs CPU, never data.
	DeltasDir string
	// TargetCacheBytes is the byte budget of the in-RAM LRU holding
	// *uncompressed* binaries that are currently acting as delta targets.
	// Bounded rather than per-artifact so that N artifacts cannot multiply
	// the server's resident memory. 0 → default.
	TargetCacheBytes int64
	// HotDeltaCacheBytes is the byte budget of the in-RAM LRU that fronts
	// disk reads of derived transfer artifacts. 0 → default.
	HotDeltaCacheBytes int64
	// DeltaConcurrency caps concurrent bsdiff generations. 0 → default.
	DeltaConcurrency int
	// Metrics, when non-nil, is used by the Store to export inflight
	// bsdiff gauges and cache stats. Every callsite is nil-safe so tests
	// and library consumers can skip metrics.
	Metrics *Metrics
	// DiskSpaceMinFreePct and DiskSpaceMinFreeMB drive the startup disk
	// usage warning for BinariesDir and DeltasDir. 0 on either disables
	// just that threshold; both 0 disables the check entirely.
	DiskSpaceMinFreePct int
	DiskSpaceMinFreeMB  int
}

// Store manages content-addressed binaries on disk, derived transfer
// artifacts on disk, and a bounded hot cache of transfer bytes in RAM.
//
// It knows nothing about versions, artifacts or "what is current" — it is
// addressed exclusively by SHA-256. That is what makes one Store serve
// N components × M versions × K architectures without duplication: two
// artifacts that happen to ship the same bytes share one file, and a delta
// is identified by its (from, to) pair regardless of which artifact asked
// for it.
//
// The memory footprint is strictly bounded:
//
//   - targetCache: byte-budget LRU of uncompressed binaries acting as delta
//     targets. Bounded by TargetCacheBytes across ALL artifacts.
//   - Source binaries: never held in process RAM. Loaded from disk at each
//     bsdiff generation; the kernel page cache handles the natural LRU.
//   - hotCache: byte-budget LRU of transfer artifacts (deltas and whole
//     compressed binaries). Serves campaign bursts from RAM.
//
// Concurrent callers requesting the same uncached artifact go through
// singleflight groups, so a thundering herd translates into a single
// generation or a single file open per key.
type Store struct {
	opts   StoreOptions
	logger *slog.Logger

	// genGroup dedupes concurrent generations for the same key (bsdiff for
	// deltas, zstd for whole binaries).
	genGroup singleflight.Group
	// readGroup dedupes concurrent disk reads of the same cached artifact
	// during a hot-cache miss.
	readGroup singleflight.Group

	// hotCache fronts the on-disk cache of transfer artifacts.
	hotCache *byteBudgetLRU
	// targetCache holds uncompressed binaries that are delta targets.
	targetCache *byteBudgetLRU

	// deltaSlots bounds how many bsdiff generations can run concurrently.
	// bsdiff is CPU- and RAM-heavy (suffix sort of the full binary); under
	// bursty loads uncapped parallelism would OOM the server.
	deltaSlots chan struct{}

	// asyncWG tracks all goroutines spawned by StartDeltaGeneration so
	// Close(ctx) can wait for them before the process exits. bsdiff itself
	// is not ctx-cancellable, so shutdown either waits for in-flight
	// generations to finish or logs the ones still running when the ctx
	// deadline expires.
	asyncWG sync.WaitGroup

	// missMu guards missCache, a tiny TTL-bounded negative cache that
	// absorbs HasBinary floods. Under an attacker or a fleet of legacy
	// devices sending random/garbage hashes every heartbeat, each miss
	// used to trigger a fresh os.Stat. The cache lets the first stat
	// speak for up to hasBinaryMissTTL seconds before the next one runs.
	// RegisterBinary wipes the cache so a freshly-uploaded binary becomes
	// visible immediately.
	missMu    sync.Mutex
	missCache map[string]time.Time
}

// Hardcoded knobs for the HasBinary negative cache. Not exposed in YAML —
// the correct values are micro-technical and operators have no intuition
// for them. Register invalidation keeps staleness bounded anyway.
const (
	hasBinaryMissTTL = 30 * time.Second
	hasBinaryMissCap = 256 // bounded to keep memory predictable under attack
)

// Open initializes a Store. Unlike the previous single-target design, Open
// does not read or register any binary — the Registry publishes artifacts
// into the store after it is open.
func Open(ctx context.Context, opts StoreOptions, logger *slog.Logger) (*Store, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if opts.BinariesDir == "" || opts.DeltasDir == "" {
		return nil, errors.New("store: binaries_dir and deltas_dir are required")
	}
	if opts.TargetCacheBytes <= 0 {
		opts.TargetCacheBytes = DefaultTargetCacheBytes
	}
	if opts.HotDeltaCacheBytes <= 0 {
		opts.HotDeltaCacheBytes = DefaultHotDeltaCacheBytes
	}
	if opts.DeltaConcurrency <= 0 {
		opts.DeltaConcurrency = DefaultDeltaGenConcurrency
	}

	for _, d := range []string{opts.BinariesDir, opts.DeltasDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("create store dir %s: %w", d, err)
		}
	}

	s := &Store{
		opts:        opts,
		logger:      logger,
		hotCache:    newByteBudgetLRU(opts.HotDeltaCacheBytes),
		targetCache: newByteBudgetLRU(opts.TargetCacheBytes),
		deltaSlots:  make(chan struct{}, opts.DeltaConcurrency),
		missCache:   make(map[string]time.Time, hasBinaryMissCap),
	}

	// Sweep stale temp files left behind by previous crashed writes.
	// atomicio creates ".tmp-*" next to the destination; a SIGKILL between
	// create and rename leaves the file on disk. Anything older than 24h
	// is safe to reclaim — no legitimate write ever takes that long.
	atomicio.SweepStaleTemp(opts.BinariesDir, []string{".tmp-"}, 24*time.Hour, logger)
	atomicio.SweepStaleTemp(opts.DeltasDir, []string{".tmp-"}, 24*time.Hour, logger)

	// One-shot disk-space visibility. Warnings only — never fatal; a
	// freshly provisioned filesystem may legitimately start near full.
	checkDiskSpace(opts.BinariesDir, opts.DiskSpaceMinFreePct, opts.DiskSpaceMinFreeMB, logger)
	if opts.DeltasDir != opts.BinariesDir {
		checkDiskSpace(opts.DeltasDir, opts.DiskSpaceMinFreePct, opts.DiskSpaceMinFreeMB, logger)
	}

	logger.Info("store opened",
		"op", "store_open",
		"target_cache_mb", opts.TargetCacheBytes>>20,
		"hot_delta_cache_mb", opts.HotDeltaCacheBytes>>20,
		"delta_concurrency", opts.DeltaConcurrency,
		"binaries_dir", opts.BinariesDir,
		"deltas_dir", opts.DeltasDir,
	)
	return s, nil
}

// ---------------------------------------------------------------------------
// Content-addressed binaries
// ---------------------------------------------------------------------------

// RegisterBinary stores a binary keyed by its SHA-256 hex and returns the
// hash. Idempotent: does nothing if already present. The binary is NOT held
// in process RAM. Invalidates the HasBinary negative cache so the new hash
// is visible on the very next heartbeat.
func (s *Store) RegisterBinary(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	path := s.binaryPath(hash)
	if _, err := os.Stat(path); err == nil {
		s.invalidateMissCache()
		return hash, nil
	}
	if err := atomicio.WriteFile(path, data, 0o644, s.logger); err != nil {
		return "", fmt.Errorf("write binary %s: %w", hash, err)
	}
	s.invalidateMissCache()
	return hash, nil
}

// RegisterBinaryFile reads path, registers its content and returns the hash
// and size. Convenience for publishing an artifact from the filesystem.
func (s *Store) RegisterBinaryFile(path string) (string, int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0, fmt.Errorf("read binary %q: %w", path, err)
	}
	hash, err := s.RegisterBinary(data)
	if err != nil {
		return "", 0, err
	}
	return hash, int64(len(data)), nil
}

// HasBinary reports whether a binary with the given hash is registered.
// A small negative TTL cache absorbs flood traffic: the first miss for a
// given hash runs os.Stat; subsequent misses for the same hash within
// hasBinaryMissTTL return false without touching the filesystem. Positive
// results are NOT cached — a stat on a present file is already cheap
// (kernel page cache) and caching there would complicate Register invariants.
func (s *Store) HasBinary(hash string) bool {
	s.missMu.Lock()
	if deadline, ok := s.missCache[hash]; ok {
		if time.Now().Before(deadline) {
			s.missMu.Unlock()
			return false
		}
		delete(s.missCache, hash) // expired
	}
	s.missMu.Unlock()

	_, err := os.Stat(s.binaryPath(hash))
	if err == nil {
		return true
	}
	// Record the miss. Bounded eviction: if the map is at cap, drop a
	// random entry. We don't need LRU — this is noise absorption.
	s.missMu.Lock()
	if len(s.missCache) >= hasBinaryMissCap {
		for k := range s.missCache {
			delete(s.missCache, k)
			break
		}
	}
	s.missCache[hash] = time.Now().Add(hasBinaryMissTTL)
	s.missMu.Unlock()
	return false
}

// BinarySize returns the on-disk size of a registered binary.
func (s *Store) BinarySize(hash string) (int64, error) {
	fi, err := os.Stat(s.binaryPath(hash))
	if errors.Is(err, os.ErrNotExist) {
		return 0, ErrBinaryNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("stat binary %s: %w", hash, err)
	}
	return fi.Size(), nil
}

// LoadBinary reads a binary by hash from disk. Source binaries are never
// kept in process RAM; the kernel page cache provides the natural LRU at
// the OS level, shared with every other reader, without inflating the Go
// heap across 24/7 operation.
func (s *Store) LoadBinary(hash string) ([]byte, error) {
	data, err := os.ReadFile(s.binaryPath(hash))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrBinaryNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read binary %s: %w", hash, err)
	}
	return data, nil
}

// invalidateMissCache clears every negative-cache entry. Called on any
// mutation of the binaries dir so a freshly uploaded binary becomes visible
// on the very next heartbeat.
func (s *Store) invalidateMissCache() {
	s.missMu.Lock()
	clear(s.missCache)
	s.missMu.Unlock()
}

func (s *Store) binaryPath(hash string) string {
	return filepath.Join(s.opts.BinariesDir, hash+".bin")
}

// ---------------------------------------------------------------------------
// Deltas
// ---------------------------------------------------------------------------

// DeltaPath returns the canonical on-disk path for the delta between two
// hashes, whether or not the file exists.
func (s *Store) DeltaPath(fromHash, toHash string) string {
	return filepath.Join(s.opts.DeltasDir, fromHash+"_"+toHash+".delta.zst")
}

// HasDelta reports whether the delta between two hashes is cached on disk.
func (s *Store) HasDelta(fromHash, toHash string) bool {
	return fileExists(s.DeltaPath(fromHash, toHash))
}

// EnsureDelta returns the on-disk path of the delta from fromHash to
// toHash, generating and caching it if necessary. Concurrent requests for
// the same pair are deduplicated via singleflight.
func (s *Store) EnsureDelta(ctx context.Context, fromHash, toHash string) (string, error) {
	if p := s.DeltaPath(fromHash, toHash); fileExists(p) {
		return p, nil
	}
	key := fromHash + "_" + toHash
	v, err, _ := s.genGroup.Do(key, func() (any, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return s.generateAndCacheDelta(fromHash, toHash)
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// StartDeltaGeneration dispatches an asynchronous delta generation for
// fromHash → toHash. Returns true when a task was dispatched, false when the
// delta is already cached or either binary is unknown. The spawned goroutine
// is tracked by Store.asyncWG so Close(ctx) can wait for it.
func (s *Store) StartDeltaGeneration(fromHash, toHash string) bool {
	if fileExists(s.DeltaPath(fromHash, toHash)) || !s.HasBinary(fromHash) || !s.HasBinary(toHash) {
		return false
	}
	key := fromHash + "_" + toHash
	s.asyncWG.Add(1)
	go func() {
		defer s.asyncWG.Done()
		_, err, _ := s.genGroup.Do(key, func() (any, error) {
			return s.generateAndCacheDelta(fromHash, toHash)
		})
		if err != nil {
			s.logger.Error("async delta generation failed",
				"op", "delta_generate", "from", fromHash, "to", toHash, "err", err,
			)
		}
	}()
	return true
}

// generateAndCacheDelta runs one bsdiff under the concurrency slot, writes the
// compressed delta to disk, and populates the hot cache so the request that
// triggered this generation can be served from RAM.
func (s *Store) generateAndCacheDelta(fromHash, toHash string) (outPath string, err error) {
	start := time.Now()
	s.opts.Metrics.IncAsyncGenerationInflight()
	defer func() {
		s.opts.Metrics.DecAsyncGenerationInflight()
		result := "ok"
		if err != nil {
			result = "error"
		}
		s.opts.Metrics.ObserveDeltaGeneration(result, time.Since(start).Seconds())
	}()

	out := s.DeltaPath(fromHash, toHash)
	if fileExists(out) {
		return out, nil
	}
	s.deltaSlots <- struct{}{}
	defer func() { <-s.deltaSlots }()

	if fileExists(out) {
		return out, nil
	}

	oldBin, err := s.LoadBinary(fromHash)
	if err != nil {
		return "", err
	}
	newBin, err := s.loadTarget(toHash)
	if err != nil {
		return "", err
	}
	s.logger.Info("generating delta", "op", "delta_generate", "from", fromHash, "to", toHash)
	patch, err := delta.Generate(oldBin, newBin)
	if err != nil {
		return "", fmt.Errorf("generate delta: %w", err)
	}
	if err := atomicio.WriteFile(out, patch, 0o644, s.logger); err != nil {
		return "", fmt.Errorf("write delta: %w", err)
	}
	s.putHot(hotKeyDelta+fromHash+"_"+toHash, patch)
	s.logger.Info("delta cached",
		"op", "delta_cache", "from", fromHash, "to", toHash,
		"size", len(patch), "hot_total_bytes", s.hotCache.Bytes(),
	)
	return out, nil
}

// loadTarget reads a binary that is acting as a delta target, going through
// the bounded target cache. During a campaign the same target is diffed
// against many sources; caching it avoids re-reading (and re-allocating) the
// whole binary per generation, while the byte budget keeps N artifacts from
// multiplying resident memory.
func (s *Store) loadTarget(hash string) ([]byte, error) {
	if data, ok := s.targetCache.Get(hash); ok {
		return data, nil
	}
	data, err := s.LoadBinary(hash)
	if err != nil {
		return nil, err
	}
	s.targetCache.Put(hash, data) // silently rejected if larger than the budget
	if s.opts.Metrics != nil {
		s.opts.Metrics.SetTargetCacheBytes(s.targetCache.Bytes())
		s.opts.Metrics.SetTargetCacheEntries(s.targetCache.Len())
	}
	return data, nil
}

// GetDeltaBytes returns the compressed delta bytes for (from, to):
//
//   - hot cache hit      → bytes from RAM, no I/O.
//   - disk hit, hot miss → read file ONCE (via singleflight across concurrent
//     callers for the same key), populate hot cache, return bytes.
//   - disk miss          → dispatch async generation and return found=false;
//     the caller should respond 404 and let the agent retry.
//
// The returned byte slice is owned by the cache; callers must not mutate it.
func (s *Store) GetDeltaBytes(ctx context.Context, fromHash, toHash string) ([]byte, bool, error) {
	key := hotKeyDelta + fromHash + "_" + toHash
	path := s.DeltaPath(fromHash, toHash)
	return s.getCached(key, path, func() {
		s.StartDeltaGeneration(fromHash, toHash)
	})
}

// PeekHotDelta reports whether (fromHash, toHash) is currently in the hot
// cache. Used by handlers that want to record a "hit/miss" metric before
// calling GetDeltaBytes (which would hide the distinction).
func (s *Store) PeekHotDelta(fromHash, toHash string) ([]byte, bool) {
	return s.hotCache.Get(hotKeyDelta + fromHash + "_" + toHash)
}

// DeltaReader is a convenience for callers that want a ReadSeeker positioned
// at the start of the delta bytes (e.g. for http.ServeContent).
func (s *Store) DeltaReader(ctx context.Context, fromHash, toHash string) (*bytes.Reader, bool, error) {
	data, found, err := s.GetDeltaBytes(ctx, fromHash, toHash)
	if err != nil || !found {
		return nil, found, err
	}
	return bytes.NewReader(data), true, nil
}

// ---------------------------------------------------------------------------
// Whole compressed binaries (full-download fallback)
// ---------------------------------------------------------------------------

// FullPath returns the canonical on-disk path of the zstd-compressed whole
// binary for hash. Addressed by the hash of the UNCOMPRESSED content, so the
// path stays stable if compression settings ever change.
func (s *Store) FullPath(hash string) string {
	return filepath.Join(s.opts.DeltasDir, hash+".full.zst")
}

// GetBinaryBytes returns the zstd-compressed bytes of a whole binary, which
// is what a full-download fallback transfers on the wire. Same three-tier
// path as GetDeltaBytes (hot cache → disk → generate), except generation is
// synchronous: zstd on an already-resident binary is orders of magnitude
// cheaper than bsdiff, so making the agent retry would cost more round trips
// than it saves server CPU.
//
// Returns found=false only when the underlying binary is not registered.
func (s *Store) GetBinaryBytes(ctx context.Context, hash string) ([]byte, bool, error) {
	key := hotKeyBinary + hash
	if data, ok := s.hotCache.Get(key); ok {
		return data, true, nil
	}
	path := s.FullPath(hash)
	if fileExists(path) {
		data, found, err := s.getCached(key, path, nil)
		if err != nil || found {
			return data, found, err
		}
		// The file vanished between the check and the read (retention GC
		// racing a request). Fall through and regenerate.
	}
	if !s.HasBinary(hash) {
		return nil, false, nil
	}
	v, err, _ := s.genGroup.Do(key, func() (any, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if data, ok := s.hotCache.Get(key); ok {
			return data, nil
		}
		raw, err := s.LoadBinary(hash)
		if err != nil {
			return nil, err
		}
		start := time.Now()
		packed, err := compression.CompressBytes(raw)
		if err != nil {
			return nil, fmt.Errorf("compress binary %s: %w", hash, err)
		}
		if err := atomicio.WriteFile(path, packed, 0o644, s.logger); err != nil {
			return nil, fmt.Errorf("write compressed binary: %w", err)
		}
		s.putHot(key, packed)
		s.logger.Info("full binary compressed and cached",
			"op", "full_cache", "hash", hash,
			"raw_size", len(raw), "packed_size", len(packed),
			"duration_ms", time.Since(start).Milliseconds(),
		)
		return packed, nil
	})
	if err != nil {
		if errors.Is(err, ErrBinaryNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return v.([]byte), true, nil
}

// PeekHotBinary reports whether the compressed whole binary for hash is in
// the hot cache, for hit/miss metrics.
func (s *Store) PeekHotBinary(hash string) ([]byte, bool) {
	return s.hotCache.Get(hotKeyBinary + hash)
}

// ---------------------------------------------------------------------------
// Shared cache plumbing
// ---------------------------------------------------------------------------

// getCached implements the hot-cache → disk → miss path shared by deltas and
// whole binaries. onMiss, when non-nil, is invoked when the artifact is not
// on disk (used to dispatch async delta generation).
func (s *Store) getCached(key, path string, onMiss func()) ([]byte, bool, error) {
	if data, ok := s.hotCache.Get(key); ok {
		return data, true, nil
	}
	if !fileExists(path) {
		if onMiss != nil {
			onMiss()
		}
		return nil, false, nil
	}
	// On disk but not hot. Collapse the thundering herd into one read.
	v, err, _ := s.readGroup.Do(key, func() (any, error) {
		// Double-check after acquiring singleflight: a peer may have populated
		// the hot cache while we were queued.
		if data, ok := s.hotCache.Get(key); ok {
			return data, nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read cached artifact: %w", err)
		}
		s.putHot(key, data)
		return data, nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return v.([]byte), true, nil
}

// putHot inserts into the hot cache and refreshes the cache gauges.
func (s *Store) putHot(key string, data []byte) {
	s.hotCache.Put(key, data)
	if s.opts.Metrics != nil {
		s.opts.Metrics.SetHotDeltaCacheBytes(s.hotCache.Bytes())
		s.opts.Metrics.SetHotDeltaCacheEntries(s.hotCache.Len())
	}
}

// InvalidateHot drops every cached transfer artifact from RAM. Called by the
// retention sweeper after deleting files from disk, so the cache can never
// serve bytes whose backing file is gone.
func (s *Store) InvalidateHot() {
	s.hotCache.Clear()
	if s.opts.Metrics != nil {
		s.opts.Metrics.SetHotDeltaCacheBytes(0)
		s.opts.Metrics.SetHotDeltaCacheEntries(0)
	}
}

// Close blocks until every async delta generation spawned by
// StartDeltaGeneration has finished, or ctx is done. Logs the number of
// goroutines still running at deadline — bsdiff is not cancellable so the
// caller cannot force them to stop; the choice is to wait longer or accept
// that one or two .tmp-* files may be orphaned in deltasDir.
//
// Returns ctx.Err() if the wait was cut short, nil on clean drain. Safe to
// call once; calling Close concurrently with StartDeltaGeneration is racy
// by design — the expected usage is: stop HTTP/CoAP servers first (no new
// StartDeltaGeneration calls will land), then Close.
func (s *Store) Close(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.asyncWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		s.logger.Info("store closed cleanly", "op", "store_close")
		return nil
	case <-ctx.Done():
		s.logger.Error("store close timed out with async generations still running",
			"op", "store_close", "err", ctx.Err(),
		)
		return ctx.Err()
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// checkDiskSpace logs a warning if the filesystem containing path is below
// either threshold (percent OR absolute MB). 0 on either threshold disables
// just that check. A non-Unix platform where Free is unsupported logs a
// single DEBUG line and returns — the service still boots.
func checkDiskSpace(path string, minPct, minMB int, logger *slog.Logger) {
	free, total, err := atomicio.Free(path)
	if err != nil {
		logger.Debug("disk-space probe unsupported; skipping warning",
			"op", "disk_space", "path", path, "err", err,
		)
		return
	}
	var warnPct, warnMB bool
	if minPct > 0 && total > 0 {
		if (free*100)/total < uint64(minPct) {
			warnPct = true
		}
	}
	if minMB > 0 {
		if free < uint64(minMB)<<20 {
			warnMB = true
		}
	}
	if warnPct || warnMB {
		logger.Warn("disk space running low",
			"op", "disk_space",
			"path", path,
			"free_mb", free>>20, "total_mb", total>>20,
			"min_free_pct", minPct, "min_free_mb", minMB,
			"breach_pct", warnPct, "breach_mb", warnMB,
		)
	} else {
		logger.Info("disk space ok",
			"op", "disk_space",
			"path", path,
			"free_mb", free>>20, "total_mb", total>>20,
		)
	}
}
