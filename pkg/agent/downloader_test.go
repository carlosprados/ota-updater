package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plgd-dev/go-coap/v3/message"
	"github.com/plgd-dev/go-coap/v3/message/codes"
	"github.com/plgd-dev/go-coap/v3/mux"
	coapnet "github.com/plgd-dev/go-coap/v3/net"
	"github.com/plgd-dev/go-coap/v3/options"
	"github.com/plgd-dev/go-coap/v3/udp"
)

func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func silentLogger() *slog.Logger {
	if os.Getenv("DOWNLOADER_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestDownloader_HTTP_HappyPath(t *testing.T) {
	payload := bytes.Repeat([]byte("AB"), 1024) // 2 KiB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "delta", time.Now(), bytes.NewReader(payload))
	}))
	defer srv.Close()

	dir := t.TempDir()
	d := NewDownloader(NewHTTPTransport(nil), DownloaderConfig{
		StatePath:    filepath.Join(dir, ".state"),
		MaxRetries:   2,
		RetryBackoff: 10 * time.Millisecond,
	}, silentLogger())

	out := filepath.Join(dir, "delta.bin")
	err := d.Download(t.Context(), FetchTarget{
		URL: srv.URL + "/delta/x/y", DeltaHash: hashHex(payload),
		TotalSize: int64(len(payload)), OutPath: out,
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(out)
	if !bytes.Equal(got, payload) {
		t.Fatalf("content mismatch")
	}
	if _, err := os.Stat(filepath.Join(dir, ".state")); !os.IsNotExist(err) {
		t.Fatalf("state file still present after success")
	}
}

// TestDownloader_HTTP_ResumesFromStateFile pre-seeds a valid partial file and
// state so the Downloader must issue a Range request and finish from there.
func TestDownloader_HTTP_ResumesFromStateFile(t *testing.T) {
	payload := bytes.Repeat([]byte("DATA"), 2048) // 8 KiB
	half := int64(len(payload) / 2)

	dir := t.TempDir()
	tmpPath := filepath.Join(dir, "delta.bin.partial")
	if err := os.WriteFile(tmpPath, payload[:half], 0o644); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, ".state")
	stateJSON := fmt.Sprintf(
		`{"delta_hash":%q,"bytes_received":%d,"temp_file":%q,"transport":"http"}`,
		hashHex(payload), half, tmpPath,
	)
	if err := os.WriteFile(statePath, []byte(stateJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	var gotRange atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange.Store(r.Header.Get("Range"))
		var offset int64
		if rh := r.Header.Get("Range"); rh != "" {
			_, _ = fmt.Sscanf(rh, "bytes=%d-", &offset)
		}
		remaining := payload[offset:]
		w.Header().Set("Content-Length", strconv.Itoa(len(remaining)))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d",
			offset, len(payload)-1, len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(remaining)
	}))
	defer srv.Close()

	d := NewDownloader(NewHTTPTransport(nil), DownloaderConfig{
		StatePath:    statePath,
		MaxRetries:   0,
		RetryBackoff: 5 * time.Millisecond,
	}, silentLogger())

	err := d.Download(t.Context(), FetchTarget{
		URL: srv.URL + "/delta/x/y", DeltaHash: hashHex(payload),
		OutPath: filepath.Join(dir, "delta.bin"),
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	rh, _ := gotRange.Load().(string)
	if !strings.HasPrefix(rh, "bytes=") {
		t.Fatalf("expected Range request, got %q", rh)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "delta.bin"))
	if !bytes.Equal(got, payload) {
		t.Fatalf("final content mismatch (%d vs %d)", len(got), len(payload))
	}
}

