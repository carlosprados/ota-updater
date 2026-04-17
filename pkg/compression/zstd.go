// Package compression provides thin zstd wrappers used by the delta pipeline.
// Deltas are compressed once on the server and decompressed many times on
// agents, so we default to the encoder's best compression level.
package compression

import (
	"bytes"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

// Compress streams zstd-compressed data from src to dst.
func Compress(dst io.Writer, src io.Reader) error {
	enc, err := zstd.NewWriter(dst, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
	if err != nil {
		return fmt.Errorf("zstd encoder: %w", err)
	}
	if _, err := io.Copy(enc, src); err != nil {
		_ = enc.Close()
		return fmt.Errorf("zstd compress: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("zstd close: %w", err)
	}
	return nil
}

// Decompress streams zstd-decompressed data from src to dst.
func Decompress(dst io.Writer, src io.Reader) error {
	dec, err := zstd.NewReader(src)
	if err != nil {
		return fmt.Errorf("zstd decoder: %w", err)
	}
	defer dec.Close()
	if _, err := io.Copy(dst, dec); err != nil {
		return fmt.Errorf("zstd decompress: %w", err)
	}
	return nil
}

// CompressBytes is a buffered convenience wrapper around Compress.
func CompressBytes(in []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := Compress(&buf, bytes.NewReader(in)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DecompressBytes is a buffered convenience wrapper around Decompress.
func DecompressBytes(in []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := Decompress(&buf, bytes.NewReader(in)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
