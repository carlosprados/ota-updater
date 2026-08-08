package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/carlosprados/ota-updater/pkg/atomicio"
	"github.com/carlosprados/ota-updater/pkg/protocol"
)

// ErrArtifactNotFound is returned when an operation names an artifact key
// that is not registered.
var ErrArtifactNotFound = errors.New("artifact not found")

// DefaultHistoryDepth is how many superseded target hashes are remembered per
// artifact. History exists for retention: a binary that is still within an
// artifact's history is a plausible delta source for a device that has not
// updated yet, so the sweeper must not delete it. Ten releases of headroom
// covers a fleet where some devices are offline for months.
const DefaultHistoryDepth = 10

// Artifact is the publication state of one track: which bytes are current,
// what they are called, and which bytes they superseded.
//
// Instances handed out by the Registry are deep copies — callers can hold
// and inspect them without racing a concurrent Publish.
type Artifact struct {
	Key        protocol.ArtifactKey `json:"key"`
	Version    string               `json:"version"`
	TargetHash string               `json:"target_hash"`
	TargetSize int64                `json:"target_size"`
	UpdatedAt  time.Time            `json:"updated_at"`
	// Source is where the target came from: a filesystem path for artifacts
	// declared in YAML (which are also watched for changes), or empty for
	// artifacts published through the admin API.
	Source string `json:"source,omitempty"`
	// History holds superseded target hashes, most recent first, capped at
	// the registry's history depth.
	History []string `json:"history,omitempty"`
}

// clone returns a deep copy so callers cannot mutate registry-owned state.
func (a *Artifact) clone() *Artifact {
	cp := *a
	if a.History != nil {
		cp.History = append([]string(nil), a.History...)
	}
	return &cp
}

// RegistryOptions parameterizes NewRegistry.
type RegistryOptions struct {
	// StatePath is the JSON file where the registry persists itself. Without
	// it, everything published through the admin API is lost on restart —
	// which for a server that is the source of truth for a fleet is data
	// loss, not a cache miss. Empty disables persistence (tests).
	StatePath string
	// HistoryDepth caps the per-artifact history. 0 → DefaultHistoryDepth.
	HistoryDepth int
	// DefaultArtifact is the key returned for heartbeats that name no
	// artifact. Empty means "the only artifact, if there is exactly one".
	DefaultArtifact string
	// OnChange is invoked after any mutation that changes an artifact's
	// target, so callers can invalidate derived caches (signed manifests).
	// Called synchronously, outside the registry lock.
	OnChange func(key protocol.ArtifactKey)
	// Metrics, when non-nil, receives artifact gauges.
	Metrics *Metrics
}

// Registry maps artifact keys to their current publication state. It is the
// piece that turns a content-addressed Store into a multi-artifact update
// server: the Store knows bytes, the Registry knows which bytes are current
// for "keystone-agent/linux/arm64" as opposed to "keystone-proxy/linux/amd64".
//
// All methods are safe for concurrent use.
type Registry struct {
	store  *Store
	logger *slog.Logger
	opts   RegistryOptions

	mu         sync.RWMutex
	artifacts  map[string]*Artifact
	defaultKey string
}

