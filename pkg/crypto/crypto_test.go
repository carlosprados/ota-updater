package crypto

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"testing"
)

func TestSignVerify_RoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	msg := []byte("manifest signing payload example")

	sig, err := Sign(priv, msg)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := Verify(pub, msg, sig); err != nil {
		t.Fatalf("Verify valid: %v", err)
	}
}

func TestVerify_TamperedMessageFails(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	msg := []byte("original")
	sig, _ := Sign(priv, msg)

	err := Verify(pub, []byte("tampered"), sig)
	if !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("expected ErrInvalidSignature, got %v", err)
	}
}

func TestEncodeLoad_RoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	privPEM, err := EncodePrivateKeyPEM(priv)
	if err != nil {
		t.Fatalf("EncodePrivateKeyPEM: %v", err)
	}
	pubPEM, err := EncodePublicKeyPEM(pub)
	if err != nil {
		t.Fatalf("EncodePublicKeyPEM: %v", err)
	}

	dir := t.TempDir()
	privPath := dir + "/priv.pem"
	pubPath := dir + "/pub.pem"
	if err := os.WriteFile(privPath, privPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pubPath, pubPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	loadedPriv, err := LoadPrivateKey(privPath)
	if err != nil {
		t.Fatalf("LoadPrivateKey: %v", err)
	}
	loadedPub, err := LoadPublicKey(pubPath)
	if err != nil {
		t.Fatalf("LoadPublicKey: %v", err)
	}

	msg := []byte("hello")
	sig, _ := Sign(loadedPriv, msg)
	if err := Verify(loadedPub, msg, sig); err != nil {
		t.Fatalf("round-trip through PEM failed: %v", err)
	}
}
