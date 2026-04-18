// Command deltabench measures bsdiff vs librsync on synthetic binaries.
//
// Each invocation runs ONE combo (algo × size × pattern) in a fresh process,
// so /proc/self/status VmHWM captures the peak RSS of that combo alone.
// The companion bench.sh orchestrates the matrix and formats results.
//
// Output is a single tab-separated line on stdout (machine-readable) plus
// human logging on stderr:
//
//	algo size_bytes pattern seed delta_bytes peak_rss_kb wall_ms
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/balena-os/librsync-go"
	"github.com/gabstv/go-bsdiff/pkg/bsdiff"
	"github.com/klauspost/compress/zstd"
)

func main() {
	algo := flag.String("algo", "bsdiff", "bsdiff|librsync")
	sizeMB := flag.Int("size-mb", 1, "synthetic binary size in MiB (ignored when -real-base is set)")
	pattern := flag.String("pattern", "small", "small (0.5% scattered) | large (10% contiguous)")
	seed := flag.Int64("seed", 42, "PRNG seed for reproducibility")
	blockLen := flag.Uint("block", 2048, "librsync block length")
	strongLen := flag.Uint("strong", 32, "librsync strong hash length")
	realBase := flag.String("real-base", "", "path to a real binary to use as 'old'; disables synthetic generation")
	realTarget := flag.String("real-target", "", "path to a real binary to use as 'new'; required with -real-base")
	flag.Parse()

	log.SetFlags(0)
	log.SetPrefix("deltabench: ")
	log.SetOutput(os.Stderr)

	var (
		oldBin, newBin []byte
		sizeBytes      int
		label          string
	)
	if *realBase != "" {
		if *realTarget == "" {
			log.Fatalf("-real-target is required when -real-base is set")
		}
		var err error
		oldBin, err = os.ReadFile(*realBase)
		if err != nil {
			log.Fatalf("read real-base: %v", err)
		}
		newBin, err = os.ReadFile(*realTarget)
		if err != nil {
			log.Fatalf("read real-target: %v", err)
		}
		sizeBytes = len(oldBin)
		label = "real"
		log.Printf("loaded real pair: old=%s (%s) new=%s (%s)",
			*realBase, humanBytes(int64(len(oldBin))), *realTarget, humanBytes(int64(len(newBin))))
	} else {
		sizeBytes = *sizeMB * 1 << 20
		oldBin, newBin = generatePair(sizeBytes, *pattern, *seed)
		label = "synthetic-" + *pattern
		log.Printf("generated pair: size=%dMB pattern=%s seed=%d", *sizeMB, *pattern, *seed)
	}

	var deltaCompressed []byte
	var wall time.Duration

	switch *algo {
	case "bsdiff":
		deltaCompressed, wall = runBsdiff(oldBin, newBin)
	case "librsync":
		deltaCompressed, wall = runLibrsync(oldBin, newBin, uint32(*blockLen), uint32(*strongLen))
	default:
		log.Fatalf("unknown algo %q", *algo)
	}

	peakKB := readVmHWM()

	// Single line, tab-separated, for bench.sh to parse.
	fmt.Printf("%s\t%d\t%s\t%d\t%d\t%d\t%d\n",
		*algo, sizeBytes, label, *seed, len(deltaCompressed), peakKB, wall.Milliseconds())

	log.Printf("delta=%s (%.2f%% of base) peak_rss=%s wall=%s",
		humanBytes(int64(len(deltaCompressed))),
		100*float64(len(deltaCompressed))/float64(len(oldBin)),
		humanKB(peakKB),
		wall,
	)
}

