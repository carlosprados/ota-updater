package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
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
	if err := os.WriteFile(store.targetPath, newContent, 0o644); err != nil {
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

