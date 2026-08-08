package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// retentionFixture builds a store+registry with one artifact and a helper to
// run sweeps.
type retentionFixture struct {
	Store    *Store
	Registry *Registry
	Opts     RetentionOptions
}

func newRetentionFixture(t *testing.T, opts ...func(*RetentionOptions)) *retentionFixture {
	t.Helper()
	s := newTestStore(t)
	r := newTestRegistry(t, s)
	o := RetentionOptions{}
	for _, fn := range opts {
		fn(&o)
	}
	return &retentionFixture{Store: s, Registry: r, Opts: o}
}

func (f *retentionFixture) sweep(t *testing.T) RetentionStats {
	t.Helper()
	ret := NewRetention(f.Store, f.Registry, f.Opts, testLogger())
	stats, err := ret.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	return stats
}

// The workhorse rule: once an artifact's target moves, every delta pointing
// at the old target is unreachable and must go.
func TestRetention_DeletesDeltasTowardSupersededTargets(t *testing.T) {
	f := newRetentionFixture(t)
	// The realistic lifecycle: v1 ships, v2 supersedes it, and the server
	// builds the v1→v2 delta. v1 is now a history entry, which is what keeps
	// the delta alive.
	binV1, binV2 := similarBinaries(16<<10, 3)
	v1, err := f.Registry.PublishBytes(testArtifact, "1", binV1)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := f.Registry.PublishBytes(testArtifact, "2", binV2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.EnsureDelta(context.Background(), v1.TargetHash, v2.TargetHash); err != nil {
		t.Fatal(err)
	}
	stalePath := f.Store.DeltaPath(v1.TargetHash, v2.TargetHash)
	if !fileExists(stalePath) {
		t.Fatalf("precondition: delta should exist")
	}

	// Sweeping now must NOT delete it: it still points at the live target.
	if stats := f.sweep(t); stats.DeltasDeleted != 0 {
		t.Fatalf("a delta toward the current target was deleted (%d)", stats.DeltasDeleted)
	}

	// Publish v3; every delta aimed at v2 becomes unreachable, because
	// agents only ever request deltas toward the CURRENT target.
	if _, err := f.Registry.PublishBytes(testArtifact, "3", []byte("a-completely-new-target")); err != nil {
		t.Fatal(err)
	}
	stats := f.sweep(t)
	if stats.DeltasDeleted != 1 {
		t.Fatalf("DeltasDeleted = %d, want 1", stats.DeltasDeleted)
	}
	if fileExists(stalePath) {
		t.Fatalf("stale delta still on disk")
	}
	if stats.BytesReclaimed <= 0 {
		t.Fatalf("BytesReclaimed = %d, want > 0", stats.BytesReclaimed)
	}
}

// Compressed whole binaries are pure cache too, keyed by the binary they
// wrap. Once that binary leaves the live set they are collectable.
func TestRetention_DeletesFullsForUnreferencedBinaries(t *testing.T) {
	f := newRetentionFixture(t)
	art, err := f.Registry.PublishBytes(testArtifact, "1", []byte("target-payload"))
	if err != nil {
		t.Fatal(err)
	}
	// Materialize the compressed form.
	if _, found, err := f.Store.GetBinaryBytes(context.Background(), art.TargetHash); err != nil || !found {
		t.Fatalf("GetBinaryBytes: err=%v found=%v", err, found)
	}
	fullPath := f.Store.FullPath(art.TargetHash)
	if !fileExists(fullPath) {
		t.Fatalf("precondition: compressed binary should be cached on disk")
	}

	// While the artifact references it, it survives.
	if stats := f.sweep(t); stats.FullsDeleted != 0 {
		t.Fatalf("a live binary's compressed form was deleted")
	}

	// Remove the artifact entirely → nothing references those bytes.
	if err := f.Registry.Remove(testArtifact); err != nil {
		t.Fatal(err)
	}
	stats := f.sweep(t)
	if stats.FullsDeleted != 1 {
		t.Fatalf("FullsDeleted = %d, want 1", stats.FullsDeleted)
	}
	if fileExists(fullPath) {
		t.Fatalf("orphaned compressed binary still on disk")
	}
}

// A source binary that aged out of every artifact's history means devices on
// that version now take the full-download path — their deltas are dead.
// A delta whose SOURCE is not in any artifact's history is dead weight: no
// device can be running bytes the server never published, and if one is, it
// takes the full-download path anyway.
func TestRetention_DeletesDeltasFromUnreferencedSources(t *testing.T) {
	f := newRetentionFixture(t)

	base := []byte("base-version-payload-aaaaaaaaaaaa")
	baseHash, err := f.Store.RegisterBinary(base)
	if err != nil {
		t.Fatal(err)
	}
	target, err := f.Registry.PublishBytes(testArtifact, "1", []byte("target-payload-bbbbbbbbbbbbbbbb"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.EnsureDelta(context.Background(), baseHash, target.TargetHash); err != nil {
		t.Fatal(err)
	}
	deltaPath := f.Store.DeltaPath(baseHash, target.TargetHash)

	// baseHash was never an artifact target, so it is not in the live set at
	// all: the delta is collectable immediately.
	stats := f.sweep(t)
	if stats.DeltasDeleted != 1 {
		t.Fatalf("DeltasDeleted = %d, want 1 (source not in any artifact history)", stats.DeltasDeleted)
	}
	if fileExists(deltaPath) {
		t.Fatalf("delta from an unreferenced source still on disk")
	}
}

func TestRetention_DeltaMaxAge(t *testing.T) {
	f := newRetentionFixture(t, func(o *RetentionOptions) {
		o.DeltaMaxAge = time.Hour
	})
	oldBin, newBin := similarBinaries(8<<10, 11)
	oldHash, _ := f.Store.RegisterBinary(oldBin)
	art, err := f.Registry.PublishBytes(testArtifact, "1", newBin)
	if err != nil {
		t.Fatal(err)
	}
	// Make oldHash a legitimate history entry so the "unreferenced source"
	// rule doesn't fire and steal the assertion.
	f.Registry.mu.Lock()
	f.Registry.artifacts[testArtifact.String()].History = []string{oldHash}
	f.Registry.mu.Unlock()

	if _, err := f.Store.EnsureDelta(context.Background(), oldHash, art.TargetHash); err != nil {
		t.Fatal(err)
	}
	p := f.Store.DeltaPath(oldHash, art.TargetHash)

	// Fresh: survives.
	if stats := f.sweep(t); stats.DeltasDeleted != 0 {
		t.Fatalf("a fresh delta was deleted by the age rule")
	}
	// Backdate well past the limit.
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if stats := f.sweep(t); stats.DeltasDeleted != 1 {
		t.Fatalf("DeltasDeleted = %d, want 1 (past delta_max_age)", stats.DeltasDeleted)
	}
}

func TestRetention_ByteCapEvictsOldestFirst(t *testing.T) {
	f := newRetentionFixture(t)
	art, err := f.Registry.PublishBytes(testArtifact, "1", []byte("current-target"))
	if err != nil {
		t.Fatal(err)
	}
	// Hand-place three delta files toward the live target so only the byte
	// cap can decide their fate.
	var paths []string
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 3; i++ {
		src := sha256HexOf([]byte{byte(i)})
		p := f.Store.DeltaPath(src, art.TargetHash)
		if err := os.WriteFile(p, make([]byte, 1000), 0o644); err != nil {
			t.Fatal(err)
		}
		// Also register the source so the "unreferenced source" rule would
		// otherwise not apply... it still would, so instead put them in the
		// artifact history.
		f.Registry.mu.Lock()
		a := f.Registry.artifacts[testArtifact.String()]
		a.History = append(a.History, src)
		f.Registry.mu.Unlock()

		mt := base.Add(time.Duration(i) * time.Minute) // ascending age
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}

	f.Opts.DeltasMaxTotalBytes = 2500 // fits two of the three 1000-byte files
	stats := f.sweep(t)
	if stats.DeltasDeleted != 1 {
		t.Fatalf("DeltasDeleted = %d, want 1 to get under the cap", stats.DeltasDeleted)
	}
	if fileExists(paths[0]) {
		t.Fatalf("the oldest delta should have been evicted first")
	}
	if !fileExists(paths[1]) || !fileExists(paths[2]) {
		t.Fatalf("newer deltas were evicted unnecessarily")
	}
}

// Binaries are the only non-reconstructible thing in the store, so orphan
// collection is opt-in and gated by a grace period.
func TestRetention_OrphanBinariesRequireOptIn(t *testing.T) {
	f := newRetentionFixture(t)
	orphan := []byte("nobody-references-me")
	hash, err := f.Store.RegisterBinary(orphan)
	if err != nil {
		t.Fatal(err)
	}
	backdate(t, f.Store.binaryPath(hash), 48*time.Hour)

	// Default: disabled.
	if stats := f.sweep(t); stats.BinariesDeleted != 0 {
		t.Fatalf("orphan binaries deleted without opt-in (%d)", stats.BinariesDeleted)
	}
	if !f.Store.HasBinary(hash) {
		t.Fatalf("binary removed while collection was disabled")
	}

	f.Opts.CollectOrphanBinaries = true
	stats := f.sweep(t)
	if stats.BinariesDeleted != 1 {
		t.Fatalf("BinariesDeleted = %d, want 1", stats.BinariesDeleted)
	}
}

func TestRetention_OrphanBinaryGracePeriod(t *testing.T) {
	f := newRetentionFixture(t, func(o *RetentionOptions) {
		o.CollectOrphanBinaries = true
		o.OrphanBinaryMinAge = time.Hour
	})
	// A binary written seconds ago is very likely one publish away from
	// becoming a target; the grace period exists exactly for that window.
	hash, err := f.Store.RegisterBinary([]byte("just-uploaded"))
	if err != nil {
		t.Fatal(err)
	}
	if stats := f.sweep(t); stats.BinariesDeleted != 0 {
		t.Fatalf("a freshly written binary was collected inside the grace period")
	}
	if !f.Store.HasBinary(hash) {
		t.Fatalf("fresh binary deleted")
	}

	backdate(t, f.Store.binaryPath(hash), 2*time.Hour)
	if stats := f.sweep(t); stats.BinariesDeleted != 1 {
		t.Fatalf("BinariesDeleted = %d, want 1 after the grace period", stats.BinariesDeleted)
	}
}

func TestRetention_NeverDeletesLiveBinaries(t *testing.T) {
	f := newRetentionFixture(t, func(o *RetentionOptions) {
		o.CollectOrphanBinaries = true
		o.OrphanBinaryMinAge = time.Nanosecond // no grace at all
	})
	v1, err := f.Registry.PublishBytes(testArtifact, "1", []byte("version-one-payload"))
	if err != nil {
		t.Fatal(err)
	}
	v2, err := f.Registry.PublishBytes(testArtifact, "2", []byte("version-two-payload"))
	if err != nil {
		t.Fatal(err)
	}
	backdate(t, f.Store.binaryPath(v1.TargetHash), time.Hour)
	backdate(t, f.Store.binaryPath(v2.TargetHash), time.Hour)

	f.sweep(t)
	if !f.Store.HasBinary(v2.TargetHash) {
		t.Fatalf("the CURRENT target was collected")
	}
	if !f.Store.HasBinary(v1.TargetHash) {
		t.Fatalf("a target still in the artifact history was collected")
	}
}

// The sweeper owns only files it can parse. Anything else — an operator's
// note, a future format, a stray temp file — is left alone.
func TestRetention_LeavesUnrecognizedFilesAlone(t *testing.T) {
	f := newRetentionFixture(t, func(o *RetentionOptions) {
		o.CollectOrphanBinaries = true
		o.OrphanBinaryMinAge = time.Nanosecond
		o.DeltaMaxAge = time.Nanosecond
	})
	strays := map[string]string{
		filepath.Join(f.Store.opts.DeltasDir, "README.txt"):          "notes",
		filepath.Join(f.Store.opts.DeltasDir, "shortname.delta.zst"): "bad hash segments",
		filepath.Join(f.Store.opts.BinariesDir, "notes.md"):          "notes",
		filepath.Join(f.Store.opts.BinariesDir, "zzz.bin"):           "non-hex basename",
	}
	for p := range strays {
		if err := os.WriteFile(p, []byte("keep me"), 0o644); err != nil {
			t.Fatal(err)
		}
		backdate(t, p, 72*time.Hour)
	}
	f.sweep(t)
	for p, why := range strays {
		if !fileExists(p) {
			t.Fatalf("sweeper deleted a file it does not own (%s): %s", why, filepath.Base(p))
		}
	}
}

func TestRetention_SweepIsIdempotent(t *testing.T) {
	f := newRetentionFixture(t)
	oldBin, newBin := similarBinaries(8<<10, 5)
	oldHash, _ := f.Store.RegisterBinary(oldBin)
	art, err := f.Registry.PublishBytes(testArtifact, "1", newBin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.EnsureDelta(context.Background(), oldHash, art.TargetHash); err != nil {
		t.Fatal(err)
	}
	first := f.sweep(t)
	second := f.sweep(t)
	if second.DeltasDeleted != 0 || second.BinariesDeleted != 0 || second.FullsDeleted != 0 {
		t.Fatalf("second sweep deleted more (%+v) after the first (%+v)", second, first)
	}
}

func TestParseTransferName(t *testing.T) {
	h1 := sha256HexOf([]byte("a"))
	h2 := sha256HexOf([]byte("b"))
	cases := []struct {
		in       string
		kind     string
		from, to string
		ok       bool
	}{
		{h1 + "_" + h2 + ".delta.zst", "delta", h1, h2, true},
		{h1 + ".full.zst", "full", h1, "", true},
		{"README", "", "", "", false},
		{"nothex_" + h2 + ".delta.zst", "", "", "", false},
		{h1 + "_nothex.delta.zst", "", "", "", false},
		{"nothex.full.zst", "", "", "", false},
		{h1 + ".delta.zst", "", "", "", false}, // no separator
		{"../../etc/passwd.full.zst", "", "", "", false},
	}
	for _, tc := range cases {
		kind, from, to, ok := parseTransferName(tc.in)
		if ok != tc.ok || kind != tc.kind || from != tc.from || to != tc.to {
			t.Errorf("parseTransferName(%q) = (%q,%q,%q,%v), want (%q,%q,%q,%v)",
				tc.in, kind, from, to, ok, tc.kind, tc.from, tc.to, tc.ok)
		}
	}
}

func TestRetention_RunStopsOnContextCancel(t *testing.T) {
	f := newRetentionFixture(t, func(o *RetentionOptions) { o.Interval = time.Hour })
	ret := NewRetention(f.Store, f.Registry, f.Opts, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ret.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("Run should return the context error")
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Run did not return after context cancellation")
	}
}

// backdate rewinds a file's mtime so age-based rules can be exercised without
// sleeping.
func backdate(t *testing.T, path string, by time.Duration) {
	t.Helper()
	ts := time.Now().Add(-by)
	if err := os.Chtimes(path, ts, ts); err != nil {
		t.Fatalf("Chtimes(%s): %v", path, err)
	}
}