// generatePair produces two size-byte binaries that share most bytes AND
// have structure that realistically represents firmware:
//
// The base is built from 4 KiB blocks where ~70% of the blocks are duplicates
// of one of 64 "template" blocks (simulates repetitive structure of real
// ELF/Go binaries — repeated strings, opcode tables, constant pools, zero
// paddings). The remaining ~30% of blocks are unique pseudo-random content
// (simulates per-function unique code). This gives the rolling-hash algorithm
// rsync uses a fair chance to find matches, which pure random bytes don't.
//
//   - small: ~0.5% of bytes are flipped at scattered positions inside ONE
//     template block's instance. Simulates a point-fix firmware change.
//   - large: a ~10% contiguous block-range is replaced with fresh content.
//     Simulates a feature-level change (whole subsystem rewritten).
//
// The base is deterministic given seed.
func generatePair(size int, pattern string, seed int64) (old, new_ []byte) {
	const blockSize = 4096
	const numTemplates = 64
	const templateFreq = 0.70 // 70% of blocks reuse a template

	r := rand.New(rand.NewSource(seed))

	// Generate the template pool.
	templates := make([][]byte, numTemplates)
	for i := range templates {
		templates[i] = make([]byte, blockSize)
		if _, err := io.ReadFull(r, templates[i]); err != nil {
			log.Fatalf("fill template: %v", err)
		}
	}

	// Assemble the base by sampling blocks.
	blocks := (size + blockSize - 1) / blockSize
	old = make([]byte, 0, blocks*blockSize)
	for i := 0; i < blocks; i++ {
		if r.Float64() < templateFreq {
			// Reuse a template. To add minor variation, XOR ~1% of its bytes
			// with random bytes — still very matchy for rolling hashes.
			blk := append([]byte(nil), templates[r.Intn(numTemplates)]...)
			for j := 0; j < blockSize/100; j++ {
				off := r.Intn(blockSize)
				blk[off] ^= byte(r.Intn(256))
			}
			old = append(old, blk...)
		} else {
			// Unique random content.
			blk := make([]byte, blockSize)
			if _, err := io.ReadFull(r, blk); err != nil {
				log.Fatalf("fill unique block: %v", err)
			}
			old = append(old, blk...)
		}
	}
	old = old[:size]
	new_ = append([]byte(nil), old...)

	switch pattern {
	case "small":
		// Flip 0.5% of bytes at scattered offsets.
		n := size / 200
		for i := 0; i < n; i++ {
			off := r.Intn(size)
			new_[off] ^= byte(r.Intn(256))
		}
	case "large":
		// Replace a 10% contiguous byte range in the middle with content drawn
		// from a DIFFERENT mix of templates (still structured, but clearly
		// distinct from the old region).
		blkLen := size / 10
		start := size/2 - blkLen/2
		for off := start; off < start+blkLen; {
			tpl := templates[r.Intn(numTemplates)]
			n := blockSize
			if off+n > start+blkLen {
				n = start + blkLen - off
			}
			copy(new_[off:off+n], tpl[:n])
			off += n
		}
	default:
		log.Fatalf("unknown pattern %q", pattern)
	}
	return old, new_
}

// runBsdiff produces the zstd-compressed bsdiff patch (matches production).
func runBsdiff(old, new_ []byte) ([]byte, time.Duration) {
	start := time.Now()
	patch, err := bsdiff.Bytes(old, new_)
	if err != nil {
		log.Fatalf("bsdiff: %v", err)
	}
	compressed := zstdCompress(patch)
	return compressed, time.Since(start)
}

// runLibrsync produces the zstd-compressed rsync delta: signature of old +
// delta against new. We measure ONLY the server-side generation cost (Signature
// + Delta). Signature computation is reported as part of the wall time —
// that's the fair comparison vs bsdiff.Bytes which produces a patch end-to-end.
func runLibrsync(old, new_ []byte, blockLen, strongLen uint32) ([]byte, time.Duration) {
	start := time.Now()

	var sigBuf bytes.Buffer
	if _, err := librsync.Signature(bytes.NewReader(old), &sigBuf, blockLen, strongLen, librsync.BLAKE2_SIG_MAGIC); err != nil {
		log.Fatalf("librsync.Signature: %v", err)
	}
	sig, err := librsync.ReadSignature(bufio.NewReader(bytes.NewReader(sigBuf.Bytes())))
	if err != nil {
		log.Fatalf("librsync.ReadSignature: %v", err)
	}
	var deltaBuf bytes.Buffer
	if err := librsync.Delta(sig, bytes.NewReader(new_), &deltaBuf); err != nil {
		log.Fatalf("librsync.Delta: %v", err)
	}
	compressed := zstdCompress(deltaBuf.Bytes())
	return compressed, time.Since(start)
}

func zstdCompress(b []byte) []byte {
	var out bytes.Buffer
	enc, err := zstd.NewWriter(&out, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		log.Fatalf("zstd writer: %v", err)
	}
	if _, err := enc.Write(b); err != nil {
		log.Fatalf("zstd write: %v", err)
	}
	if err := enc.Close(); err != nil {
		log.Fatalf("zstd close: %v", err)
	}
	return out.Bytes()
}

// readVmHWM returns the process high-water-mark RSS in KiB from
// /proc/self/status. Linux only.
func readVmHWM() int64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		log.Printf("read /proc/self/status: %v", err)
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VmHWM:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb
	}
	return 0
}

func humanBytes(b int64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.2f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.2f KiB", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%d B", b)
}

func humanKB(kb int64) string {
	return humanBytes(kb * 1024)
}
