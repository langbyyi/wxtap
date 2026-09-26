package replay

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"sort"
	"time"
	"unicode/utf8"
)

// HAREntry is one request/response pair to render into a HAR document. Status
// and RespHeaders come from the caller: stored traffic records do not carry
// the wire-level HTTP status, so the value here is exactly what the caller
// could honestly reconstruct (0 when it could not).
type HAREntry struct {
	When          time.Time
	Target        Target
	Status        int
	RespHeaders   map[string]string
	Body          []byte
	BodyTruncated bool
}

// HAR 1.2 fixed fields. headersSize/bodySize are -1 (info not computed) and
// timings are all-zero placeholders: WxTap exports captured/replayed exchanges
// for other tooling to read, and a made-up byte count would be less honest
// than the -1 the spec defines for "not available".
const (
	harVersion     = "1.2"
	harCreatorName = "WxTap"
	harHTTPVersion = "HTTP/1.1"
	harNotComputed = -1
	// harOctetStream is the fallback mimeType for bodies whose Content-Type
	// header was never captured.
	harOctetStream = "application/octet-stream"
)

type harDocument struct {
	Log harLog `json:"log"`
}

type harLog struct {
	Version string     `json:"version"`
	Creator harCreator `json:"creator"`
	Entries []harEntry `json:"entries"`
}

type harCreator struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type harEntry struct {
	StartedDateTime string         `json:"startedDateTime"`
	Time            float64        `json:"time"`
	Request         harRequest     `json:"request"`
	Response        harResponse    `json:"response"`
	Cache           map[string]any `json:"cache"`
	Timings         harTimings     `json:"timings"`
}

type harRequest struct {
	Method      string       `json:"method"`
	URL         string       `json:"url"`
	HTTPVersion string       `json:"httpVersion"`
	Cookies     []any        `json:"cookies"`
	Headers     []harPair    `json:"headers"`
	QueryString []harPair    `json:"queryString"`
	HeadersSize int          `json:"headersSize"`
	BodySize    int          `json:"bodySize"`
	PostData    *harPostData `json:"postData,omitempty"`
}

type harPostData struct {
	MimeType string `json:"mimeType"`
	Text     string `json:"text"`
}

type harResponse struct {
	Status      int        `json:"status"`
	StatusText  string     `json:"statusText"`
	HTTPVersion string     `json:"httpVersion"`
	Cookies     []any      `json:"cookies"`
	Headers     []harPair  `json:"headers"`
	Content     harContent `json:"content"`
	RedirectURL string     `json:"redirectURL"`
	HeadersSize int        `json:"headersSize"`
	BodySize    int        `json:"bodySize"`
}

type harContent struct {
	Size     int    `json:"size"`
	MimeType string `json:"mimeType"`
	Text     string `json:"text,omitempty"`
	// Encoding is set (to "base64") only when the body is not valid UTF-8:
	// a consumer that misses the field still decodes the text correctly, one
	// that honours it gets the exact bytes back.
	Encoding string `json:"encoding,omitempty"`
}

type harTimings struct {
	Send    float64 `json:"send"`
	Wait    float64 `json:"wait"`
	Receive float64 `json:"receive"`
}

type harPair struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// BuildHAR renders the entries as one HAR 1.2 document. Header order is
// sorted (map iteration is random; a diffable export must be stable), the
// query string is re-parsed from the target URL, and a non-UTF-8 body travels
// base64-encoded with encoding:"base64". The output is empty rather than nil
// for a zero-entry call so callers always have a parseable document.
func BuildHAR(entries []HAREntry) []byte {
	log := harLog{
		Version: harVersion,
		Creator: harCreator{Name: harCreatorName, Version: "1.0"},
		Entries: make([]harEntry, 0, len(entries)),
	}
	for _, entry := range entries {
		log.Entries = append(log.Entries, harEntryOf(entry))
	}
	data, err := json.MarshalIndent(harDocument{Log: log}, "", "  ")
	if err != nil {
		// harEntryOf only produces string/int/float fields, so a marshal
		// failure is unreachable in practice; an empty document beats nil.
		return []byte("{}")
	}
	return data
}

func harEntryOf(entry HAREntry) harEntry {
	headers := sortedPairs(entry.Target.Headers)
	respHeaders := sortedPairs(entry.RespHeaders)
	postData := (*harPostData)(nil)
	if entry.Target.Body != "" {
		postData = &harPostData{
			MimeType: headerValue(entry.Target.Headers, "Content-Type"),
			Text:     entry.Target.Body,
		}
		if postData.MimeType == "" {
			postData.MimeType = harOctetStream
		}
	}
	content := harContent{
		Size:     len(entry.Body),
		MimeType: headerValue(entry.RespHeaders, "Content-Type"),
	}
	if content.MimeType == "" {
		content.MimeType = harOctetStream
	}
	if len(entry.Body) > 0 {
		if utf8.Valid(entry.Body) {
			content.Text = string(entry.Body)
		} else {
			content.Text = base64.StdEncoding.EncodeToString(entry.Body)
			content.Encoding = "base64"
		}
	}
	return harEntry{
		StartedDateTime: entry.When.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		Request: harRequest{
			Method:      entry.Target.Method,
			URL:         entry.Target.URL,
			HTTPVersion: harHTTPVersion,
			Cookies:     []any{},
			Headers:     headers,
			QueryString: queryStringOf(entry.Target.URL),
			HeadersSize: harNotComputed,
			BodySize:    harNotComputed,
			PostData:    postData,
		},
		Response: harResponse{
			Status:      entry.Status,
			HTTPVersion: harHTTPVersion,
			Cookies:     []any{},
			Headers:     respHeaders,
			Content:     content,
			HeadersSize: harNotComputed,
			BodySize:    harNotComputed,
		},
		Cache:   map[string]any{},
		Timings: harTimings{},
	}
}

// queryStringOf parses the pairs out of the raw URL; an unparseable URL yields
// an empty (non-nil) list so the field stays a valid array.
func queryStringOf(rawURL string) []harPair {
	pairs := []harPair{}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return pairs
	}
	for _, name := range sortedQueryNames(parsed.Query()) {
		for _, value := range parsed.Query()[name] {
			pairs = append(pairs, harPair{Name: name, Value: value})
		}
	}
	return pairs
}

func sortedQueryNames(query url.Values) []string {
	names := make([]string, 0, len(query))
	for name := range query {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// sortedPairs sorts by name (then value) and always returns a non-nil slice.
func sortedPairs(headers map[string]string) []harPair {
	pairs := make([]harPair, 0, len(headers))
	for name, value := range headers {
		pairs = append(pairs, harPair{Name: name, Value: value})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].Name != pairs[j].Name {
			return pairs[i].Name < pairs[j].Name
		}
		return pairs[i].Value < pairs[j].Value
	})
	return pairs
}

// headerValue reads one header case-insensitively (the wire is
// case-insensitive for names); "" when absent.
func headerValue(headers map[string]string, name string) string {
	for key, value := range headers {
		if len(key) == len(name) && asciiEqualFold(key, name) {
			return value
		}
	}
	return ""
}

func asciiEqualFold(a, b string) bool {
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
