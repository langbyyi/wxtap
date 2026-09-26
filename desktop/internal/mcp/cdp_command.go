package mcp

// CDP 逃生舱：一受白名单管控的原始 CDP 命令直通。存在它的理由是 DevTools
// 协议面太大（DOM 快照、性能指标、DOM 查询、网络体读取…），逐个包装不现实；
// 约束靠两道闸：域名白名单 deny-by-default（Browser.* 会关掉宿主、Target.*
// 会创建桥不管理的会话、HeapProfiler/Tracing 会灌出巨量载荷，一律拒），以及
// 响应体截断。专用工具能做的事仍然优先用专用工具——它们有语义化的错误与
// 归一化过的返回。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// cdpAllowedDomains gates the raw-CDP escape hatch by method prefix.
var cdpAllowedDomains = map[string]bool{
	"Console": true, "DOM": true, "DOMSnapshot": true, "Debugger": true,
	"Log": true, "Network": true, "Overlay": true, "Page": true,
	"Performance": true, "Runtime": true,
}

// cdpResponseLimit caps one raw CDP response: a heap snapshot or DOM snapshot
// can be megabytes, and this answer flows straight into an agent's context.
const cdpResponseLimit = 2 << 20

// cdpTimeoutCeiling bounds one raw CDP command. The dedicated tools run their
// CDP calls at 5-15s; a raw command has no reason to pin a goroutine for
// longer than a generous minute.
const cdpTimeoutCeiling = 60 * 1000

func (s *Server) callCDPCommand(ctx context.Context, raw json.RawMessage) (any, error) {
	if s.deps.Core == nil {
		return nil, fmt.Errorf("core engine unavailable")
	}
	args := map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("invalid tool arguments: %w", err)
		}
	}
	method := stringArg(args, "method", "")
	if method == "" {
		return nil, fmt.Errorf("missing required argument \"method\"")
	}
	domain := method
	if index := strings.Index(method, "."); index > 0 {
		domain = method[:index]
	}
	if !cdpAllowedDomains[domain] {
		return nil, fmt.Errorf("CDP domain %q is not allowed (allowed: Console, DOM, DOMSnapshot, Debugger, Log, Network, Overlay, Page, Performance, Runtime); dedicated tools cover capture, navigation and UI driving", domain)
	}
	var params map[string]any
	switch typed := args["params"].(type) {
	case nil:
		params = map[string]any{}
	case map[string]any:
		params = typed
	default:
		return nil, fmt.Errorf("params must be a JSON object")
	}
	timeoutMs := intArg(args, "timeoutMs", 10000)
	if timeoutMs <= 0 {
		timeoutMs = 10000
	}
	if timeoutMs > cdpTimeoutCeiling {
		timeoutMs = cdpTimeoutCeiling
	}
	resp, err := s.cdp(ctx, method, params, timeoutMs)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	if len(data) > cdpResponseLimit {
		preview := string(data[:cdpResponseLimit])
		for i := 0; i < 4 && !utf8.ValidString(preview); i++ {
			preview = preview[:len(preview)-1]
		}
		return map[string]any{
			"truncated": true, "bytes": len(data), "result_preview": preview,
			"note": fmt.Sprintf("response exceeded %d bytes and was truncated; narrow the command (e.g. queryDepth / limit params) instead of retrying as-is", cdpResponseLimit),
		}, nil
	}
	return map[string]any{"result": resp["result"], "method": method}, nil
}
