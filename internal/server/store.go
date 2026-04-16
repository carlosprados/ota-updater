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

	"github.com/amplia/ota-updater/internal/delta"
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
	logger      *slog.Logger

	targetBin  []byte
	targetHash string

	group singleflight.Group

	mu       sync.RWMutex
	binCache map[string][]byte

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
func (s *Store) TargetHash() string { return s.targetHash }

// TargetBinary returns a reference to the in-memory target binary. Callers
// must not mutate the returned slice.
func (s *Store) TargetBinary() []byte { return s.targetBin }

// DeltaPath returns the canonical on-disk path for the delta between two
// hashes, whether or not the file exists.
func (s *Store) DeltaPath(fromHash, toHash string) string {
	return filepath.Join(s.deltasDir, fromHash+"_"+toHash+".delta.zst")
}

// HasDelta reports whether the delta from fromHash to the current target is
// already cached on disk.
func (s *Store) HasDelta(fromHash string) bool {
	_, err := os.Stat(s.DeltaPath(fromHash, s.targetHash))
	return err == nil
}

// HasBinary reports whether a source binary with the given hash is registered
// in the store (checked on disk, not cache).
func (s *Store) HasBinary(hash string) bool {
	_, err := os.Stat(s.binaryPath(hash))
	return err == nil
}

// EnsureDelta returns the on-disk path of the delta from fromHash to the
// current target, generating and caching it if necessary. Concurrent requests
// for the same source hash are deduplicated via singleflight — only one
// bsdiff generation runs at a time per (from, target) pair.
func (s *Store) EnsureDelta(ctx context.Context, fromHash string) (string, error) {
	if p := s.DeltaPath(fromHash, s.targetHash); fileExists(p) {
		return p, nil
	}
	key := fromHash + "_" + s.targetHash
	v, err, _ := s.group.Do(key, func() (any, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return s.generateAndCache(fromHash)
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
	if s.HasDelta(fromHash) || !s.HasBinary(fromHash) {
		return false
	}
	key := fromHash + "_" + s.targetHash
	go func() {
		_, err, _ := s.group.Do(key, func() (any, error) {
			return s.generateAndCache(fromHash)
		})
		if err != nil {
			s.logger.Error("async delta generation failed",
				"from", fromHash, "to", s.targetHash, "err", err,
			)
		}
	}()
	return true
}

// generateAndCache is the shared work unit behind EnsureDelta and
// StartDeltaGeneration. It acquires a slot on deltaSlots (bounded bsdiff
// concurrency) before spending memory and CPU, and re-checks the cache
// after entering the singleflight slot so a recently-finished peer wins
// the race cleanly.
func (s *Store) generateAndCache(fromHash string) (string, error) {
	out := s.DeltaPath(fromHash, s.targetHash)
	if fileExists(out) {
		return out, nil
	}
	s.deltaSlots <- struct{}{}
	defer func() { <-s.deltaSlots }()

	// Re-check after acquiring the slot: a peer may have finished while we
	// were queued for CPU, in which case we can short-circuit without ever
	// touching bsdiff.
	if fileExists(out) {
		return out, nil
	}

	oldBin, err := s.loadBinary(fromHash)
	if err != nil {
		return "", err
	}
	s.logger.Info("generating delta", "from", fromHash, "to", s.targetHash, "op", "delta_generate")
	patch, err := delta.Generate(oldBin, s.targetBin)
	if err != nil {
		return "", fmt.Errorf("generate delta: %w", err)
	}
	if err := writeAtomic(out, patch, 0o644); err != nil {
		return "", fmt.Errorf("write delta: %w", err)
	}
	s.logger.Info("delta cached",
		"from", fromHash, "to", s.targetHash, "size", len(patch), "op", "delta_cache",
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
