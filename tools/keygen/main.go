// Command keygen generates an Ed25519 keypair for the OTA update system.
//
// Usage:
//
//	go run ./tools/keygen -out ./keys
//
// Outputs:
//
//	<out>/server.key  — PKCS#8 PEM private key (mode 0600)
//	<out>/agent.pub   — PKIX   PEM public key  (mode 0644)
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/amplia/ota-updater/pkg/crypto"
)

func main() {
	out := flag.String("out", "./keys", "output directory for keypair")
	flag.Parse()

	if err := run(*out); err != nil {
		log.Fatalf("keygen: %v", err)
	}
}

func run(outDir string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate ed25519 keypair: %w", err)
	}

	privPEM, err := crypto.EncodePrivateKeyPEM(priv)
	if err != nil {
		return err
	}
	pubPEM, err := crypto.EncodePublicKeyPEM(pub)
	if err != nil {
		return err
	}

	privPath := filepath.Join(outDir, "server.key")
	pubPath := filepath.Join(outDir, "agent.pub")

	if err := writeFile(privPath, privPEM, 0o600); err != nil {
		return fmt.Errorf("write private key: %w", err)
	}
	if err := writeFile(pubPath, pubPEM, 0o644); err != nil {
		return fmt.Errorf("write public key: %w", err)
	}

	fmt.Printf("wrote %s (0600)\n", privPath)
	fmt.Printf("wrote %s (0644)\n", pubPath)
	return nil
}

// writeFile writes data to path with the given permissions, refusing to
// overwrite an existing file to avoid accidental key destruction.
func writeFile(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}