// NewRegistry creates a registry backed by store. When opts.StatePath names
// an existing file, previously published artifacts are restored.
func NewRegistry(store *Store, opts RegistryOptions, logger *slog.Logger) (*Registry, error) {
	if store == nil {
		return nil, errors.New("registry: store is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if opts.HistoryDepth <= 0 {
		opts.HistoryDepth = DefaultHistoryDepth
	}
	r := &Registry{
		store:      store,
		logger:     logger,
		opts:       opts,
		artifacts:  make(map[string]*Artifact),
		defaultKey: opts.DefaultArtifact,
	}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

// ---------------------------------------------------------------------------
// Publication
// ---------------------------------------------------------------------------

// PublishBytes makes data the current target of key, registering it in the
// content-addressed store. Returns the resulting artifact state.
//
// Publishing the same bytes twice is a no-op beyond refreshing Version and
// UpdatedAt: the target hash is unchanged, so no cache is invalidated and no
// device sees a spurious update.
func (r *Registry) PublishBytes(key protocol.ArtifactKey, version string, data []byte) (*Artifact, error) {
	if err := key.Validate(); err != nil {
		return nil, fmt.Errorf("publish: %w", err)
	}
	hash, err := r.store.RegisterBinary(data)
	if err != nil {
		return nil, fmt.Errorf("publish %s: %w", key, err)
	}
	return r.setTarget(key, version, hash, int64(len(data)), "")
}

// PublishFile makes the content of path the current target of key. The path
// is remembered as the artifact's Source so a file watcher can re-publish it
// on change.
func (r *Registry) PublishFile(key protocol.ArtifactKey, version, path string) (*Artifact, error) {
	if err := key.Validate(); err != nil {
		return nil, fmt.Errorf("publish: %w", err)
	}
	hash, size, err := r.store.RegisterBinaryFile(path)
	if err != nil {
		return nil, fmt.Errorf("publish %s: %w", key, err)
	}
	return r.setTarget(key, version, hash, size, path)
}

// setTarget applies a new target under the lock and fires OnChange outside it.
func (r *Registry) setTarget(key protocol.ArtifactKey, version, hash string, size int64, source string) (*Artifact, error) {
	ks := key.String()

	r.mu.Lock()
	art, exists := r.artifacts[ks]
	if !exists {
		art = &Artifact{Key: key}
		r.artifacts[ks] = art
	}
	previous := art.TargetHash
	hashChanged := previous != hash
	// A version-only republish (same bytes, new label) still has to invalidate
	// derived caches: the signed manifest carries TargetVersion, so leaving it
	// memoized would serve the old label until the entry aged out.
	changed := hashChanged || art.Version != version
	if hashChanged && previous != "" {
		// Newest first, deduplicated, capped.
		hist := make([]string, 0, len(art.History)+1)
		hist = append(hist, previous)
		for _, h := range art.History {
			if h != previous {
				hist = append(hist, h)
			}
		}
		if len(hist) > r.opts.HistoryDepth {
			hist = hist[:r.opts.HistoryDepth]
		}
		art.History = hist
	}
	art.Version = version
	art.TargetHash = hash
	art.TargetSize = size
	art.UpdatedAt = time.Now().UTC()
	if source != "" {
		art.Source = source
	}
	// First artifact ever published becomes the implicit default, so a
	// single-artifact deployment needs no extra configuration.
	if r.defaultKey == "" && len(r.artifacts) == 1 {
		r.defaultKey = ks
	}
	snapshot := art.clone()
	count := len(r.artifacts)
	r.mu.Unlock()

	if err := r.persist(); err != nil {
		// Persistence failure is loud but not fatal: the in-memory state is
		// correct and the fleet keeps updating. Losing it on restart is
		// recoverable by re-publishing; refusing the publish is not.
		r.logger.Error("persist registry state failed",
			"op", "registry_publish", "artifact", ks, "err", err)
	}
	if r.opts.Metrics != nil {
		r.opts.Metrics.SetArtifactCount(count)
		r.opts.Metrics.SetArtifactTargetSize(ks, size)
	}
	if changed {
		r.logger.Info("artifact published",
			"op", "registry_publish",
			"artifact", ks,
			"version", version,
			"target_hash", hash,
			"previous_hash", previous,
			"size", size,
		)
		if r.opts.OnChange != nil {
			r.opts.OnChange(key)
		}
	} else {
		r.logger.Debug("artifact republished with identical content",
			"op", "registry_publish", "artifact", ks, "target_hash", hash)
	}
	return snapshot, nil
}

// Republish re-reads an artifact's Source file and publishes it. Used by the
// file watcher and by POST /admin/reload. Returns ErrArtifactNotFound for
// unknown keys, and an error for artifacts with no Source (API-published).
func (r *Registry) Republish(key protocol.ArtifactKey) (*Artifact, error) {
	r.mu.RLock()
	art, ok := r.artifacts[key.String()]
	var source, version string
	if ok {
		source, version = art.Source, art.Version
	}
	r.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("republish %s: %w", key, ErrArtifactNotFound)
	}
	if source == "" {
		return nil, fmt.Errorf("republish %s: artifact has no source file (published via API)", key)
	}
	return r.PublishFile(key, version, source)
}

// ---------------------------------------------------------------------------
// Lookup
// ---------------------------------------------------------------------------

// Resolve maps the Artifact field of a heartbeat to a registered artifact.
// An empty name selects the default artifact, which keeps single-artifact
// deployments and pre-multi-artifact agents working unchanged.
func (r *Registry) Resolve(name string) (*Artifact, error) {
	if name == "" {
		r.mu.RLock()
		defer r.mu.RUnlock()
		if r.defaultKey == "" {
			return nil, fmt.Errorf("no artifact named and no default configured: %w", ErrArtifactNotFound)
		}
		art, ok := r.artifacts[r.defaultKey]
		if !ok {
			return nil, fmt.Errorf("default artifact %q: %w", r.defaultKey, ErrArtifactNotFound)
		}
		return art.clone(), nil
	}
	key, err := protocol.ParseArtifactKey(name)
	if err != nil {
		return nil, err
	}
	return r.Get(key)
}

// Get returns a snapshot of the artifact registered under key.
func (r *Registry) Get(key protocol.ArtifactKey) (*Artifact, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	art, ok := r.artifacts[key.String()]
	if !ok {
		return nil, fmt.Errorf("artifact %q: %w", key, ErrArtifactNotFound)
	}
	return art.clone(), nil
}

// List returns snapshots of every artifact, ordered by key for stable output.
func (r *Registry) List() []*Artifact {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Artifact, 0, len(r.artifacts))
	for _, a := range r.artifacts {
		out = append(out, a.clone())
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Key.String() < out[j].Key.String()
	})
	return out
}

