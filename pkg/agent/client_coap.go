package agent

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/fxamacker/cbor/v2"
	"github.com/plgd-dev/go-coap/v3/message"
	"github.com/plgd-dev/go-coap/v3/message/codes"
	"github.com/plgd-dev/go-coap/v3/udp"

	"github.com/amplia/ota-updater/pkg/protocol"
)

// CoAPClient implements ProtocolClient over CoAP+CBOR (UDP plain, no DTLS).
// It is paired with CoAPTransport for the delta download leg.
type CoAPClient struct {
	BaseURL string // e.g. "coap://server:5683"
}

// NewCoAPClient returns a CoAPClient bound to baseURL.
func NewCoAPClient(baseURL string) *CoAPClient {
	return &CoAPClient{BaseURL: strings.TrimRight(baseURL, "/")}
}

// Name implements ProtocolClient.
func (c *CoAPClient) Name() string { return "coap" }

// Heartbeat implements ProtocolClient. POSTs CBOR, decodes CBOR.
func (c *CoAPClient) Heartbeat(ctx context.Context, hb *protocol.Heartbeat) (*protocol.ManifestResponse, error) {
	if hb == nil {
		return nil, fmt.Errorf("coap heartbeat: nil request")
	}
	host, err := c.host()
	if err != nil {
		return nil, err
	}
	body, err := cbor.Marshal(hb)
	if err != nil {
		return nil, fmt.Errorf("coap heartbeat: marshal: %w", err)
	}
	co, err := udp.Dial(host)
	if err != nil {
		return nil, fmt.Errorf("coap heartbeat: dial: %w", err)
	}
	defer co.Close()

	resp, err := co.Post(ctx, protocol.PathHeartbeat, message.AppCBOR, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("coap heartbeat: post: %w", err)
	}
	if resp.Code() != codes.Content && resp.Code() != codes.Changed {
		return nil, fmt.Errorf("coap heartbeat: status %s", resp.Code())
	}
	respBody, err := resp.ReadBody()
	if err != nil {
		return nil, fmt.Errorf("coap heartbeat: read body: %w", err)
	}
	var out protocol.ManifestResponse
	if err := cbor.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("coap heartbeat: decode: %w", err)
	}
	return &out, nil
}

// Report implements ProtocolClient. POSTs CBOR, ignores response body.
func (c *CoAPClient) Report(ctx context.Context, rep *protocol.UpdateReport) error {
	if rep == nil {
		return fmt.Errorf("coap report: nil request")
	}
	host, err := c.host()
	if err != nil {
		return err
	}
	body, err := cbor.Marshal(rep)
	if err != nil {
		return fmt.Errorf("coap report: marshal: %w", err)
	}
	co, err := udp.Dial(host)
	if err != nil {
		return fmt.Errorf("coap report: dial: %w", err)
	}
	defer co.Close()

	resp, err := co.Post(ctx, protocol.PathReport, message.AppCBOR, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("coap report: post: %w", err)
	}
	if resp.Code() != codes.Changed && resp.Code() != codes.Content {
		return fmt.Errorf("coap report: status %s", resp.Code())
	}
	return nil
}

// DeltaURL implements ProtocolClient. CoAP transport always wants a coap://
// URL; the manifest's endpoint is the path component.
func (c *CoAPClient) DeltaURL(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	if u, err := url.Parse(endpoint); err == nil && u.IsAbs() {
		return endpoint
	}
	if !strings.HasPrefix(endpoint, "/") {
		endpoint = "/" + endpoint
	}
	return c.BaseURL + path.Clean(endpoint)
}

// host extracts the host:port for udp.Dial from BaseURL. The CoAP default
// port is 5683 when the URL omits it.
func (c *CoAPClient) host() (string, error) {
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return "", fmt.Errorf("coap client: parse base url: %w", err)
	}
	if u.Scheme != "coap" {
		return "", fmt.Errorf("coap client: base url must be coap://, got %q", u.Scheme)
	}
	host := u.Host
	if u.Port() == "" {
		host = u.Hostname() + ":5683"
	}
	return host, nil
}
