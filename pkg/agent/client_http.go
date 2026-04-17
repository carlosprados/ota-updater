package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/amplia/ota-updater/pkg/protocol"
)

// HTTPClient implements ProtocolClient over HTTP+JSON. It is paired with
// HTTPTransport for the delta download leg.
type HTTPClient struct {
	BaseURL string       // e.g. "http://server:8080" — no trailing slash required
	HTTP    *http.Client // nil → http.DefaultClient
}

// NewHTTPClient returns an HTTPClient bound to baseURL. baseURL must include
// the scheme and host; trailing slashes are tolerated.
func NewHTTPClient(baseURL string, httpClient *http.Client) *HTTPClient {
	return &HTTPClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    httpClient,
	}
}

// Name implements ProtocolClient.
func (c *HTTPClient) Name() string { return "http" }

// Heartbeat implements ProtocolClient. POSTs JSON, decodes JSON.
func (c *HTTPClient) Heartbeat(ctx context.Context, hb *protocol.Heartbeat) (*protocol.ManifestResponse, error) {
	if hb == nil {
		return nil, fmt.Errorf("http heartbeat: nil request")
	}
	body, err := json.Marshal(hb)
	if err != nil {
		return nil, fmt.Errorf("http heartbeat: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+protocol.PathHeartbeat, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("http heartbeat: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("http heartbeat: do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		// Drain a small amount for diagnostics, then surface the status.
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("http heartbeat: status %s: %s", resp.Status, bytes.TrimSpace(errBody))
	}
	var out protocol.ManifestResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("http heartbeat: decode: %w", err)
	}
	return &out, nil
}

// Report implements ProtocolClient. POSTs JSON, ignores response body.
func (c *HTTPClient) Report(ctx context.Context, rep *protocol.UpdateReport) error {
	if rep == nil {
		return fmt.Errorf("http report: nil request")
	}
	body, err := json.Marshal(rep)
	if err != nil {
		return fmt.Errorf("http report: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+protocol.PathReport, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("http report: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("http report: do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("http report: status %s", resp.Status)
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// DeltaURL implements ProtocolClient. The endpoint is treated as a path
// relative to BaseURL; an empty endpoint is invalid (the manifest must
// always carry one when UpdateAvailable=true).
func (c *HTTPClient) DeltaURL(endpoint string) string {
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

func (c *HTTPClient) client() *http.Client {
	if c.HTTP == nil {
		return http.DefaultClient
	}
	return c.HTTP
}
