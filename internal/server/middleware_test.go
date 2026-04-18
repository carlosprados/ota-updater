package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/plgd-dev/go-coap/v3/message"
	"github.com/plgd-dev/go-coap/v3/message/codes"
	"github.com/plgd-dev/go-coap/v3/mux"
	coapnet "github.com/plgd-dev/go-coap/v3/net"
	"github.com/plgd-dev/go-coap/v3/options"
	"github.com/plgd-dev/go-coap/v3/udp"

	"github.com/amplia/ota-updater/pkg/protocol"
)

func TestRecoverHTTP_PanicBecomes500(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	boom := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})
	srv := httptest.NewServer(recoverHTTP(boom, logger))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/x")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", resp.StatusCode)
	}
}

func TestHTTP_HeartbeatBodyTooLarge(t *testing.T) {
	base, _, _, _, done := httpFixture(t)
	defer done()

	big := bytes.NewReader(make([]byte, maxHeartbeatBody+1024))
	resp, err := http.Post(base+protocol.PathHeartbeat, "application/json", big)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("oversize body should not yield 200")
	}
}

func TestRecoverCoAP_PanicBecomes5xx(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := mux.NewRouter()
	r.Use(recoverCoAP(logger))
	_ = r.Handle("/boom", mux.HandlerFunc(func(mux.ResponseWriter, *mux.Message) {
		panic("boom-coap")
	}))

	l, err := coapnet.NewListenUDP("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := udp.NewServer(options.WithMux(r))
	serveDone := make(chan struct{})
	go func() { defer close(serveDone); _ = srv.Serve(l) }()
	defer func() { srv.Stop(); l.Close(); <-serveDone }()

	co, err := udp.Dial(l.LocalAddr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer co.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := co.Get(ctx, "/boom")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.Code() != codes.InternalServerError {
		t.Fatalf("code=%s, want 5.00 InternalServerError", resp.Code())
	}
}

func TestManifester_Cache_HitReturnsSameInstance(t *testing.T) {
	m, _, s, oldHash := manifesterFixture(t)
	if _, err := s.EnsureDelta(context.Background(), oldHash); err != nil {
		t.Fatalf("EnsureDelta: %v", err)
	}
	hb := &protocol.Heartbeat{DeviceID: "dev-1", VersionHash: oldHash}
	r1, err := m.Build(context.Background(), hb)
	if err != nil {
		t.Fatalf("Build 1: %v", err)
	}
	r2, err := m.Build(context.Background(), hb)
	if err != nil {
		t.Fatalf("Build 2: %v", err)
	}
	if r1 != r2 {
		t.Fatalf("cache miss: different pointers returned for identical heartbeat")
	}
	// sanity: response is the signed kind
	if r1.Signature == "" {
		t.Fatalf("expected signed manifest, got empty Signature")
	}
}

func TestManifester_Invalidate_ForcesRebuild(t *testing.T) {
	m, _, s, oldHash := manifesterFixture(t)
	if _, err := s.EnsureDelta(context.Background(), oldHash); err != nil {
		t.Fatalf("EnsureDelta: %v", err)
	}
	hb := &protocol.Heartbeat{DeviceID: "dev-1", VersionHash: oldHash}
	r1, _ := m.Build(context.Background(), hb)
	m.Invalidate()
	r2, _ := m.Build(context.Background(), hb)
	if r1 == r2 {
		t.Fatalf("Invalidate did not force rebuild (same pointer)")
	}
	// Deterministic Ed25519 + same inputs → equal signatures.
	if r1.Signature != r2.Signature {
		t.Fatalf("rebuilt signature differs from cached one")
	}
}

// TestCoAP_HeartbeatInvalidCBOR_RecoverPath documents that malformed CBOR
// is rejected with 4.00 Bad Request rather than panicking the handler.
func TestCoAP_HeartbeatInvalidCBOR_RecoverPath(t *testing.T) {
	addr, _, _, _, done := coapFixture(t, false)
	defer done()

	co := coapDial(t, addr)
	defer co.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// "not-cbor" bytes should fail Unmarshal cleanly.
	resp, err := co.Post(ctx, protocol.PathHeartbeat, message.AppCBOR, strings.NewReader("not-cbor"))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if resp.Code() != codes.BadRequest {
		t.Fatalf("code=%s, want 4.00 BadRequest", resp.Code())
	}
}

// Compile-time sanity that cbor is referenced (keeps the import honest even
// if the suite is trimmed later).
var _ = cbor.Marshal
