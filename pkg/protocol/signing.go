package protocol

import (
	"encoding/hex"
	"fmt"
)

// ManifestSigningPayload returns the canonical byte sequence that the server
// signs (and the agent verifies) for a ManifestResponse.
//
// Layout (strict, no separators, no length prefix, no version byte):
//
//	payload := targetHashRaw || deltaHashRaw   // 32 + 32 = 64 bytes
//
// Where:
//   - targetHashRaw = raw 32-byte SHA-256 of the reconstructed target binary
//     (what runs on the device after patching).
//   - deltaHashRaw  = raw 32-byte SHA-256 of the zstd-compressed delta bytes
//     as transferred on the wire (no other transformation).
//
// Both inputs are lowercase hex SHA-256 digests (64 chars each). Decoding
// happens here; the canonical payload is always the raw bytes. The payload
// is NOT re-hashed before signing — Ed25519 hashes the message internally
// with SHA-512, so adding an outer hash would be redundant.
//
// Signing over (target, delta) jointly lets the agent reject a corrupt delta
// right after download — before bspatch — saving the scarce NB-IoT downlink.
// The target hash still anchors the authenticity of the activated binary
// regardless of which delta path was taken. See docs/signing.md for the
// full rationale, verification order, and threat model.
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
