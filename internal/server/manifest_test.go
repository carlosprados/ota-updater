package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

	"github.com/amplia/ota-updater/internal/crypto"
	"github.com/amplia/ota-updater/internal/protocol"
)

// manifesterFixture reuses storeFixture from store_test.go and layers on a
// freshly generated Ed25519 keypair and a Manifester configured with a short
// retry window to keep tests fast.
func manifesterFixture(t *testing.T) (*Manifester, ed25519.PublicKey, *Store, string) {
	t.Helper()
	s, oldHash := storeFixture(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	m := NewManifester(s, priv, ManifesterConfig{
		ChunkSize:     1024,
		RetryAfter:    1,
		TargetVersion: "1.1.0",
	}, nil)
	return m, pub, s, oldHash
}

func TestManifester_TargetAlreadyCurrent(t *testing.T) {
	m, _, s, _ := manifesterFixture(t)
	resp, err := m.Build(context.Background(), &protocol.Heartbeat{
		DeviceID:    "dev-1",
		VersionHash: s.TargetHash(),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if resp.UpdateAvailable {
		t.Fatalf("expected UpdateAvailable=false")
	}
}

func TestManifester_UnknownSource(t *testing.T) {
	m, _, _, _ := manifesterFixture(t)
	resp, err := m.Build(context.Background(), &protocol.Heartbeat{
		DeviceID:    "dev-1",
		VersionHash: "deadbeef",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if resp.UpdateAvailable {
		t.Fatalf("unknown source should yield no update")
	}
}

func TestManifester_DeltaCached_SignedResponse(t *testing.T) {
	m, pub, s, oldHash := manifesterFixture(t)
	// pre-generate the delta so we hit the cached path
	if _, err := s.EnsureDelta(context.Background(), oldHash); err != nil {
		t.Fatalf("EnsureDelta: %v", err)
	}

	resp, err := m.Build(context.Background(), &protocol.Heartbeat{
		DeviceID:    "dev-1",
		VersionHash: oldHash,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !resp.UpdateAvailable {
		t.Fatalf("expected UpdateAvailable=true")
	}
	if resp.RetryAfter != 0 {
		t.Fatalf("cached delta should not set RetryAfter")
	}
	if resp.TargetHash != s.TargetHash() {
		t.Fatalf("unexpected TargetHash")
	}
	if resp.DeltaSize <= 0 || resp.DeltaHash == "" || resp.Signature == "" {
		t.Fatalf("missing delta metadata or signature")
	}
	if resp.DeltaEndpoint != protocol.DeltaPath(oldHash, s.TargetHash()) {
		t.Fatalf("unexpected DeltaEndpoint %s", resp.DeltaEndpoint)
	}
	wantChunks := int((resp.DeltaSize + int64(resp.ChunkSize) - 1) / int64(resp.ChunkSize))
	if resp.TotalChunks != wantChunks {
		t.Fatalf("TotalChunks=%d, want %d", resp.TotalChunks, wantChunks)
	}

	// Signature verifies over ManifestSigningPayload(TargetHash, DeltaHash)
	payload, err := protocol.ManifestSigningPayload(resp.TargetHash, resp.DeltaHash)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	sig, err := hex.DecodeString(resp.Signature)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if err := crypto.Verify(pub, payload, sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
}

func TestManifester_DeltaMissing_DispatchesAsync(t *testing.T) {
	m, _, s, oldHash := manifesterFixture(t)
	// delta is NOT pre-generated: we expect the RetryAfter path.

	resp, err := m.Build(context.Background(), &protocol.Heartbeat{
		DeviceID:    "dev-1",
		VersionHash: oldHash,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !resp.UpdateAvailable {
		t.Fatalf("expected UpdateAvailable=true")
	}
	if resp.RetryAfter <= 0 {
		t.Fatalf("expected RetryAfter>0, got %d", resp.RetryAfter)
	}
	if resp.Signature != "" || resp.DeltaHash != "" {
		t.Fatalf("async path must not return delta metadata or signature")
	}

	// Wait for async generation to complete and verify the cache now exists.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s.HasDelta(oldHash) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("async delta generation did not complete within deadline")
}
