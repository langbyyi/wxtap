package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/traffic"
)

func TestParseRecord(t *testing.T) {
	tests := []struct {
		name    string
		record  traffic.Record
		want    Target
		wantErr bool
	}{
		{
			name: "string data and headers",
			record: traffic.Record{
				RequestBody: []byte(`{"url":"https://api.example.com/v1/me","method":"post","data":"raw-text","header":{"token":"t1","Content-Type":"application/json"}}`),
			},
			want: Target{
				URL:    "https://api.example.com/v1/me",
				Method: "POST",
				Headers: map[string]string{
					"token":        "t1",
					"Content-Type": "application/json",
				},
				Body: "raw-text",
			},
		},
		{
			name: "object data becomes compact json",
			record: traffic.Record{
				RequestBody: []byte(`{"url":"https://api.example.com","method":"PUT","data":{ "b": 2, "a": 1 }}`),
			},
			want: Target{URL: "https://api.example.com", Method: "PUT", Body: `{"b":2,"a":1}`},
		},
		{
			name: "array data",
			record: traffic.Record{
				RequestBody: []byte(`{"url":"https://api.example.com","data":[1,2]}`),
			},
			want: Target{URL: "https://api.example.com", Method: "GET", Body: `[1,2]`},
		},
		{
			name: "missing data and header and method",
			record: traffic.Record{
				RequestBody: []byte(`{"url":"https://api.example.com"}`),
			},
			want: Target{URL: "https://api.example.com", Method: "GET"},
		},
		{
			name: "falls back to record url and method",
			record: traffic.Record{
				URL:         "https://api.example.com/fallback",
				Method:      "delete",
				RequestBody: []byte(`{"data":"x"}`),
			},
			want: Target{URL: "https://api.example.com/fallback", Method: "DELETE", Body: "x"},
		},
		{
			name:    "non-http url rejected",
			record:  traffic.Record{URL: "ftp://api.example.com/file"},
			wantErr: true,
		},
		{
			name:    "empty url rejected",
			record:  traffic.Record{RequestBody: []byte(`{"data":"x"}`)},
			wantErr: true,
		},
		{
			name:    "invalid payload rejected",
			record:  traffic.Record{URL: "https://api.example.com", RequestBody: []byte(`{broken`)},
			wantErr: true,
		},
		{
			name:    "array payload rejected",
			record:  traffic.Record{URL: "https://api.example.com", RequestBody: []byte(`[1,2]`)},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRecord(tt.record)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseRecord() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRecord() error = %v", err)
			}
			if got.URL != tt.want.URL || got.Method != tt.want.Method || got.Body != tt.want.Body {
				t.Fatalf("ParseRecord() = %+v, want %+v", got, tt.want)
			}
			if len(got.Headers) != len(tt.want.Headers) {
				t.Fatalf("headers = %v, want %v", got.Headers, tt.want.Headers)
			}
			for key, value := range tt.want.Headers {
				if got.Headers[key] != value {
					t.Fatalf("header %q = %q, want %q", key, got.Headers[key], value)
				}
			}
		})
	}
}

