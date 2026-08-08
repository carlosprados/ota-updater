package server

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"github.com/carlosprados/ota-updater/pkg/crypto"
	"github.com/carlosprados/ota-updater/pkg/protocol"
)

// DefaultManifestCacheSize is the default entry count for the signed-manifest
// LRU. Each entry holds a tiny ManifestResponse (~500 B); 4096 keeps memory
// around 2 MiB while comfortably covering even a large fleet of distinct
// source versions across several artifacts during a campaign.
const DefaultManifestCacheSize = 4096

// ManifesterConfig tunes Manifester behavior. Zero values fall back to
// protocol defaults.
type ManifesterConfig struct {
	ChunkSize  int      // bytes per download chunk; 0 → protocol.DefaultChunkSize
	RetryAfter int      // seconds to tell agents to wait while a delta generates; 0 → 30
	CacheSize  int      // signed-manifest LRU entry count; 0 → DefaultManifestCacheSize
	Metrics    *Metrics // optional; when set, cache entries and signature failures are exported
	// AllowFullDownload enables the whole-binary fallback for agents whose
	// current version the server cannot diff against. Default true (set
	// explicitly via config). Disabling it restores the old behavior where
	// such an agent is told "no update available" and stays stranded — only
	// sensible when downlink is so scarce that a full transfer is never
	// acceptable and stranded devices are handled out of band.
	AllowFullDownload bool
}

// Manifester builds signed ManifestResponse payloads in response to agent
// heartbeats. It does not speak any transport — it just produces the struct.
//
// Signed responses are cached in an entry-count LRU keyed by
// (artifact, fromHash, targetHash). The cache is bounded by CacheSize so that
// a long history of distinct source versions across many artifacts cannot
// grow the Go heap. Cache entries are immutable; callers receive a shared
// pointer and must NOT mutate it. The Registry's OnChange hook calls
// InvalidateArtifact whenever a target moves.
type Manifester struct {
	store      *Store
	registry   *Registry
	priv       ed25519.PrivateKey
	chunkSize  int
	retryAfter int
	allowFull  bool
	logger     *slog.Logger
	metrics    *Metrics

	cache *entryLRU[*protocol.ManifestResponse]
}

