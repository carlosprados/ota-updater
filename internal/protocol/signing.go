package protocol

import (
	"encoding/hex"
	"fmt"
)

// ManifestSigningPayload returns the canonical byte sequence that the server
// signs (and the agent verifies) for a ManifestResponse: the concatenation of
// the raw target binary hash and the raw compressed delta hash.
//
// Signing over both lets the agent reject a corrupt delta immediately after
// download — before spending CPU and memory on bspatch — saving the scarce
// NB-IoT downlink budget. The target hash still anchors the authenticity of
// the activated binary regardless of which delta path was taken.
//
// Both inputs must be lowercase hex-encoded SHA-256 digests (64 chars each).
// The returned slice is targetHashRaw || deltaHashRaw (64 bytes for SHA-256).
func ManifestSigningPayload(targetHashHex, deltaHashHex string) ([]byte, error) {
	target, err := hex.DecodeString(targetHashHex)
	if err != nil {
		return nil, fmt.Errorf("decode target hash: %w", err)
	}
	delta, err := hex.DecodeString(deltaHashHex)
	if err != nil {
		return nil, fmt.Errorf("decode delta hash: %w", err)
	}
	out := make([]byte, 0, len(target)+len(delta))
	out = append(out, target...)
	out = append(out, delta...)
	return out, nil
}
