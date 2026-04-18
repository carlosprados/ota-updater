package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amplia/ota-updater/pkg/protocol"
)

func adminFixture(t *testing.T) (token string, base string, logging *Logging, s *Store, m *Manifester, oldHash string, done func()) {
	t.Helper()
	token = "s3cret-admin"
	mtr, _, store, oh := manifesterFixture(t)
	logging, err := NewLoggingTo(LoggingConfig{Level: "info", Format: "text"}, io.Discard)
	if err != nil {
		t.Fatalf("NewLoggingTo: %v", err)
	}
	mux := http.NewServeMux()
	RegisterAdminHandlers(mux, AdminDeps{
		Token:      token,
		Store:      store,
		Manifester: mtr,
		Logging:    logging,
		Logger:     logging.Logger(),
	})
	srv := httptest.NewServer(mux)
	return token, srv.URL, logging, store, mtr, oh, srv.Close
}

func TestAdmin_Loglevel_Auth(t *testing.T) {
	token, base, logging, _, _, _, done := adminFixture(t)
	defer done()

	// missing auth → 401
	resp, err := http.Post(base+"/admin/loglevel", "application/json",
		strings.NewReader(`{"level":"debug"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-auth status=%d, want 401", resp.StatusCode)
	}

	// wrong token → 401
	req, _ := http.NewRequest(http.MethodPost, base+"/admin/loglevel",
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
	req, _ = http.NewRequest(http.MethodPost, base+"/admin/loglevel",
		strings.NewReader(`{"level":"debug"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authed status=%d, want 200", resp.StatusCode)
	}
	if logging.Level() != slog.LevelDebug {
		t.Fatalf("level=%v, want Debug", logging.Level())
	}
}

func TestAdmin_Loglevel_UnknownLevel(t *testing.T) {
	token, base, _, _, _, _, done := adminFixture(t)
	defer done()

	req, _ := http.NewRequest(http.MethodPost, base+"/admin/loglevel",
		strings.NewReader(`{"level":"trace"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", resp.StatusCode)
	}
}

func TestAdmin_Reload_InvalidatesCache(t *testing.T) {
	token, base, _, store, mtr, oldHash, done := adminFixture(t)
	defer done()

	// prime the manifest cache via a heartbeat simulation
	if _, err := store.EnsureDelta(context.Background(), oldHash); err != nil {
		t.Fatalf("EnsureDelta: %v", err)
	}
	firstTarget := store.TargetHash()
	_, _ = mtr.Build(context.Background(), &protocol.Heartbeat{
		DeviceID: "dev-1", VersionHash: oldHash,
	})

	// rewrite the target binary with different content so the hash changes
	newContent := append([]byte("new-"), make([]byte, 32<<10)...)
	if err := os.WriteFile(store.opts.TargetPath, newContent, 0o644); err != nil {
		t.Fatalf("write new target: %v", err)
	}

	req, _ := http.NewRequest(http.MethodPost, base+"/admin/reload", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	var body struct {
		TargetHash   string `json:"target_hash"`
		PreviousHash string `json:"previous_hash"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.PreviousHash != firstTarget {
		t.Fatalf("previous_hash=%s, want %s", body.PreviousHash, firstTarget)
	}
	if body.TargetHash == firstTarget {
		t.Fatalf("target_hash did not change after reload")
	}
	if store.TargetHash() != body.TargetHash {
		t.Fatalf("store.TargetHash() out of sync with reload response")
	}
}


func TestAdminRateLimit_429AfterBurst(t *testing.T) {
	// Wire admin with a tiny bucket: 0 refill, burst=3. Once 3 failing
	// requests land, the 4th must return 429.
	token := "the-correct-admin-token-of-32-chars"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	RegisterAdminHandlers(mux, AdminDeps{
		Token:           token,
		Logger:          logger,
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

	// First 3 wrong-token requests return 401 (burst still has tokens).
	for i := 0; i < 3; i++ {
		if got := send("Bearer wrong"); got != 401 {
			t.Fatalf("req %d wrong token: got %d, want 401", i, got)
		}
	}
	// 4th request exhausts the bucket → 429.
	if got := send("Bearer wrong"); got != 429 {
		t.Fatalf("req 4 expected 429 (rate-limited), got %d", got)
	}
}

func TestAdminRateLimit_SuccessfulRequestsNeverCounted(t *testing.T) {
	// With burst=1, if we use the CORRECT token repeatedly, we never hit
	// 429. The limiter only runs on 401-path.
	token := "a-strong-enough-token-of-32-chars"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()

	// We need a Store+Manifester for reload to succeed; minimal fixture:
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "binaries")
	deltaDir := filepath.Join(tmp, "deltas")
	target := filepath.Join(tmp, "target.bin")
	if err := os.WriteFile(target, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := Open(context.Background(), StoreOptions{
		BinariesDir: binDir, DeltasDir: deltaDir, TargetPath: target,
	}, logger)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32))
	_ = pub
	mfr := NewManifester(store, priv, ManifesterConfig{}, logger)

	RegisterAdminHandlers(mux, AdminDeps{
		Token:           token,
		Store:           store,
		Manifester:      mfr,
		Logger:          logger,
		RateLimitPerSec: 0.001,
		RateLimitBurst:  1,
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for i := 0; i < 10; i++ {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/admin/reload", nil)
		req.Header.Set("Authorization", "Bearer "+token)
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
