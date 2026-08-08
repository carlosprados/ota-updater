package server

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/carlosprados/ota-updater/pkg/protocol"
)

func TestRegistry_PublishBytes_SetsTargetAndDefault(t *testing.T) {
	s := newTestStore(t)
	r := newTestRegistry(t, s)

	art, err := r.PublishBytes(testArtifact, "1.0.0", []byte("payload-v1"))
	if err != nil {
		t.Fatalf("PublishBytes: %v", err)
	}
	if art.TargetHash != sha256HexOf([]byte("payload-v1")) {
		t.Fatalf("target hash does not address the published bytes")
	}
	if art.TargetSize != int64(len("payload-v1")) {
		t.Fatalf("TargetSize = %d", art.TargetSize)
	}
	// The bytes must be in the store, not just recorded in the registry.
	if !s.HasBinary(art.TargetHash) {
		t.Fatalf("published bytes were not registered in the store")
	}
	// First publish becomes the implicit default, so single-artifact
	// deployments need no extra configuration.
	if r.Default() != testArtifact.String() {
		t.Fatalf("Default() = %q, want %q", r.Default(), testArtifact)
	}
}

func TestRegistry_Publish_RecordsHistoryNewestFirst(t *testing.T) {
	s := newTestStore(t)
	r := newTestRegistry(t, s)

	var hashes []string
	for _, v := range []string{"v1", "v2", "v3"} {
		art, err := r.PublishBytes(testArtifact, v, []byte("payload-"+v))
		if err != nil {
			t.Fatalf("publish %s: %v", v, err)
		}
		hashes = append(hashes, art.TargetHash)
	}
	art, err := r.Get(testArtifact)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if art.TargetHash != hashes[2] {
		t.Fatalf("current target is not the last published")
	}
	if len(art.History) != 2 {
		t.Fatalf("History = %v, want 2 entries", art.History)
	}
	if art.History[0] != hashes[1] || art.History[1] != hashes[0] {
		t.Fatalf("History %v is not newest-first", art.History)
	}
}

