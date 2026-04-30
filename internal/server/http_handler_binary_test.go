package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// binaryFixture stands up a tiny handler with only the binary endpoint
// wired. Heartbeat/delta etc. are nil because we only exercise /binaries.
func binaryFixture(t *testing.T) (baseURL, dir string, close func()) {
	t.Helper()
	dir = t.TempDir()
	h := NewHTTPHandler(HTTPConfig{BinariesDir: dir})
	srv := httptest.NewServer(h)
	return srv.URL, dir, srv.Close
}

func TestHTTP_Binary_Served(t *testing.T) {
	base, dir, done := binaryFixture(t)
	defer done()

	want := []byte("hello-binary-payload")
	if err := os.WriteFile(filepath.Join(dir, "myapp-1.0.0"), want, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	resp, err := http.Get(base + "/binaries/myapp-1.0.0")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type=%q, want application/octet-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control=%q, want no-store", cc)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("body=%q, want %q", got, want)
	}
}

func TestHTTP_Binary_RangeSupported(t *testing.T) {
	base, dir, done := binaryFixture(t)
	defer done()

	full := []byte("0123456789ABCDEF")
	if err := os.WriteFile(filepath.Join(dir, "rangeable.bin"), full, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, base+"/binaries/rangeable.bin", nil)
	req.Header.Set("Range", "bytes=4-9")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status=%d, want 206", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != "456789" {
		t.Fatalf("body=%q, want 456789", got)
	}
}

func TestHTTP_Binary_NotFound(t *testing.T) {
	base, _, done := binaryFixture(t)
	defer done()

	resp, err := http.Get(base + "/binaries/nope")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", resp.StatusCode)
	}
}

func TestHTTP_Binary_RejectsTraversalAndInvalidNames(t *testing.T) {
	base, dir, done := binaryFixture(t)
	defer done()

	// Drop a "secret" outside the binaries dir at a path the test will try
	// to reach via traversal. If sanitisation works, the GET resolves to 404
	// and the file is never read.
	parent := filepath.Dir(dir)
	secret := filepath.Join(parent, "secret.txt")
	if err := os.WriteFile(secret, []byte("must-not-leak"), 0o644); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(secret) })

	cases := []struct {
		name string
		path string // raw URL path (not encoded by net/url)
	}{
		{"dot-dot", "/binaries/..%2Fsecret.txt"},
		{"dot-dot-slash", "/binaries/..%2F..%2Fsecret.txt"},
		{"absolute", "/binaries/%2Fetc%2Fpasswd"},
		{"hidden-dot", "/binaries/.hidden"},
		{"with-slash", "/binaries/sub%2Ffile"},
		{"empty", "/binaries/"},
		{"with-space", "/binaries/has%20space"},
		{"with-tilde", "/binaries/~root"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(base + tc.path)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status=%d, want 404 for %q. body=%q", resp.StatusCode, tc.path, body)
			}
		})
	}
}

func TestHTTP_Binary_RejectsDirectoryTarget(t *testing.T) {
	base, dir, done := binaryFixture(t)
	defer done()

	if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	resp, err := http.Get(base + "/binaries/subdir")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (directory should not be served)", resp.StatusCode)
	}
}

func TestHTTP_Binary_RejectsNonGet(t *testing.T) {
	base, _, done := binaryFixture(t)
	defer done()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			req, _ := http.NewRequest(method, base+"/binaries/whatever", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s: %v", method, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("status=%d, want 405", resp.StatusCode)
			}
		})
	}
}

func TestHTTP_Binary_NotRegisteredWhenDirEmpty(t *testing.T) {
	// When BinariesDir is empty, the route is not wired.
	h := NewHTTPHandler(HTTPConfig{})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/binaries/anything")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	// http.ServeMux returns 404 for unmatched routes, which is the expected
	// "feature off" behaviour.
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 when BinariesDir is empty", resp.StatusCode)
	}
}