// Default returns the key currently serving heartbeats that name no artifact.
func (r *Registry) Default() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defaultKey
}

// SetDefault designates which artifact answers heartbeats with no artifact
// name. The key must already be registered.
func (r *Registry) SetDefault(key protocol.ArtifactKey) error {
	ks := key.String()
	r.mu.Lock()
	if _, ok := r.artifacts[ks]; !ok {
		r.mu.Unlock()
		return fmt.Errorf("set default %q: %w", ks, ErrArtifactNotFound)
	}
	r.defaultKey = ks
	r.mu.Unlock()
	if err := r.persist(); err != nil {
		r.logger.Error("persist registry state failed",
			"op", "registry_default", "artifact", ks, "err", err)
	}
	return nil
}

// Remove unregisters an artifact. The bytes stay in the store until the
// retention sweeper collects them, so an accidental removal followed by a
// re-publish costs nothing.
func (r *Registry) Remove(key protocol.ArtifactKey) error {
	ks := key.String()
	r.mu.Lock()
	if _, ok := r.artifacts[ks]; !ok {
		r.mu.Unlock()
		return fmt.Errorf("remove %q: %w", ks, ErrArtifactNotFound)
	}
	delete(r.artifacts, ks)
	if r.defaultKey == ks {
		r.defaultKey = ""
		// Fall back to the sole survivor, if there is exactly one, mirroring
		// the "first publish becomes default" rule.
		if len(r.artifacts) == 1 {
			for k := range r.artifacts {
				r.defaultKey = k
			}
		}
	}
	count := len(r.artifacts)
	r.mu.Unlock()

	if err := r.persist(); err != nil {
		r.logger.Error("persist registry state failed",
			"op", "registry_remove", "artifact", ks, "err", err)
	}
	if r.opts.Metrics != nil {
		r.opts.Metrics.SetArtifactCount(count)
		r.opts.Metrics.DeleteArtifactTargetSize(ks)
	}
	r.logger.Info("artifact removed", "op", "registry_remove", "artifact", ks)
	if r.opts.OnChange != nil {
		r.opts.OnChange(key)
	}
	return nil
}

// LiveHashes returns every binary hash the registry still considers relevant:
// current targets plus per-artifact history. The retention sweeper treats
// everything else as collectable.
func (r *Registry) LiveHashes() map[string]struct{} {
	r.mu.RLock()
	defer r.mu.RUnlock()
	live := make(map[string]struct{}, len(r.artifacts)*(1+r.opts.HistoryDepth))
	for _, a := range r.artifacts {
		if a.TargetHash != "" {
			live[a.TargetHash] = struct{}{}
		}
		for _, h := range a.History {
			live[h] = struct{}{}
		}
	}
	return live
}

// CurrentTargets returns just the current target hash of every artifact.
// Deltas whose destination is not in this set can never be requested again.
func (r *Registry) CurrentTargets() map[string]struct{} {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]struct{}, len(r.artifacts))
	for _, a := range r.artifacts {
		if a.TargetHash != "" {
			out[a.TargetHash] = struct{}{}
		}
	}
	return out
}

// IsCurrentTarget reports whether hash is the current target of any
// artifact. Transport handlers use it to bound what a request can ask the
// server to materialize: without it, anyone able to name two known hashes
// could trigger an arbitrary bsdiff, which is the most expensive operation
// the server performs.
func (r *Registry) IsCurrentTarget(hash string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, a := range r.artifacts {
		if a.TargetHash == hash {
			return true
		}
	}
	return false
}

