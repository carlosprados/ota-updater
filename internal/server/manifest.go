package server

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/amplia/ota-updater/internal/crypto"
	"github.com/amplia/ota-updater/internal/protocol"
)

// ManifesterConfig tunes Manifester behavior. Zero values fall back to
// protocol defaults.
type ManifesterConfig struct {
	ChunkSize     int    // bytes per download chunk; 0 → protocol.DefaultChunkSize
	RetryAfter    int    // seconds to tell agents to wait while a delta generates; 0 → 30
	TargetVersion string // human-readable version label returned to agents
}

// Manifester builds signed ManifestResponse payloads in response to agent
// heartbeats. It does not speak any transport — it just produces the struct.
//
// For scale (thousands of agents), signed responses are cached in memory
// keyed by (fromHash, targetHash). Cache entries are immutable; callers
// receive a shared pointer and must NOT mutate it. Call Invalidate() after
// changing the target binary (see Store.Reload at step 9).
type Manifester struct {
	store         *Store
	priv          ed25519.PrivateKey
	chunkSize     int
	retryAfter    int
	targetVersion string
	logger        *slog.Logger

	mu    sync.RWMutex
	cache map[string]*protocol.ManifestResponse
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
		cache:         make(map[string]*protocol.ManifestResponse),
	}
}

// Invalidate drops every cached manifest. Call after the target binary
// changes (Store.Reload) so that the next heartbeat rebuilds a fresh,
// correctly-signed response for the new target.
func (m *Manifester) Invalidate() {
	m.mu.Lock()
	clear(m.cache)
	m.mu.Unlock()
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

	if !m.store.HasDelta(hb.VersionHash) {
		m.store.StartDeltaGeneration(hb.VersionHash)
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

	key := hb.VersionHash + "_" + targetHash
	if cached := m.cacheGet(key); cached != nil {
		return cached, nil
	}

	deltaPath := m.store.DeltaPath(hb.VersionHash, targetHash)
	data, err := os.ReadFile(deltaPath)
	if err != nil {
		return nil, fmt.Errorf("read cached delta: %w", err)
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
	m.cachePut(key, resp)
	m.logger.Info("manifest built and cached",
		"device_id", hb.DeviceID,
		"from", hb.VersionHash,
		"to", targetHash,
		"delta_size", size,
	)
	return resp, nil
}

func (m *Manifester) cacheGet(key string) *protocol.ManifestResponse {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cache[key]
}

func (m *Manifester) cachePut(key string, resp *protocol.ManifestResponse) {
	m.mu.Lock()
	m.cache[key] = resp
	m.mu.Unlock()
}

func chunkCount(size int64, chunk int) int {
	if chunk <= 0 {
		return 0
	}
	return int((size + int64(chunk) - 1) / int64(chunk))
}
