package replay

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultMaxBodyBytes is the response-body cut-off when a caller passes
// maxBody <= 0: 256KB of body is ample for classification and keeps one
// replay from pulling a download into memory.
const DefaultMaxBodyBytes = 256 << 10

// Client sends replay targets. It never follows redirects (the redirect
// status itself is a finding), never verifies TLS (replay targets are
// operator-chosen and commonly self-signed on intranets), and truncates
// response bodies at maxBody.
type Client struct {
	http    *http.Client
	maxBody int
}

// NewClient builds a client. An empty upstreamProxy connects directly; a
// non-empty one must be an http(s) URL with a host or the call fails. timeout
// <= 0 takes 30s and maxBody <= 0 takes DefaultMaxBodyBytes.
func NewClient(upstreamProxy string, timeout time.Duration, maxBody int) (*Client, error) {
	var proxy func(*http.Request) (*url.URL, error)
	if upstream := strings.TrimSpace(upstreamProxy); upstream != "" {
		parsed, err := url.Parse(upstream)
		if err != nil {
			return nil, fmt.Errorf("parse upstream proxy %q: %w", upstream, err)
		}
		if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return nil, fmt.Errorf("upstream proxy must be an http(s) URL with a host, got %q", upstream)
		}
		proxy = http.ProxyURL(parsed)
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if maxBody <= 0 {
		maxBody = DefaultMaxBodyBytes
	}
	transport := &http.Transport{
		Proxy: proxy,
		// nil Proxy means direct connection on purpose: env proxies would
		// silently reroute an authorized replay campaign.
		//nolint:gosec // G402: intentional, replay targets are operator-chosen endpoints (same stance as internal/mcp httpTool).
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
		ForceAttemptHTTP2: true,
	}
	return &Client{
		http: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			// ErrUseLastResponse returns the redirect response as-is; a replay
			// that lands on a 302-to-login must be judged on the 302, not on
			// the login page a silent follow would fetch.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		maxBody: maxBody,
	}, nil
}

// Send transmits one target and always returns a Result: transport failures
// land in Err with the timing still filled in, and a body longer than
// maxBody is truncated with Truncated set. A Host entry in Headers overrides
// the URL's host the way a raw request would; every other header is written
// with its exact captured casing.
func (c *Client) Send(ctx context.Context, t Target) (result Result) {
	start := time.Now()
	defer func() { result.ElapsedMs = time.Since(start).Milliseconds() }()

	req, err := t.request(ctx)
	if err != nil {
		result.Err = err.Error()
		return result
	}
	resp, err := c.http.Do(req)
	if err != nil {
		result.Err = err.Error()
		return result
	}
	defer func() { _ = resp.Body.Close() }()

	result.StatusCode = resp.StatusCode
	headers := make(map[string]string, len(resp.Header))
	for key, values := range resp.Header {
		headers[key] = strings.Join(values, ", ")
	}
	result.Headers = headers
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(c.maxBody)+1))
	if err != nil {
		// Keep whatever was read before the failure; a half body plus an
		// error is more honest than discarding both.
		result.Body = body
		result.Err = "read response body: " + err.Error()
		return result
	}
	if len(body) > c.maxBody {
		body = body[:c.maxBody]
		result.Truncated = true
	}
	result.Body = body
	return result
}

// request materialises the target as an *http.Request.
func (t Target) request(ctx context.Context) (*http.Request, error) {
	if strings.TrimSpace(t.URL) == "" {
		return nil, errors.New("target has no url")
	}
	method := strings.ToUpper(strings.TrimSpace(t.Method))
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if t.Body != "" {
		body = strings.NewReader(t.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.URL, body)
	if err != nil {
		return nil, err
	}
	for key, value := range t.Headers {
		if strings.EqualFold(key, "Host") {
			// Host is a protocol field, not a Header-map entry; net/http only
			// honours Request.Host.
			req.Host = value
			continue
		}
		// Direct map assignment preserves the captured casing; Header.Set
		// would canonicalise the key.
		req.Header[key] = []string{value}
	}
	return req, nil
}
