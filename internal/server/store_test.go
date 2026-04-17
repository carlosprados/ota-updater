package server

import (
	"context"
	"crypto/sha256"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/amplia/ota-updater/pkg/delta"
)

// storeFixture creates a Store with an old binary registered and a target
// binary whose hash differs by a small mutation. Returns the store and the
// registered oldHash.
func storeFixture(t *testing.T) (*Store, string) {
	t.Helper()
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "binaries")
	deltaDir := filepath.Join(tmp, "deltas")

	rng := rand.New(rand.NewSource(42))
	oldBin := make([]byte, 256<<10) // 256 KiB keeps the test under a second
	_, _ = rng.Read(oldBin)

	newBin := make([]byte, len(oldBin))
	copy(newBin, oldBin)
	for i := 0; i < len(newBin); i += 100 {
		newBin[i] ^= 0x5A
	}

	targetPath := filepath.Join(tmp, "target.bin")
	if err := os.WriteFile(targetPath, newBin, 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}

	// silent logger for tests
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := Open(context.Background(), StoreOptions{
		BinariesDir: binDir, DeltasDir: deltaDir, TargetPath: targetPath,
	}, logger)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	oldHash, err := s.RegisterBinary(oldBin)
	if err != nil {
		t.Fatalf("RegisterBinary: %v", err)
	}
	return s, oldHash
}

func TestStore_EnsureDelta_RoundTrip(t *testing.T) {
	s, oldHash := storeFixture(t)
	ctx := context.Background()

	path, err := s.EnsureDelta(ctx, oldHash)
	if err != nil {
		t.Fatalf("EnsureDelta: %v", err)
	}
	if path != s.DeltaPath(oldHash, s.TargetHash()) {
		t.Fatalf("unexpected delta path: %s", path)
	}

	compressed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read delta: %v", err)
	}

	oldBin, err := s.loadBinary(oldHash)
	if err != nil {
		t.Fatalf("loadBinary: %v", err)
	}
	reconstructed, err := delta.Apply(oldBin, compressed)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if sha256.Sum256(reconstructed) != sha256.Sum256(s.TargetBinary()) {
		t.Fatalf("reconstructed hash != target hash")
	}
}

func TestStore_EnsureDelta_CachedShortCircuit(t *testing.T) {
	s, oldHash := storeFixture(t)
	ctx := context.Background()

	first, err := s.EnsureDelta(ctx, oldHash)
	if err != nil {
		t.Fatalf("first EnsureDelta: %v", err)
	}
	info1, _ := os.Stat(first)

	second, err := s.EnsureDelta(ctx, oldHash)
	if err != nil {
		t.Fatalf("second EnsureDelta: %v", err)
	}
	info2, _ := os.Stat(second)

	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Fatalf("cached delta was regenerated (mtime changed)")
	}
}

func TestStore_EnsureDelta_UnknownSource(t *testing.T) {
	s, _ := storeFixture(t)
	_, err := s.EnsureDelta(context.Background(), "deadbeef")
	if err == nil {
		t.Fatalf("expected ErrBinaryNotFound, got nil")
	}
}

// TestStore_EnsureDelta_Concurrent exercises singleflight dedup: N goroutines
// request the same delta simultaneously; all must succeed with valid patches.
func TestStore_EnsureDelta_Concurrent(t *testing.T) {
	s, oldHash := storeFixture(t)
	ctx := context.Background()

	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	paths := make(chan string, n)

	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := s.EnsureDelta(ctx, oldHash)
			if err != nil {
				errs <- err
				return
			}
			paths <- p
		}()
	}
	wg.Wait()
	close(errs)
	close(paths)

	for err := range errs {
		t.Errorf("concurrent EnsureDelta: %v", err)
	}
	expected := s.DeltaPath(oldHash, s.TargetHash())
	for p := range paths {
		if p != expected {
			t.Errorf("unexpected delta path %s, want %s", p, expected)
		}
	}
}
