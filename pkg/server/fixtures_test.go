package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	mrand "math/rand"
	"path/filepath"
	"testing"

	"github.com/carlosprados/ota-updater/pkg/protocol"
)

// testLogger returns a logger that discards everything, so a failing test
// shows assertion output rather than a wall of structured logs.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// sha256HexOf is the content address of data, matching how the Store keys
// everything.
func sha256HexOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// testArtifact is the key used by fixtures that don't care about the
// multi-artifact dimension.
var testArtifact = protocol.ArtifactKey{Name: "app", OS: "linux", Arch: "amd64"}

// similarBinaries returns two byte slices that differ in a small fraction of
// their bytes, which is what makes bsdiff produce a small patch. Deterministic
// via a fixed seed so delta sizes don't vary between runs.
func similarBinaries(size int, seed int64) (oldBin, newBin []byte) {
	rng := mrand.New(mrand.NewSource(seed))
	oldBin = make([]byte, size)
	_, _ = rng.Read(oldBin)
	newBin = make([]byte, size)
	copy(newBin, oldBin)
	for i := 0; i < len(newBin); i += 100 {
		newBin[i] ^= 0x5A
	}
	return oldBin, newBin
}

// newTestStore opens a Store rooted in a fresh temp dir.
func newTestStore(t *testing.T, opts ...func(*StoreOptions)) *Store {
	t.Helper()
	tmp := t.TempDir()
	o := StoreOptions{
		BinariesDir: filepath.Join(tmp, "binaries"),
		DeltasDir:   filepath.Join(tmp, "deltas"),
	}
	for _, fn := range opts {
		fn(&o)
	}
	s, err := Open(context.Background(), o, testLogger())
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	return s
}

// newTestRegistry builds a Registry over store, persisting next to it.
func newTestRegistry(t *testing.T, store *Store, opts ...func(*RegistryOptions)) *Registry {
	t.Helper()
	o := RegistryOptions{
		StatePath: filepath.Join(t.TempDir(), "artifacts.json"),
	}
	for _, fn := range opts {
		fn(&o)
	}
	r, err := NewRegistry(store, o, testLogger())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r
}

// storeFixture registers two similar binaries and returns the store plus both
// hashes. No registry involved: the Store is content-addressed and its tests
// have no business knowing what "current" means.
func storeFixture(t *testing.T) (s *Store, oldHash, targetHash string) {
	t.Helper()
	s = newTestStore(t)
	oldBin, newBin := similarBinaries(256<<10, 42) // 256 KiB keeps it under a second
	var err error
	if oldHash, err = s.RegisterBinary(oldBin); err != nil {
		t.Fatalf("RegisterBinary(old): %v", err)
	}
	if targetHash, err = s.RegisterBinary(newBin); err != nil {
		t.Fatalf("RegisterBinary(new): %v", err)
	}
	return s, oldHash, targetHash
}

// serverFixture is the full stack most transport and manifest tests need: a
// store holding two versions, a registry publishing the newer one as
// testArtifact, and a manifester signing with a fresh keypair.
type serverFixture struct {
	Store      *Store
	Registry   *Registry
	Manifester *Manifester
	Pub        ed25519.PublicKey
	OldHash    string
	TargetHash string
	OldBin     []byte
	TargetBin  []byte
}

func newServerFixture(t *testing.T, opts ...func(*ManifesterConfig)) *serverFixture {
	t.Helper()
	s := newTestStore(t)
	oldBin, newBin := similarBinaries(64<<10, 7)

	oldHash, err := s.RegisterBinary(oldBin)
	if err != nil {
		t.Fatalf("RegisterBinary(old): %v", err)
	}

	reg := newTestRegistry(t, s)
	art, err := reg.PublishBytes(testArtifact, "1.1.0", newBin)
	if err != nil {
		t.Fatalf("PublishBytes: %v", err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	cfg := ManifesterConfig{
		ChunkSize:         1024,
		RetryAfter:        1,
		AllowFullDownload: true,
	}
	for _, fn := range opts {
		fn(&cfg)
	}
	m := NewManifester(s, reg, priv, cfg, testLogger())

	return &serverFixture{
		Store: s, Registry: reg, Manifester: m, Pub: pub,
		OldHash: oldHash, TargetHash: art.TargetHash,
		OldBin: oldBin, TargetBin: newBin,
	}
}

// heartbeat is a convenience for building a heartbeat against the fixture's
// artifact.
func (f *serverFixture) heartbeat(versionHash string) *protocol.Heartbeat {
	return &protocol.Heartbeat{
		DeviceID:    "dev-1",
		VersionHash: versionHash,
		Artifact:    testArtifact.String(),
	}
}
