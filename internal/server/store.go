// Package server implements the update-server side of the OTA system: binary
// and delta storage, manifest generation, and HTTP/CoAP transport handlers.
package server

import (
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

// Store manages on-disk binaries and cached deltas with concurrency-safe
// access. Layout:
//
//	<binariesDir>/{hash}.bin                — versioned source binaries
//	<deltasDir>/{fromHash}_{toHash}.delta.zst — cached zstd+bsdiff deltas
type Store struct {
	binariesDir string
	deltasDir   string
	targetPath  string // source-of-truth for Reload
	logger      *slog.Logger

	// mu guards targetBin, targetHash and binCache. Acquiring it briefly lets
	// Reload swap the target state while in-flight operations keep working on
	// the snapshot they captured before the swap.
	mu         sync.RWMutex
	targetBin  []byte
	targetHash string
	binCache   map[string][]byte

	group singleflight.Group

	// deltaSlots bounds how many bsdiff generations can run concurrently.
	// bsdiff is CPU- and RAM-heavy (suffix sort of the full binary). Under
	// bursts of heartbeats from many distinct source versions, uncapped
	// parallelism would OOM the server.
	deltaSlots chan struct{}
}

// DefaultDeltaGenConcurrency is the cap on concurrent bsdiff runs. Two keeps
// CPU spikes manageable on modest VMs while still hiding some I/O latency.
const DefaultDeltaGenConcurrency = 2

// Open initializes a Store. The target binary is read into memory (required
// for bsdiff), its SHA-256 is computed, and it is persisted in binariesDir as
// <hash>.bin if not already present.
func Open(ctx context.Context, binariesDir, deltasDir, targetPath string, logger *slog.Logger) (*Store, error) {
	if logger == nil {
		logger = slog.Default()
	}
	for _, d := range []string{binariesDir, deltasDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("create store dir %s: %w", d, err)
		}
	}
	data, err := os.ReadFile(targetPath)
	if err != nil {
		return nil, fmt.Errorf("read target binary: %w", err)
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])

	s := &Store{
		binariesDir: binariesDir,
		deltasDir:   deltasDir,
		targetPath:  targetPath,
		logger:      logger,
		targetBin:   data,
		targetHash:  hash,
		binCache:    map[string][]byte{hash: data},
		deltaSlots:  make(chan struct{}, DefaultDeltaGenConcurrency),
	}

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
		"binaries_dir", binariesDir,
		"deltas_dir", deltasDir,
	)
	return s, nil
}

// TargetHash returns the SHA-256 hex of the current target binary.
func (s *Store) TargetHash() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.targetHash
}

// TargetBinary returns a reference to the in-memory target binary. Callers
// must not mutate the returned slice. A concurrent Reload may swap the
// backing slice afterwards; callers that store the reference see a frozen
// snapshot.
func (s *Store) TargetBinary() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.targetBin
}

// targetSnapshot returns (targetHash, targetBin) under a single RLock, so
// the two values are guaranteed to belong to the same generation.
func (s *Store) targetSnapshot() (string, []byte) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.targetHash, s.targetBin
}

// DeltaPath returns the canonical on-disk path for the delta between two
// hashes, whether or not the file exists.
func (s *Store) DeltaPath(fromHash, toHash string) string {
	return filepath.Join(s.deltasDir, fromHash+"_"+toHash+".delta.zst")
}

// HasDelta reports whether the delta from fromHash to the current target is
// already cached on disk.
func (s *Store) HasDelta(fromHash string) bool {
	return fileExists(s.DeltaPath(fromHash, s.TargetHash()))
}

// HasBinary reports whether a source binary with the given hash is registered
// in the store (checked on disk, not cache).
func (s *Store) HasBinary(hash string) bool {
	_, err := os.Stat(s.binaryPath(hash))
	return err == nil
}

// Reload re-reads the target binary from the path passed to Open, recomputes
// its SHA-256, and atomically swaps the cached target state. If the read or
// hash fails, the previous state is preserved unchanged so the server never
// ends up unable to serve anything. Call Manifester.Invalidate() after a
// successful reload to purge the signed-manifest cache.
func (s *Store) Reload(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := os.ReadFile(s.targetPath)
	if err != nil {
		return fmt.Errorf("reload target %q: %w", s.targetPath, err)
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])

	s.mu.Lock()
	prev := s.targetHash
	s.targetBin = data
	s.targetHash = hash
	s.binCache[hash] = data
	s.mu.Unlock()

	// Best-effort persist of the (new) target under binariesDir so future
	// agents still on this version can be served a delta FROM it. Failure is
	// non-fatal — the in-memory state is already usable.
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
	)
	return nil
}

