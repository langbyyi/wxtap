package mcp

// 合并形态工具的行为测试：session 组合引导、debugger_control/breakpoint/
// inspect、page_stack、hook_drain 的 waitMs、存储参数化、selector 参数转发。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// sessionBridge returns canned payloads per IPC method, with an injectable
// failure point, so session_start's orchestration can be walked end to end.
type sessionBridge struct {
	calls      []string
	failMethod string
	multi      bool
	empty      bool
}

func (b *sessionBridge) Call(_ context.Context, method string, _ map[string]any) (any, error) {
	b.calls = append(b.calls, method)
	if method == b.failMethod {
		return nil, context.DeadlineExceeded
	}
	switch method {
	case "miniapp.list":
		if b.empty {
			return map[string]any{"list": []any{}}, nil
		}
		if b.multi {
			return map[string]any{"miniapps": []any{
				map[string]any{"id": 1, "appid": "wxaaa", "name": "一号"},
				map[string]any{"id": 2, "appid": "wxbbb", "name": "二号"},
			}}, nil
		}
		return map[string]any{"miniapps": []any{map[string]any{"id": 1, "appid": "wxaaa", "name": "一号"}}}, nil
	case "navigator.pages":
		return map[string]any{"pages": []any{"pages/index"}, "tab_bar_pages": []any{}, "current_route": "pages/index"}, nil
	}
	return map[string]any{"ok": true}, nil
}

func TestSessionStartOrchestratesHappyPath(t *testing.T) {
	bridge := &sessionBridge{}
	messages := exchange(t, Deps{Core: &fakeCore{}, AppBridge: bridge},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"session_start","arguments":{"hooks":["wxapi","cloud"]}}}`+"\n")
	if toolFailed(t, messages, 1) {
		t.Fatalf("session_start 不该失败: %s", toolText(t, messages, 1))
	}
	var payload struct {
		OK      bool           `json:"ok"`
		Capture map[string]any `json:"capture"`
		Pages   map[string]any `json:"pages"`
	}
	if err := json.Unmarshal([]byte(toolText(t, messages, 1)), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.OK || payload.Pages == nil {
		t.Fatalf("应有 ok 与页面栈: %s", toolText(t, messages, 1))
	}
	if _, has := payload.Capture["wxapi"]; !has {
		t.Fatalf("capture 应含 wxapi: %s", toolText(t, messages, 1))
	}
	// 顺序：node/wechat → engine.start → miniapp.list → 两个 hook.start → stats…
	joined := strings.Join(bridge.calls, ",")
	engineAt := strings.Index(joined, "engine.start")
	listAt := strings.Index(joined, "miniapp.list")
	if engineAt == -1 || listAt == -1 || engineAt > listAt {
		t.Fatalf("engine.start 必须先于 miniapp.list: %v", bridge.calls)
	}
	if !strings.Contains(joined, "wxapi.start") || !strings.Contains(joined, "cloud.start") {
		t.Fatalf("应开启请求的两个钩子: %v", bridge.calls)
	}
}

func TestSessionStartStopsAtEngineFailureWithAttribution(t *testing.T) {
	bridge := &sessionBridge{failMethod: "engine.start"}
	messages := exchange(t, Deps{Core: &fakeCore{}, AppBridge: bridge},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"session_start","arguments":{}}}`+"\n")
	var payload struct {
		OK   bool   `json:"ok"`
		Step string `json:"step"`
	}
	if err := json.Unmarshal([]byte(toolText(t, messages, 1)), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.OK || payload.Step != "engine_start" {
		t.Fatalf("应在 engine_start 步停下并归因: %s", toolText(t, messages, 1))
	}
	for _, call := range bridge.calls {
		if call == "wxapi.start" {
			t.Fatal("引擎失败后不得继续开采集")
		}
	}
}

func TestSessionStartMultipleTargetsAsksForChoice(t *testing.T) {
	bridge := &sessionBridge{multi: true}
	messages := exchange(t, Deps{Core: &fakeCore{}, AppBridge: bridge},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"session_start","arguments":{}}}`+"\n")
	var payload struct {
		OK        string `json:"step"`
		Available []any  `json:"available"`
	}
	if err := json.Unmarshal([]byte(toolText(t, messages, 1)), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.OK != "target" || len(payload.Available) != 2 {
		t.Fatalf("多开未指定目标应返回清单: %s", toolText(t, messages, 1))
	}
}

func TestSessionStartExplicitTargetSwitches(t *testing.T) {
	bridge := &sessionBridge{multi: true}
	messages := exchange(t, Deps{Core: &fakeCore{}, AppBridge: bridge},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"session_start","arguments":{"target":2}}}`+"\n")
	if toolFailed(t, messages, 1) {
		t.Fatalf("显式 target 不该失败: %s", toolText(t, messages, 1))
	}
	if !strings.Contains(strings.Join(bridge.calls, ","), "miniapp.switch") {
		t.Fatalf("应调用 miniapp.switch: %v", bridge.calls)
	}
}

