package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

// httpRoundTripFixture spins up an httptest server with hand-rolled handlers
// for /heartbeat and /report; tests assert the HTTPClient round-trips
// JSON correctly and surfaces non-2xx as errors.
func httpRoundTripFixture(t *testing.T, hb http.HandlerFunc, rep http.HandlerFunc) (*HTTPClient, *httptest.Server) {
	t.Helper()
	mux := http.NewServeMux()
	if hb != nil {
		mux.HandleFunc("POST "+protocol.PathHeartbeat, hb)
	}
	if rep != nil {
		mux.HandleFunc("POST "+protocol.PathReport, rep)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewHTTPClient(srv.URL, srv.Client()), srv
}

func TestHTTPClient_Heartbeat_RoundTrip(t *testing.T) {
	want := protocol.ManifestResponse{
		UpdateAvailable: true,
		TargetVersion:   "v2",
		TargetHash:      strings.Repeat("a", 64),
		DeltaHash:       strings.Repeat("b", 64),
		Signature:       strings.Repeat("c", 128),
		DeltaEndpoint:   "/delta/from/to",
	}
	var receivedHB protocol.Heartbeat
	c, _ := httpRoundTripFixture(t,
		func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &receivedHB); err != nil {
				t.Errorf("unmarshal: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(want)
		},
		nil,
	)
	got, err := c.Heartbeat(context.Background(), &protocol.Heartbeat{
		DeviceID: "dev-X", VersionHash: strings.Repeat("d", 64),
	})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if receivedHB.DeviceID != "dev-X" {
		t.Fatalf("server received DeviceID = %q", receivedHB.DeviceID)
	}
	if got.TargetHash != want.TargetHash || got.Signature != want.Signature {
		t.Fatalf("response mismatch: %+v vs %+v", got, want)
	}
}

func TestHTTPClient_Heartbeat_Non2xx(t *testing.T) {
	c, _ := httpRoundTripFixture(t,
		func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		},
		nil,
	)
	_, err := c.Heartbeat(context.Background(), &protocol.Heartbeat{DeviceID: "x"})
	if err == nil {
		t.Fatalf("expected error from 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want status mentioned", err)
	}
}

func TestHTTPClient_Heartbeat_NilRequest(t *testing.T) {
	c := NewHTTPClient("http://x", nil)
	if _, err := c.Heartbeat(context.Background(), nil); err == nil {
		t.Fatalf("nil heartbeat should error")
	}
}

func TestHTTPClient_Heartbeat_ContextCancelled(t *testing.T) {
	c, _ := httpRoundTripFixture(t,
		func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(2 * time.Second)
			w.WriteHeader(http.StatusOK)
		},
		nil,
	)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.Heartbeat(ctx, &protocol.Heartbeat{DeviceID: "x"})
	if err == nil {
		t.Fatalf("expected error from cancelled context")
	}
}

func TestHTTPClient_Report_RoundTrip(t *testing.T) {
	var got protocol.UpdateReport
	c, _ := httpRoundTripFixture(t,
		nil,
		func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &got)
			w.WriteHeader(http.StatusAccepted)
		},
	)
	rep := &protocol.UpdateReport{
		DeviceID: "dev-1", PreviousHash: "p", NewHash: "n", Success: true,
	}
	if err := c.Report(context.Background(), rep); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if got.DeviceID != "dev-1" || !got.Success {
		t.Fatalf("server received %+v", got)
	}
}

func TestHTTPClient_Report_Non2xx(t *testing.T) {
	c, _ := httpRoundTripFixture(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusBadRequest)
	})
	err := c.Report(context.Background(), &protocol.UpdateReport{DeviceID: "x"})
	if err == nil {
		t.Fatalf("expected error from 400 response")
	}
}

