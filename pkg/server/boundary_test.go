package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carlosprados/ota-updater/pkg/protocol"
)

// Regression for the review finding: an unauthenticated heartbeat used to
// carry a path-traversal string in VersionHash all the way to the filesystem.
// With a plausible `*.bin` reachable above the store, HasBinary returned true,
// Build took the DELTA branch, and the sweeper wrote a delta outside
// deltas_dir — after running bsdiff, which peaks near 20x the input, against
// a file of the attacker's choosing.
func TestBuild_TraversalVersionHashNeverReachesDisk(t *testing.T) {
	f := newServerFixture(t)

	// Plant the file the traversal would name, one level above the store.
	outside := filepath.Dir(f.Store.opts.BinariesDir)
	if err := os.WriteFile(filepath.Join(outside, "secret.bin"),
		[]byte("content that must never leave the host"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, hash := range []string{
		"../secret",
		"../../etc/passwd",
		strings.Repeat("a", 63),        // too short
		strings.Repeat("a", 65),        // too long
		strings.Repeat("A", 64),        // uppercase hex
		"zz" + strings.Repeat("a", 62), // non-hex
	} {
		t.Run(hash, func(t *testing.T) {
			resp, err := f.Manifester.Build(context.Background(), &protocol.Heartbeat{
				DeviceID:    "probe",
				VersionHash: hash,
				Artifact:    testArtifact.String(),
			})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			// The device is NOT stranded: it still gets an actionable update,
			// via the full-download path that needs no source binary.
			if !resp.UpdateAvailable || resp.BinaryEndpoint == "" {
				t.Fatalf("an unusable version hash must still yield a full download, got %+v", resp)
			}
			if resp.DeltaEndpoint != "" {
				t.Fatalf("delta branch taken for an unusable version hash: %q", resp.DeltaEndpoint)
			}
			// And nothing was written outside the store.
			stray := filepath.Join(outside, hash+"_"+f.TargetHash+".delta.zst")
			if _, err := os.Stat(stray); err == nil {
				t.Fatalf("a delta was written outside deltas_dir: %s", stray)
			}
		})
	}

	// Give any (incorrectly) dispatched async generation a chance to land
	// before asserting the store stayed clean.
	time.Sleep(200 * time.Millisecond)
	entries, err := os.ReadDir(f.Store.opts.DeltasDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "secret") || strings.Contains(e.Name(), "passwd") {
			t.Fatalf("attacker-named artifact in the store: %s", e.Name())
		}
	}
}

// The cache key for an unknown source must not be attacker-controlled: every
// unknown source shares one entry, because the full-download response depends
// only on the target.
func TestBuild_UnknownSourcesShareOneCacheEntry(t *testing.T) {
	f := newServerFixture(t)
	before := f.Manifester.cache.Len()

	var first *protocol.ManifestResponse
	for i := 0; i < 50; i++ {
		hash := hashOfIndex(i)
		resp, err := f.Manifester.Build(context.Background(), f.heartbeat(hash))
		if err != nil {
			t.Fatalf("Build(%d): %v", i, err)
		}
		if resp.BinaryEndpoint == "" {
			t.Fatalf("expected the full-download branch for an unknown source")
		}
		if first == nil {
			first = resp
		} else if resp != first {
			t.Fatalf("request %d rebuilt the manifest instead of reusing the shared entry", i)
		}
	}

	if got := f.Manifester.cache.Len() - before; got != 1 {
		t.Fatalf("50 distinct unknown sources created %d cache entries, want 1", got)
	}
}

// Known sources must still be memoized per (artifact, from, to) — collapsing
// them would serve the wrong delta endpoint.
func TestBuild_KnownSourceKeepsItsOwnCacheEntry(t *testing.T) {
	f := newServerFixture(t)
	if _, err := f.Store.EnsureDelta(context.Background(), f.OldHash, f.TargetHash); err != nil {
		t.Fatal(err)
	}
	resp, err := f.Manifester.Build(context.Background(), f.heartbeat(f.OldHash))
	if err != nil {
		t.Fatal(err)
	}
	if resp.DeltaEndpoint != protocol.DeltaPath(f.OldHash, f.TargetHash) {
		t.Fatalf("known source lost its own delta endpoint: %q", resp.DeltaEndpoint)
	}
	// An unknown source alongside it must not clobber that entry.
	if _, err := f.Manifester.Build(context.Background(), f.heartbeat(hashOfIndex(999))); err != nil {
		t.Fatal(err)
	}
	again, err := f.Manifester.Build(context.Background(), f.heartbeat(f.OldHash))
	if err != nil {
		t.Fatal(err)
	}
	if again != resp {
		t.Fatalf("the known source's cache entry was evicted by an unknown one")
	}
}

func TestBuild_RejectsMalformedHeartbeat(t *testing.T) {
	f := newServerFixture(t)
	cases := map[string]*protocol.Heartbeat{
		"no device id":       {VersionHash: f.OldHash},
		"device id too long": {DeviceID: strings.Repeat("d", protocol.MaxDeviceIDLen+1), VersionHash: f.OldHash},
		"newline in id":      {DeviceID: "dev\nfake=1", VersionHash: f.OldHash},
		"version too long":   {DeviceID: "d", Version: strings.Repeat("v", protocol.MaxVersionLen+1)},
		"bad artifact":       {DeviceID: "d", Artifact: "bad name/linux/arm64"},
	}
	for name, hb := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := f.Manifester.Build(context.Background(), hb)
			if err == nil {
				t.Fatalf("expected rejection")
			}
			if !errors.Is(err, protocol.ErrInvalidMessage) {
				t.Fatalf("err = %v, want ErrInvalidMessage so transports can answer 400", err)
			}
		})
	}
}

// hashOfIndex builds a distinct, syntactically valid, unregistered hash.
func hashOfIndex(i int) string {
	return sha256HexOf([]byte{byte(i), byte(i >> 8), 0xAB})
}
