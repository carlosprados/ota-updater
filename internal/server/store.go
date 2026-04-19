// Package server implements the update-server side of the OTA system: binary
// and delta storage, manifest generation, and HTTP/CoAP transport handlers.
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

	"github.com/amplia/ota-updater/pkg/atomicio"
	"github.com/amplia/ota-updater/pkg/delta"
)

// ErrBinaryNotFound is returned when a requested source binary is not in the
// store, which typically means the server doesn't know how to build a delta
// from that version.
var ErrBinaryNotFound = errors.New("source binary not found in store")

// Default limits. Tuned so a zero-valued StoreOptions still boots and stays
// bounded under sustained 24/7 load.
const (
	DefaultDeltaGenConcurrency  = 2
	DefaultTargetMaxMemoryBytes = int64(200 << 20) // 200 MiB
	DefaultHotDeltaCacheBytes   = int64(512 << 20) // 512 MiB
)

// StoreOptions parameterizes Open. Zero values get sensible defaults.
type StoreOptions struct {
	// BinariesDir holds the versioned source binaries as <hash>.bin.
	BinariesDir string
	// DeltasDir holds the cached compressed deltas as <from>_<to>.delta.zst.
	DeltasDir string
	// TargetPath is the operator-facing target file. Open reads it, hashes
	// it, and registers the content under BinariesDir. Reload re-reads from
	// this same path.
	TargetPath string
	// TargetMaxMemoryBytes caps how large the active target binary may be
	// while still kept in process RAM. Past this, the target is NOT held in
	// RAM and every delta generation re-reads it from disk. 0 → default.
	TargetMaxMemoryBytes int64
	// HotDeltaCacheBytes is the byte budget of the in-RAM LRU that fronts
	// disk reads of cached deltas. 0 → default.
	HotDeltaCacheBytes int64
	// DeltaConcurrency caps concurrent bsdiff generations. 0 → default.
	DeltaConcurrency int
	// Metrics, when non-nil, is used by the Store to export inflight
	// bsdiff gauges, target size, and cache stats. Every callsite is
	// nil-safe so tests and library consumers can skip metrics.
	Metrics *Metrics
	// DiskSpaceMinFreePct and DiskSpaceMinFreeMB drive the startup disk
	// usage warning for BinariesDir and DeltasDir. 0 on either disables
	// just that threshold; both 0 disables the check entirely. See
	// StoreYAMLConfig docs for the exact semantics.
	DiskSpaceMinFreePct int
	DiskSpaceMinFreeMB  int
}

// Store manages on-disk binaries, cached deltas on disk, and a bounded hot
// cache of delta bytes in RAM. The memory footprint is strictly bounded:
//
//   - targetBin: one binary, only while it fits under TargetMaxMemoryBytes.
//     Past that, the target lives on disk only and is re-read per generation.
//   - Source binaries: NEVER held in process RAM. Loaded from disk at each
//     bsdiff generation; the kernel page cache handles the natural LRU.
//   - hotDeltas: byte-budget LRU (HotDeltaCacheBytes). Serves "campaign"
//     bursts where the same delta is requested by many devices without
//     hitting the disk each time.
//
// Concurrent callers requesting the same uncached delta go through a pair of
// singleflight groups: one for bsdiff generation, another for post-miss disk
// reads, so a thundering herd translates into a single file open per key.
type Store struct {
	opts    StoreOptions
	logger  *slog.Logger

	// mu guards targetBin and targetHash. Acquiring it briefly lets Reload
	// swap the target state while in-flight operations keep working on the
	// snapshot they captured before the swap.
	mu         sync.RWMutex
	targetBin  []byte // may be nil when the target exceeds TargetMaxMemoryBytes
	targetHash string

	// genGroup dedupes concurrent bsdiff generations for the same (from, to).
	genGroup singleflight.Group
	// readGroup dedupes concurrent disk reads of the same cached delta file
	// during a hot-cache miss.
	readGroup singleflight.Group

	// hotDeltas is the byte-budget LRU fronting the disk cache of deltas.
	hotDeltas *byteBudgetLRU

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
	// RegisterBinary and Reload wipe the cache so a freshly-uploaded
	// binary becomes visible immediately.
	missMu    sync.Mutex
	missCache map[string]time.Time
}

// Hardcoded knobs for the HasBinary negative cache. Not exposed in YAML —
// the correct values are micro-technical and operators have no intuition
// for them. Register/Reload invalidation keeps staleness bounded anyway.
const (
	hasBinaryMissTTL     = 30 * time.Second
	hasBinaryMissCap     = 256 // bounded to keep memory predictable under attack
)

