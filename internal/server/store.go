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

	"golang.org/x/sync/singleflight"

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
}

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
	}
	s.setTargetBin(data)

	targetStorePath := s.binaryPath(hash)
	if _, err := os.Stat(targetStorePath); errors.Is(err, os.ErrNotExist) {
		if err := writeAtomic(targetStorePath, data, 0o644); err != nil {
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
// in the store (checked on disk).
func (s *Store) HasBinary(hash string) bool {
	_, err := os.Stat(s.binaryPath(hash))
	return err == nil
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

	targetStorePath := s.binaryPath(hash)
	if !fileExists(targetStorePath) {
		if werr := writeAtomic(targetStorePath, data, 0o644); werr != nil {
			s.logger.Warn("persist reloaded target failed",
				"op", "store_reload", "target_hash", hash, "err", werr,
			)
		}
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
func (s *Store) StartDeltaGeneration(fromHash string) bool {
	targetHash, targetBin := s.targetSnapshot()
	if fileExists(s.DeltaPath(fromHash, targetHash)) || !s.HasBinary(fromHash) {
		return false
	}
	key := fromHash + "_" + targetHash
	go func() {
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

// generateAndCache runs one bsdiff under the concurrency slot, writes the
// compressed delta to disk, and populates the hot cache so the first
// request that triggered this generation can be served from RAM.
func (s *Store) generateAndCache(fromHash, targetHash string, targetBin []byte) (string, error) {
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
	if err := writeAtomic(out, patch, 0o644); err != nil {
		return "", fmt.Errorf("write delta: %w", err)
	}
	s.hotDeltas.Put(fromHash+"_"+targetHash, patch)
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
		return data, nil
	})
	if err != nil {
		return nil, false, err
	}
	return v.([]byte), true, nil
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

// RegisterBinary stores a source binary in BinariesDir keyed by its SHA-256
// hex. Returns the computed hash. Idempotent: does nothing if already present.
// The binary is NOT cached in process RAM.
func (s *Store) RegisterBinary(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	path := s.binaryPath(hash)
	if _, err := os.Stat(path); err == nil {
		return hash, nil
	}
	if err := writeAtomic(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write binary %s: %w", hash, err)
	}
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

// writeAtomic writes data to path via a temp file in the same directory
// followed by rename, so readers never observe a partial file.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