func TestSessionStatusIsReadOnlySnapshot(t *testing.T) {
	bridge := &sessionBridge{}
	messages := exchange(t, Deps{Core: &fakeCore{}, AppBridge: bridge},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"session_status","arguments":{}}}`+"\n")
	if toolFailed(t, messages, 1) {
		t.Fatalf("session_status 不该失败: %s", toolText(t, messages, 1))
	}
	for _, method := range bridge.calls {
		switch method {
		case "engine.start", "wxapi.start", "cloud.start", "miniapp.switch":
			t.Fatalf("状态快照不得有副作用: %v", bridge.calls)
		}
	}
}

func TestDebuggerControlRoutesByAction(t *testing.T) {
	core := &debugCore{}
	messages := exchange(t, Deps{Core: core}, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"debugger_control","arguments":{"action":"pause"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"debugger_control","arguments":{"action":"step_over"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"debugger_control","arguments":{"action":"hOP"}}}`,
	}, "\n")+"\n")
	var first struct {
		Paused bool `json:"paused"`
	}
	if err := json.Unmarshal([]byte(toolText(t, messages, 1)), &first); err != nil {
		t.Fatal(err)
	}
	if !first.Paused {
		t.Fatalf("pause 应返回暂停快照: %s", toolText(t, messages, 1))
	}
	var second struct {
		Paused bool `json:"paused"`
	}
	if err := json.Unmarshal([]byte(toolText(t, messages, 2)), &second); err != nil {
		t.Fatal(err)
	}
	if second.Paused {
		t.Fatalf("step_over 后应恢复运行（fake 已置为未暂停）: %s", toolText(t, messages, 2))
	}
	if !toolFailed(t, messages, 3) {
		t.Fatal("非法 action 必须报错")
	}
}

func TestDebuggerBreakpointSetListRemove(t *testing.T) {
	core := &debugCore{}
	messages := exchange(t, Deps{Core: core}, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"debugger_breakpoint","arguments":{"action":"set","url":"appservice/app.js","line":10}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"debugger_breakpoint","arguments":{"action":"list"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"debugger_breakpoint","arguments":{"action":"remove","breakpoint_id":"bp:1"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"debugger_breakpoint","arguments":{"action":"list"}}}`,
	}, "\n")+"\n")

	var setResult struct {
		BreakpointID string `json:"breakpoint_id"`
		Line         int    `json:"line"`
	}
	if err := json.Unmarshal([]byte(toolText(t, messages, 1)), &setResult); err != nil {
		t.Fatal(err)
	}
	if setResult.BreakpointID != "bp:1" || setResult.Line != 10 {
		t.Fatalf("set 应返回 CDP 的断点 id 与 1-based 行号: %s", toolText(t, messages, 1))
	}

	var listed struct {
		Count       int `json:"count"`
		Breakpoints []struct {
			ID   string `json:"breakpoint_id"`
			Line int    `json:"line"`
		} `json:"breakpoints"`
	}
	if err := json.Unmarshal([]byte(toolText(t, messages, 2)), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Count != 1 || listed.Breakpoints[0].ID != "bp:1" {
		t.Fatalf("list 应读到注册表里的断点: %s", toolText(t, messages, 2))
	}

	if toolFailed(t, messages, 3) {
		t.Fatalf("remove 不该失败: %s", toolText(t, messages, 3))
	}
	if err := json.Unmarshal([]byte(toolText(t, messages, 4)), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Count != 0 {
		t.Fatalf("remove 后 list 应为空: %s", toolText(t, messages, 4))
	}
}

func TestDebuggerInspectEvaluateOnFrame(t *testing.T) {
	core := &debugCore{paused: true, pausedSeq: 4}
	messages := exchange(t, Deps{Core: core}, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"debugger_inspect","arguments":{"expression":"token"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"debugger_inspect","arguments":{}}}`,
	}, "\n")+"\n")
	var evaluated struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal([]byte(toolText(t, messages, 1)), &evaluated); err != nil {
		t.Fatal(err)
	}
	if evaluated.Value != "secret-value" {
		t.Fatalf("表达式模式应返回帧求值: %s", toolText(t, messages, 1))
	}
	var scopes struct {
		Scopes []any `json:"scopes"`
	}
	if err := json.Unmarshal([]byte(toolText(t, messages, 2)), &scopes); err != nil {
		t.Fatal(err)
	}
	if len(scopes.Scopes) == 0 {
		t.Fatalf("无表达式模式应返回作用域变量: %s", toolText(t, messages, 2))
	}
}

