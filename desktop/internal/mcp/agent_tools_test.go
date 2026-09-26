package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// recordingCore captures the CDP commands the MCP layer issues, so tests can
// assert on the page-side expression a tool built.
type recordingCore struct {
	fakeCore
	methods   []string
	lastExpr  string
	emptyPage bool
	// expressions / contextCalls record what was evaluated and, for the calls
	// that carried a contextId, which execution context each expression was
	// aimed at — the contract the context resolvers exist to keep.
	expressions  []string
	contextCalls []contextCall
	// contexts overrides the execution-context layout this fake exposes.
	contexts cdpContexts
	// pageMisses makes the page webview answer "no" for that many probes
	// first — the way a tab transition briefly reports the outgoing route while
	// that page's webview is already gone.
	pageMisses int
	// lastInputParams is the payload of the most recent Input.dispatchMouseEvent.
	lastInputParams map[string]any
	// errorPayload / payloadMarker make a probe-passing evaluate whose expression
	// contains payloadMarker answer with that payload instead of the canned
	// success — the {error} shape a page-side try/catch returns.
	errorPayload  string
	payloadMarker string
}

func (c *recordingCore) CDPCommand(_ context.Context, method string, params map[string]any, _ int) (map[string]any, error) {
	c.methods = append(c.methods, method)
	if method == "Input.dispatchMouseEvent" {
		c.lastInputParams = params
	}
	if method == "Runtime.evaluate" {
		c.lastExpr, _ = params["expression"].(string)
		c.expressions = append(c.expressions, c.lastExpr)
		if raw, present := params["contextId"]; present {
			c.contextCalls = append(c.contextCalls, contextCall{ID: probeContextID(raw), Expr: c.lastExpr})
			if c.pageMisses > 0 && strings.Contains(c.lastExpr, probePageMarker) {
				c.pageMisses--
				return evaluateStringResult("no"), nil
			}
			if frame, ok := answerContextProbe(c.lastExpr, params, c.contexts); ok {
				return frame, nil
			}
		}
		if c.errorPayload != "" && strings.Contains(c.lastExpr, c.payloadMarker) {
			return evaluateStringResult(c.errorPayload), nil
		}
	}
	return map[string]any{"result": map[string]any{"result": map[string]any{"type": "string", "value": `{"ok":true}`}}}, nil
}

func (c *recordingCore) HookDrain(_ context.Context, name string, afterSeq int64, limit int, _ int64, _ int) (DrainPage, error) {
	if c.emptyPage {
		return DrainPage{NextSeq: afterSeq, NextUpdateSeq: 0}, nil
	}
	return c.fakeCore.HookDrain(context.Background(), name, afterSeq, limit, 0, 0) //nolint:lll // 与 Core 接口同形
}

// emptyCore 一直返回空页：验证 hook_wait 的超时路径（timedOut 标注）。
type emptyCore struct{ fakeCore }

func (c *emptyCore) HookDrain(_ context.Context, _ string, afterSeq int64, _ int, _ int64, _ int) (DrainPage, error) {
	return DrainPage{NextSeq: afterSeq}, nil
}

