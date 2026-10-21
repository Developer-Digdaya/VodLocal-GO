package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultTimeout is the request timeout NewClient uses when it is given a
// non-positive one.
const DefaultTimeout = 30 * time.Second

// DefaultMaxResponseBytes caps the response body GetJSON is willing to read
// (4 MiB). Anything larger is rejected with ErrBadResponse instead of being
// buffered without limit.
const DefaultMaxResponseBytes int64 = 4 << 20

// maxErrorSnippet bounds how much of an error body is quoted in an error
// message.
const maxErrorSnippet = 256

// Client is a small GET+decode helper for the upstream adapters. It only knows
// how to build the URL, send a request with a context, classify the transport
// and HTTP failures into the sentinel errors of this package, and decode a JSON
// body into out. Everything about the shape of a concrete API lives in the
// adapter that embeds or uses it.
//
// A zero Client is usable as long as BaseURL is set: HTTP falls back to
// http.DefaultClient and no extra headers are sent.
type Client struct {
	// BaseURL is the root of the upstream API, e.g.
	// "https://api.example.com/v1". A trailing slash is ignored.
	BaseURL string

	// HTTP is the client used for the requests. It is normally set by
	// NewClient, which also gives it a timeout.
	HTTP *http.Client

	// Header, when not nil, is added to every request.
	Header http.Header
}

// NewClient returns a Client for baseURL whose requests time out after
// timeout. A non-positive timeout falls back to DefaultTimeout, because a zero
// http.Client timeout would mean "no timeout at all".
func NewClient(baseURL string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: timeout},
	}
}

// GetJSON issues a GET request to path (relative to BaseURL) with query and
// decodes the JSON response body into out. A nil out skips decoding and is
// useful for calls whose result is not needed.
//
// Failures are reported with the sentinel errors of this package so that callers
// can branch with errors.Is:
//
//   - 404 maps to ErrNotFound;
//   - 408, 504, a context deadline or a transport timeout map to
//     ErrUpstreamTimeout;
//   - any other 5xx maps to ErrUpstream, and so does a transport failure;
//   - unparseable JSON, an empty body, an unexpected status or a body larger
//     than DefaultMaxResponseBytes maps to ErrBadResponse.
//
// A cancelled context is returned wrapped as-is (errors.Is(err,
// context.Canceled)), not as ErrUpstream*, so cancellation can be told apart
// from an upstream problem.
func (c *Client) GetJSON(ctx context.Context, path string, query url.Values, out any) error {
	if c == nil {
		return errors.New("provider: GetJSON pada *Client nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	endpoint := c.endpoint(path, query)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("%w: menyusun GET %q: %v", ErrBadResponse, endpoint, err)
	}
	req.Header.Set("Accept", "application/json")
	for name, values := range c.Header {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return classifyError(err)
	}
	defer resp.Body.Close()

	// Read one byte past the limit so that an oversized body can be detected
	// without buffering all of it.
	body, err := io.ReadAll(io.LimitReader(resp.Body, DefaultMaxResponseBytes+1))
	if err != nil {
		return classifyError(fmt.Errorf("membaca body: %w", err))
	}
	if int64(len(body)) > DefaultMaxResponseBytes {
		return fmt.Errorf("%w: body melebihi %d byte", ErrBadResponse, DefaultMaxResponseBytes)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return statusError(resp.StatusCode, body)
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%w: decode JSON dari %s: %v", ErrBadResponse, endpoint, err)
	}
	return nil
}

// endpoint joins BaseURL, path and query into an absolute URL. Repeated slashes
// at the seam are collapsed and the query is encoded deterministically.
func (c *Client) endpoint(path string, query url.Values) string {
	base := strings.TrimRight(c.BaseURL, "/")
	rel := strings.TrimLeft(path, "/")

	endpoint := base
	switch {
	case rel == "":
	case base == "":
		endpoint = "/" + rel
	default:
		endpoint = base + "/" + rel
	}

	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	return endpoint
}

// httpClient returns the client to use for the request.
func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// statusError maps a non-2xx status to a sentinel error, quoting the beginning
// of the body to make the failure diagnosable.
func statusError(status int, body []byte) error {
	detail := errorSnippet(body)
	switch {
	case status == http.StatusNotFound:
		return fmt.Errorf("%w: status %d%s", ErrNotFound, status, detail)

	case status == http.StatusRequestTimeout, status == http.StatusGatewayTimeout:
		return fmt.Errorf("%w: status %d%s", ErrUpstreamTimeout, status, detail)

	case status >= 500:
		return fmt.Errorf("%w: status %d%s", ErrUpstream, status, detail)

	default:
		// 3xx, 401, 403, 429, ...: the upstream answered something this
		// client cannot use.
		return fmt.Errorf("%w: status %d%s", ErrBadResponse, status, detail)
	}
}

// errorSnippet renders body as a ": <snippet>" suffix, or "" when it is empty.
func errorSnippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return ""
	}
	if len(s) > maxErrorSnippet {
		s = s[:maxErrorSnippet] + "..."
	}
	return ": " + s
}

// classifyError maps a transport or read failure to a sentinel error. Both the
// sentinel and the original error stay reachable through errors.Is/errors.As.
func classifyError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		// Caller-initiated cancellation is not an upstream problem.
		return fmt.Errorf("provider: request dibatalkan: %w", err)

	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %w", ErrUpstreamTimeout, err)
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("%w: %w", ErrUpstreamTimeout, err)
	}

	return fmt.Errorf("%w: %w", ErrUpstream, err)
}