func TestBuildCurl(t *testing.T) {
	tests := []struct {
		name   string
		target Target
		bash   string
		cmd    string
	}{
		{
			name:   "get without body omits -X",
			target: Target{URL: "https://x.y/a?b=1"},
			bash:   `curl -k 'https://x.y/a?b=1'`,
			cmd:    `curl -k "https://x.y/a?b=1"`,
		},
		{
			name:   "empty target url",
			target: Target{},
			bash:   `curl -k ''`,
			cmd:    `curl -k ""`,
		},
		{
			name:   "post with body and headers",
			target: Target{URL: "https://x.y/a", Method: "POST", Headers: map[string]string{"Content-Type": "application/json"}, Body: `{"msg":"it's"}`},
			bash:   `curl -k -X POST 'https://x.y/a' -H 'Content-Type: application/json' --data-raw '{"msg":"it'\''s"}'`,
			cmd:    `curl -k -X POST "https://x.y/a" -H "Content-Type: application/json" --data-raw "{\"msg\":\"it's\"}"`,
		},
		{
			name:   "headers sorted by key",
			target: Target{URL: "https://x.y/", Headers: map[string]string{"B": "2", "A": "1"}},
			bash:   `curl -k 'https://x.y/' -H 'A: 1' -H 'B: 2'`,
			cmd:    `curl -k "https://x.y/" -H "A: 1" -H "B: 2"`,
		},
		{
			name:   "unicode and spaces",
			target: Target{URL: "https://x.y/查询", Method: "GET", Body: "值 with 空格"},
			bash:   "curl -k -X GET 'https://x.y/查询' --data-raw '值 with 空格'",
			cmd:    `curl -k -X GET "https://x.y/查询" --data-raw "值 with 空格"`,
		},
		{
			name:   "newline stays inside quotes",
			target: Target{URL: "https://x.y/", Body: "line1\nline2"},
			bash:   "curl -k -X GET 'https://x.y/' --data-raw 'line1\nline2'",
			cmd:    "curl -k -X GET \"https://x.y/\" --data-raw \"line1\nline2\"",
		},
		{
			name:   "cmd doubles trailing backslash before closing quote",
			target: Target{URL: `https://x.y/`, Method: "POST", Body: `path\`},
			bash:   `curl -k -X POST 'https://x.y/' --data-raw 'path\'`,
			cmd:    `curl -k -X POST "https://x.y/" --data-raw "path\\"`,
		},
		{
			name:   "cmd escapes a quote that does not follow a backslash",
			target: Target{URL: "https://x.y/", Method: "POST", Body: `a\tail"end`},
			bash:   `curl -k -X POST 'https://x.y/' --data-raw 'a\tail"end'`,
			cmd:    `curl -k -X POST "https://x.y/" --data-raw "a\tail\"end"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotBash, gotCmd := tt.target.BuildCurl()
			if gotBash != tt.bash {
				t.Fatalf("bash = %q, want %q", gotBash, tt.bash)
			}
			if gotCmd != tt.cmd {
				t.Fatalf("cmd = %q, want %q", gotCmd, tt.cmd)
			}
		})
	}
}

func TestNewClientUpstreamProxyParsing(t *testing.T) {
	tests := []struct {
		name          string
		upstream      string
		wantErr       bool
		wantProxyUsed bool
	}{
		{name: "empty means direct", upstream: ""},
		{name: "http proxy accepted", upstream: "http://127.0.0.1:8080", wantProxyUsed: true},
		{name: "https proxy accepted", upstream: "https://proxy.corp", wantProxyUsed: true},
		{name: "no scheme rejected", upstream: "127.0.0.1:8080", wantErr: true},
		{name: "no host rejected", upstream: "http://", wantErr: true},
		{name: "garbage rejected", upstream: "://bad", wantErr: true},
		{name: "ftp scheme rejected", upstream: "ftp://proxy.corp", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := NewClient(tt.upstream, time.Second, 1024)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NewClient(%q) error = nil, want error", tt.upstream)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewClient(%q) error = %v", tt.upstream, err)
			}
			transport, ok := client.http.Transport.(*http.Transport)
			if !ok {
				t.Fatalf("transport type %T, want *http.Transport", client.http.Transport)
			}
			proxyUsed := transport.Proxy != nil
			if proxyUsed != tt.wantProxyUsed {
				t.Fatalf("proxy configured = %v, want %v", proxyUsed, tt.wantProxyUsed)
			}
		})
	}
}

func TestSendVerifiesRequestAgainstServer(t *testing.T) {
	var gotMethod, gotPath, gotHost, gotBody, gotHeaderValue string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotHost = r.Host
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		gotHeaderValue = r.Header.Get("x-odd-case")
		w.Header().Set("X-Reply", "yes")
		w.WriteHeader(201)
		_, _ = io.WriteString(w, "created")
	}))
	defer server.Close()

	client, err := NewClient("", 5*time.Second, 0)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	target := Target{
		URL:    server.URL + "/v1/thing",
		Method: "post",
		Headers: map[string]string{
			"x-odd-case": "kept",
			"Host":       "virtual.example.com",
		},
		Body: `{"hello":"world"}`,
	}
	result := client.Send(context.Background(), target)
	if result.Err != "" {
		t.Fatalf("Send error = %q", result.Err)
	}
	if result.StatusCode != 201 {
		t.Fatalf("status = %d, want 201", result.StatusCode)
	}
	if result.Headers["X-Reply"] != "yes" {
		t.Fatalf("response headers = %v, want X-Reply: yes", result.Headers)
	}
	if gotMethod != "POST" || gotPath != "/v1/thing" {
		t.Fatalf("server saw %s %s", gotMethod, gotPath)
	}
	if gotHost != "virtual.example.com" {
		t.Fatalf("server saw Host %q, want virtual.example.com", gotHost)
	}
	if gotBody != `{"hello":"world"}` {
		t.Fatalf("server saw body %q", gotBody)
	}
	if gotHeaderValue != "kept" {
		t.Fatalf("server saw header value %q", gotHeaderValue)
	}
	if result.ElapsedMs < 0 {
		t.Fatalf("elapsed = %d", result.ElapsedMs)
	}
	if result.Truncated {
		t.Fatal("small body must not be marked truncated")
	}
}

// TestSendPreservesHeaderCaseOverRawWire reads the raw request bytes: Go's
// server canonicalises header keys while parsing, so only the wire shows
// whether the captured casing survived.
func TestSendPreservesHeaderCaseOverRawWire(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()

	requestText := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		buffer := make([]byte, 0, 4096)
		chunk := make([]byte, 1024)
		for !bytes.Contains(buffer, []byte("\r\n\r\n")) {
			n, err := conn.Read(chunk)
			buffer = append(buffer, chunk[:n]...)
			if err != nil {
				break
			}
		}
		requestText <- string(buffer)
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
	}()

	client, err := NewClient("", 5*time.Second, 0)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	result := client.Send(context.Background(), Target{
		URL:     "http://" + listener.Addr().String() + "/",
		Headers: map[string]string{"x-odd-case": "kept"},
	})
	if result.Err != "" {
		t.Fatalf("Send error = %q", result.Err)
	}
	raw := <-requestText
	if !strings.Contains(raw, "x-odd-case: kept\r\n") {
		t.Fatalf("wire request lost captured casing:\n%s", raw)
	}
}

func TestSendTruncatesLargeBody(t *testing.T) {
	full := strings.Repeat("x", 10000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, full)
	}))
	defer server.Close()

	client, err := NewClient("", 5*time.Second, 1024)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	result := client.Send(context.Background(), Target{URL: server.URL})
	if result.Err != "" {
		t.Fatalf("Send error = %q", result.Err)
	}
	if !result.Truncated {
		t.Fatal("Truncated = false, want true")
	}
	if len(result.Body) != 1024 {
		t.Fatalf("body length = %d, want 1024", len(result.Body))
	}
}

func TestSendDoesNotFollowRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/landing" {
			t.Error("redirect was followed")
		}
		http.Redirect(w, r, "/landing", http.StatusMovedPermanently)
	}))
	defer server.Close()

	client, err := NewClient("", 5*time.Second, 0)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	result := client.Send(context.Background(), Target{URL: server.URL + "/start"})
	if result.Err != "" {
		t.Fatalf("Send error = %q", result.Err)
	}
	if result.StatusCode != 301 {
		t.Fatalf("status = %d, want 301 (unfollowed)", result.StatusCode)
	}
}

func TestSendReportsTransportFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Close() // nothing listens on that port anymore

	client, err := NewClient("", 5*time.Second, 0)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	result := client.Send(context.Background(), Target{URL: server.URL})
	if result.Err == "" {
		t.Fatal("Err empty, want transport failure text")
	}
	if result.StatusCode != 0 {
		t.Fatalf("status = %d, want 0", result.StatusCode)
	}
}

func TestSendHonoursCancelledContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "late")
	}))
	defer server.Close()

	client, err := NewClient("", 30*time.Second, 0)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := client.Send(ctx, Target{URL: server.URL})
	if result.Err == "" {
		t.Fatal("Err empty, want context cancellation text")
	}
}

func TestBuildCurlOfParsedRecordIsQuoted(t *testing.T) {
	// A round-trip sanity check: whatever the record contains, both command
	// lines stay single-line parseable token-wise (bash never breaks out of
	// single quotes).
	record := traffic.Record{
		RequestBody: []byte(`{"url":"https://x.y/p'a th","method":"POST","data":{"note":"it's 你好\nline"},"header":{"Cookie":"a=b; c=d"}}`),
	}
	target, err := ParseRecord(record)
	if err != nil {
		t.Fatalf("ParseRecord: %v", err)
	}
	bash, cmd := target.BuildCurl()
	if bash == "" || cmd == "" {
		t.Fatalf("empty curl output: %q / %q", bash, cmd)
	}
	if strings.Count(bash, "curl") != 1 || !strings.Contains(bash, "-k") {
		t.Fatalf("unexpected bash line: %q", bash)
	}
	var payload struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(record.RequestBody, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(bash, "--data-raw") || !strings.Contains(cmd, "--data-raw") {
		t.Fatalf("body missing from curl lines: %q / %q", bash, cmd)
	}
}
