// Package cloudapi exposes the miniapp's cloud functions to external tools
// over a loopback HTTP API (CloudApiServer): POST /call delegates to
// the same IPC surface the GUI uses (cloud.call / cloud.call_container).
package cloudapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Dispatch routes a call into the IPC surface; the server delegates every
// /call request through it.
type Dispatch func(method string, params map[string]any) (any, error)

// Server is the loopback HTTP API. Zero value is unusable; use New.
type Server struct {
	mu       sync.Mutex
	dispatch Dispatch
	listener net.Listener
	srv      *http.Server
	port     int
}

// New creates the server around a dispatch function.
func New(dispatch Dispatch) *Server {
	return &Server{dispatch: dispatch}
}

// Start binds 127.0.0.1:port (0 = ephemeral) and serves until Stop. Starting
// a running server restarts it.
func (s *Server) Start(port int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		s.shutdownLocked()
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		// Surfaced verbatim in the cloud view, so it is Chinese like the rest of
		// the UI and names the one thing the user can act on.
		return fmt.Errorf("无法启动云函数 API 服务（127.0.0.1:%d，端口被占用时请在上方换一个端口）：%w", port, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.listener = listener
	s.srv = srv
	s.port = listener.Addr().(*net.TCPAddr).Port
	go func() { _ = srv.Serve(listener) }()
	return nil
}

// Stop shuts the server down (safe if never started). In-flight /call
// handlers are waited for (bounded): shutdown must not return while a handler
// can still reach the IPC router, or the App's task drain could race a late
// background task spawned by that handler.
func (s *Server) Stop() {
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()
	if srv == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		// This API has no long-lived streams: handlers finish promptly once
		// the listener is closed; the timeout only bounds a wedged handler.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		_ = srv.Close()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv == srv {
		s.srv = nil
		s.listener = nil
		s.port = 0
	}
}

// Running reports whether the server is up.
func (s *Server) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listener != nil
}

// Port returns the bound port (0 when stopped).
func (s *Server) Port() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.port
}

func (s *Server) shutdownLocked() {
	_ = s.srv.Close()
	s.srv = nil
	s.listener = nil
	s.port = 0
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	// Origin is the only part of a cross-origin request a page cannot forge,
	// and a text/plain POST needs no preflight at all: without this gate the
	// missing CORS headers below still let any visited page drive cloud calls.
	if !originAllowed(r.Header.Get("Origin")) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin requests are not allowed"})
		return
	}
	switch r.Method {
	case http.MethodOptions:
		// No Access-Control-Allow-* headers: this loopback forwarder is for
		// native callers, and a wildcard would let any web page drive cloud
		// calls through the user's WeChat session.
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"service": "WxTap Cloud API",
			"usage":   `POST /call — {"name": "funcName", "data": {}}`,
		})
	case http.MethodPost:
		s.handlePost(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	}
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request) {
	if strings.TrimRight(r.URL.Path, "/") != "/call" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	var body struct {
		Name string          `json:"name"`
		Data json.RawMessage `json:"data"`
	}
	// Cloud payloads are operator-sized; the cap keeps a loopback caller from
	// buffering an unbounded JSON body.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCloudAPIBodyBytes)).Decode(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	if body.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing 'name' field"})
		return
	}
	result, err := s.callFunction(body.Name, body.Data)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// callFunction dispatches from a JSON object carrying both a
// path and a method/header key is a callContainer invocation, anything else
// targets a cloud function by name.
func (s *Server) callFunction(name string, data json.RawMessage) (any, error) {
	var dict map[string]any
	isDict := len(data) > 0 && json.Unmarshal(data, &dict) == nil && dict != nil
	if isDict {
		_, hasPath := dict["path"]
		_, hasMethod := dict["method"]
		_, hasHeader := dict["header"]
		if hasPath && (hasMethod || hasHeader) {
			params := map[string]any{
				"path":   orDefault(dict["path"], name),
				"method": orDefault(dict["method"], "POST"),
				"header": dict["header"],
				"data":   dict["data"],
			}
			return s.dispatch("cloud.call_container", params)
		}
	}
	var dataAny any
	if len(data) > 0 {
		_ = json.Unmarshal(data, &dataAny)
	}
	return s.dispatch("cloud.call", map[string]any{"name": name, "data": dataAny})
}

func orDefault(v any, fallback any) any {
	if v == nil {
		return fallback
	}
	return v
}

// originAllowed reports whether a browser-supplied Origin may drive the
// loopback API. An absent Origin means a native caller (curl, MCP client);
// loopback pages and the devtools:// scheme (the Electron DevTools window)
// are allowed; every other page origin is not, including the opaque
// "null" sent by file:// pages and sandboxed frames.
func originAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	if strings.HasPrefix(origin, "devtools://") {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	switch parsed.Hostname() {
	case "127.0.0.1", "localhost", "::1":
	default:
		return false
	}
	return parsed.Scheme == "http" || parsed.Scheme == "https"
}

func writeJSON(w http.ResponseWriter, code int, obj any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		return
	}
	body := bytes.TrimRight(buf.Bytes(), "\n")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// maxCloudAPIBodyBytes caps one forwarded cloud call body. Function payloads
// (including container request bodies) stay well below this in practice.
const maxCloudAPIBodyBytes = 16 << 20
