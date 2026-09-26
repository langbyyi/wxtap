package mcp

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// SSEServer serves the MCP surface over HTTP SSE for the
// mcp.start HTTP mode: GET /sse opens a stream whose first event names the
// POST endpoint for the session, POST /messages?sessionId=… submits
// JSON-RPC requests, and responses flow back on the stream.
type SSEServer struct {
	server *Server

	mu       sync.Mutex
	sessions map[string]chan []byte
	httpSrv  *http.Server
	port     int
}

// NewSSE wraps an MCP server for SSE transport.
func NewSSE(server *Server) *SSEServer {
	return &SSEServer{server: server, sessions: map[string]chan []byte{}}
}

// Start binds 127.0.0.1:port (0 = ephemeral) and serves until Stop. Starting
// a running server restarts it (same pattern as cloudapi.Server).
func (s *SSEServer) Start(port int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.httpSrv != nil {
		s.stopLocked()
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		// Surfaced verbatim in the MCP page, so it is Chinese like the rest of the
		// UI and names the one thing the user can act on. The Core's own listener
		// failure reads the same way.
		return fmt.Errorf("无法启动 MCP 服务（127.0.0.1:%d，端口被占用时请在本页换一个端口）：%w", port, err)
	}
	s.httpSrv = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.port = listener.Addr().(*net.TCPAddr).Port
	// The serve goroutine must not read s.httpSrv: by the time it runs, the
	// Start lock may be gone and a concurrent Stop can be writing the field.
	srv := s.httpSrv
	go func() { _ = srv.Serve(listener) }()
	return nil
}

// Stop shuts the server down (safe if never started).
func (s *SSEServer) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.httpSrv != nil {
		s.stopLocked()
	}
}

// Running reports whether the server is up.
func (s *SSEServer) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.httpSrv != nil
}

// Port returns the bound port (0 when stopped).
func (s *SSEServer) Port() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.port
}

func (s *SSEServer) stopLocked() {
	// Force-close: SSE streams are always in-flight, a graceful shutdown
	// would block on them forever.
	_ = s.httpSrv.Close()
	s.httpSrv = nil
	s.port = 0
	s.sessions = map[string]chan []byte{}
}

// Handler returns the HTTP handler exposing /sse and /messages (legacy
// HTTP+SSE transport) plus /mcp (Streamable HTTP, the current MCP transport).
// All three share one listener so a client only needs the port.
func (s *SSEServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", s.handleSSE)
	mux.HandleFunc("/messages", s.handleMessages)
	mux.HandleFunc("/mcp", s.handleStreamable)
	return mux
}

func (s *SSEServer) handleSSE(w http.ResponseWriter, r *http.Request) {
	// 与 /mcp 同一道 DNS rebinding 防线（见 http.go 的 isLoopbackHost）：
	// 服务只绑 127.0.0.1，但浏览器页面可以对 loopback 发跨站请求并读走 SSE
	// 流——拿到 sessionId 就等于拿到全部工具。
	if host := r.Host; !isLoopbackHost(host) {
		http.Error(w, "invalid host", http.StatusForbidden)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	session, err := newSessionID()
	if err != nil {
		http.Error(w, "cannot create session", http.StatusInternalServerError)
		return
	}
	inbox := make(chan []byte, 16)
	s.mu.Lock()
	s.sessions[session] = inbox
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.sessions, session)
		s.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, "event: endpoint\ndata: /messages?sessionId="+session+"\n\n"); err != nil {
		return
	}
	flusher.Flush()

	for {
		select {
		case data, ok := <-inbox:
			if !ok {
				return
			}
			if _, err := w.Write([]byte("event: message\ndata: " + string(data) + "\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *SSEServer) handleMessages(w http.ResponseWriter, r *http.Request) {
	// 与 /sse 同一道 DNS rebinding 防线：恶意页面拿到 sessionId 就能驱动全部工具。
	if host := r.Host; !isLoopbackHost(host) {
		http.Error(w, "invalid host", http.StatusForbidden)
		return
	}
	session := r.URL.Query().Get("sessionId")
	s.mu.Lock()
	inbox, exists := s.sessions[session]
	s.mu.Unlock()
	if !exists {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	// MCP messages are small; a cap keeps a loopback caller from buffering an
	// unbounded body. MaxBytesReader also closes the connection cleanly.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSSEBodyBytes))
	if err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if len(body) == 0 {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	var req request
	if json.Unmarshal(body, &req) != nil || req.Method == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	req.ctx = r.Context()
	if response := s.server.dispatchSafely(&req); response != nil {
		data, err := json.Marshal(response)
		if err == nil {
			select {
			case inbox <- data:
			default: // client is not reading; drop instead of blocking
			}
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

func newSessionID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// maxSSEBodyBytes caps one MCP message body (tool arguments are JSON payloads,
// far below this in practice).
const maxSSEBodyBytes = 4 << 20
