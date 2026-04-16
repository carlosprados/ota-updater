package server

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/plgd-dev/go-coap/v3/message"
	"github.com/plgd-dev/go-coap/v3/message/codes"
	coapnet "github.com/plgd-dev/go-coap/v3/net"
	"github.com/plgd-dev/go-coap/v3/options"
	"github.com/plgd-dev/go-coap/v3/udp"
	udpClient "github.com/plgd-dev/go-coap/v3/udp/client"

	"github.com/amplia/ota-updater/internal/crypto"
	"github.com/amplia/ota-updater/internal/protocol"
)

// coapFixture spins up an in-process CoAP server on an ephemeral UDP port.
// If preGenerate is true the delta for oldHash is cached before returning.
func coapFixture(t *testing.T, preGenerate bool) (addr string, pub []byte, s *Store, oldHash string, teardown func()) {
	t.Helper()
	m, pubKey, store, oh := manifesterFixture(t)
	if preGenerate {
		if _, err := store.EnsureDelta(context.Background(), oh); err != nil {
			t.Fatalf("EnsureDelta: %v", err)
		}
	}
	router, err := NewCoAPRouter(CoAPConfig{Store: store, Manifester: m})
	if err != nil {
		t.Fatalf("NewCoAPRouter: %v", err)
	}

	l, err := coapnet.NewListenUDP("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	srv := udp.NewServer(options.WithMux(router))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(l)
	}()

	teardown = func() {
		srv.Stop()
		_ = l.Close()
		<-done
	}
	return l.LocalAddr().String(), pubKey, store, oh, teardown
}

func coapDial(t *testing.T, addr string) *udpClient.Conn {
	t.Helper()
	co, err := udp.Dial(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return co
}

func TestCoAP_Heartbeat_Current(t *testing.T) {
	addr, _, s, _, done := coapFixture(t, false)
	defer done()

	co := coapDial(t, addr)
	defer co.Close()

	body, _ := cbor.Marshal(protocol.Heartbeat{
		DeviceID: "dev-1", VersionHash: s.TargetHash(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := co.Post(ctx, protocol.PathHeartbeat, message.AppCBOR, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if resp.Code() != codes.Content {
		t.Fatalf("code=%s, want 2.05 Content", resp.Code())
	}
	payload, _ := resp.ReadBody()
	var mr protocol.ManifestResponse
	if err := cbor.Unmarshal(payload, &mr); err != nil {
		t.Fatalf("cbor decode: %v", err)
	}
	if mr.UpdateAvailable {
		t.Fatalf("expected UpdateAvailable=false")
	}
}

func TestCoAP_Heartbeat_CachedSignature(t *testing.T) {
	addr, pub, s, oldHash, done := coapFixture(t, true)
	defer done()

	co := coapDial(t, addr)
	defer co.Close()

	body, _ := cbor.Marshal(protocol.Heartbeat{DeviceID: "dev-1", VersionHash: oldHash})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := co.Post(ctx, protocol.PathHeartbeat, message.AppCBOR, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	payload, _ := resp.ReadBody()

	var mr protocol.ManifestResponse
	if err := cbor.Unmarshal(payload, &mr); err != nil {
		t.Fatalf("cbor decode: %v", err)
	}
	if !mr.UpdateAvailable || mr.Signature == "" {
		t.Fatalf("expected signed manifest, got %+v", mr)
	}
	if mr.TargetHash != s.TargetHash() {
		t.Fatalf("TargetHash mismatch")
	}

	signingPayload, err := protocol.ManifestSigningPayload(mr.TargetHash, mr.DeltaHash)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	sig, err := hex.DecodeString(mr.Signature)
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	if err := crypto.Verify(pub, signingPayload, sig); err != nil {
		t.Fatalf("signature verify failed (CoAP path): %v", err)
	}
}

func TestCoAP_Delta_FullDownload(t *testing.T) {
	addr, _, s, oldHash, done := coapFixture(t, true)
	defer done()

	co := coapDial(t, addr)
	defer co.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := co.Get(ctx, protocol.DeltaPath(oldHash, s.TargetHash()))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.Code() != codes.Content {
		t.Fatalf("code=%s, want 2.05 Content", resp.Code())
	}
	data, err := io.ReadAll(mustBodyReader(t, resp.Body()))
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("empty delta body")
	}
}

func TestCoAP_Delta_404_TriggersAsync(t *testing.T) {
	addr, _, s, oldHash, done := coapFixture(t, false)
	defer done()

	co := coapDial(t, addr)
	defer co.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := co.Get(ctx, protocol.DeltaPath(oldHash, s.TargetHash()))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.Code() != codes.NotFound {
		t.Fatalf("code=%s, want 4.04 NotFound", resp.Code())
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s.HasDelta(oldHash) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("async delta generation did not complete")
}

func TestCoAP_Report(t *testing.T) {
	addr, _, _, _, done := coapFixture(t, false)
	defer done()

	co := coapDial(t, addr)
	defer co.Close()

	body, _ := cbor.Marshal(protocol.UpdateReport{
		DeviceID: "dev-1", PreviousHash: "p", NewHash: "n", Success: true,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := co.Post(ctx, protocol.PathReport, message.AppCBOR, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if resp.Code() != codes.Changed {
		t.Fatalf("code=%s, want 2.04 Changed", resp.Code())
	}
}

// mustBodyReader asserts that the message has a non-nil body and returns it.
// The go-coap Body() returns io.ReadSeeker which may be nil for zero-length
// responses; not applicable to the delta test, but defensive.
func mustBodyReader(t *testing.T, rs io.ReadSeeker) io.Reader {
	t.Helper()
	if rs == nil {
		t.Fatalf("nil body reader")
	}
	return rs
}
