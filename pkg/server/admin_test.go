package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carlosprados/ota-updater/pkg/protocol"
)

// adminFixture wires the admin mux over a serverFixture whose artifact is
// backed by a real file, so POST /admin/reload has something to re-read.
type adminFixture struct {
	*serverFixture
	Token      string
	Base       string
	Logging    *Logging
	Retention  *Retention
	TargetFile string
}

func newAdminFixture(t *testing.T) *adminFixture {
	t.Helper()
	f := newServerFixture(t)

	// Re-publish the artifact from a file so it becomes file-backed.
	targetFile := filepath.Join(t.TempDir(), "target.bin")
	if err := os.WriteFile(targetFile, f.TargetBin, 0o644); err != nil {
		t.Fatalf("write target file: %v", err)
	}
	if _, err := f.Registry.PublishFile(testArtifact, "1.1.0", targetFile); err != nil {
		t.Fatalf("PublishFile: %v", err)
	}

	logging, err := NewLoggingTo(LoggingConfig{Level: "info", Format: "text"}, io.Discard)
	if err != nil {
		t.Fatalf("NewLoggingTo: %v", err)
	}
	ret := NewRetention(f.Store, f.Registry, RetentionOptions{}, testLogger())

	const token = "an-admin-token-of-at-least-32-chars"
	mux := http.NewServeMux()
	RegisterAdminHandlers(mux, AdminDeps{
		Token:      token,
		Store:      f.Store,
		Registry:   f.Registry,
		Manifester: f.Manifester,
		Retention:  ret,
		Logging:    logging,
		Logger:     logging.Logger(),
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &adminFixture{
		serverFixture: f, Token: token, Base: srv.URL,
		Logging: logging, Retention: ret, TargetFile: targetFile,
	}
}

// do issues an authenticated admin request.
func (a *adminFixture) do(t *testing.T, method, path, body string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, a.Base+path, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func TestAdmin_Loglevel_Auth(t *testing.T) {
	a := newAdminFixture(t)

	// missing auth → 401
	resp, err := http.Post(a.Base+"/admin/loglevel", "application/json",
		strings.NewReader(`{"level":"debug"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-auth status=%d, want 401", resp.StatusCode)
	}

	// wrong token → 401
	req, _ := http.NewRequest(http.MethodPost, a.Base+"/admin/loglevel",
		strings.NewReader(`{"level":"debug"}`))
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong-token status=%d, want 401", resp.StatusCode)
	}

	// correct token → 200 + level changes
	resp = a.do(t, http.MethodPost, "/admin/loglevel", `{"level":"debug"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authed status=%d, want 200", resp.StatusCode)
	}
	if a.Logging.Level() != slog.LevelDebug {
		t.Fatalf("level=%v, want Debug", a.Logging.Level())
	}
}

func TestAdmin_Loglevel_UnknownLevel(t *testing.T) {
	a := newAdminFixture(t)
	resp := a.do(t, http.MethodPost, "/admin/loglevel", `{"level":"trace"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", resp.StatusCode)
	}
}

func TestAdmin_Reload_PicksUpNewTarget(t *testing.T) {
	a := newAdminFixture(t)
	firstTarget := a.TargetHash

	// Rewrite the source file with different content so the hash changes.
	newContent := append([]byte("new-"), bytes.Repeat([]byte("x"), 32<<10)...)
	if err := os.WriteFile(a.TargetFile, newContent, 0o644); err != nil {
		t.Fatalf("write new target: %v", err)
	}

	resp := a.do(t, http.MethodPost, "/admin/reload", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	var body struct {
		Reloaded map[string]string `json:"reloaded"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := body.Reloaded[testArtifact.String()]
	if got == "" {
		t.Fatalf("reload response does not mention the artifact: %+v", body)
	}
	if got == firstTarget {
		t.Fatalf("target hash did not change after reload")
	}
	if got != sha256HexOf(newContent) {
		t.Fatalf("reloaded hash does not match the new file content")
	}

	art, err := a.Registry.Get(testArtifact)
	if err != nil {
		t.Fatalf("Get after reload: %v", err)
	}
	if art.TargetHash != got {
		t.Fatalf("registry out of sync with the reload response")
	}
	// The superseded target must be retained as a delta source.
	if len(art.History) == 0 || art.History[0] != firstTarget {
		t.Fatalf("previous target %s not recorded in history %v", firstTarget, art.History)
	}
}

// A per-artifact reload must leave the other artifacts untouched, which is
// what lets one component's CI pipeline deploy without disturbing the rest.
func TestAdmin_Reload_SingleArtifact(t *testing.T) {
	a := newAdminFixture(t)
	other := protocol.ArtifactKey{Name: "sidecar", OS: "linux", Arch: "amd64"}
	otherFile := filepath.Join(t.TempDir(), "sidecar.bin")
	if err := os.WriteFile(otherFile, []byte("sidecar-v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Registry.PublishFile(other, "0.1.0", otherFile); err != nil {
		t.Fatalf("publish sidecar: %v", err)
	}
	sidecarBefore, _ := a.Registry.Get(other)

	// Change BOTH files but reload only the main artifact.
	if err := os.WriteFile(a.TargetFile, []byte("main-v2-content-here"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherFile, []byte("sidecar-v2"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp := a.do(t, http.MethodPost, "/admin/reload",
		`{"artifact":"`+testArtifact.String()+`"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}

	sidecarAfter, _ := a.Registry.Get(other)
	if sidecarAfter.TargetHash != sidecarBefore.TargetHash {
		t.Fatalf("targeted reload disturbed an unrelated artifact")
	}
	main, _ := a.Registry.Get(testArtifact)
	if main.TargetHash == a.TargetHash {
		t.Fatalf("targeted reload did not update the requested artifact")
	}
}

func TestAdmin_Reload_UnknownArtifact(t *testing.T) {
	a := newAdminFixture(t)
	resp := a.do(t, http.MethodPost, "/admin/reload", `{"artifact":"ghost/linux/arm64"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", resp.StatusCode)
	}
}

func TestAdmin_Artifacts_PublishListDelete(t *testing.T) {
	a := newAdminFixture(t)

	binPath := filepath.Join(t.TempDir(), "newcomp.bin")
	if err := os.WriteFile(binPath, []byte("newcomp-v1-payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp := a.do(t, http.MethodPost, "/admin/artifacts",
		`{"name":"newcomp","os":"linux","arch":"arm64","version":"2.0.0","binary":"`+binPath+`"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("publish status=%d, want 200", resp.StatusCode)
	}
	var published Artifact
	if err := json.NewDecoder(resp.Body).Decode(&published); err != nil {
		t.Fatalf("decode publish: %v", err)
	}
	if published.Version != "2.0.0" || published.Key.Name != "newcomp" {
		t.Fatalf("unexpected published artifact: %+v", published)
	}

	listResp := a.do(t, http.MethodGet, "/admin/artifacts", "")
	defer listResp.Body.Close()
	var list struct {
		Artifacts []Artifact `json:"artifacts"`
		Default   string     `json:"default"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Artifacts) != 2 {
		t.Fatalf("expected 2 artifacts, got %d", len(list.Artifacts))
	}

	delResp := a.do(t, http.MethodDelete, "/admin/artifacts?artifact=newcomp/linux/arm64", "")
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status=%d, want 204", delResp.StatusCode)
	}
	if _, err := a.Registry.Get(protocol.ArtifactKey{Name: "newcomp", OS: "linux", Arch: "arm64"}); err == nil {
		t.Fatalf("artifact still registered after delete")
	}
}

func TestAdmin_Artifacts_PublishRejectsBadInput(t *testing.T) {
	a := newAdminFixture(t)
	cases := []struct {
		name string
		body string
	}{
		{"missing binary", `{"name":"x","os":"linux","arch":"arm64"}`},
		{"bad key charset", `{"name":"bad name","binary":"/tmp/x"}`},
		{"half-specified platform", `{"name":"x","os":"linux","binary":"/tmp/x"}`},
		{"traversal in name", `{"name":"..","binary":"/tmp/x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := a.do(t, http.MethodPost, "/admin/artifacts", tc.body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400", resp.StatusCode)
			}
		})
	}
}

func TestAdmin_Artifacts_PublishMissingFileIs400(t *testing.T) {
	a := newAdminFixture(t)
	resp := a.do(t, http.MethodPost, "/admin/artifacts",
		`{"name":"ghost","binary":"/definitely/not/here.bin"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (operator error, not a server fault)", resp.StatusCode)
	}
}

func TestAdmin_Default_SwitchesResolution(t *testing.T) {
	a := newAdminFixture(t)
	other := protocol.ArtifactKey{Name: "other", OS: "linux", Arch: "amd64"}
	if _, err := a.Registry.PublishBytes(other, "3.0.0", []byte("other-payload")); err != nil {
		t.Fatalf("publish other: %v", err)
	}

	resp := a.do(t, http.MethodPost, "/admin/default", `{"artifact":"`+other.String()+`"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	art, err := a.Registry.Resolve("")
	if err != nil {
		t.Fatalf("Resolve(\"\"): %v", err)
	}
	if art.Key != other {
		t.Fatalf("default resolved to %v, want %v", art.Key, other)
	}
}

func TestAdmin_Default_UnknownArtifact(t *testing.T) {
	a := newAdminFixture(t)
	resp := a.do(t, http.MethodPost, "/admin/default", `{"artifact":"ghost"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", resp.StatusCode)
	}
}

func TestAdmin_GC_ReturnsStats(t *testing.T) {
	a := newAdminFixture(t)
	// Generate a delta toward a target that is about to be superseded, so
	// the sweep has something unambiguously collectable.
	if _, err := a.Store.EnsureDelta(context.Background(), a.OldHash, a.TargetHash); err != nil {
		t.Fatalf("EnsureDelta: %v", err)
	}
	if _, err := a.Registry.PublishBytes(testArtifact, "1.2.0", []byte("brand-new-target")); err != nil {
		t.Fatalf("publish new target: %v", err)
	}

	resp := a.do(t, http.MethodPost, "/admin/gc", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	var stats RetentionStats
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if stats.DeltasDeleted != 1 {
		t.Fatalf("DeltasDeleted=%d, want 1 (delta toward a superseded target)", stats.DeltasDeleted)
	}
	if stats.BytesReclaimed <= 0 {
		t.Fatalf("BytesReclaimed=%d, want > 0", stats.BytesReclaimed)
	}
}

func TestAdmin_GC_NotConfigured(t *testing.T) {
	f := newServerFixture(t)
	const token = "an-admin-token-of-at-least-32-chars"
	mux := http.NewServeMux()
	RegisterAdminHandlers(mux, AdminDeps{
		Token: token, Store: f.Store, Registry: f.Registry,
		Manifester: f.Manifester, Logger: testLogger(),
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/admin/gc", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 when retention is disabled", resp.StatusCode)
	}
}

func TestAdminRateLimit_429AfterBurst(t *testing.T) {
	// Wire admin with a tiny bucket: ~0 refill, burst=3. Once 3 failing
	// requests land, the 4th must return 429.
	token := "the-correct-admin-token-of-32-chars"
	mux := http.NewServeMux()
	RegisterAdminHandlers(mux, AdminDeps{
		Token:           token,
		Logger:          testLogger(),
		RateLimitPerSec: 0.001, // effectively no refill within the test window
		RateLimitBurst:  3,
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	send := func(auth string) int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/admin/reload", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	for i := 0; i < 3; i++ {
		if got := send("Bearer wrong"); got != 401 {
			t.Fatalf("req %d wrong token: got %d, want 401", i, got)
		}
	}
	if got := send("Bearer wrong"); got != 429 {
		t.Fatalf("req 4 expected 429 (rate-limited), got %d", got)
	}
}

func TestAdminRateLimit_SuccessfulRequestsNeverCounted(t *testing.T) {
	// With burst=1, repeated CORRECT-token requests must never hit 429: the
	// limiter only runs on the 401 path.
	a := newAdminFixture(t)

	// Rebuild the mux with a near-zero refill rate and burst of 1.
	mux := http.NewServeMux()
	RegisterAdminHandlers(mux, AdminDeps{
		Token:           a.Token,
		Store:           a.Store,
		Registry:        a.Registry,
		Manifester:      a.Manifester,
		Logger:          testLogger(),
		RateLimitPerSec: 0.001,
		RateLimitBurst:  1,
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for i := 0; i < 10; i++ {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/admin/reload", nil)
		req.Header.Set("Authorization", "Bearer "+a.Token)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("req %d: got %d, want 200 (legitimate requests never throttle)",
				i, resp.StatusCode)
		}
	}
}
