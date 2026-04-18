package delta

import (
	"crypto/sha256"
	"math/rand"
	"testing"
)

// TestGenerateApplyRoundTrip validates the full delta pipeline (bsdiff → zstd
// → bspatch) against a pseudo-random "old" binary mutated into a "new" one.
// This is the primary confidence check for the go-bsdiff dependency.
func TestGenerateApplyRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	oldBin := make([]byte, 1<<20) // 1 MiB
	if _, err := rng.Read(oldBin); err != nil {
		t.Fatalf("rng: %v", err)
	}

	newBin := make([]byte, len(oldBin))
	copy(newBin, oldBin)
	for i := 0; i < len(newBin); i += 100 { // mutate ~1% of bytes
		newBin[i] ^= 0xAB
	}
	newBin = append(newBin, []byte("PATCH-TRAILING-DATA")...)

	delta, err := Generate(oldBin, newBin)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	t.Logf("oldBin=%d newBin=%d delta(zstd)=%d ratio=%.2f%%",
		len(oldBin), len(newBin), len(delta),
		float64(len(delta))/float64(len(newBin))*100)

	reconstructed, err := Apply(oldBin, delta)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if sha256.Sum256(reconstructed) != sha256.Sum256(newBin) {
		t.Fatalf("reconstructed hash does not match newBin")
	}
}
