package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/plgd-dev/go-coap/v3/message/codes"
	"github.com/plgd-dev/go-coap/v3/udp"
)

// CoAPTransport fetches deltas over CoAP (UDP plain). go-coap handles
// Block2 transparently, so we receive the full payload from a single Get.
// Resume is NOT supported in this first iteration — callers that pass a
// non-zero offset receive ErrResumeUnsupported; the Downloader catches it
// and restarts the transfer from zero. Future work: replace Get with
// a Block2-aware call that accepts a starting block number.
type CoAPTransport struct {
	// DialTimeout bounds udp.Dial. Zero uses go-coap's default.
	DialTimeout time.Duration
}

// NewCoAPTransport returns a DeltaTransport backed by go-coap's UDP client.
func NewCoAPTransport(dialTimeout time.Duration) *CoAPTransport {
	return &CoAPTransport{DialTimeout: dialTimeout}
}

// Name implements DeltaTransport.
func (t *CoAPTransport) Name() string { return "coap" }

// FetchRange implements DeltaTransport. Downloads the full resource regardless
// of offset because resume over CoAP Block2 is not implemented yet; if the
// caller asked for a non-zero offset, returns ErrResumeUnsupported so the
// Downloader discards its partial state and retries from 0.
func (t *CoAPTransport) FetchRange(ctx context.Context, rawURL string, offset int64) (io.ReadCloser, int64, error) {
	if offset > 0 {
		return nil, 0, ErrResumeUnsupported
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, 0, fmt.Errorf("parse coap url: %w", err)
	}
	if u.Scheme != "coap" {
		return nil, 0, fmt.Errorf("coap transport requires coap:// scheme, got %q", u.Scheme)
	}
	host := u.Host
	if u.Port() == "" {
		host = u.Hostname() + ":5683"
	}

	co, err := udp.Dial(host)
	if err != nil {
		return nil, 0, fmt.Errorf("coap dial: %w", err)
	}
	defer co.Close()

	resp, err := co.Get(ctx, u.Path)
	if err != nil {
		return nil, 0, fmt.Errorf("coap GET: %w", err)
	}
	if resp.Code() != codes.Content {
		return nil, 0, fmt.Errorf("coap status %s", resp.Code())
	}
	data, err := resp.ReadBody()
	if err != nil {
		return nil, 0, fmt.Errorf("coap read body: %w", err)
	}
	return io.NopCloser(bytes.NewReader(data)), 0, nil
}
