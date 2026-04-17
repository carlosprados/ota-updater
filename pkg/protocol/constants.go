package protocol

import "time"

// ProtocolVersion is bumped on incompatible wire-format changes.
const ProtocolVersion = 1

// Transport-agnostic resource paths. HTTP and CoAP handlers must mirror these.
const (
	PathHeartbeat = "/heartbeat"
	PathDelta     = "/delta"
	PathReport    = "/report"
	PathHealth    = "/health"
)

// Defaults tuned for NB-IoT constrained links (~20-60 kbps, high latency).
const (
	DefaultChunkSize     = 4096
	DefaultCheckInterval = time.Hour
	DefaultMaxRetries    = 10
	DefaultRetryBackoff  = 30 * time.Second
	DefaultWatchdogTime  = 60 * time.Second
)

// DeltaPath returns the canonical resource path for a delta between two binary
// hashes. Used by both HTTP and CoAP transports.
func DeltaPath(fromHash, toHash string) string {
	return PathDelta + "/" + fromHash + "/" + toHash
}