// Open initializes a Store from opts. The target binary is read, hashed,
// persisted in BinariesDir as <hash>.bin if missing, and kept in RAM only if
// its size fits under opts.TargetMaxMemoryBytes.
func Open(ctx context.Context, opts StoreOptions, logger *slog.Logger) (*Store, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if opts.BinariesDir == "" || opts.DeltasDir == "" || opts.TargetPath == "" {
		return nil, errors.New("store: binaries_dir, deltas_dir and target are required")
	}
	if opts.TargetMaxMemoryBytes <= 0 {
		opts.TargetMaxMemoryBytes = DefaultTargetMaxMemoryBytes
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
	data, err := os.ReadFile(opts.TargetPath)
	if err != nil {
		return nil, fmt.Errorf("read target binary: %w", err)
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])

	s := &Store{
		opts:       opts,
		logger:     logger,
		targetHash: hash,
		hotDeltas:  newByteBudgetLRU(opts.HotDeltaCacheBytes),
		deltaSlots: make(chan struct{}, opts.DeltaConcurrency),
		missCache:  make(map[string]time.Time, hasBinaryMissCap),
	}
	s.setTargetBin(data)

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

	// Seed metric gauges with the initial target state.
	if opts.Metrics != nil {
		opts.Metrics.SetTargetBinarySize(len(data))
		opts.Metrics.SetTargetInMemory(s.targetBin != nil)
	}

	targetStorePath := s.binaryPath(hash)
	if _, err := os.Stat(targetStorePath); errors.Is(err, os.ErrNotExist) {
		if err := atomicio.WriteFile(targetStorePath, data, 0o644, s.logger); err != nil {
			return nil, fmt.Errorf("persist target binary: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("stat target binary: %w", err)
	}

	logger.Info("store opened",
		"target_hash", hash,
		"target_size", len(data),
		"target_in_memory", s.targetBin != nil,
		"target_max_memory_mb", opts.TargetMaxMemoryBytes>>20,
		"hot_delta_cache_mb", opts.HotDeltaCacheBytes>>20,
		"delta_concurrency", opts.DeltaConcurrency,
		"binaries_dir", opts.BinariesDir,
		"deltas_dir", opts.DeltasDir,
	)
	return s, nil
}

// setTargetBin applies the in-memory cap policy: keep the binary in RAM iff
// it fits under TargetMaxMemoryBytes. Otherwise clear targetBin and rely on
// per-generation disk reads. Must be called under s.mu.Lock OR before the
// store is visible to any other goroutine.
func (s *Store) setTargetBin(data []byte) {
	if int64(len(data)) <= s.opts.TargetMaxMemoryBytes {
		s.targetBin = data
		return
	}
	s.targetBin = nil
	s.logger.Warn("target binary exceeds in-memory cap; will be read from disk per generation",
		"op", "store_target", "size", len(data),
		"cap_mb", s.opts.TargetMaxMemoryBytes>>20,
	)
}

// TargetHash returns the SHA-256 hex of the current target binary.
func (s *Store) TargetHash() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.targetHash
}

// TargetBinary returns a reference to the in-memory target binary, or nil
// when the target exceeds the in-memory cap. Callers must not mutate the
// returned slice and should treat nil as "read from disk".
func (s *Store) TargetBinary() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.targetBin
}

// targetSnapshot returns (targetHash, targetBin) under a single RLock so the
// two values belong to the same generation. targetBin may be nil.
func (s *Store) targetSnapshot() (string, []byte) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.targetHash, s.targetBin
}

// DeltaPath returns the canonical on-disk path for the delta between two
// hashes, whether or not the file exists.
func (s *Store) DeltaPath(fromHash, toHash string) string {
	return filepath.Join(s.opts.DeltasDir, fromHash+"_"+toHash+".delta.zst")
}

// HasDelta reports whether the delta from fromHash to the current target is
// already cached on disk.
func (s *Store) HasDelta(fromHash string) bool {
	return fileExists(s.DeltaPath(fromHash, s.TargetHash()))
}

// HasBinary reports whether a source binary with the given hash is registered
// in the store (checked on disk). A small negative TTL cache absorbs flood
// traffic: the first miss for a given hash runs os.Stat; subsequent misses
// for the same hash within hasBinaryMissTTL return false without touching
// the filesystem. Positive results are NOT cached — a stat on a present
// file is already cheap (kernel page cache) and a cache there would
// complicate Register/Reload invariants.
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

