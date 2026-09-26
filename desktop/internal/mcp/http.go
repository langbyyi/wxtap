package mcp

// Streamable HTTP 传输（MCP 2025-03-26 起，/mcp 单端点）。与 legacy 的
// /sse + /messages 双端点并存于同一端口：现代客户端（Claude、Cursor、Codex
// 等）走 POST /mcp，旧客户端继续走 /sse。本服务器不推送服务器-initiated 流，
// 所以请求一律以单条 application/json 应答——这是规范允许的最小实现，也正是
// stdio 路径 dispatch 的同一份语义。

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

func (s *SSEServer) handleStreamable(w http.ResponseWriter, r *http.Request) {
	// 绑定在 127.0.0.1 上，但 DNS rebinding 防护仍要校验 Host：浏览器页面
	// 可以对 loopback 发跨站 POST。
	if host := r.Host; !isLoopbackHost(host) {
		http.Error(w, "invalid host", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.handleStreamablePost(w, r)
	case http.MethodGet, http.MethodDelete:
		// GET 是服务器-initiated SSE 流（本服务器不提供），DELETE 是会话
		// 释放（本服务器无状态）。都按规范回 405 并带 Allow。
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	default:
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *SSEServer) handleStreamablePost(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "content-type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
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
	// 断连即取消：客户端放弃的请求不再继续服务端长轮询（hook_drain waitMs、
	// hook_wait、暂停等待都监视这个 ctx）。
	req.ctx = r.Context()
	// 通知（无 id）不产生应答；202 Accepted 即规范要求的确认。
	if req.ID == nil {
		s.server.dispatchSafely(&req)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	response := s.server.dispatchSafely(&req)
	if response == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	data, err := json.Marshal(response)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func isLoopbackHost(host string) bool {
	if index := strings.LastIndex(host, ":"); index >= 0 && !strings.Contains(host, "]") {
		host = host[:index]
	}
	return host == "127.0.0.1" || host == "localhost" || host == "::1" || host == "[::1]"
}