func TestRegistry_Publish_HistoryCappedAtDepth(t *testing.T) {
	s := newTestStore(t)
	r := newTestRegistry(t, s, func(o *RegistryOptions) { o.HistoryDepth = 3 })

	for i := 0; i < 10; i++ {
		if _, err := r.PublishBytes(testArtifact, "v", []byte{byte(i)}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	art, _ := r.Get(testArtifact)
	if len(art.History) != 3 {
		t.Fatalf("History length = %d, want the configured depth 3", len(art.History))
	}
}

// Publishing identical bytes must not push a duplicate onto the history, or
// a CI job that redeploys the same build would evict the real ancestors.
func TestRegistry_Publish_IdenticalBytesDoNotGrowHistory(t *testing.T) {
	s := newTestStore(t)
	r := newTestRegistry(t, s)

	if _, err := r.PublishBytes(testArtifact, "v1", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PublishBytes(testArtifact, "v2", []byte("b")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := r.PublishBytes(testArtifact, "v2", []byte("b")); err != nil {
			t.Fatal(err)
		}
	}
	art, _ := r.Get(testArtifact)
	if len(art.History) != 1 {
		t.Fatalf("History = %v, want exactly 1 entry after repeated identical publishes", art.History)
	}
}

func TestRegistry_OnChange_FiresOnHashAndVersionChanges(t *testing.T) {
	s := newTestStore(t)
	var fired int
	r := newTestRegistry(t, s, func(o *RegistryOptions) {
		o.OnChange = func(protocol.ArtifactKey) { fired++ }
	})

	if _, err := r.PublishBytes(testArtifact, "v1", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if fired != 1 {
		t.Fatalf("initial publish fired %d times, want 1", fired)
	}
	// Identical bytes AND identical version: nothing observable changed.
	if _, err := r.PublishBytes(testArtifact, "v1", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if fired != 1 {
		t.Fatalf("no-op republish fired OnChange (%d)", fired)
	}
	// Same bytes, new label: the manifest carries TargetVersion, so caches
	// still have to be invalidated.
	if _, err := r.PublishBytes(testArtifact, "v2", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if fired != 2 {
		t.Fatalf("version-only republish did not fire OnChange (%d)", fired)
	}
}

func TestRegistry_Resolve(t *testing.T) {
	s := newTestStore(t)
	r := newTestRegistry(t, s)
	other := protocol.ArtifactKey{Name: "other", OS: "linux", Arch: "arm64"}
	if _, err := r.PublishBytes(testArtifact, "1.0.0", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PublishBytes(other, "2.0.0", []byte("b")); err != nil {
		t.Fatal(err)
	}

	// Empty name → default (the first published).
	art, err := r.Resolve("")
	if err != nil {
		t.Fatalf("Resolve(\"\"): %v", err)
	}
	if art.Key != testArtifact {
		t.Fatalf("default resolved to %v", art.Key)
	}
	// Explicit name → that artifact.
	art, err = r.Resolve(other.String())
	if err != nil {
		t.Fatalf("Resolve(other): %v", err)
	}
	if art.Version != "2.0.0" {
		t.Fatalf("resolved wrong artifact: %+v", art)
	}
	// Unknown name → classified error so transports can answer 404.
	if _, err := r.Resolve("ghost/linux/arm64"); !IsArtifactNotFound(err) {
		t.Fatalf("unknown artifact error = %v, want artifact-not-found", err)
	}
	// Malformed name → a validation error, never a silent default.
	if _, err := r.Resolve("bad name"); err == nil {
		t.Fatalf("malformed artifact name should error")
	}
}

func TestRegistry_Resolve_NoDefaultConfigured(t *testing.T) {
	s := newTestStore(t)
	r := newTestRegistry(t, s)
	a := protocol.ArtifactKey{Name: "a"}
	b := protocol.ArtifactKey{Name: "b"}
	if _, err := r.PublishBytes(a, "1", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PublishBytes(b, "1", []byte("b")); err != nil {
		t.Fatal(err)
	}
	// "a" became the implicit default on first publish; drop it and the
	// remaining single artifact takes over.
	if err := r.Remove(a); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	art, err := r.Resolve("")
	if err != nil {
		t.Fatalf("Resolve after removing the default: %v", err)
	}
	if art.Key != b {
		t.Fatalf("sole survivor should become the default, got %v", art.Key)
	}
}

func TestRegistry_SetDefault_RejectsUnknown(t *testing.T) {
	s := newTestStore(t)
	r := newTestRegistry(t, s)
	err := r.SetDefault(protocol.ArtifactKey{Name: "ghost"})
	if !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("SetDefault error = %v, want ErrArtifactNotFound", err)
	}
}

// Persistence is what makes the admin API usable: without it, every artifact
// published through the API disappears on the next restart.
func TestRegistry_PersistsAndRestores(t *testing.T) {
	s := newTestStore(t)
	statePath := filepath.Join(t.TempDir(), "artifacts.json")

	r1, err := NewRegistry(s, RegistryOptions{StatePath: statePath}, testLogger())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if _, err := r1.PublishBytes(testArtifact, "1.0.0", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	art1, err := r1.PublishBytes(testArtifact, "2.0.0", []byte("v2"))
	if err != nil {
		t.Fatal(err)
	}

	// A brand-new registry over the same state file and store.
	r2, err := NewRegistry(s, RegistryOptions{StatePath: statePath}, testLogger())
	if err != nil {
		t.Fatalf("NewRegistry (restore): %v", err)
	}
	art2, err := r2.Get(testArtifact)
	if err != nil {
		t.Fatalf("Get after restore: %v", err)
	}
	if art2.TargetHash != art1.TargetHash || art2.Version != "2.0.0" {
		t.Fatalf("restored artifact differs: %+v vs %+v", art2, art1)
	}
	if len(art2.History) != 1 {
		t.Fatalf("history not restored: %v", art2.History)
	}
	if r2.Default() != testArtifact.String() {
		t.Fatalf("default not restored, got %q", r2.Default())
	}
}

// A corrupt state file must stop the boot. Starting with an empty registry
// would tell the whole fleet "no update available", which is a much worse
// failure than refusing to start.
func TestRegistry_CorruptStateIsFatal(t *testing.T) {
	s := newTestStore(t)
	statePath := filepath.Join(t.TempDir(), "artifacts.json")
	if err := os.WriteFile(statePath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRegistry(s, RegistryOptions{StatePath: statePath}, testLogger()); err == nil {
		t.Fatalf("NewRegistry should refuse to start on a corrupt state file")
	}
}

func TestRegistry_UnsupportedStateVersionIsFatal(t *testing.T) {
	s := newTestStore(t)
	statePath := filepath.Join(t.TempDir(), "artifacts.json")
	if err := os.WriteFile(statePath, []byte(`{"version":999,"artifacts":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRegistry(s, RegistryOptions{StatePath: statePath}, testLogger()); err == nil {
		t.Fatalf("NewRegistry should refuse an unknown state version")
	}
}

func TestRegistry_MissingStateFileIsFirstBoot(t *testing.T) {
	s := newTestStore(t)
	r, err := NewRegistry(s, RegistryOptions{
		StatePath: filepath.Join(t.TempDir(), "does-not-exist.json"),
	}, testLogger())
	if err != nil {
		t.Fatalf("a missing state file is a first boot, not an error: %v", err)
	}
	if len(r.List()) != 0 {
		t.Fatalf("fresh registry should be empty")
	}
}

func TestRegistry_Republish_RequiresSourceFile(t *testing.T) {
	s := newTestStore(t)
	r := newTestRegistry(t, s)
	if _, err := r.PublishBytes(testArtifact, "1.0.0", []byte("bytes-only")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Republish(testArtifact); err == nil {
		t.Fatalf("an API-published artifact has no source file to re-read")
	}

	file := filepath.Join(t.TempDir(), "app.bin")
	if err := os.WriteFile(file, []byte("from-file-v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PublishFile(testArtifact, "1.1.0", file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("from-file-v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	art, err := r.Republish(testArtifact)
	if err != nil {
		t.Fatalf("Republish: %v", err)
	}
	if art.TargetHash != sha256HexOf([]byte("from-file-v2")) {
		t.Fatalf("Republish did not pick up the new file content")
	}
}

func TestRegistry_LiveHashesAndCurrentTargets(t *testing.T) {
	s := newTestStore(t)
	r := newTestRegistry(t, s)
	other := protocol.ArtifactKey{Name: "other"}

	v1, _ := r.PublishBytes(testArtifact, "1", []byte("a1"))
	v2, _ := r.PublishBytes(testArtifact, "2", []byte("a2"))
	o1, _ := r.PublishBytes(other, "1", []byte("b1"))

	current := r.CurrentTargets()
	if _, ok := current[v2.TargetHash]; !ok {
		t.Fatalf("current targets missing the latest publish")
	}
	if _, ok := current[v1.TargetHash]; ok {
		t.Fatalf("a superseded target must not be reported as current")
	}
	if _, ok := current[o1.TargetHash]; !ok {
		t.Fatalf("current targets missing the second artifact")
	}

	live := r.LiveHashes()
	for _, h := range []string{v1.TargetHash, v2.TargetHash, o1.TargetHash} {
		if _, ok := live[h]; !ok {
			t.Fatalf("live hashes missing %s", h)
		}
	}

	if !r.IsCurrentTarget(v2.TargetHash) || r.IsCurrentTarget(v1.TargetHash) {
		t.Fatalf("IsCurrentTarget disagrees with CurrentTargets")
	}
	if !r.IsLive(v1.TargetHash) {
		t.Fatalf("a historical target must still count as live")
	}
	if r.IsLive(sha256HexOf([]byte("never-published"))) {
		t.Fatalf("unpublished bytes must not be live")
	}
}

func TestRegistry_Remove(t *testing.T) {
	s := newTestStore(t)
	r := newTestRegistry(t, s)
	art, _ := r.PublishBytes(testArtifact, "1.0.0", []byte("payload"))

	if err := r.Remove(testArtifact); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := r.Get(testArtifact); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("Get after Remove = %v, want ErrArtifactNotFound", err)
	}
	// The bytes stay in the store until retention collects them, so an
	// accidental removal is cheap to undo.
	if !s.HasBinary(art.TargetHash) {
		t.Fatalf("Remove must not delete bytes from the store")
	}
	if err := r.Remove(testArtifact); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("double Remove = %v, want ErrArtifactNotFound", err)
	}
}

func TestRegistry_WatchedSources(t *testing.T) {
	s := newTestStore(t)
	r := newTestRegistry(t, s)
	file := filepath.Join(t.TempDir(), "watched.bin")
	if err := os.WriteFile(file, []byte("watched"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PublishFile(testArtifact, "1", file); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PublishBytes(protocol.ArtifactKey{Name: "api-only"}, "1", []byte("x")); err != nil {
		t.Fatal(err)
	}

	watched := r.WatchedSources()
	if len(watched) != 1 {
		t.Fatalf("WatchedSources = %v, want only the file-backed artifact", watched)
	}
	if watched[testArtifact] != file {
		t.Fatalf("watched source path = %q, want %q", watched[testArtifact], file)
	}
}

func TestRegistry_PublishRejectsInvalidKey(t *testing.T) {
	s := newTestStore(t)
	r := newTestRegistry(t, s)
	if _, err := r.PublishBytes(protocol.ArtifactKey{Name: ".."}, "1", []byte("x")); err == nil {
		t.Fatalf("a traversal name must be rejected at publish time")
	}
}