// IsLive reports whether hash is a current target or in any artifact's
// history — i.e. whether the server is willing to serve it as a whole binary.
func (r *Registry) IsLive(hash string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, a := range r.artifacts {
		if a.TargetHash == hash {
			return true
		}
		for _, h := range a.History {
			if h == hash {
				return true
			}
		}
	}
	return false
}

// WatchedSources returns the artifacts that were published from a file and
// can therefore be auto-reloaded when that file changes.
func (r *Registry) WatchedSources() map[protocol.ArtifactKey]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[protocol.ArtifactKey]string)
	for _, a := range r.artifacts {
		if a.Source != "" {
			out[a.Key] = a.Source
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

// registryState is the on-disk JSON shape. Versioned so a future format
// change can be detected rather than silently misparsed.
type registryState struct {
	Version   int         `json:"version"`
	Default   string      `json:"default,omitempty"`
	Artifacts []*Artifact `json:"artifacts"`
}

const registryStateVersion = 1

func (r *Registry) persist() error {
	if r.opts.StatePath == "" {
		return nil
	}
	r.mu.RLock()
	st := registryState{
		Version:   registryStateVersion,
		Default:   r.defaultKey,
		Artifacts: make([]*Artifact, 0, len(r.artifacts)),
	}
	for _, a := range r.artifacts {
		st.Artifacts = append(st.Artifacts, a.clone())
	}
	r.mu.RUnlock()
	sort.Slice(st.Artifacts, func(i, j int) bool {
		return st.Artifacts[i].Key.String() < st.Artifacts[j].Key.String()
	})

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal registry state: %w", err)
	}
	if err := atomicio.WriteFile(r.opts.StatePath, data, 0o644, r.logger); err != nil {
		return fmt.Errorf("write registry state: %w", err)
	}
	return nil
}

// load restores persisted state. A missing file is not an error — it is a
// first boot. A corrupt file IS an error: silently starting with an empty
// registry would tell every device in the fleet "no update available", which
// is a worse failure than refusing to boot.
func (r *Registry) load() error {
	if r.opts.StatePath == "" {
		return nil
	}
	data, err := os.ReadFile(r.opts.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read registry state: %w", err)
	}
	var st registryState
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("parse registry state %q: %w", r.opts.StatePath, err)
	}
	if st.Version != registryStateVersion {
		return fmt.Errorf("registry state %q: unsupported version %d (want %d)",
			r.opts.StatePath, st.Version, registryStateVersion)
	}
	restored, dropped := 0, 0
	for _, a := range st.Artifacts {
		if err := a.Key.Validate(); err != nil {
			r.logger.Warn("dropping invalid artifact from persisted state",
				"op", "registry_load", "err", err)
			dropped++
			continue
		}
		// A target whose bytes are gone (manual store wipe, aggressive
		// retention) cannot be served. Keep the artifact but log loudly:
		// the operator needs to re-publish, and pretending it is fine would
		// surface as mysterious 404s on the delta endpoint instead.
		if !r.store.HasBinary(a.TargetHash) {
			r.logger.Warn("persisted artifact target missing from store",
				"op", "registry_load", "artifact", a.Key.String(),
				"target_hash", a.TargetHash)
		}
		r.artifacts[a.Key.String()] = a
		restored++
	}
	if r.defaultKey == "" {
		r.defaultKey = st.Default
	}
	r.logger.Info("registry state restored",
		"op", "registry_load", "path", r.opts.StatePath,
		"artifacts", restored, "dropped", dropped, "default", r.defaultKey)
	if r.opts.Metrics != nil {
		r.opts.Metrics.SetArtifactCount(restored)
		for _, a := range r.artifacts {
			r.opts.Metrics.SetArtifactTargetSize(a.Key.String(), a.TargetSize)
		}
	}
	return nil
}

// ReconcileConfig publishes every artifact declared in YAML. Config is
// authoritative for the artifacts it declares: an operator editing the file
// expects it to win over whatever the admin API published earlier. Artifacts
// absent from the config are left untouched.
func (r *Registry) ReconcileConfig(ctx context.Context, declared []ArtifactYAMLConfig) error {
	for _, d := range declared {
		key := protocol.ArtifactKey{Name: d.Name, OS: d.OS, Arch: d.Arch}
		if err := key.Validate(); err != nil {
			return fmt.Errorf("config artifact %q: %w", d.Name, err)
		}
		if _, err := r.PublishFile(key, d.Version, d.Binary); err != nil {
			return fmt.Errorf("config artifact %q: %w", key, err)
		}
	}
	return nil
}