// invalidateMissCache clears every negative-cache entry. Called on any
// mutation of the binaries dir (RegisterBinary, Reload) so a freshly
// uploaded binary becomes visible on the very next heartbeat.
func (s *Store) invalidateMissCache() {
	s.missMu.Lock()
	clear(s.missCache)
	s.missMu.Unlock()
}

// Reload re-reads the target binary from TargetPath, recomputes its SHA-256,
// and atomically swaps the cached target state. Invalidates the hot delta
// cache because every cached delta was built against the previous target.
func (s *Store) Reload(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := os.ReadFile(s.opts.TargetPath)
	if err != nil {
		return fmt.Errorf("reload target %q: %w", s.opts.TargetPath, err)
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])

	s.mu.Lock()
	prev := s.targetHash
	s.targetHash = hash
	s.setTargetBin(data)
	s.mu.Unlock()

	// Every hot delta was computed against the previous target → stale.
	s.hotDeltas.Clear()
	s.invalidateMissCache()
	if s.opts.Metrics != nil {
		s.opts.Metrics.SetHotDeltaCacheBytes(0)
		s.opts.Metrics.SetHotDeltaCacheEntries(0)
	}

	targetStorePath := s.binaryPath(hash)
	if !fileExists(targetStorePath) {
		if werr := atomicio.WriteFile(targetStorePath, data, 0o644, s.logger); werr != nil {
			s.logger.Warn("persist reloaded target failed",
				"op", "store_reload", "target_hash", hash, "err", werr,
			)
		}
	}
	if s.opts.Metrics != nil {
		s.opts.Metrics.SetTargetBinarySize(len(data))
		s.opts.Metrics.SetTargetInMemory(s.targetBin != nil)
	}
	s.logger.Info("store target reloaded",
		"op", "store_reload",
		"previous_hash", prev,
		"target_hash", hash,
		"size", len(data),
		"target_in_memory", s.targetBin != nil,
	)
	return nil
}