func TestHookWaitReturnsImmediatelyWhenRecordsExist(t *testing.T) {
	messages := exchange(t, Deps{Core: &fakeCore{}}, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hook_wait","arguments":{"name":"wxapi","timeoutMs":2000}}}`+"\n")
	if toolFailed(t, messages, 1) {
		t.Fatalf("hook_wait 不该失败: %s", toolText(t, messages, 1))
	}
	var payload struct {
		TimedOut bool  `json:"timedOut"`
		Records  []any `json:"records"`
	}
	if err := json.Unmarshal([]byte(toolText(t, messages, 1)), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.TimedOut || len(payload.Records) != 1 {
		t.Fatalf("应立即返回 1 条记录且不超时: %s", toolText(t, messages, 1))
	}
}

func TestHookWaitTimesOutHonestly(t *testing.T) {
	messages := exchange(t, Deps{Core: &emptyCore{}}, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hook_wait","arguments":{"name":"cloud","timeoutMs":300}}}`+"\n")
	if toolFailed(t, messages, 1) {
		t.Fatalf("超时是正常结果不是错误: %s", toolText(t, messages, 1))
	}
	var payload struct {
		TimedOut bool `json:"timedOut"`
	}
	if err := json.Unmarshal([]byte(toolText(t, messages, 1)), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.TimedOut {
		t.Fatalf("应如实标注 timedOut: %s", toolText(t, messages, 1))
	}
}

func TestHookWaitRejectsNonRecordHooks(t *testing.T) {
	messages := exchange(t, Deps{Core: &fakeCore{}}, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hook_wait","arguments":{"name":"navigator"}}}`+"\n")
	if !toolFailed(t, messages, 1) {
		t.Fatal("navigator 没有记录流，必须拒绝")
	}
}

func TestTrafficRecordsAggregatesBodies(t *testing.T) {
	messages := exchange(t, Deps{Traffic: fakeTraffic{}}, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"traffic_records","arguments":{"apiType":"wx.request"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"traffic_records","arguments":{"apiType":"wx.request","bodyBytes":0}}}`,
	}, "\n")+"\n")

	var page struct {
		Items []struct {
			ID       string `json:"id"`
			Response struct {
				Text      string `json:"text"`
				Truncated bool   `json:"truncated"`
			} `json:"response_body"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(toolText(t, messages, 1)), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != "a1" {
		t.Fatalf("应聚合 fakeTraffic 的一条记录: %s", toolText(t, messages, 1))
	}
	if !strings.Contains(page.Items[0].Response.Text, "LTAI") {
		t.Fatalf("响应文体应内联: %s", toolText(t, messages, 1))
	}

	if text := toolText(t, messages, 2); strings.Contains(text, "response_body") {
		t.Fatalf("bodyBytes=0 时不应取文体: %s", text)
	}
}

func TestCDPCommandDomainAllowlist(t *testing.T) {
	core := &recordingCore{}
	messages := exchange(t, Deps{Core: core}, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"cdp_command","arguments":{"method":"Browser.close"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"cdp_command","arguments":{"method":"Target.getTargets"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"cdp_command","arguments":{"method":"DOMSnapshot.captureSnapshot"}}}`,
	}, "\n")+"\n")

	for _, id := range []float64{1, 2} {
		text := toolText(t, messages, id)
		if !toolFailed(t, messages, id) || !strings.Contains(text, "not allowed") {
			t.Fatalf("域 %v 应被白名单拒绝: %s", id, text)
		}
	}
	if len(core.methods) != 1 || core.methods[0] != "DOMSnapshot.captureSnapshot" {
		t.Fatalf("只有放行的命令触达 Core: %v", core.methods)
	}
	if toolFailed(t, messages, 3) {
		t.Fatalf("白名单域应放行: %s", toolText(t, messages, 3))
	}
	if !strings.Contains(toolText(t, messages, 3), "captureSnapshot") {
		// 结果里带回 method 方便 agent 对账。
		t.Fatalf("应回显 method: %s", toolText(t, messages, 3))
	}
}

func TestCDPCommandTruncatesHugeResponses(t *testing.T) {
	messages := exchange(t, Deps{Core: &truncatingCore{}}, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"cdp_command","arguments":{"method":"DOMSnapshot.captureSnapshot"}}}`+"\n")
	var payload struct {
		Truncated bool   `json:"truncated"`
		Bytes     int    `json:"bytes"`
		Preview   string `json:"result_preview"`
	}
	if err := json.Unmarshal([]byte(toolText(t, messages, 1)), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Truncated || payload.Bytes <= cdpResponseLimit || payload.Preview == "" {
		t.Fatalf("超大响应应截断并带预览: %s", toolText(t, messages, 1))
	}
}

// truncatingCore yields a multi-megabyte result for one command.
type truncatingCore struct{ fakeCore }

func (c *truncatingCore) CDPCommand(_ context.Context, method string, _ map[string]any, _ int) (map[string]any, error) {
	return map[string]any{"result": map[string]any{"data": strings.Repeat("x", cdpResponseLimit+100)}}, nil
}

func TestStorageTamperTools(t *testing.T) {
	core := &recordingCore{}
	messages := exchange(t, Deps{Core: core, AppBridge: fakeAppBridge{}}, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_set_storage","arguments":{"key":"token","value":{"a":1}}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"miniapp_remove_storage","arguments":{"key":"token"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"miniapp_clear_storage","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"miniapp_set_storage","arguments":{"value":1}}}`,
	}, "\n")+"\n")

	for _, id := range []float64{1, 2, 3} {
		if toolFailed(t, messages, id) {
			t.Fatalf("存储工具 %v 不该失败: %s", id, toolText(t, messages, id))
		}
	}
	if !strings.Contains(core.lastExpr, "wx.clearStorageSync") {
		t.Fatalf("最后一条应是 clear: %s", core.lastExpr)
	}
	if !toolFailed(t, messages, 4) {
		t.Fatal("缺 key 必须报错")
	}
}

// bigTraffic serves a >256KB body to prove traffic_get_body truncates.
type bigTraffic struct{ fakeTraffic }

func (bigTraffic) GetBody(_ context.Context, id string, part string) ([]byte, error) {
	if part == "response" {
		return []byte(strings.Repeat("y", (256<<10)+50)), nil
	}
	return []byte("{}"), nil
}

func TestTrafficGetBodyTruncatesHugeBodies(t *testing.T) {
	messages := exchange(t, Deps{Traffic: bigTraffic{}}, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"traffic_get_body","arguments":{"id":"a1","part":"response"}}}`+"\n")
	var payload struct {
		Truncated  bool   `json:"truncated"`
		TotalBytes int    `json:"totalBytes"`
		Body       string `json:"body"`
	}
	if err := json.Unmarshal([]byte(toolText(t, messages, 1)), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Truncated || payload.TotalBytes != (256<<10)+50 || len(payload.Body) != 256<<10 {
		t.Fatalf("大文体应截断到 256KB 并标注: truncated=%v total=%d len=%d", payload.Truncated, payload.TotalBytes, len(payload.Body))
	}
}

func TestNewToolAnnotations(t *testing.T) {
	// 注解覆盖全量面（含被吸收的旧名），档案取 all。
	annotations := map[string]toolAnnotations{}
	for _, tool := range New(Deps{ToolProfile: "all"}).tools() {
		annotations[tool.Name] = tool.Annotations
	}
	cases := []struct {
		name        string
		readOnly    bool
		destructive bool
		openWorld   bool
	}{
		{"hook_wait", true, false, false},
		{"traffic_records", true, false, false},
		{"miniapp_set_storage", false, false, false},
		// all:true 与 miniapp_clear_storage 同义（全量清空），注解必须同判；
		// 单键删除共享这条注解，描述里已写明两种形态。
		{"miniapp_remove_storage", false, true, false},
		{"cdp_command", false, false, false},
		// 按次携带凭据打微信官方端点，是出网调用。
		{"wxopen_call", false, false, true},
		{"ak_verify", false, false, true},
		{"miniapp_http_request", false, false, true},
	}
	for _, want := range cases {
		got, ok := annotations[want.name]
		if !ok {
			t.Fatalf("工具 %s 缺少注解", want.name)
		}
		if got.ReadOnly != want.readOnly || got.Destructive != want.destructive || got.OpenWorld != want.openWorld {
			t.Fatalf("%s 注解不对: %+v，应为 readOnly=%v destructive=%v openWorld=%v", want.name, got, want.readOnly, want.destructive, want.openWorld)
		}
	}
}
