// Package protocol defines the wire-format types exchanged between edge-agent
// and update-server. Structs carry both JSON and CBOR tags so the same types
// serialize over HTTP (JSON) and CoAP (CBOR) without duplication.
package protocol

// Heartbeat is sent by the agent to the server on each update check.
type Heartbeat struct {
	DeviceID    string `json:"device_id"         cbor:"1,keyasint"`
	VersionHash string `json:"version_hash"      cbor:"2,keyasint"` // SHA-256 hex of current running binary
	HWInfo      HWInfo `json:"hw_info"           cbor:"3,keyasint"`
	Timestamp   int64  `json:"timestamp"         cbor:"4,keyasint"` // unix seconds
	// Version is the human-readable semver ("1.2.3") of the running binary,
	// injected at build time via ldflags. Optional and advisory: the
	// authoritative identity on the wire is VersionHash. Logged by the
	// server for fleet observability and used by the agent to compare
	// against ManifestResponse.TargetVersion under the update policy.
	Version string `json:"version,omitempty" cbor:"5,keyasint,omitempty"`
	// Artifact names the publication track this heartbeat is about, in
	// ArtifactKey canonical form ("keystone-agent/linux/arm64"). Empty means
	// "the server's default artifact", which keeps single-artifact
	// deployments working without any client change.
	Artifact string `json:"artifact,omitempty" cbor:"6,keyasint,omitempty"`
}

// HWInfo describes the device runtime environment.
type HWInfo struct {
	Arch     string `json:"arch"      cbor:"1,keyasint"` // GOARCH value (e.g. "arm64")
	OS       string `json:"os"        cbor:"2,keyasint"` // GOOS value (e.g. "linux")
	FreeRAM  uint64 `json:"free_ram"  cbor:"3,keyasint"` // bytes
	FreeDisk uint64 `json:"free_disk" cbor:"4,keyasint"` // bytes
}

// ManifestResponse is returned by the server in response to a Heartbeat.
// When UpdateAvailable is true and RetryAfter > 0, the delta is still being
// generated server-side and the agent should retry after that many seconds.
//
// # Transfer mode
//
// Exactly one of DeltaEndpoint or BinaryEndpoint is set on an actionable
// response, and it selects how the agent reconstructs the target:
//
//   - DeltaEndpoint  → download the patch, bspatch it onto the running binary.
//   - BinaryEndpoint → download the whole compressed target, no patching.
//     Used when the server cannot build a delta: it has never seen the
//     agent's current version (new device, sideloaded build, post-rollback
//     divergence) or the source binary has aged out of retention.
//
// DeltaSize and DeltaHash describe *the bytes that travel on the wire* in
// both modes — the compressed patch, or the compressed target. That is what
// makes the signature scheme mode-agnostic: the signed payload is always
// (TargetHash, DeltaHash) and the agent verifies the transferred bytes before
// doing anything with them. See docs/signing.md.
type ManifestResponse struct {
	UpdateAvailable bool   `json:"update_available"           cbor:"1,keyasint"`
	TargetVersion   string `json:"target_version,omitempty"   cbor:"2,keyasint,omitempty"`
	TargetHash      string `json:"target_hash,omitempty"      cbor:"3,keyasint,omitempty"` // SHA-256 hex of reconstructed target binary
	DeltaSize       int64  `json:"delta_size,omitempty"       cbor:"4,keyasint,omitempty"` // compressed delta size in bytes
	DeltaHash       string `json:"delta_hash,omitempty"       cbor:"5,keyasint,omitempty"` // SHA-256 hex of compressed delta
	ChunkSize       int    `json:"chunk_size,omitempty"       cbor:"6,keyasint,omitempty"`
	TotalChunks     int    `json:"total_chunks,omitempty"     cbor:"7,keyasint,omitempty"`
	// Signature is lowercase-hex Ed25519 (128 chars) over
	// ManifestSigningPayload(TargetHash, DeltaHash). Empty when
	// UpdateAvailable=false or when RetryAfter>0 (delta not yet cached).
	// See docs/signing.md for the full scheme and verification order.
	Signature     string `json:"signature,omitempty" cbor:"8,keyasint,omitempty"`
	DeltaEndpoint string `json:"delta_endpoint,omitempty"   cbor:"9,keyasint,omitempty"`  // transport-relative path
	RetryAfter    int    `json:"retry_after,omitempty"      cbor:"10,keyasint,omitempty"` // seconds; >0 means delta not ready yet
	// BinaryEndpoint is the transport-relative path of the whole compressed
	// target, set instead of DeltaEndpoint when no delta can be built. See
	// the "Transfer mode" section above.
	BinaryEndpoint string `json:"binary_endpoint,omitempty" cbor:"11,keyasint,omitempty"`
	// TargetSize is the uncompressed size of the reconstructed target in
	// bytes. Advisory: lets the agent check it has the disk headroom to
	// stage the new slot before burning downlink on the transfer.
	TargetSize int64 `json:"target_size,omitempty" cbor:"12,keyasint,omitempty"`
	// Artifact echoes the resolved ArtifactKey the server answered for. Lets
	// an agent that sent an empty Artifact learn the server's default, and
	// makes multi-artifact traffic readable in packet captures and logs.
	Artifact string `json:"artifact,omitempty" cbor:"13,keyasint,omitempty"`
}

// UpdateReport is sent by the agent after an update attempt completes.
type UpdateReport struct {
	DeviceID       string `json:"device_id"                 cbor:"1,keyasint"`
	PreviousHash   string `json:"previous_hash"             cbor:"2,keyasint"`
	NewHash        string `json:"new_hash"                  cbor:"3,keyasint"`
	Success        bool   `json:"success"                   cbor:"4,keyasint"`
	RollbackReason string `json:"rollback_reason,omitempty" cbor:"5,keyasint,omitempty"`
	Timestamp      int64  `json:"timestamp"                 cbor:"6,keyasint"` // unix seconds
}
