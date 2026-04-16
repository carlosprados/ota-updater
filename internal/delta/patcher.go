package delta

import (
	"fmt"

	"github.com/gabstv/go-bsdiff/pkg/bspatch"

	"github.com/amplia/ota-updater/internal/compression"
)

// Apply reconstructs the target binary from oldBin and a zstd-compressed
// bsdiff patch. Callers must verify the SHA-256 of the result against the
// manifest's TargetHash before activating the new slot.
func Apply(oldBin, compressedPatch []byte) ([]byte, error) {
	patch, err := compression.DecompressBytes(compressedPatch)
	if err != nil {
		return nil, fmt.Errorf("decompress delta: %w", err)
	}
	newBin, err := bspatch.Bytes(oldBin, patch)
	if err != nil {
		return nil, fmt.Errorf("bspatch: %w", err)
	}
	return newBin, nil
}
