package server

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"

	"github.com/amplia/ota-updater/pkg/crypto"
	"github.com/amplia/ota-updater/pkg/protocol"
)

// DefaultManifestCacheSize is the default entry count for the signed-manifest
// LRU. Each entry holds a tiny ManifestResponse (~500 B); 4096 keeps memory
// around 2 MiB while comfortably covering even a large fleet of distinct
// source versions during a campaign.
const DefaultManifestCacheSize = 4096

// ManifesterConfig tunes Manifester behavior. Zero values fall back to
// protocol defaults.
type ManifesterConfig struct {
	ChunkSize     int    // bytes per download chunk; 0 → protocol.DefaultChunkSize
	RetryAfter    int    // seconds to tell agents to wait while a delta generates; 0 → 30
	TargetVersion string // human-readable version label returned to agents
	CacheSize     int    // signed-manifest LRU entry count; 0 → DefaultManifestCacheSize
}

// Manifester builds signed ManifestResponse payloads in response to agent
// heartbeats. It does not speak any transport — it just produces the struct.
//
// Signed responses are cached in an entry-count LRU keyed by
// (fromHash, targetHash). The cache is bounded by CacheSize so that a long
// history of distinct source versions cannot grow the Go heap. Cache entries
// are immutable; callers receive a shared pointer and must NOT mutate it.
// Call Invalidate() after changing the target binary (see Store.Reload).
type Manifester struct {
	store         *Store
	priv          ed25519.PrivateKey
	chunkSize     int
	retryAfter    int
	targetVersion string
	logger        *slog.Logger

	cache *entryLRU[*protocol.ManifestResponse]
}

// NewManifester returns a Manifester using the given store for delta
// materialization and priv for Ed25519 signatures.
func NewManifester(store *Store, priv ed25519.PrivateKey, cfg ManifesterConfig, logger *slog.Logger) *Manifester {
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
		store:         store,
		priv:          priv,
		chunkSize:     cfg.ChunkSize,
		retryAfter:    cfg.RetryAfter,
		targetVersion: cfg.TargetVersion,
		logger:        logger,
		cache:         newEntryLRU[*protocol.ManifestResponse](cfg.CacheSize),
	}
}

// Invalidate drops every cached manifest. Call after the target binary
// changes (Store.Reload) so that the next heartbeat rebuilds a fresh,
// correctly-signed response for the new target.
func (m *Manifester) Invalidate() {
	m.cache.Clear()
}

// Build produces a ManifestResponse for the given heartbeat. Possible
// outcomes:
//
//   - agent already on target        → UpdateAvailable=false
//   - server doesn't know the source → UpdateAvailable=false (logged warning)
//   - delta not yet cached           → UpdateAvailable=true, RetryAfter>0,
//     asynchronous generation dispatched
//   - delta cached                   → full signed manifest (memoized)
func (m *Manifester) Build(ctx context.Context, hb *protocol.Heartbeat) (*protocol.ManifestResponse, error) {
	targetHash := m.store.TargetHash()

	if hb.VersionHash == targetHash {
		return &protocol.ManifestResponse{UpdateAvailable: false}, nil
	}

	if !m.store.HasBinary(hb.VersionHash) {
		m.logger.Warn("heartbeat from unknown source version",
			"device_id", hb.DeviceID,
			"version_hash", hb.VersionHash,
			"target_hash", targetHash,
		)
		return &protocol.ManifestResponse{UpdateAvailable: false}, nil
	}

	key := hb.VersionHash + "_" + targetHash
	if cached, ok := m.cache.Get(key); ok {
		return cached, nil
	}

	data, found, err := m.store.GetDeltaBytes(ctx, hb.VersionHash)
	if err != nil {
		return nil, fmt.Errorf("fetch delta bytes: %w", err)
	}
	if !found {
		// Not on disk yet — GetDeltaBytes already dispatched async generation.
		m.logger.Info("delta not ready, async generation dispatched",
			"device_id", hb.DeviceID,
			"from", hb.VersionHash,
			"to", targetHash,
			"retry_after", m.retryAfter,
		)
		return &protocol.ManifestResponse{
			UpdateAvailable: true,
			TargetVersion:   m.targetVersion,
			TargetHash:      targetHash,
			RetryAfter:      m.retryAfter,
		}, nil
	}
	sum := sha256.Sum256(data)
	deltaHash := hex.EncodeToString(sum[:])
	size := int64(len(data))

	payload, err := protocol.ManifestSigningPayload(targetHash, deltaHash)
	if err != nil {
		return nil, fmt.Errorf("build signing payload: %w", err)
	}
	sig, err := crypto.Sign(m.priv, payload)
	if err != nil {
		return nil, fmt.Errorf("sign manifest: %w", err)
	}

	resp := &protocol.ManifestResponse{
		UpdateAvailable: true,
		TargetVersion:   m.targetVersion,
		TargetHash:      targetHash,
		DeltaSize:       size,
		DeltaHash:       deltaHash,
		ChunkSize:       m.chunkSize,
		TotalChunks:     chunkCount(size, m.chunkSize),
		Signature:       hex.EncodeToString(sig),
		DeltaEndpoint:   protocol.DeltaPath(hb.VersionHash, targetHash),
	}
	m.cache.Put(key, resp)
	m.logger.Info("manifest built and cached",
		"device_id", hb.DeviceID,
		"from", hb.VersionHash,
		"to", targetHash,
		"delta_size", size,
	)
	return resp, nil
}

func chunkCount(size int64, chunk int) int {
	if chunk <= 0 {
		return 0
	}
	return int((size + int64(chunk) - 1) / int64(chunk))
}