// EnsureDelta returns the on-disk path of the delta from fromHash to the
// current target, generating and caching it if necessary. Concurrent requests
// for the same source hash are deduplicated via singleflight — only one
// bsdiff generation runs at a time per (from, target) pair. A target snapshot
// is captured at entry so a concurrent Reload doesn't cause split-brain work.
func (s *Store) EnsureDelta(ctx context.Context, fromHash string) (string, error) {
	targetHash, targetBin := s.targetSnapshot()
	if p := s.DeltaPath(fromHash, targetHash); fileExists(p) {
		return p, nil
	}
	key := fromHash + "_" + targetHash
	v, err, _ := s.group.Do(key, func() (any, error) {
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
// fromHash → current target, returning immediately. Returns true if a task
// was dispatched, false if the delta is already cached or the source binary
// is unknown. Uses the same singleflight group as EnsureDelta, so concurrent
// sync/async requests for the same pair coalesce into a single bsdiff run.
func (s *Store) StartDeltaGeneration(fromHash string) bool {
	targetHash, targetBin := s.targetSnapshot()
	if fileExists(s.DeltaPath(fromHash, targetHash)) || !s.HasBinary(fromHash) {
		return false
	}
	key := fromHash + "_" + targetHash
	go func() {
		_, err, _ := s.group.Do(key, func() (any, error) {
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

// generateAndCache is the shared work unit behind EnsureDelta and
// StartDeltaGeneration. It acquires a slot on deltaSlots (bounded bsdiff
// concurrency) before spending memory and CPU. The (targetHash, targetBin)
// snapshot is passed in so this method is free of locks on the target state.
func (s *Store) generateAndCache(fromHash, targetHash string, targetBin []byte) (string, error) {
	out := s.DeltaPath(fromHash, targetHash)
	if fileExists(out) {
		return out, nil
	}
	s.deltaSlots <- struct{}{}
	defer func() { <-s.deltaSlots }()

	// Re-check after acquiring the slot: a peer may have finished while we
	// were queued for CPU.
	if fileExists(out) {
		return out, nil
	}

	oldBin, err := s.loadBinary(fromHash)
	if err != nil {
		return "", err
	}
	s.logger.Info("generating delta", "op", "delta_generate", "from", fromHash, "to", targetHash)
	patch, err := delta.Generate(oldBin, targetBin)
	if err != nil {
		return "", fmt.Errorf("generate delta: %w", err)
	}
	if err := writeAtomic(out, patch, 0o644); err != nil {
		return "", fmt.Errorf("write delta: %w", err)
	}
	s.logger.Info("delta cached",
		"op", "delta_cache", "from", fromHash, "to", targetHash, "size", len(patch),
	)
	return out, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// RegisterBinary stores a source binary in binariesDir keyed by its SHA-256
// hex. Returns the computed hash. Idempotent: does nothing if already present.
func (s *Store) RegisterBinary(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	path := s.binaryPath(hash)
	if _, err := os.Stat(path); err == nil {
		s.cacheBinary(hash, data)
		return hash, nil
	}
	if err := writeAtomic(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write binary %s: %w", hash, err)
	}
	s.cacheBinary(hash, data)
	return hash, nil
}

func (s *Store) binaryPath(hash string) string {
	return filepath.Join(s.binariesDir, hash+".bin")
}

func (s *Store) cacheBinary(hash string, data []byte) {
	s.mu.Lock()
	s.binCache[hash] = data
	s.mu.Unlock()
}

// loadBinary reads a binary by hash from cache or disk. Returns
// ErrBinaryNotFound when the hash is unknown.
func (s *Store) loadBinary(hash string) ([]byte, error) {
	s.mu.RLock()
	if data, ok := s.binCache[hash]; ok {
		s.mu.RUnlock()
		return data, nil
	}
	s.mu.RUnlock()

	data, err := os.ReadFile(s.binaryPath(hash))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrBinaryNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read binary %s: %w", hash, err)
	}
	s.cacheBinary(hash, data)
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
