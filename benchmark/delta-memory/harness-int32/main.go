package main

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"github.com/gabstv/go-bsdiff/pkg/bspatch"

	bsdiff32 "bsdiff32/pkg"
)

func main() {
	pfx := os.Getenv("PB")
	oldBin, err := os.ReadFile("/tmp/pb/" + pfx + "v0")
	must(err)
	newBin, err := os.ReadFile("/tmp/pb/" + pfx + "v1")
	must(err)

	start := time.Now()
	patch, err := bsdiff32.Bytes(oldBin, newBin)
	must(err)
	gen := time.Since(start)

	// Acid test: the STOCK library must apply this patch and reproduce the
	// target byte for byte. The on-disk format is unchanged; only the
	// internal index width differs.
	out, err := bspatch.Bytes(oldBin, patch)
	must(err)
	if !bytes.Equal(out, newBin) {
		panic("MISMATCH: the int32 fork produced a bad patch")
	}
	fmt.Printf("int32-bsdiff  bytes=%-10d secs=%.2f  roundtrip=OK\n", len(patch), gen.Seconds())
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
