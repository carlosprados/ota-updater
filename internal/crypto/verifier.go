package crypto

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
)

const publicKeyPEMType = "PUBLIC KEY" // PKIX (SubjectPublicKeyInfo)

// ErrInvalidSignature is returned when signature verification fails.
var ErrInvalidSignature = errors.New("invalid signature")

// LoadPublicKey reads an Ed25519 public key from a PKIX PEM file.
func LoadPublicKey(path string) (ed25519.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read public key: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("decode public key PEM: no block found")
	}
	if block.Type != publicKeyPEMType {
		return nil, fmt.Errorf("decode public key PEM: unexpected type %q", block.Type)
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKIX public key: %w", err)
	}
	pub, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("unexpected public key type %T, want ed25519.PublicKey", key)
	}
	return pub, nil
}

// VerifyHash verifies an Ed25519 signature over the given raw hash bytes.
// Returns ErrInvalidSignature when the signature does not match.
func VerifyHash(pub ed25519.PublicKey, hash, sig []byte) error {
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid public key size %d", len(pub))
	}
	if !ed25519.Verify(pub, hash, sig) {
		return ErrInvalidSignature
	}
	return nil
}

// EncodePublicKeyPEM returns the PKIX-PEM encoding of the given Ed25519 public
// key. Useful for the keygen tool.
func EncodePublicKeyPEM(pub ed25519.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("marshal PKIX public key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: publicKeyPEMType, Bytes: der}), nil
}
