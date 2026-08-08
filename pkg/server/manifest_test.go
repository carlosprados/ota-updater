package server

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/carlosprados/ota-updater/pkg/compression"
	"github.com/carlosprados/ota-updater/pkg/crypto"
	"github.com/carlosprados/ota-updater/pkg/protocol"
)

func TestManifester_TargetAlreadyCurrent(t *testing.T) {
	f := newServerFixture(t)
	resp, err := f.Manifester.Build(context.Background(), f.heartbeat(f.TargetHash))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if resp.UpdateAvailable {
		t.Fatalf("expected UpdateAvailable=false")
	}
	if resp.Artifact != testArtifact.String() {
		t.Fatalf("Artifact = %q, want %q", resp.Artifact, testArtifact)
	}
}

func TestManifester_UnknownArtifact(t *testing.T) {
	f := newServerFixture(t)
	_, err := f.Manifester.Build(context.Background(), &protocol.Heartbeat{
		DeviceID:    "dev-1",
		VersionHash: f.OldHash,
		Artifact:    "no-such-thing/linux/arm64",
	})
	if err == nil {
		t.Fatalf("expected an error for an unregistered artifact")
	}
	if !IsArtifactNotFound(err) {
		t.Fatalf("error should be classified as artifact-not-found, got %v", err)
	}
}

// An empty Artifact must resolve to the server's default, which is what
// keeps single-artifact deployments and pre-multi-artifact agents working.
func TestManifester_EmptyArtifactResolvesToDefault(t *testing.T) {
	f := newServerFixture(t)
	resp, err := f.Manifester.Build(context.Background(), &protocol.Heartbeat{
		DeviceID:    "dev-1",
		VersionHash: f.TargetHash,
	})
	if err != nil {
		t.Fatalf("Build with empty artifact: %v", err)
	}
	if resp.Artifact != testArtifact.String() {
		t.Fatalf("Artifact = %q, want the default %q", resp.Artifact, testArtifact)
	}
}

func TestManifester_DeltaCached_SignedResponse(t *testing.T) {
	f := newServerFixture(t)
	// Pre-generate the delta so we hit the cached path.
	if _, err := f.Store.EnsureDelta(context.Background(), f.OldHash, f.TargetHash); err != nil {
		t.Fatalf("EnsureDelta: %v", err)
	}

	resp, err := f.Manifester.Build(context.Background(), f.heartbeat(f.OldHash))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !resp.UpdateAvailable {
		t.Fatalf("expected UpdateAvailable=true")
	}
	if resp.RetryAfter != 0 {
		t.Fatalf("cached delta should not set RetryAfter")
	}
	if resp.TargetHash != f.TargetHash {
		t.Fatalf("unexpected TargetHash")
	}
	if resp.BinaryEndpoint != "" {
		t.Fatalf("delta mode must not set BinaryEndpoint")
	}
	if resp.DeltaEndpoint != protocol.DeltaPath(f.OldHash, f.TargetHash) {
		t.Fatalf("unexpected DeltaEndpoint %s", resp.DeltaEndpoint)
	}
	if resp.TargetSize != int64(len(f.TargetBin)) {
		t.Fatalf("TargetSize = %d, want %d", resp.TargetSize, len(f.TargetBin))
	}
	if resp.DeltaSize <= 0 || resp.DeltaHash == "" || resp.Signature == "" {
		t.Fatalf("missing delta metadata or signature")
	}
	wantChunks := int((resp.DeltaSize + int64(resp.ChunkSize) - 1) / int64(resp.ChunkSize))
	if resp.TotalChunks != wantChunks {
		t.Fatalf("TotalChunks=%d, want %d", resp.TotalChunks, wantChunks)
	}
	assertSignature(t, f, resp)
}

// The whole point of the full-download fallback: a device whose current
// binary the server has never seen still gets a signed, actionable manifest
// instead of "no update available".
func TestManifester_UnknownSource_FallsBackToFullDownload(t *testing.T) {
	f := newServerFixture(t)
	unknown := "ab" + repeatHex(62)

	resp, err := f.Manifester.Build(context.Background(), f.heartbeat(unknown))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !resp.UpdateAvailable {
		t.Fatalf("unknown source must still yield an update via full download")
	}
	if resp.DeltaEndpoint != "" {
		t.Fatalf("full mode must not set DeltaEndpoint")
	}
	if resp.BinaryEndpoint != protocol.BinaryPath(f.TargetHash) {
		t.Fatalf("BinaryEndpoint = %q, want %q",
			resp.BinaryEndpoint, protocol.BinaryPath(f.TargetHash))
	}
	assertSignature(t, f, resp)

	// DeltaHash must describe the bytes actually transferred — the
	// zstd-compressed target — which is what makes one signature scheme
	// cover both modes.
	packed, found, err := f.Store.GetBinaryBytes(context.Background(), f.TargetHash)
	if err != nil || !found {
		t.Fatalf("GetBinaryBytes: err=%v found=%v", err, found)
	}
	if resp.DeltaSize != int64(len(packed)) {
		t.Fatalf("DeltaSize = %d, want the compressed size %d", resp.DeltaSize, len(packed))
	}
	// And those bytes must decompress back to the exact target.
	raw, err := compression.DecompressBytes(packed)
	if err != nil {
		t.Fatalf("DecompressBytes: %v", err)
	}
	if sha256HexOf(raw) != f.TargetHash {
		t.Fatalf("decompressed full transfer does not hash to TargetHash")
	}
}

