package mcp

// 审计工具面的边界回归：定义齐全、转发目标正确、参数白名单、截断与上限的
// 诚实标注。语义本身归 GUI 的 IPC 实现（internal/api/ipc/audit.go），这里只
// 守 MCP 这一侧的边界。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// auditBridge 记录每次转发（方法 + 参数），并按方法回放 canned 载荷；
// 未命中 canned 的方法回 {ok:true}。
type auditBridge struct {
	calls    []forwardedCall
	payloads map[string]any
}

func (b *auditBridge) Call(_ context.Context, method string, params map[string]any) (any, error) {
	b.calls = append(b.calls, forwardedCall{method: method, args: params})
	if payload, ok := b.payloads[method]; ok {
		return payload, nil
	}
	return map[string]any{"ok": true}, nil
}

// 八个工具必须全部出现在 tools/list（profile all），且名字冻结：
// 这份清单是 agent 侧的文档，改名就是破坏兼容。
func TestAuditToolsAreAllAdvertised(t *testing.T) {
	messages := exchange(t, Deps{ToolProfile: "all"},
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n")
	result := byID(messages, 1)["result"].(map[string]any)
	advertised := map[string]bool{}
	for _, raw := range result["tools"].([]any) {
		tool := raw.(map[string]any)
		advertised[tool["name"].(string)] = true
	}
	for _, name := range auditToolNames {
		if !advertised[name] {
			t.Fatalf("tools/list 缺少审计工具 %s", name)
		}
	}
}

// 八个工具在默认 lean 清单里也必须可见：藏起入口，agent 就接不上
// 「采集 → 检测 → 读明细」的闭环。
func TestAuditToolsAreInLeanCatalog(t *testing.T) {
	messages := exchange(t, Deps{},
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n")
	result := byID(messages, 1)["result"].(map[string]any)
	advertised := map[string]bool{}
	for _, raw := range result["tools"].([]any) {
		tool := raw.(map[string]any)
		advertised[tool["name"].(string)] = true
	}
	for _, name := range auditToolNames {
		if !advertised[name] {
			t.Fatalf("lean 清单缺少审计工具 %s", name)
		}
	}
}

// 转发目标与参数白名单：声明的键透传、未声明的键丢弃——这是「工具不退化成
// 通用调用器」的边界（AppBridge 能到达每个已注册方法）。
func TestAuditToolsForwardAndWhitelistArguments(t *testing.T) {
	bridge := &auditBridge{}
	messages := exchange(t, Deps{AppBridge: bridge}, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"asset_scan","arguments":{"dir":"C:/out/wxone","includeTraffic":false,"cloudFns":["login","pay"]}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"asset_list","arguments":{"kind":"api","host":"api.example.com","query":"user","offset":10,"limit":20,"save":true}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"traffic_curl","arguments":{"id":"rec-1","part":"response"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"traffic_replay","arguments":{"id":"rec-9","method":"POST"}}}`,
	}, "\n")+"\n")

	for id := 1; id <= 4; id++ {
		if toolFailed(t, messages, float64(id)) {
			t.Fatalf("工具 id %d 不该失败: %s", id, toolText(t, messages, float64(id)))
		}
	}
	want := []struct {
		method string
		args   map[string]any
	}{
		{"assets.scan", map[string]any{
			"dir": "C:/out/wxone", "includeTraffic": false, "cloudFns": []any{"login", "pay"},
		}},
		{"assets.list", map[string]any{
			"kind": "api", "host": "api.example.com", "query": "user",
			"offset": float64(10), "limit": float64(20),
		}},
		{"traffic.curl", map[string]any{"id": "rec-1"}},
		{"traffic.replay", map[string]any{"id": "rec-9"}},
	}
	if len(bridge.calls) != len(want) {
		t.Fatalf("expected %d forwarded calls, got %d", len(want), len(bridge.calls))
	}
	for i, expect := range want {
		if bridge.calls[i].method != expect.method {
			t.Fatalf("第 %d 次转发到 %s，应为 %s", i+1, bridge.calls[i].method, expect.method)
		}
		if fmt.Sprint(bridge.calls[i].args) != fmt.Sprint(expect.args) {
			t.Fatalf("第 %d 次转发参数 = %v，应为 %v", i+1, bridge.calls[i].args, expect.args)
		}
		for _, forbidden := range []string{"save", "part", "method"} {
			if _, present := bridge.calls[i].args[forbidden]; present {
				t.Fatalf("%s 把未声明的参数 %q 透到了 IPC：%v", expect.method, forbidden, bridge.calls[i].args)
			}
		}
	}
}

// asset_export 的 content 上限：超过 64KB 截断并如实标注 truncated 与
// totalBytes；没超限的不得多嘴。
func TestAssetExportCapsOversizedContent(t *testing.T) {
	big := &auditBridge{payloads: map[string]any{
		"assets.export": map[string]any{"ok": true, "content": strings.Repeat("h", auditResultCap+100)},
	}}
	messages := exchange(t, Deps{AppBridge: big},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"asset_export","arguments":{"format":"txt"}}}`+"\n")
	if toolFailed(t, messages, 1) {
		t.Fatalf("不该失败: %s", toolText(t, messages, 1))
	}
	var payload struct {
		Content    string `json:"content"`
		Truncated  bool   `json:"truncated"`
		TotalBytes int    `json:"totalBytes"`
	}
	if err := json.Unmarshal([]byte(toolText(t, messages, 1)), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Truncated || payload.TotalBytes != auditResultCap+100 {
		t.Fatalf("超限必须截断并标注总字节数: %+v", payload)
	}
	if len(payload.Content) >= auditResultCap+100 {
		t.Fatalf("content 必须被截到 64KB 之内: %d", len(payload.Content))
	}

	small := &auditBridge{payloads: map[string]any{
		"assets.export": map[string]any{"ok": true, "content": "https://api.example.com"},
	}}
	messages = exchange(t, Deps{AppBridge: small},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"asset_export","arguments":{"format":"nuclei"}}}`+"\n")
	var okPayload struct {
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(toolText(t, messages, 1)), &okPayload); err != nil {
		t.Fatal(err)
	}
	if okPayload.Truncated {
		t.Fatalf("未超限不得标注 truncated: %s", toolText(t, messages, 1))
	}
}

// traffic_curl 缺 id 必须在边界拦下（不得触达 IPC），报错指明缺哪个参数。
func TestTrafficCurlRequiresID(t *testing.T) {
	bridge := &auditBridge{}
	messages := exchange(t, Deps{AppBridge: bridge},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"traffic_curl","arguments":{}}}`+"\n")
	if !toolFailed(t, messages, 1) {
		t.Fatalf("缺 id 必须失败: %s", toolText(t, messages, 1))
	}
	if !strings.Contains(toolText(t, messages, 1), `missing required argument "id"`) {
		t.Fatalf("报错要指明缺 id: %s", toolText(t, messages, 1))
	}
	if len(bridge.calls) != 0 {
		t.Fatalf("缺参的调用不得触达 IPC: %v", bridge.calls)
	}
}