func TestHTTPClient_DeltaURL(t *testing.T) {
	c := NewHTTPClient("http://server:8080/", nil)
	cases := map[string]string{
		"/delta/a/b":              "http://server:8080/delta/a/b",
		"delta/a/b":               "http://server:8080/delta/a/b",
		"http://other/delta/a/b":  "http://other/delta/a/b",
		"https://other/delta/a/b": "https://other/delta/a/b",
		"":                        "",
	}
	for in, want := range cases {
		if got := c.DeltaURL(in); got != want {
			t.Errorf("DeltaURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHTTPClient_BaseURL_TrimsTrailingSlash(t *testing.T) {
	c := NewHTTPClient("http://x/", nil)
	if c.BaseURL != "http://x" {
		t.Fatalf("BaseURL = %q", c.BaseURL)
	}
}

func TestCoAPClient_Names(t *testing.T) {
	c := NewCoAPClient("coap://server")
	if c.Name() != "coap" {
		t.Fatalf("Name = %q", c.Name())
	}
}

func TestCoAPClient_DeltaURL(t *testing.T) {
	c := NewCoAPClient("coap://server:5683")
	cases := map[string]string{
		"/delta/a/b":            "coap://server:5683/delta/a/b",
		"delta/a/b":             "coap://server:5683/delta/a/b",
		"coap://other/delta/x":  "coap://other/delta/x",
		"":                      "",
	}
	for in, want := range cases {
		if got := c.DeltaURL(in); got != want {
			t.Errorf("DeltaURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCoAPClient_RejectsNonCoAPBaseURL(t *testing.T) {
	c := NewCoAPClient("http://server")
	_, err := c.Heartbeat(context.Background(), &protocol.Heartbeat{DeviceID: "x"})
	if err == nil {
		t.Fatalf("non-coap base URL should error")
	}
	if !strings.Contains(err.Error(), "coap://") {
		t.Fatalf("err = %v, want scheme mention", err)
	}
}

func TestCoAPClient_Heartbeat_NilRequest(t *testing.T) {
	c := NewCoAPClient("coap://x")
	if _, err := c.Heartbeat(context.Background(), nil); err == nil {
		t.Fatalf("nil heartbeat should error")
	}
}

// coapServerFixture spins up an in-process CoAP UDP server with a tiny mux
// the test owns. heartbeatHandler and reportHandler may be nil to opt out
// of registering that resource. Returns the address (e.g. "127.0.0.1:43271")
// and a cleanup function.
func coapServerFixture(t *testing.T, heartbeatHandler, reportHandler mux.HandlerFunc) (string, func()) {
	t.Helper()
	r := mux.NewRouter()
	if heartbeatHandler != nil {
		if err := r.Handle(protocol.PathHeartbeat, heartbeatHandler); err != nil {
			t.Fatalf("register heartbeat: %v", err)
		}
	}
	if reportHandler != nil {
		if err := r.Handle(protocol.PathReport, reportHandler); err != nil {
			t.Fatalf("register report: %v", err)
		}
	}
	l, err := coapnet.NewListenUDP("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	srv := udp.NewServer(options.WithMux(r))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(l)
	}()
	return l.LocalAddr().String(), func() {
		srv.Stop()
		_ = l.Close()
		<-done
	}
}

func TestCoAPClient_Report_RoundTrip(t *testing.T) {
	var seen atomic.Pointer[protocol.UpdateReport]
	addr, cleanup := coapServerFixture(t,
		nil,
		func(w mux.ResponseWriter, r *mux.Message) {
			body, _ := r.ReadBody()
			var rep protocol.UpdateReport
			if err := cbor.Unmarshal(body, &rep); err != nil {
				_ = w.SetResponse(codes.BadRequest, message.TextPlain, nil)
				return
			}
			seen.Store(&rep)
			_ = w.SetResponse(codes.Changed, message.TextPlain, nil)
		},
	)
	defer cleanup()

	c := NewCoAPClient("coap://" + addr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rep := &protocol.UpdateReport{
		DeviceID: "dev-1", PreviousHash: "p", NewHash: "n",
		Success: true, Timestamp: 12345,
	}
	if err := c.Report(ctx, rep); err != nil {
		t.Fatalf("Report: %v", err)
	}

	got := seen.Load()
	if got == nil {
		t.Fatalf("server received no report")
	}
	if got.DeviceID != "dev-1" || !got.Success || got.NewHash != "n" {
		t.Fatalf("server received %+v", got)
	}
}

func TestCoAPClient_Heartbeat_RoundTrip(t *testing.T) {
	want := protocol.ManifestResponse{
		UpdateAvailable: true,
		TargetHash:      strings.Repeat("a", 64),
		DeltaHash:       strings.Repeat("b", 64),
		Signature:       strings.Repeat("c", 128),
		DeltaEndpoint:   "/delta/from/to",
	}
	addr, cleanup := coapServerFixture(t,
		func(w mux.ResponseWriter, r *mux.Message) {
			buf, _ := cbor.Marshal(want)
			_ = w.SetResponse(codes.Content, message.AppCBOR, bytes.NewReader(buf))
		},
		nil,
	)
	defer cleanup()

	c := NewCoAPClient("coap://" + addr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := c.Heartbeat(ctx, &protocol.Heartbeat{DeviceID: "dev-X"})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if got.TargetHash != want.TargetHash || got.Signature != want.Signature {
		t.Fatalf("response mismatch: %+v vs %+v", got, want)
	}
}

func TestCoAPClient_Report_NilRequest(t *testing.T) {
	c := NewCoAPClient("coap://x")
	if err := c.Report(context.Background(), nil); err == nil {
		t.Fatalf("nil report should error")
	}
}

func TestCoAPClient_Report_RejectsNonCoAPBaseURL(t *testing.T) {
	c := NewCoAPClient("http://x")
	err := c.Report(context.Background(), &protocol.UpdateReport{DeviceID: "x"})
	if err == nil {
		t.Fatalf("non-coap base URL should error")
	}
	if !strings.Contains(err.Error(), "coap://") {
		t.Fatalf("err = %v", err)
	}
}

func TestMismatchedPairError_Message(t *testing.T) {
	_, err := NewClientPair(&fakeClient{name: "http"}, &fakeTransport{name: "coap"})
	if err == nil {
		t.Fatalf("expected mismatch error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "http") || !strings.Contains(msg, "coap") {
		t.Fatalf("error message %q should mention both transports", msg)
	}
}

func TestCoAPClient_Heartbeat_DialFailure(t *testing.T) {
	// Use a TEST-NET-1 address (RFC 5737, unroutable on the public internet)
	// + a timeout context to surface a clean dial/post error.
	c := NewCoAPClient("coap://192.0.2.1:5683")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := c.Heartbeat(ctx, &protocol.Heartbeat{DeviceID: "x"})
	if err == nil {
		t.Fatalf("expected dial/post error against unreachable host")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// Either flavor of failure is acceptable; the point is no panic and
		// a typed error.
	}
}
