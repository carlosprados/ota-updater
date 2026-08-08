package main

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/carlosprados/ota-updater/pkg/delta"
)

// nextPow2 returns the smallest power of two >= n, clamped to zstd's max
// window. --patch-from only wins when the whole reference fits in the window.
func nextPow2(n int) int {
	w := 1 << 20
	for w < n && w < (1<<29) {
		w <<= 1
	}
	return w
}

// patchFrom is the pure-Go equivalent of `zstd --patch-from`: compress the new
// content with the old content installed as a raw dictionary and a window big
// enough to reach all of it.
func patchFrom(old, new []byte) ([]byte, error) {
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderDictRaw(1, old),
		zstd.WithWindowSize(nextPow2(len(old))),
		zstd.WithEncoderLevel(zstd.SpeedBestCompression),
	)
	if err != nil {
		return nil, err
	}
	out := enc.EncodeAll(new, nil)
	return out, enc.Close()
}

func patchApply(old, patch []byte) ([]byte, error) {
	dec, err := zstd.NewReader(nil,
		zstd.WithDecoderDictRaw(1, old),
		zstd.WithDecoderMaxWindow(1<<29),
	)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	return dec.DecodeAll(patch, nil)
}

func main() {
	mode := os.Args[1]
	oldBin, err := os.ReadFile("/tmp/pb/"+os.Getenv("PB")+"v0")
	must(err)
	newBin, err := os.ReadFile("/tmp/pb/"+os.Getenv("PB")+"v1")
	must(err)

	start := time.Now()
	switch mode {
	case "gen-bsdiff":
		p, err := delta.Generate(oldBin, newBin)
		must(err)
		report(mode, len(p), start)
		must(os.WriteFile("/tmp/pb/patch.bsdiff", p, 0o644))

	case "gen-zstddict":
		p, err := patchFrom(oldBin, newBin)
		must(err)
		report(mode, len(p), start)
		must(os.WriteFile("/tmp/pb/patch.zstddict", p, 0o644))

	case "apply-bsdiff":
		p, err := os.ReadFile("/tmp/pb/patch.bsdiff")
		must(err)
		out, err := delta.Apply(oldBin, p)
		must(err)
		if !bytes.Equal(out, newBin) {
			panic("bsdiff reconstruction mismatch")
		}
		report(mode, len(out), start)

	case "apply-zstddict":
		p, err := os.ReadFile("/tmp/pb/patch.zstddict")
		must(err)
		out, err := patchApply(oldBin, p)
		must(err)
		if !bytes.Equal(out, newBin) {
			panic("zstddict reconstruction mismatch")
		}
		report(mode, len(out), start)

	default:
		panic("unknown mode " + mode)
	}
}

func report(mode string, size int, start time.Time) {
	fmt.Printf("%-16s bytes=%-10d secs=%.2f\n", mode, size, time.Since(start).Seconds())
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
