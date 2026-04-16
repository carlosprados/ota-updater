// Package delta produces and applies zstd-compressed bsdiff patches used to
// transport binary updates over NB-IoT links. Callers are responsible for
// hashing inputs/outputs and verifying the manifest signature — this package
// only handles the diff/patch + compression plumbing.
package delta

import (
	"fmt"

	"github.com/gabstv/go-bsdiff/pkg/bsdiff"

	"github.com/amplia/ota-updater/internal/compression"
)

// Generate produces a zstd-compressed bsdiff patch that transforms oldBin into
// newBin. Suitable for caching on disk as `{from_hash}_{to_hash}.delta.zst`.
func Generate(oldBin, newBin []byte) ([]byte, error) {
	patch, err := bsdiff.Bytes(oldBin, newBin)
	if err != nil {
		return nil, fmt.Errorf("bsdiff: %w", err)
	}
	compressed, err := compression.CompressBytes(patch)
	if err != nil {
		return nil, fmt.Errorf("compress delta: %w", err)
	}
	return compressed, nil
}
