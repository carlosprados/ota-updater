package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// HTTPTransport fetches deltas over HTTP/HTTPS using Range requests.
// A zero-value HTTPTransport uses http.DefaultClient.
type HTTPTransport struct {
	Client *http.Client
}

// NewHTTPTransport returns a DeltaTransport backed by the given *http.Client.
// Pass nil to use http.DefaultClient.
func NewHTTPTransport(client *http.Client) *HTTPTransport {
	return &HTTPTransport{Client: client}
}

// Name implements DeltaTransport.
func (t *HTTPTransport) Name() string { return "http" }

// FetchRange implements DeltaTransport. Returns:
//
//   - (body, offset, nil)  on 206 Partial Content when offset>0
//   - (body, 0, nil)       on 200 OK, regardless of the requested offset
//   - (nil, 0, err)        on transport errors or non-2xx responses
func (t *HTTPTransport) FetchRange(ctx context.Context, rawURL string, offset int64) (io.ReadCloser, int64, error) {
	client := t.Client
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	switch resp.StatusCode {
	case http.StatusPartialContent:
		return resp.Body, offset, nil
	case http.StatusOK:
		return resp.Body, 0, nil
	default:
		resp.Body.Close()
		return nil, 0, fmt.Errorf("http %s", resp.Status)
	}
}
