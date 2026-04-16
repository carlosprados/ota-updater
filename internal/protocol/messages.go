// Package protocol defines the wire-format types exchanged between edge-agent
// and update-server. Structs carry both JSON and CBOR tags so the same types
// serialize over HTTP (JSON) and CoAP (CBOR) without duplication.
package protocol

// Heartbeat is sent by the agent to the server on each update check.
type Heartbeat struct {
	DeviceID    string `json:"device_id"    cbor:"1,keyasint"`
	VersionHash string `json:"version_hash" cbor:"2,keyasint"` // SHA-256 hex of current running binary
	HWInfo      HWInfo `json:"hw_info"      cbor:"3,keyasint"`
	Timestamp   int64  `json:"timestamp"    cbor:"4,keyasint"` // unix seconds
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
type ManifestResponse struct {
	UpdateAvailable bool   `json:"update_available"           cbor:"1,keyasint"`
	TargetVersion   string `json:"target_version,omitempty"   cbor:"2,keyasint,omitempty"`
	TargetHash      string `json:"target_hash,omitempty"      cbor:"3,keyasint,omitempty"` // SHA-256 hex of reconstructed target binary
	DeltaSize       int64  `json:"delta_size,omitempty"       cbor:"4,keyasint,omitempty"` // compressed delta size in bytes
	DeltaHash       string `json:"delta_hash,omitempty"       cbor:"5,keyasint,omitempty"` // SHA-256 hex of compressed delta
	ChunkSize       int    `json:"chunk_size,omitempty"       cbor:"6,keyasint,omitempty"`
	TotalChunks     int    `json:"total_chunks,omitempty"     cbor:"7,keyasint,omitempty"`
	Signature       string `json:"signature,omitempty"        cbor:"8,keyasint,omitempty"` // Ed25519 sig (hex) of TargetHash raw bytes
	DeltaEndpoint   string `json:"delta_endpoint,omitempty"   cbor:"9,keyasint,omitempty"` // transport-relative path
	RetryAfter      int    `json:"retry_after,omitempty"      cbor:"10,keyasint,omitempty"` // seconds; >0 means delta not ready yet
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