func TestDownloader_HashMismatch(t *testing.T) {
	payload := []byte("wrong-content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	dir := t.TempDir()
	d := NewDownloader(NewHTTPTransport(nil), DownloaderConfig{
		StatePath: filepath.Join(dir, ".state"), MaxRetries: 0,
	}, silentLogger())

	err := d.Download(t.Context(), FetchTarget{
		URL: srv.URL, DeltaHash: hashHex([]byte("different")),
		OutPath: filepath.Join(dir, "delta.bin"),
	})
	if err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("expected hash mismatch, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "delta.bin")); !os.IsNotExist(err) {
		t.Fatalf("leftover final file after hash failure")
	}
}

func TestDownloader_ServerIgnoresRange_RestartsFromZero(t *testing.T) {
	payload := bytes.Repeat([]byte("Z"), 2048)

	dir := t.TempDir()
	statePath := filepath.Join(dir, ".state")
	tmpPath := filepath.Join(dir, "delta.bin.partial")
	_ = os.WriteFile(tmpPath, []byte("garbage"), 0o644)
	_ = os.WriteFile(statePath,
		[]byte(fmt.Sprintf(`{"delta_hash":%q,"bytes_received":7,"temp_file":%q,"transport":"http"}`,
			hashHex(payload), tmpPath)),
		0o644)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.WriteHeader(http.StatusOK) // ignore Range
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	d := NewDownloader(NewHTTPTransport(nil), DownloaderConfig{
		StatePath: statePath, MaxRetries: 2, RetryBackoff: 5 * time.Millisecond,
	}, silentLogger())

	err := d.Download(t.Context(), FetchTarget{
		URL: srv.URL, DeltaHash: hashHex(payload),
		OutPath: filepath.Join(dir, "delta.bin"),
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "delta.bin"))
	if !bytes.Equal(got, payload) {
		t.Fatalf("content mismatch after 200-ignores-range path")
	}
}

func TestDownloader_ResumeUnsupported_RestartsFromZero(t *testing.T) {
	payload := bytes.Repeat([]byte("X"), 512)
	trans := &testTransport{payload: payload}

	dir := t.TempDir()
	statePath := filepath.Join(dir, ".state")
	tmpPath := filepath.Join(dir, "delta.bin.partial")
	_ = os.WriteFile(tmpPath, []byte("junk"), 0o644)
	_ = os.WriteFile(statePath,
		[]byte(fmt.Sprintf(`{"delta_hash":%q,"bytes_received":4,"temp_file":%q,"transport":"fake"}`,
			hashHex(payload), tmpPath)),
		0o644)

	d := NewDownloader(trans, DownloaderConfig{
		StatePath: statePath, MaxRetries: 2, RetryBackoff: 5 * time.Millisecond,
	}, silentLogger())

	err := d.Download(t.Context(), FetchTarget{
		URL: "fake://ignored", DeltaHash: hashHex(payload),
		OutPath: filepath.Join(dir, "delta.bin"),
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if trans.rejected.Load() != 1 {
		t.Fatalf("expected exactly 1 ResumeUnsupported rejection, got %d", trans.rejected.Load())
	}
	if trans.servedAt0.Load() != 1 {
		t.Fatalf("expected exactly 1 offset=0 serve, got %d", trans.servedAt0.Load())
	}
}

// testTransport is a deterministic DeltaTransport used by the resume-fallback
// test. It refuses any non-zero offset with ErrResumeUnsupported, otherwise
// returns the full payload.
type testTransport struct {
	payload   []byte
	rejected  atomic.Int32
	servedAt0 atomic.Int32
}

func (t *testTransport) Name() string { return "fake" }

func (t *testTransport) FetchRange(_ context.Context, _ string, offset int64) (io.ReadCloser, int64, error) {
	if offset > 0 {
		t.rejected.Add(1)
		return nil, 0, ErrResumeUnsupported
	}
	t.servedAt0.Add(1)
	return io.NopCloser(bytes.NewReader(t.payload)), 0, nil
}

func TestDownloader_CoAP_HappyPath(t *testing.T) {
	payload := bytes.Repeat([]byte("CB"), 512) // 1 KiB

	r := mux.NewRouter()
	_ = r.Handle("/delta/x/y", mux.HandlerFunc(func(w mux.ResponseWriter, _ *mux.Message) {
		_ = w.SetResponse(codes.Content, message.AppOctets, bytes.NewReader(payload))
	}))
	l, err := coapnet.NewListenUDP("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := udp.NewServer(options.WithMux(r))
	go func() { _ = srv.Serve(l) }()
	defer func() { srv.Stop(); _ = l.Close() }()
	addr := l.LocalAddr().String()

	dir := t.TempDir()
	d := NewDownloader(NewCoAPTransport(0), DownloaderConfig{
		StatePath: filepath.Join(dir, ".state"), MaxRetries: 1,
		RetryBackoff: 10 * time.Millisecond,
	}, silentLogger())

	err = d.Download(t.Context(), FetchTarget{
		URL:       "coap://" + addr + "/delta/x/y",
		DeltaHash: hashHex(payload), TotalSize: int64(len(payload)),
		OutPath: filepath.Join(dir, "delta.bin"),
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "delta.bin"))
	if !bytes.Equal(got, payload) {
		t.Fatalf("content mismatch")
	}
}
