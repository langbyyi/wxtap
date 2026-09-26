package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// readEvent consumes one SSE event and returns its event name and data.
func readEvent(t *testing.T, reader *bufio.Reader) (string, string) {
	t.Helper()
	event, data := "", ""
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read event: %v (event=%q data=%q)", err, event, data)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "":
			if event != "" || data != "" {
				return event, data
			}
		}
	}
}

func TestSSERoundTrip(t *testing.T) {
	sse := NewSSE(New(Deps{}))
	httpServer := httptest.NewServer(sse.Handler())
	defer httpServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/sse", nil)
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sse connect: %v", err)
	}
	defer func() { _ = stream.Body.Close() }()
	reader := bufio.NewReader(stream.Body)

	event, endpoint := readEvent(t, reader)
	if event != "endpoint" {
		t.Fatalf("first event: %q", event)
	}

	resp, err := http.Post(httpServer.URL+endpoint, "application/json", strings.NewReader(
		`{"jsonrpc":"2.0","id":7,"method":"initialize","params":{}}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("post status: %d", resp.StatusCode)
	}

	event, data := readEvent(t, reader)
	if event != "message" {
		t.Fatalf("second event: %q", event)
	}
	var payload struct {
		Result struct {
			ServerInfo struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("decode %q: %v", data, err)
	}
	if string(payload.ID) != "7" || payload.Result.ServerInfo.Name != "wxtap-desktop" {
		t.Fatalf("payload: %s", data)
	}
}

func TestSSEUnknownSessionFails(t *testing.T) {
	sse := NewSSE(New(Deps{}))
	httpServer := httptest.NewServer(sse.Handler())
	defer httpServer.Close()

	resp, err := http.Post(httpServer.URL+"/messages?sessionId=nope", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "session") {
		t.Fatalf("body: %s", body)
	}
}

func TestSSEStartStopLifecycle(t *testing.T) {
	sse := NewSSE(New(Deps{}))
	if sse.Running() {
		t.Fatal("running before start")
	}
	if err := sse.Start(0); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !sse.Running() || sse.Port() == 0 {
		t.Fatalf("running=%v port=%d", sse.Running(), sse.Port())
	}
	// Serving works on the bound port.
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/sse", sse.Port()))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	resp.Body.Close()
	sse.Stop()
	if sse.Running() || sse.Port() != 0 {
		t.Fatalf("after stop: running=%v port=%d", sse.Running(), sse.Port())
	}
}

func TestSSENotificationsAreNotAnswered(t *testing.T) {
	sse := NewSSE(New(Deps{}))
	httpServer := httptest.NewServer(sse.Handler())
	defer httpServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/sse", nil)
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sse connect: %v", err)
	}
	defer func() { _ = stream.Body.Close() }()
	reader := bufio.NewReader(stream.Body)
	_, endpoint := readEvent(t, reader)

	resp, err := http.Post(httpServer.URL+endpoint, "application/json", strings.NewReader(
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if err != nil {
		t.Fatalf("post notification: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notification status: %d", resp.StatusCode)
	}
}

// A loopback POST must not be able to balloon memory: MCP messages are small,
// so oversized bodies are rejected before ReadAll buffers them.
func TestSSERejectsOversizedBody(t *testing.T) {
	sse := NewSSE(New(Deps{}))
	httpServer := httptest.NewServer(sse.Handler())
	defer httpServer.Close()

	// Open a session first: the message endpoint requires a valid session.
	stream, err := http.Get(httpServer.URL + "/sse")
	if err != nil {
		t.Fatalf("sse connect: %v", err)
	}
	defer func() { _ = stream.Body.Close() }()
	reader := bufio.NewReader(stream.Body)
	_, endpoint := readEvent(t, reader)
	postURL := httpServer.URL + endpoint

	huge := bytes.Repeat([]byte("a"), maxSSEBodyBytes+1024)
	resp, err := http.Post(postURL, "application/json", bytes.NewReader(huge))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: status %d want %d", resp.StatusCode, http.StatusRequestEntityTooLarge)
	}
}

// The SSE stream must stay unreadable from arbitrary web pages: the session
// id it hands out is the only credential for the message endpoint.
func TestSSEDoesNotAdvertiseWildcardCORS(t *testing.T) {
	sse := NewSSE(New(Deps{}))
	httpServer := httptest.NewServer(sse.Handler())
	defer httpServer.Close()

	req, err := http.NewRequest(http.MethodGet, httpServer.URL+"/sse", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sse connect: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if origin := resp.Header.Get("Access-Control-Allow-Origin"); origin != "" {
		t.Fatalf("SSE advertised %q", origin)
	}
}