func TestManifester_UnknownSource_FullDownloadDisabled(t *testing.T) {
	f := newServerFixture(t, func(c *ManifesterConfig) { c.AllowFullDownload = false })
	resp, err := f.Manifester.Build(context.Background(), f.heartbeat("cd"+repeatHex(62)))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if resp.UpdateAvailable {
		t.Fatalf("with full download disabled an unknown source must yield no update")
	}
}

func TestManifester_DeltaMissing_DispatchesAsync(t *testing.T) {
	f := newServerFixture(t)
	// The delta is NOT pre-generated: we expect the RetryAfter path.
	resp, err := f.Manifester.Build(context.Background(), f.heartbeat(f.OldHash))
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

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if f.Store.HasDelta(f.OldHash, f.TargetHash) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("async delta generation did not complete within deadline")
}

// A republish with identical bytes but a new version label must not keep
// serving the stale TargetVersion out of the manifest cache.
func TestManifester_VersionOnlyRepublish_InvalidatesCache(t *testing.T) {
	f := newServerFixture(t)
	f.Registry.opts.OnChange = f.Manifester.InvalidateArtifact

	if _, err := f.Store.EnsureDelta(context.Background(), f.OldHash, f.TargetHash); err != nil {
		t.Fatalf("EnsureDelta: %v", err)
	}
	first, err := f.Manifester.Build(context.Background(), f.heartbeat(f.OldHash))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if first.TargetVersion != "1.1.0" {
		t.Fatalf("TargetVersion = %q, want 1.1.0", first.TargetVersion)
	}

	// Same bytes, new label.
	if _, err := f.Registry.PublishBytes(testArtifact, "1.2.0", f.TargetBin); err != nil {
		t.Fatalf("republish: %v", err)
	}
	second, err := f.Manifester.Build(context.Background(), f.heartbeat(f.OldHash))
	if err != nil {
		t.Fatalf("Build after republish: %v", err)
	}
	if second.TargetVersion != "1.2.0" {
		t.Fatalf("TargetVersion = %q after republish, want 1.2.0 (stale cache entry served)",
			second.TargetVersion)
	}
}

// Two artifacts publishing the SAME bytes must not collide in the manifest
// cache: their responses differ in Artifact and TargetVersion.
func TestManifester_SharedBytesAcrossArtifacts_NoCacheCollision(t *testing.T) {
	f := newServerFixture(t)
	other := protocol.ArtifactKey{Name: "app", OS: "linux", Arch: "arm64"}
	if _, err := f.Registry.PublishBytes(other, "9.9.9", f.TargetBin); err != nil {
		t.Fatalf("publish second artifact: %v", err)
	}
	if _, err := f.Store.EnsureDelta(context.Background(), f.OldHash, f.TargetHash); err != nil {
		t.Fatalf("EnsureDelta: %v", err)
	}

	a, err := f.Manifester.Build(context.Background(), f.heartbeat(f.OldHash))
	if err != nil {
		t.Fatalf("Build(amd64): %v", err)
	}
	b, err := f.Manifester.Build(context.Background(), &protocol.Heartbeat{
		DeviceID: "dev-2", VersionHash: f.OldHash, Artifact: other.String(),
	})
	if err != nil {
		t.Fatalf("Build(arm64): %v", err)
	}
	if a.TargetVersion == b.TargetVersion {
		t.Fatalf("both artifacts returned version %q; cache keys collided", a.TargetVersion)
	}
	if a.Artifact == b.Artifact {
		t.Fatalf("both responses claim artifact %q", a.Artifact)
	}
}

func assertSignature(t *testing.T, f *serverFixture, resp *protocol.ManifestResponse) {
	t.Helper()
	payload, err := protocol.ManifestSigningPayload(resp.TargetHash, resp.DeltaHash)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	sig, err := hex.DecodeString(resp.Signature)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if err := crypto.Verify(f.Pub, payload, sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
}

// repeatHex builds a filler hex string so tests can construct syntactically
// valid but unregistered 64-char hashes.
func repeatHex(n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = '0'
	}
	return string(out)
}
