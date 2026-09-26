package mcp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postMCP is one Streamable HTTP request against the test server's /mcp. It
// returns plain values rather than the response so the body cannot outlive the
// helper that closes it. The decoded map is only filled for JSON responses —
// HTTP-level error replies carry plain text bodies.
func postMCP(t *testing.T, url string, body string) (status int, contentType string, message map[string]any) {
	t.Helper()
	resp, err := http.Post(url+"/mcp", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post /mcp: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	message = map[string]any{}
	contentType = resp.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/json") {
		if err := json.Unmarshal(raw, &message); err != nil {
			t.Fatalf("decode response %q: %v", raw, err)
		}
	}
	return resp.StatusCode, contentType, message
}

func TestStreamableHTTPRoundTrip(t *testing.T) {
	sse := NewSSE(New(Deps{Core: &fakeCore{}, AppBridge: fakeAppBridge{}}))
	httpServer := httptest.NewServer(sse.Handler())
	defer httpServer.Close()

	// initialize：回声客户端请求的协议版本。
	status, contentType, message := postMCP(t, httpServer.URL, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if status != http.StatusOK {
		t.Fatalf("initialize status: %d", status)
	}
	if !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("initialize content-type: %q", contentType)
	}
	result, _ := message["result"].(map[string]any)
	if result == nil || result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize result: %v", message)
	}
	if _, ok := result["instructions"].(string); !ok {
		t.Fatalf("initialize 必须带 instructions: %v", result)
	}

	// tools/call 走同一个端点。
	_, _, message = postMCP(t, httpServer.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"engine_status","arguments":{}}}`)
	result, _ = message["result"].(map[string]any)
	if result == nil || result["isError"] == true {
		t.Fatalf("engine_status via /mcp failed: %v", message)
	}
}

func TestStreamableHTTPNegotiatesOlderProtocol(t *testing.T) {
	sse := NewSSE(New(Deps{}))
	httpServer := httptest.NewServer(sse.Handler())
	defer httpServer.Close()

	for _, request_sent := range []string{"2024-11-05", "2025-03-26", "2025-06-18"} {
		_, _, message := postMCP(t, httpServer.URL,
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+request_sent+`"}}`)
		result, _ := message["result"].(map[string]any)
		if result == nil || result["protocolVersion"] != request_sent {
			t.Fatalf("版本 %s 未被回声: %v", request_sent, message)
		}
	}
	// 未知版本回服务器的最新版。
	_, _, message := postMCP(t, httpServer.URL, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	result, _ := message["result"].(map[string]any)
	if result == nil || result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("未知版本应回最新支持版: %v", message)
	}
}

func TestStreamableHTTPRejectsBadRequests(t *testing.T) {
	sse := NewSSE(New(Deps{}))
	httpServer := httptest.NewServer(sse.Handler())
	defer httpServer.Close()

	// 非法 JSON。
	brokenStatus, _, _ := postMCP(t, httpServer.URL, `{broken`)
	if brokenStatus != http.StatusBadRequest {
		t.Fatalf("broken json status: %d", brokenStatus)
	}
	// 错误的 Content-Type。
	badResp, err := http.Post(httpServer.URL+"/mcp", "text/plain", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("content-type status: %d", badResp.StatusCode)
	}
	// GET / DELETE 不提供（无服务器-initiated 流、无会话）。
	getResp, err := http.Get(httpServer.URL + "/mcp")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("get status: %d", getResp.StatusCode)
	}
	// 跨站 Host 必须被拒（DNS rebinding 防护）。
	req, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Host = "evil.example.com"
	hostResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("host post: %v", err)
	}
	hostResp.Body.Close()
	if hostResp.StatusCode != http.StatusForbidden {
		t.Fatalf("evil host status: %d", hostResp.StatusCode)
	}
}

func TestStreamableHTTPAcceptsNotificationsWith202(t *testing.T) {
	sse := NewSSE(New(Deps{}))
	httpServer := httptest.NewServer(sse.Handler())
	defer httpServer.Close()

	resp, err := http.Post(httpServer.URL+"/mcp", "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if err != nil {
		t.Fatalf("post notification: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notification status: %d", resp.StatusCode)
	}
}
