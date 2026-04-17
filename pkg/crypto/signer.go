// Package crypto provides Ed25519 signing (server-side) and verification
// (agent-side) for OTA update manifests.
//
// The signed payload is not a raw hash but a canonical composite built by
// protocol.ManifestSigningPayload: targetHashRaw || deltaHashRaw (64 bytes).
// This package intentionally stays format-agnostic; callers build the payload
// via the protocol package and hand it to Sign/Verify. See docs/signing.md
// for the full scheme.
package crypto

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
)

const privateKeyPEMType = "PRIVATE KEY" // PKCS#8

// LoadPrivateKey reads an Ed25519 private key from a PKCS#8 PEM file.
func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("decode private key PEM: no block found")
	}
	if block.Type != privateKeyPEMType {
		return nil, fmt.Errorf("decode private key PEM: unexpected type %q", block.Type)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKCS8 private key: %w", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("unexpected private key type %T, want ed25519.PrivateKey", key)
	}
	return priv, nil
}

// Sign signs the given message bytes with the provided Ed25519 private key.
//
// Pass the raw message (e.g. the 64-byte payload returned by
// protocol.ManifestSigningPayload), not a pre-hashed digest: Ed25519 hashes
// the input internally with SHA-512. Returns a 64-byte signature.
//
// See docs/signing.md §4 for the server-side flow that uses this function.
func Sign(priv ed25519.PrivateKey, msg []byte) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid private key size %d", len(priv))
	}
	return ed25519.Sign(priv, msg), nil
}

// EncodePrivateKeyPEM returns the PKCS#8-PEM encoding of the given Ed25519
// private key. Useful for the keygen tool.
func EncodePrivateKeyPEM(priv ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal PKCS8 private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: privateKeyPEMType, Bytes: der}), nil
}