func TestMiniappPageStackMergesReads(t *testing.T) {
	messages := exchange(t, Deps{Core: &fakeCore{}, AppBridge: fakeAppBridge{}},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_page_stack","arguments":{}}}`+"\n")
	var payload struct {
		Stack        []any  `json:"stack"`
		CurrentRoute string `json:"current_route"`
	}
	if err := json.Unmarshal([]byte(toolText(t, messages, 1)), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Stack) != 1 || payload.CurrentRoute != "pages/index" {
		t.Fatalf("page_stack 应合并栈与当前路由: %s", toolText(t, messages, 1))
	}
}

func TestHookDrainWaitMs(t *testing.T) {
	// 有记录：立即返回，不等待。
	messages := exchange(t, Deps{Core: &fakeCore{}},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hook_drain","arguments":{"name":"wxapi","afterSeq":0,"limit":10,"waitMs":1000}}}`+"\n")
	if toolFailed(t, messages, 1) {
		t.Fatalf("有记录时不该失败: %s", toolText(t, messages, 1))
	}
	// 无记录 + 短超时：如实返回空页。
	messages = exchange(t, Deps{Core: &emptyCore{}},
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"hook_drain","arguments":{"name":"wxapi","afterSeq":0,"limit":10,"waitMs":250}}}`+"\n")
	if toolFailed(t, messages, 2) {
		t.Fatalf("等待超时是正常结果: %s", toolText(t, messages, 2))
	}
}

func TestStorageParameterMerges(t *testing.T) {
	core := &recordingCore{}
	messages := exchange(t, Deps{Core: core, AppBridge: fakeAppBridge{}}, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_get_storage","arguments":{"key":"token"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"miniapp_get_storage","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"miniapp_remove_storage","arguments":{"all":true}}}`,
	}, "\n")+"\n")
	for _, id := range []float64{1, 2, 3} {
		if toolFailed(t, messages, id) {
			t.Fatalf("存储合并形态 %v 不该失败: %s", id, toolText(t, messages, id))
		}
	}
	if !strings.Contains(core.lastExpr, "wx.clearStorageSync") {
		t.Fatalf("all:true 应落到 clearStorageSync: %s", core.lastExpr)
	}
}

func TestSelectorToolsForwardDeclaredArgs(t *testing.T) {
	bridge := &paramBridge{}
	messages := exchange(t, Deps{AppBridge: bridge}, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"sessionkey_scan","arguments":{"source":"traffic","limit":5}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"sessionkey_scan","arguments":{"source":"disk","dir":"C:/users/x"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"navigator_visit","arguments":{"action":"start"}}}`,
	}, "\n")+"\n")
	if toolFailed(t, messages, 1) || toolFailed(t, messages, 2) || toolFailed(t, messages, 3) {
		t.Fatal("合法 selector 不该失败")
	}
	if bridge.calls[0].method != "sessionkey.scanTraffic" {
		t.Fatalf("traffic 应落到 scanTraffic: %v", bridge.calls)
	}
	if bridge.calls[0].args["limit"] != float64(5) {
		t.Fatalf("Args 白名单内的参数必须转发: %v", bridge.calls[0].args)
	}
	if bridge.calls[1].method != "sessionkey.scan" || bridge.calls[1].args["dir"] != "C:/users/x" {
		t.Fatalf("disk 应落到 sessionkey.scan 并带 dir: %v", bridge.calls[1])
	}
	if bridge.calls[2].method != "navigator.autoVisit" {
		t.Fatalf("start 应落到 autoVisit: %v", bridge.calls)
	}
}

func TestLeanCatalogExcludesAbsorbedNames(t *testing.T) {
	lean := map[string]bool{}
	all := map[string]bool{}
	for _, tool := range New(Deps{}).tools() {
		lean[tool.Name] = true
	}
	for _, tool := range New(Deps{ToolProfile: "all"}).tools() {
		all[tool.Name] = true
	}
	for name := range lean {
		if !all[name] {
			t.Fatalf("lean 工具 %s 不在 all 档案里（lean 必须是 all 的子集）", name)
		}
	}
	if all["traffic_list"] == false || all["hook_wait"] == false || all["miniapp_get_routes"] == false {
		t.Fatal("all 档案必须保留被吸收的旧名（供旧客户端）")
	}
	if lean["traffic_list"] || lean["hook_wait"] || lean["miniapp_get_routes"] {
		t.Fatal("lean 目录不得广播被吸收的旧名")
	}
}

func TestWxopenAndCodeTreePassThrough(t *testing.T) {
	bridge := &paramBridge{}
	messages := exchange(t, Deps{AppBridge: bridge}, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wxopen_endpoints","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"wxopen_call","arguments":{"mode":"mini","access_key":"ak","secret_key":"sk","endpoint":"cgi-bin/token","params":{"page":"1"}}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"wxopen_call","arguments":{"mode":"mini","access_key":"ak","secret_key":"sk"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"code_tree","arguments":{"appid":"wxone"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"code_tree","arguments":{}}}`,
	}, "\n")+"\n")

	if toolFailed(t, messages, 1) || toolFailed(t, messages, 2) || toolFailed(t, messages, 4) {
		t.Fatal("合法调用不该失败")
	}
	if bridge.calls[0].method != "wxopen.endpoints" {
		t.Fatalf("endpoints 落点不对: %v", bridge.calls)
	}
	if bridge.calls[1].method != "wxopen.call" {
		t.Fatalf("call 落点不对: %v", bridge.calls)
	}
	if bridge.calls[1].args["endpoint"] != "cgi-bin/token" {
		t.Fatalf("endpoint 应转发: %v", bridge.calls[1].args)
	}
	// 白名单内的 params 对象必须原样转发。
	params, _ := bridge.calls[1].args["params"].(map[string]any)
	if params == nil || params["page"] != "1" {
		t.Fatalf("params 应转发: %v", bridge.calls[1].args)
	}
	if !toolFailed(t, messages, 3) {
		t.Fatal("缺 endpoint 必须报错")
	}
	if len(bridge.calls) != 3 || bridge.calls[2].method != "code.project" || bridge.calls[2].args["appid"] != "wxone" {
		t.Fatalf("code_tree 应落 code.project 并带 appid: %v", bridge.calls)
	}
	if !toolFailed(t, messages, 5) {
		t.Fatal("code_tree 缺 appid 必须报错")
	}
}

