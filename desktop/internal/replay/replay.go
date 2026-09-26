// Package replay turns captured wx.request records into standalone HTTP
// targets, renders them as curl command lines for the operator, and sends
// them through an optionally proxied HTTP client for authorized security
// testing.
package replay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/langbyyi/wxtap/desktop/internal/traffic"
)

// Target is one replayable HTTP request. Header keys keep the original case
// they were captured with: the wire is case-insensitive for header names, but
// a replay is only faithful when the exact bytes travel again.
type Target struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

// Result is what one replay produced. A replay never fails at the call site:
// transport and decoding problems land in Err while the fields that did make
// it (elapsed time, partial body) stay filled, so a caller can always render
// an honest record of what happened.
type Result struct {
	StatusCode int               `json:"status,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       []byte            `json:"-"`
	// Truncated reports that Body was cut at the client's maxBody limit; a
	// consumer comparing bodies must know the bytes end early.
	Truncated bool   `json:"truncated,omitempty"`
	ElapsedMs int64  `json:"elapsedMs"`
	Err       string `json:"error,omitempty"`
}

// ParseRecord rebuilds a replayable Target from one captured wx.request
// record. The request body of a wxapi record is the hook's JSON payload
// {url, method, data, header}; data stays a raw string when it was one and is
// carried as compacted JSON otherwise, header fills Headers with keys kept in
// their original case. A record whose payload is not valid JSON or whose URL
// is not http(s) is rejected — those records cannot be replayed faithfully.
func ParseRecord(rec traffic.Record) (Target, error) {
	var payload struct {
		URL     string          `json:"url"`
		Method  string          `json:"method"`
		Data    json.RawMessage `json:"data"`
		Header  map[string]any  `json:"header"`
		Headers map[string]any  `json:"headers"`
	}
	urlStr := strings.TrimSpace(rec.URL)
	method := strings.TrimSpace(rec.Method)
	target := Target{}
	if len(rec.RequestBody) > 0 {
		if err := json.Unmarshal(rec.RequestBody, &payload); err != nil {
			return Target{}, fmt.Errorf("parse request payload: %w", err)
		}
		if u := strings.TrimSpace(payload.URL); u != "" {
			urlStr = u
		}
		if m := strings.TrimSpace(payload.Method); m != "" {
			method = m
		}
		body, err := dataBody(payload.Data)
		if err != nil {
			return Target{}, err
		}
		target.Body = body
		target.Headers = stringifyHeaders(payload.Header)
		if target.Headers == nil {
			target.Headers = stringifyHeaders(payload.Headers)
		}
	}
	if method == "" {
		method = "GET"
	}
	parsed, err := url.Parse(urlStr)
	if err != nil {
		return Target{}, fmt.Errorf("parse url %q: %w", urlStr, err)
	}
	// url.Parse lowercases the scheme, so the comparison is exact.
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return Target{}, fmt.Errorf("url must be http(s) with a host, got %q", urlStr)
	}
	target.URL = urlStr
	target.Method = strings.ToUpper(method)
	return target, nil
}

// dataBody converts the hook's data field into the raw request body: a JSON
// string decodes to its literal text (that is what wx.request put on the
// wire), anything else travels as compacted JSON. Compacting the original
// bytes instead of re-marshalling through any keeps number literals exactly
// as captured.
func dataBody(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "", nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return "", fmt.Errorf("decode string data: %w", err)
		}
		return text, nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, trimmed); err != nil {
		return "", fmt.Errorf("compact data payload: %w", err)
	}
	return buf.String(), nil
}

// stringifyHeaders converts the hook's header object into string values;
// non-string JSON values render as their JSON text so nothing is lost.
func stringifyHeaders(raw map[string]any) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	headers := make(map[string]string, len(raw))
	for key, value := range raw {
		headers[key] = stringifyValue(value)
	}
	return headers
}

func stringifyValue(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(encoded)
	}
}