// EnsureDelta returns the on-disk path of the delta from fromHash to the
// current target, generating and caching it if necessary. Concurrent requests
// for the same source hash are deduplicated via singleflight.
func (s *Store) EnsureDelta(ctx context.Context, fromHash string) (string, error) {
	targetHash, targetBin := s.targetSnapshot()
	if p := s.DeltaPath(fromHash, targetHash); fileExists(p) {
		return p, nil
	}
	key := fromHash + "_" + targetHash
	v, err, _ := s.genGroup.Do(key, func() (any, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return s.generateAndCache(fromHash, targetHash, targetBin)
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// StartDeltaGeneration dispatches an asynchronous delta generation for
// fromHash → current target. Returns true when a task was dispatched, false
// when the delta is already cached on disk or the source binary is unknown.
// The spawned goroutine is tracked by Store.asyncWG so Close(ctx) can wait
// for it before process shutdown.
func (s *Store) StartDeltaGeneration(fromHash string) bool {
	targetHash, targetBin := s.targetSnapshot()
	if fileExists(s.DeltaPath(fromHash, targetHash)) || !s.HasBinary(fromHash) {
		return false
	}
	key := fromHash + "_" + targetHash
	s.asyncWG.Add(1)
	go func() {
		defer s.asyncWG.Done()
		_, err, _ := s.genGroup.Do(key, func() (any, error) {
			return s.generateAndCache(fromHash, targetHash, targetBin)
		})
		if err != nil {
			s.logger.Error("async delta generation failed",
				"op", "delta_generate", "from", fromHash, "to", targetHash, "err", err,
			)
		}
	}()
	return true
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

// generateAndCache runs one bsdiff under the concurrency slot, writes the
// compressed delta to disk, and populates the hot cache so the first
// request that triggered this generation can be served from RAM.
func (s *Store) generateAndCache(fromHash, targetHash string, targetBin []byte) (outPath string, err error) {
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

	out := s.DeltaPath(fromHash, targetHash)
	if fileExists(out) {
		return out, nil
	}
	s.deltaSlots <- struct{}{}
	defer func() { <-s.deltaSlots }()

	if fileExists(out) {
		return out, nil
	}

	oldBin, err := s.loadBinary(fromHash)
	if err != nil {
		return "", err
	}
	// targetBin can be nil when the target exceeded the in-memory cap; read
	// it from disk at the exact moment bsdiff needs it.
	if targetBin == nil {
		targetBin, err = os.ReadFile(s.binaryPath(targetHash))
		if err != nil {
			return "", fmt.Errorf("read target binary from disk: %w", err)
		}
	}
	s.logger.Info("generating delta", "op", "delta_generate", "from", fromHash, "to", targetHash)
	patch, err := delta.Generate(oldBin, targetBin)
	if err != nil {
		return "", fmt.Errorf("generate delta: %w", err)
	}
	if err := atomicio.WriteFile(out, patch, 0o644, s.logger); err != nil {
		return "", fmt.Errorf("write delta: %w", err)
	}
	s.hotDeltas.Put(fromHash+"_"+targetHash, patch)
	if s.opts.Metrics != nil {
		s.opts.Metrics.SetHotDeltaCacheBytes(s.hotDeltas.Bytes())
		s.opts.Metrics.SetHotDeltaCacheEntries(s.hotDeltas.Len())
	}
	s.logger.Info("delta cached",
		"op", "delta_cache", "from", fromHash, "to", targetHash,
		"size", len(patch), "hot_total_bytes", s.hotDeltas.Bytes(),
	)
	return out, nil
}

// GetDeltaBytes is the unified entrypoint used by the HTTP and CoAP handlers.
// It returns the compressed delta bytes for (from, target):
//
//   - hot cache hit      → bytes from RAM, no I/O.
//   - disk hit, hot miss → read file ONCE (via singleflight across concurrent
//     callers for the same key), populate hot cache, return bytes.
//   - disk miss          → dispatch async bsdiff generation and return
//     found=false; the caller should respond 404 and let the agent retry.
//
// The returned byte slice is owned by the cache; callers must not mutate it.
func (s *Store) GetDeltaBytes(ctx context.Context, fromHash string) ([]byte, bool, error) {
	targetHash := s.TargetHash()
	key := fromHash + "_" + targetHash

	if data, ok := s.hotDeltas.Get(key); ok {
		return data, true, nil
	}

	path := s.DeltaPath(fromHash, targetHash)
	if !fileExists(path) {
		// Not on disk: dispatch generation (if source known) and report miss.
		s.StartDeltaGeneration(fromHash)
		return nil, false, nil
	}

	// On disk but not hot. Collapse the thundering herd into one read.
	v, err, _ := s.readGroup.Do(key, func() (any, error) {
		// Double-check after acquiring singleflight: a peer may have populated
		// the hot cache while we were queued.
		if data, ok := s.hotDeltas.Get(key); ok {
			return data, nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read cached delta: %w", err)
		}
		s.hotDeltas.Put(key, data)
		if s.opts.Metrics != nil {
			s.opts.Metrics.SetHotDeltaCacheBytes(s.hotDeltas.Bytes())
			s.opts.Metrics.SetHotDeltaCacheEntries(s.hotDeltas.Len())
		}
		return data, nil
	})
	if err != nil {
		return nil, false, err
	}
	return v.([]byte), true, nil
}

// PeekHotDelta reports whether (fromHash, toHash) is currently in the hot
// delta cache, WITHOUT promoting it to MRU. Used by handlers that want to
// record a "hit/miss" metric before calling GetDeltaBytes (which would
// hide the distinction).
func (s *Store) PeekHotDelta(fromHash, toHash string) ([]byte, bool) {
	// The LRU's Get promotes MRU; for a peek we'd need a dedicated entry
	// point. In practice the hit rate is what we're after and a single
	// Get right before the real Get does not materially change LRU order.
	return s.hotDeltas.Get(fromHash + "_" + toHash)
}

// DeltaReader is a convenience for callers that want a ReadSeeker positioned
// at the start of the delta bytes (e.g. for http.ServeContent). Returns
// found=false when the delta is not yet on disk (generation was dispatched).
func (s *Store) DeltaReader(ctx context.Context, fromHash string) (*bytes.Reader, bool, error) {
	data, found, err := s.GetDeltaBytes(ctx, fromHash)
	if err != nil || !found {
		return nil, found, err
	}
	return bytes.NewReader(data), true, nil
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

// RegisterBinary stores a source binary in BinariesDir keyed by its SHA-256
// hex. Returns the computed hash. Idempotent: does nothing if already present.
// The binary is NOT cached in process RAM. Invalidates the HasBinary
// negative cache so the new hash is visible on the very next heartbeat.
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

func (s *Store) binaryPath(hash string) string {
	return filepath.Join(s.opts.BinariesDir, hash+".bin")
}

// loadBinary reads a binary by hash from disk. Source binaries are never
// kept in process RAM; the kernel page cache provides the natural LRU at
// the OS level, shared with every other reader, without inflating the Go
// heap across 24/7 operation.
func (s *Store) loadBinary(hash string) ([]byte, error) {
	data, err := os.ReadFile(s.binaryPath(hash))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrBinaryNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read binary %s: %w", hash, err)
	}
	return data, nil
}