func TestHookDrainEmptyPageMarshalsArrays(t *testing.T) {
	messages := exchange(t, Deps{Core: &emptyCore{}},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hook_drain","arguments":{"name":"wxapi","afterSeq":0,"limit":10}}}`+"\n")
	text := toolText(t, messages, 1)
	if strings.Contains(text, `"records":null`) || strings.Contains(text, `"updates":null`) {
		t.Fatalf("空流必须序列化为 [] 而不是 null: %s", text)
	}
	if !strings.Contains(text, `"records":[]`) || !strings.Contains(text, `"updates":[]`) {
		t.Fatalf("应含空数组: %s", text)
	}
}

func TestSetStorageValueSchemaAllowsScalars(t *testing.T) {
	messages := exchange(t, Deps{}, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n")
	for _, tool := range byID(messages, 1)["result"].(map[string]any)["tools"].([]any) {
		entry := tool.(map[string]any)
		if entry["name"] != "miniapp_set_storage" {
			continue
		}
		value := entry["inputSchema"].(map[string]any)["properties"].(map[string]any)["value"].(map[string]any)
		if _, pinned := value["type"]; pinned {
			t.Fatalf("value 必须允许任意 JSON 类型（string token 是最常用形态）: %v", value)
		}
		return
	}
	t.Fatal("miniapp_set_storage 不在 tools/list 里")
}

func TestCDPCommandParamsMustBeObject(t *testing.T) {
	messages := exchange(t, Deps{Core: &fakeCore{}},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"cdp_command","arguments":{"method":"DOM.getDocument","params":"x"}}}`+"\n")
	if !toolFailed(t, messages, 1) {
		t.Fatalf("params 非对象必须报错而不是静默丢弃: %s", toolText(t, messages, 1))
	}
}

func TestTrafficGetBodyValidatesPart(t *testing.T) {
	messages := exchange(t, Deps{Traffic: bigTraffic{}}, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"traffic_get_body","arguments":{"id":"a1","part":"banana"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"traffic_get_body","arguments":{"id":"a1"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"traffic_get_body","arguments":{"part":"response"}}}`,
	}, "\n")+"\n")

	if !toolFailed(t, messages, 1) {
		t.Fatalf("非法 part 必须报错: %s", toolText(t, messages, 1))
	}
	if toolFailed(t, messages, 2) {
		t.Fatalf("缺省 part 应取 response: %s", toolText(t, messages, 2))
	}
	if !toolFailed(t, messages, 3) {
		t.Fatal("缺 id 必须报错")
	}
}

func TestHookDrainSchemaEnumDropsNavigator(t *testing.T) {
	messages := exchange(t, Deps{}, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n")
	for _, tool := range byID(messages, 1)["result"].(map[string]any)["tools"].([]any) {
		entry := tool.(map[string]any)
		if entry["name"] != "hook_drain" {
			continue
		}
		name := entry["inputSchema"].(map[string]any)["properties"].(map[string]any)["name"].(map[string]any)
		enum := name["enum"].([]any)
		for _, value := range enum {
			if value == "navigator" {
				t.Fatalf("navigator 没有记录流，schema 不得提供该取值: %v", enum)
			}
		}
		return
	}
	t.Fatal("hook_drain 不在 tools/list 里")
}
