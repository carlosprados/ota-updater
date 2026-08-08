package server

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/carlosprados/ota-updater/pkg/crypto"
	"github.com/carlosprados/ota-updater/pkg/protocol"
)

// httpFixture sets up a running test server with a pre-cached delta so Range
// and ServeContent paths are exercisable.
func httpFixture(t *testing.T) (base string, f *serverFixture, done func()) {
	t.Helper()
	f = newServerFixture(t)
	if _, err := f.Store.EnsureDelta(context.Background(), f.OldHash, f.TargetHash); err != nil {
		t.Fatalf("EnsureDelta: %v", err)
	}
	return newHTTPTestServer(t, f), f, func() {}
}

// newHTTPTestServer mounts the handler over f and returns its base URL. The
// server is torn down via t.Cleanup so callers cannot forget.
func newHTTPTestServer(t *testing.T, f *serverFixture) string {
	t.Helper()
	srv := httptest.NewServer(NewHTTPHandler(HTTPConfig{
		Store: f.Store, Registry: f.Registry, Manifester: f.Manifester, Logger: testLogger(),
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestHTTP_Heartbeat_Current(t *testing.T) {
	base, f, done := httpFixture(t)
	defer done()

	body, _ := json.Marshal(protocol.Heartbeat{
		DeviceID:    "dev-1",
		VersionHash: f.TargetHash,
		Artifact:    testArtifact.String(),
	})
	resp, err := http.Post(base+protocol.PathHeartbeat, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST heartbeat: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}

	var mr protocol.ManifestResponse
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if mr.UpdateAvailable {
		t.Fatalf("expected UpdateAvailable=false")
	}
}

func TestHTTP_Heartbeat_CachedSignature(t *testing.T) {
	base, f, done := httpFixture(t)
	defer done()

	body, _ := json.Marshal(protocol.Heartbeat{DeviceID: "dev-1", VersionHash: f.OldHash, Artifact: testArtifact.String()})
	resp, err := http.Post(base+protocol.PathHeartbeat, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST heartbeat: %v", err)
	}
	defer resp.Body.Close()

	var mr protocol.ManifestResponse
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !mr.UpdateAvailable || mr.Signature == "" {
		t.Fatalf("expected signed manifest, got %+v", mr)
	}
	payload, err := protocol.ManifestSigningPayload(mr.TargetHash, mr.DeltaHash)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	sig, err := hex.DecodeString(mr.Signature)
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	if err := crypto.Verify(f.Pub, payload, sig); err != nil {
		t.Fatalf("signature verify failed: %v", err)
	}
	wantEndpoint := protocol.DeltaPath(f.OldHash, f.TargetHash)
	if mr.DeltaEndpoint != wantEndpoint {
		t.Fatalf("DeltaEndpoint=%s, want %s", mr.DeltaEndpoint, wantEndpoint)
	}
}

func TestHTTP_Delta_FullDownload(t *testing.T) {
	base, f, done := httpFixture(t)
	defer done()

	url := base + protocol.DeltaPath(f.OldHash, f.TargetHash)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET delta: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type=%q", ct)
	}
	if ar := resp.Header.Get("Accept-Ranges"); ar != "bytes" {
		t.Fatalf("Accept-Ranges=%q, want bytes", ar)
	}
	data, _ := io.ReadAll(resp.Body)
	if len(data) == 0 {
		t.Fatalf("empty body")
	}
}

func TestHTTP_Delta_Range(t *testing.T) {
	base, f, done := httpFixture(t)
	defer done()

	url := base + protocol.DeltaPath(f.OldHash, f.TargetHash)
	// full fetch first to know the expected slice
	full, _ := http.Get(url)
	fullBody, _ := io.ReadAll(full.Body)
	full.Body.Close()

	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Range", "bytes=10-19")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET delta range: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status=%d, want 206", resp.StatusCode)
	}
	chunk, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(chunk, fullBody[10:20]) {
		t.Fatalf("range chunk mismatch")
	}
}

func TestHTTP_Delta_404_TriggersAsync(t *testing.T) {
	// Build a fresh fixture WITHOUT pre-generating the delta so we can
	// verify the 404 path dispatches async generation.
	f := newServerFixture(t)
	url := newHTTPTestServer(t, f) + protocol.DeltaPath(f.OldHash, f.TargetHash)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET delta: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", resp.StatusCode)
	}

	// Async generation should populate the cache shortly.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if f.Store.HasDelta(f.OldHash, f.TargetHash) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("async delta generation did not complete")
}

func TestHTTP_Delta_InvalidHashSegment(t *testing.T) {
	base, _, done := httpFixture(t)
	defer done()

	resp, err := http.Get(base + "/delta/NOTHEX/" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", resp.StatusCode)
	}
}

func TestHTTP_Report(t *testing.T) {
	base, _, done := httpFixture(t)
	defer done()

	body, _ := json.Marshal(protocol.UpdateReport{
		DeviceID: "dev-1", PreviousHash: "p", NewHash: "n", Success: true,
	})
	resp, err := http.Post(base+protocol.PathReport, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST report: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status=%d, want 202", resp.StatusCode)
	}
}

func TestHTTP_Health(t *testing.T) {
	base, f, done := httpFixture(t)
	defer done()

	resp, err := http.Get(base + protocol.PathHealth)
	if err != nil {
		t.Fatalf("GET health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var body struct {
		Status    string            `json:"status"`
		Artifacts map[string]string `json:"artifacts"`
		Default   string            `json:"default"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if body.Artifacts[testArtifact.String()] != f.TargetHash {
		t.Fatalf("health does not report the artifact target: %+v", body)
	}
	if body.Default != testArtifact.String() {
		t.Fatalf("health default = %q, want %q", body.Default, testArtifact)
	}
}
