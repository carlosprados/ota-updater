package protocol

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

func TestManifestSigningPayload_Deterministic(t *testing.T) {
	target := strings.Repeat("ab", 32) // 32 bytes SHA-256
	delta := strings.Repeat("cd", 32)

	a, err := ManifestSigningPayload(target, delta)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	b, err := ManifestSigningPayload(target, delta)
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("payload not deterministic")
	}
	if len(a) != 64 {
		t.Fatalf("unexpected payload length %d, want 64", len(a))
	}

	wantPrefix, _ := hex.DecodeString(target)
	if !bytes.HasPrefix(a, wantPrefix) {
		t.Fatalf("payload does not start with target hash bytes")
	}
	wantSuffix, _ := hex.DecodeString(delta)
	if !bytes.HasSuffix(a, wantSuffix) {
		t.Fatalf("payload does not end with delta hash bytes")
	}
}

func TestManifestSigningPayload_BadHex(t *testing.T) {
	_, err := ManifestSigningPayload("zz", strings.Repeat("cd", 32))
	if err == nil {
		t.Fatalf("expected error for invalid target hex")
	}
	_, err = ManifestSigningPayload(strings.Repeat("ab", 32), "zz")
	if err == nil {
		t.Fatalf("expected error for invalid delta hex")
	}
}