// NewManifester returns a Manifester using registry for "what is current",
// store for byte materialization, and priv for Ed25519 signatures.
func NewManifester(store *Store, registry *Registry, priv ed25519.PrivateKey, cfg ManifesterConfig, logger *slog.Logger) *Manifester {
	if cfg.ChunkSize == 0 {
		cfg.ChunkSize = protocol.DefaultChunkSize
	}
	if cfg.RetryAfter == 0 {
		cfg.RetryAfter = 30
	}
	if cfg.CacheSize <= 0 {
		cfg.CacheSize = DefaultManifestCacheSize
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Manifester{
		store:      store,
		registry:   registry,
		priv:       priv,
		chunkSize:  cfg.ChunkSize,
		retryAfter: cfg.RetryAfter,
		allowFull:  cfg.AllowFullDownload,
		logger:     logger,
		cache:      newEntryLRU[*protocol.ManifestResponse](cfg.CacheSize),
		metrics:    cfg.Metrics,
	}
}

// Invalidate drops every cached manifest. Call after a change that could
// affect any artifact.
func (m *Manifester) Invalidate() {
	m.cache.Clear()
	if m.metrics != nil {
		m.metrics.SetManifestCacheEntries(0)
	}
}

// InvalidateArtifact drops cached manifests after an artifact changed. Wired
// to the Registry's OnChange hook.
//
// It clears the whole cache rather than evicting one artifact's entries. Most
// stale entries are already unreachable — the cache is keyed by (artifact,
// from, to) and a normal publish moves `to` — but two cases are not:
// republishing identical bytes under a new version string, and removing an
// artifact. Both would otherwise keep serving a stale TargetVersion forever.
// Publishes are rare (a handful a day) and each rebuilt entry costs one
// Ed25519 signature, so a full clear is the right trade against maintaining
// prefix eviction in the LRU.
func (m *Manifester) InvalidateArtifact(key protocol.ArtifactKey) {
	m.cache.Clear()
	if m.metrics != nil {
		m.metrics.SetManifestCacheEntries(0)
	}
	m.logger.Debug("manifest cache invalidated",
		"op", "manifest_invalidate", "artifact", key.String())
}

// cacheKey namespaces manifests per artifact. Two artifacts can legitimately
// share a (from, to) pair — same bytes published on two tracks — but their
// responses differ in TargetVersion and Artifact, so they must not collide.
func cacheKey(artifact, from, to string) string {
	return artifact + "|" + from + "|" + to
}

// Build produces a ManifestResponse for the given heartbeat. Possible
// outcomes:
//
//   - unknown artifact               → error (caller maps to 404)
//   - agent already on target        → UpdateAvailable=false
//   - delta not yet cached           → UpdateAvailable=true, RetryAfter>0,
//     asynchronous generation dispatched
//   - delta cached                   → signed manifest, DeltaEndpoint set
//   - source unknown to the server   → signed manifest, BinaryEndpoint set
//     (full download), or UpdateAvailable=false if full download is disabled
func (m *Manifester) Build(ctx context.Context, hb *protocol.Heartbeat) (*protocol.ManifestResponse, error) {
	art, err := m.registry.Resolve(hb.Artifact)
	if err != nil {
		return nil, fmt.Errorf("resolve artifact %q: %w", hb.Artifact, err)
	}
	artifactKey := art.Key.String()
	targetHash := art.TargetHash

	if targetHash == "" {
		return nil, fmt.Errorf("artifact %q has no published target", artifactKey)
	}
	if hb.VersionHash == targetHash {
		return &protocol.ManifestResponse{UpdateAvailable: false, Artifact: artifactKey}, nil
	}

	key := cacheKey(artifactKey, hb.VersionHash, targetHash)
	if cached, ok := m.cache.Get(key); ok {
		return cached, nil
	}

	// The server can only build a patch when it still holds the bytes the
	// device is running. A device that was flashed at the factory, sideloaded,
	// or whose source binary aged out of retention has no diffable ancestor
	// here — that is the case the full download exists for.
	if m.store.HasBinary(hb.VersionHash) {
		resp, err := m.buildDelta(ctx, hb, art)
		if err != nil {
			return nil, err
		}
		if resp != nil {
			return resp, nil
		}
		// buildDelta returned nil: the delta is still generating. Do NOT
		// cache the retry response — it is transient by definition.
		m.logger.Info("delta not ready, async generation dispatched",
			"op", "manifest", "device_id", hb.DeviceID, "artifact", artifactKey,
			"from", hb.VersionHash, "to", targetHash, "retry_after", m.retryAfter,
		)
		return &protocol.ManifestResponse{
			UpdateAvailable: true,
			Artifact:        artifactKey,
			TargetVersion:   art.Version,
			TargetHash:      targetHash,
			TargetSize:      art.TargetSize,
			RetryAfter:      m.retryAfter,
		}, nil
	}

	if !m.allowFull {
		m.logger.Warn("heartbeat from unknown source version and full download disabled",
			"op", "manifest", "device_id", hb.DeviceID, "artifact", artifactKey,
			"version_hash", hb.VersionHash, "target_hash", targetHash,
		)
		return &protocol.ManifestResponse{UpdateAvailable: false, Artifact: artifactKey}, nil
	}
	return m.buildFull(ctx, hb, art)
}

// buildDelta produces the patch-mode manifest. Returns (nil, nil) when the
// delta is not cached yet and generation was dispatched.
func (m *Manifester) buildDelta(ctx context.Context, hb *protocol.Heartbeat, art *Artifact) (*protocol.ManifestResponse, error) {
	data, found, err := m.store.GetDeltaBytes(ctx, hb.VersionHash, art.TargetHash)
	if err != nil {
		return nil, fmt.Errorf("fetch delta bytes: %w", err)
	}
	if !found {
		return nil, nil
	}
	resp, err := m.sign(art, data, protocol.DeltaPath(hb.VersionHash, art.TargetHash), false)
	if err != nil {
		return nil, err
	}
	m.memoize(art, hb.VersionHash, resp)
	m.logger.Info("manifest built and cached",
		"op", "manifest", "mode", "delta",
		"device_id", hb.DeviceID, "artifact", art.Key.String(),
		"from", hb.VersionHash, "to", art.TargetHash, "transfer_size", len(data),
	)
	return resp, nil
}

// buildFull produces the full-download manifest: the whole target, zstd
// compressed, signed exactly like a delta.
func (m *Manifester) buildFull(ctx context.Context, hb *protocol.Heartbeat, art *Artifact) (*protocol.ManifestResponse, error) {
	data, found, err := m.store.GetBinaryBytes(ctx, art.TargetHash)
	if err != nil {
		return nil, fmt.Errorf("fetch full binary bytes: %w", err)
	}
	if !found {
		// The artifact's own target is missing from the store. Nothing can
		// be served; this is an operator error (store wiped, retention
		// misconfigured) that must be visible, not silently degraded.
		return nil, fmt.Errorf("artifact %q target %s: %w",
			art.Key, art.TargetHash, ErrBinaryNotFound)
	}
	resp, err := m.sign(art, data, protocol.BinaryPath(art.TargetHash), true)
	if err != nil {
		return nil, err
	}
	m.memoize(art, hb.VersionHash, resp)
	m.logger.Info("manifest built and cached",
		"op", "manifest", "mode", "full",
		"device_id", hb.DeviceID, "artifact", art.Key.String(),
		"from", hb.VersionHash, "to", art.TargetHash, "transfer_size", len(data),
		"reason", "source binary unknown to server",
	)
	return resp, nil
}

// sign builds and signs a manifest for the given transfer payload. The signed
// payload is (targetHash, sha256(transferBytes)) in both modes — see
// docs/signing.md. full selects which endpoint field carries the path.
func (m *Manifester) sign(art *Artifact, transfer []byte, endpoint string, full bool) (*protocol.ManifestResponse, error) {
	sum := sha256.Sum256(transfer)
	transferHash := hex.EncodeToString(sum[:])
	size := int64(len(transfer))

	payload, err := protocol.ManifestSigningPayload(art.TargetHash, transferHash)
	if err != nil {
		return nil, fmt.Errorf("build signing payload: %w", err)
	}
	sig, err := crypto.Sign(m.priv, payload)
	if err != nil {
		if m.metrics != nil {
			m.metrics.IncSignatureFailure()
		}
		return nil, fmt.Errorf("sign manifest: %w", err)
	}
	resp := &protocol.ManifestResponse{
		UpdateAvailable: true,
		Artifact:        art.Key.String(),
		TargetVersion:   art.Version,
		TargetHash:      art.TargetHash,
		TargetSize:      art.TargetSize,
		DeltaSize:       size,
		DeltaHash:       transferHash,
		ChunkSize:       m.chunkSize,
		TotalChunks:     chunkCount(size, m.chunkSize),
		Signature:       hex.EncodeToString(sig),
	}
	if full {
		resp.BinaryEndpoint = endpoint
	} else {
		resp.DeltaEndpoint = endpoint
	}
	return resp, nil
}

// memoize stores a signed manifest and refreshes the cache gauge.
func (m *Manifester) memoize(art *Artifact, fromHash string, resp *protocol.ManifestResponse) {
	m.cache.Put(cacheKey(art.Key.String(), fromHash, art.TargetHash), resp)
	if m.metrics != nil {
		m.metrics.SetManifestCacheEntries(m.cache.Len())
	}
}

// IsArtifactNotFound reports whether err came from resolving an unknown
// artifact, so transports can answer 404 instead of 500.
func IsArtifactNotFound(err error) bool {
	return errors.Is(err, ErrArtifactNotFound) || errors.Is(err, protocol.ErrEmptyArtifactKey)
}

func chunkCount(size int64, chunk int) int {
	if chunk <= 0 {
		return 0
	}
	return int((size + int64(chunk) - 1) / int64(chunk))
}