// traffic_replay 与 traffic_curl 同一参数纪律：缺 id 在 MCP 边界拒绝。
func TestTrafficReplayRequiresID(t *testing.T) {
	bridge := &auditBridge{}
	messages := exchange(t, Deps{AppBridge: bridge},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"traffic_replay","arguments":{}}}`+"\n")
	if !toolFailed(t, messages, 1) {
		t.Fatalf("缺 id 必须失败: %s", toolText(t, messages, 1))
	}
	if len(bridge.calls) != 0 {
		t.Fatalf("缺参的调用不得触达 IPC: %v", bridge.calls)
	}
}

// 审计工具的注解必须与真实行为一致：traffic_replay 对外发请求（openWorld、
// 非只读、非破坏、不可重复），asset_scan 替换内存清单（非只读、非破坏），
// 其余只读。
func TestAuditToolAnnotationsMatchTheirBehavior(t *testing.T) {
	for _, name := range auditToolNames {
		annotations := annotationsFor(name)
		switch name {
		case "traffic_replay":
			if !annotations.OpenWorld || annotations.ReadOnly || annotations.Destructive || annotations.Idempotent {
				t.Fatalf("%s 注解不对: %+v", name, annotations)
			}
		case "asset_scan":
			if annotations.ReadOnly || annotations.Destructive || annotations.OpenWorld {
				t.Fatalf("%s 写内存清单，不得标只读: %+v", name, annotations)
			}
		default:
			if !annotations.ReadOnly || annotations.OpenWorld || annotations.Destructive {
				t.Fatalf("%s 应为纯只读: %+v", name, annotations)
			}
		}
		if strings.TrimSpace(auditDescription(name)) == "" {
			t.Fatalf("%s 缺少描述", name)
		}
	}
}

// auditDescription takes one tool's description out of the definition table.
func auditDescription(name string) string {
	for _, def := range auditToolDefs() {
		if def.Name == name {
			return def.Description
		}
	}
	return ""
}
