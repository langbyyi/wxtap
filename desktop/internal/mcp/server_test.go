package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/langbyyi/wxtap/desktop/internal/api/ipc"
)

type fakeTraffic struct{}

func (fakeTraffic) List(_ context.Context, params ListParams) (Page, error) {
	if params.APType == "wx.request" {
		return Page{Items: []Item{{
			ID:         "a1",
			URL:        "https://api.test.com",
			Status:     "success",
			DurationMs: 1830,
		}}, Total: 1}, nil
	}
	return Page{Items: []Item{}}, nil
}

func (fakeTraffic) GetBody(_ context.Context, id string, part string) ([]byte, error) {
	if id == "a1" && part == "response" {
		return []byte(`{"token":"LTAI5tABCDEFghijklmnop"}`), nil
	}
	return nil, nil
}

// panickyTraffic stands in for a provider bug: the MCP process must survive it.
type panickyTraffic struct{}

func (panickyTraffic) List(context.Context, ListParams) (Page, error) {
	panic("traffic provider exploded")
}

func (panickyTraffic) GetBody(context.Context, string, string) ([]byte, error) {
	panic("traffic provider exploded")
}

// TestToolCallSurvivesProviderPanic keeps a buggy tool from killing the MCP
// session: the panic has to become one JSON-RPC error and the server must go
// on answering.
func TestToolCallSurvivesProviderPanic(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"traffic_list","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping","params":{}}`,
	}, "\n") + "\n"
	messages := exchange(t, Deps{Traffic: panickyTraffic{}}, input)
	if len(messages) != 2 {
		t.Fatalf("got %d responses, want 2: %v", len(messages), messages)
	}
	if first := byID(messages, 1); first == nil || first["error"] == nil {
		t.Fatalf("panicking tool must answer with a JSON-RPC error, got %v", messages)
	}
	if second := byID(messages, 2); second == nil || second["result"] == nil {
		t.Fatalf("server must keep serving after a tool panic, got %v", messages)
	}
}

func (fakeCore) Status(_ context.Context) (Status, error) {
	return Status{Frida: true, Miniapp: true}, nil
}

// fakeCore records the cursors it was asked for, so a test can prove the tool
// forwards afterUpdateSeq instead of always re-reading from zero. lastLimit
// lets the boundary tests observe the page width hook_drain actually asked for.
type fakeCore struct {
	lastUpdateSeq   int64
	lastUpdateLimit int
	lastLimit       int
}

func (f *fakeCore) HookDrain(_ context.Context, name string, afterSeq int64, limit int, afterUpdateSeq int64, updateLimit int) (DrainPage, error) {
	f.lastUpdateSeq = afterUpdateSeq
	f.lastUpdateLimit = updateLimit
	f.lastLimit = limit
	return DrainPage{
		NextUpdateSeq: afterUpdateSeq + 1,
		Records:       []DrainedRecord{{Seq: 1, Record: map[string]any{"name": "x"}}},
		Updates:       []DrainedUpdate{{Seq: 1, Update: map[string]any{"rid": "wx.request-wxone-1-1", "status": "success", "durationMs": float64(1830)}}},
		NextSeq:       1,
		HasMore:       false,
	}, nil
}

// rpc exchange helper: feed input lines, collect output lines.
func exchange(t *testing.T, deps Deps, input string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	server := New(deps)
	if err := server.Serve(strings.NewReader(input), &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	var messages []map[string]any
	scanner := bufio.NewScanner(&out)
	// 大文体工具的响应单行可到 256KB+，上限对齐 Serve 的 16MB。
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var msg map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			t.Fatalf("bad output line %q: %v", scanner.Text(), err)
		}
		messages = append(messages, msg)
	}
	return messages
}

func byID(messages []map[string]any, id float64) map[string]any {
	for _, m := range messages {
		if m["id"] == id {
			return m
		}
	}
	return nil
}

// toolText returns the text block of one tools/call result.
func toolText(t *testing.T, messages []map[string]any, id float64) string {
	t.Helper()
	message := byID(messages, id)
	if message == nil {
		t.Fatalf("no response for id %v", id)
	}
	result, _ := message["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("empty content for id %v: %v", id, message)
	}
	block, _ := content[0].(map[string]any)
	text, _ := block["text"].(string)
	return text
}

// toolFailed reports whether a tools/call result carries isError.
func toolFailed(t *testing.T, messages []map[string]any, id float64) bool {
	t.Helper()
	message := byID(messages, id)
	if message == nil {
		t.Fatalf("no response for id %v", id)
	}
	result, _ := message["result"].(map[string]any)
	failed, _ := result["isError"].(bool)
	return failed
}

// stringSet reads a JSON array or object into a set of its names/keys. Both the
// in-process form (schema() stores required as []string) and the decoded form
// ([]any after a JSON round trip) have to work, or the assertion silently sees
// an empty set.
func stringSet(value any) map[string]bool {
	out := map[string]bool{}
	switch typed := value.(type) {
	case []string:
		for _, name := range typed {
			out[name] = true
		}
	case []any:
		for _, item := range typed {
			if name, ok := item.(string); ok {
				out[name] = true
			}
		}
	case map[string]any:
		for name := range typed {
			out[name] = true
		}
	}
	return out
}

// forwardedCall is one IPC call the MCP layer made, with its arguments.
type forwardedCall struct {
	method string
	args   map[string]any
}

// paramBridge records the method *and* the arguments, which the argument
// allow-list tests need; captureBridge only keeps the method names.
type paramBridge struct{ calls []forwardedCall }

func (b *paramBridge) Call(_ context.Context, method string, params map[string]any) (any, error) {
	b.calls = append(b.calls, forwardedCall{method: method, args: params})
	return map[string]any{"ok": true}, nil
}

// 握手里的版本必须来自注入，而不是包内硬编码：这里曾写死 2.0.0，而应用自己是
// 1.0.0，等于多出一个没人对账的版本号。
func TestInitializeReportsTheInjectedVersion(t *testing.T) {
	messages := exchange(t, Deps{Version: "v1.0.0"}, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n")
	init := byID(messages, 1)
	if init == nil {
		t.Fatal("initialize missing")
	}
	info := init["result"].(map[string]any)["serverInfo"].(map[string]any)
	if info["version"] != "1.0.0" {
		t.Fatalf("握手应上报注入的版本（去掉 v 前缀），实际 %v", info["version"])
	}

	// 没注入时不能编一个看起来像发布号的版本。
	empty := exchange(t, Deps{}, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n")
	emptyInit := byID(empty, 1)
	if emptyInit == nil {
		t.Fatal("initialize missing")
	}
	emptyInfo := emptyInit["result"].(map[string]any)["serverInfo"].(map[string]any)
	if emptyInfo["version"] != "unknown" {
		t.Fatalf("未注入版本时应报 unknown，实际 %v", emptyInfo["version"])
	}
}

func TestInitializeAndToolList(t *testing.T) {
	messages := exchange(t, Deps{Traffic: fakeTraffic{}, Core: &fakeCore{}}, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	}, "\n")+"\n")

	init := byID(messages, 1)
	if init == nil || init["result"] == nil {
		t.Fatalf("initialize failed: %v", init)
	}
	result := init["result"].(map[string]any)
	if result["protocolVersion"] == nil || result["serverInfo"] == nil {
		t.Fatalf("initialize result incomplete: %v", result)
	}

	// 默认（lean）目录：合并形态在，被吸收的旧形态不在。
	list := byID(messages, 2)
	if list == nil {
		t.Fatalf("tools/list missing")
	}
	tools := list["result"].(map[string]any)["tools"].([]any)
	if len(tools) < 5 {
		t.Fatalf("expected at least 5 tools, got %d", len(tools))
	}
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{
		"session_start", "session_status",
		"hook_start", "hook_stop", "hook_drain",
		"traffic_records", "traffic_get_body", "scan_dir",
		"miniapp_screenshot", "miniapp_click", "miniapp_type", "miniapp_navigate",
		"miniapp_evaluate", "miniapp_console_log", "miniapp_page_stack",
		"miniapp_get_storage", "miniapp_set_storage", "miniapp_remove_storage",
		"miniapp_cloud_captures", "miniapp_cloud_scan", "miniapp_http_request",
		"wxapi_replay", "navigator_guard", "navigator_visit", "navigator_blocked_redirects",
		"extract_inventory", "miniapp_decompile", "code_projects", "code_list_dir",
		"miniapp_read_file", "miniapp_search_code", "miniapp_scan_sensitive",
		"ak_verify", "sessionkey_scan", "code_tree", "wxopen_endpoints", "wxopen_call",
		"debugger_state", "debugger_control", "debugger_breakpoint",
		"debugger_list_scripts", "debugger_inspect", "miniapp_get_source", "cdp_command",
		"hook_list", "hook_inject",
	} {
		if !names[want] {
			t.Fatalf("lean catalog tool %s missing", want)
		}
	}
	for _, absorbed := range []string{
		"traffic_list", "traffic_stats", "hook_wait", "hook_stats",
		"wechat_status", "node_status", "engine_status", "engine_start", "engine_stop",
		"miniapp_list", "miniapp_switch", "targets_list",
		"miniapp_get_info", "miniapp_get_routes", "miniapp_get_current_route",
		"navigator_page_stack", "navigator_auto_visit", "navigator_auto_visit_state",
		"navigator_stop_auto_visit",
		"miniapp_get_storage_key", "miniapp_clear_storage", "miniapp_call_cloud",
		"miniapp_list_packages", "miniapp_get_skills", "miniapp_scroll",
		"miniapp_set_breakpoint", "miniapp_remove_breakpoint",
		"debugger_enable", "debugger_pause", "debugger_resume",
		"debugger_step_over", "debugger_step_into", "debugger_step_out",
		"debugger_call_stack", "debugger_get_scopes", "debugger_evaluate_on_call_frame",
		"debugger_pause_on_exceptions",
		"sessionkey_users", "sessionkey_scan_traffic", "sessionkey_scan_disk",
		"sessionkey_scan_decompiled", "paths_get",
	} {
		if names[absorbed] {
			t.Fatalf("被吸收的工具 %s 不该再出现在 lean 目录里", absorbed)
		}
	}
}

// 被吸收 ≠ 被删除：合并形态广播的同时，旧名字必须继续可调用（tools/call
// 不得回答 unknown tool），否则硬编码了旧名的客户端一夜之间全断。
func TestAbsorbedToolsStayDispatchable(t *testing.T) {
	absorbed := []string{
		"traffic_list", "traffic_stats", "hook_wait", "hook_stats",
		"wechat_status", "node_status", "engine_status", "engine_stop",
		"miniapp_list", "targets_list", "paths_get",
		"miniapp_get_routes", "miniapp_get_current_route", "navigator_page_stack",
		"navigator_auto_visit", "navigator_auto_visit_state", "navigator_stop_auto_visit",
		"miniapp_get_storage_key", "miniapp_clear_storage", "miniapp_call_cloud",
		"miniapp_list_packages", "miniapp_get_skills", "miniapp_scroll",
		"miniapp_set_breakpoint", "miniapp_remove_breakpoint",
		"debugger_enable", "debugger_pause", "debugger_resume",
		"debugger_step_over", "debugger_step_into", "debugger_step_out",
		"debugger_call_stack", "debugger_get_scopes", "debugger_evaluate_on_call_frame",
		"debugger_pause_on_exceptions",
		"sessionkey_users", "sessionkey_scan_traffic", "sessionkey_scan_disk",
		"sessionkey_scan_decompiled",
	}
	lines := make([]string, 0, len(absorbed))
	for index, name := range absorbed {
		lines = append(lines, fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":{}}}`,
			index+1, name))
	}
	messages := exchange(t, Deps{Traffic: fakeTraffic{}, Core: &fakeCore{}, AppBridge: fakeAppBridge{}}, strings.Join(lines, "\n")+"\n")
	for index, name := range absorbed {
		if text := toolText(t, messages, float64(index+1)); strings.Contains(text, "unknown tool") {
			t.Errorf("被吸收工具 %s 必须仍可调用，不得回答 unknown tool：%s", name, text)
		}
	}
}

// 直通工具的转发目标必须与界面调的那条 IPC 方法一致：工具名与 IPC 方法名不同名，
// 转发错了 agent 拿到的就是另一份事实（甚至是别人的数据）。这里断言的是实际转发
// 出去的方法名与参数，不是工具自己回声出来的东西。
func TestPassThroughToolsForwardToTheirIPCMethod(t *testing.T) {
	bridge := &paramBridge{}
	messages := exchange(t, Deps{AppBridge: bridge}, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"traffic_stats","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"navigator_page_stack","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"sessionkey_scan_traffic","arguments":{"limit":50}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"sessionkey_scan_decompiled","arguments":{}}}`,
		// 缺省窗口由后端取：不在参数里凭空造一个 limit=0 传下去。
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"sessionkey_scan_traffic","arguments":{}}}`,
		// 建立会话
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"engine_start","arguments":{"cdpPort":31999}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"miniapp_switch","arguments":{"id":7}}}`,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"wechat_status","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"targets_list","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"node_status","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"engine_stop","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"miniapp_list","arguments":{}}}`,
		// 采集健康 / 驱动 / 验证
		`{"jsonrpc":"2.0","id":13,"method":"tools/call","params":{"name":"navigator_auto_visit","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":14,"method":"tools/call","params":{"name":"navigator_auto_visit_state","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":15,"method":"tools/call","params":{"name":"navigator_stop_auto_visit","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":16,"method":"tools/call","params":{"name":"navigator_blocked_redirects","arguments":{}}}`,
		// 代码 / 凭据闭环
		`{"jsonrpc":"2.0","id":17,"method":"tools/call","params":{"name":"code_projects","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":18,"method":"tools/call","params":{"name":"code_list_dir","arguments":{"path":"C:/out/wxone"}}}`,
		`{"jsonrpc":"2.0","id":19,"method":"tools/call","params":{"name":"paths_get","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":20,"method":"tools/call","params":{"name":"ak_verify","arguments":{"mode":"mini","access_key":"wx1","secret_key":"s1"}}}`,
		`{"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":"sessionkey_users","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":22,"method":"tools/call","params":{"name":"sessionkey_scan_disk","arguments":{"dir":"C:/users/wx1"}}}`,
		`{"jsonrpc":"2.0","id":23,"method":"tools/call","params":{"name":"extract_inventory","arguments":{"dir":"C:/pkg"}}}`,
		`{"jsonrpc":"2.0","id":24,"method":"tools/call","params":{"name":"hook_list","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":25,"method":"tools/call","params":{"name":"hook_inject","arguments":{"filename":"probe.js"}}}`,
		`{"jsonrpc":"2.0","id":26,"method":"tools/call","params":{"name":"wxapi_replay","arguments":{"api_name":"request","options":{"url":"https://example.test"}}}}`,
	}, "\n")+"\n")

	wantMethod := map[float64]string{
		1:  "traffic.stats",
		2:  "navigator.pageStack",
		3:  "sessionkey.scanTraffic",
		4:  "sessionkey.scanDecompiled",
		5:  "sessionkey.scanTraffic",
		6:  "engine.start",
		7:  "miniapp.switch",
		8:  "wechat.status",
		9:  "targets.list",
		10: "node.status",
		11: "engine.stop",
		12: "miniapp.list",
		13: "navigator.autoVisit",
		14: "navigator.autoVisitState",
		15: "navigator.stopAutoVisit",
		16: "navigator.getBlockedRedirects",
		17: "code.projects",
		18: "code.expandDir",
		19: "settings.getPaths",
		20: "ak.verify",
		21: "sessionkey.users",
		22: "sessionkey.scan",
		23: "extract.inventory",
		24: "hook.list",
		25: "hook.inject",
		26: "wxapi.replay",
	}
	// 转发顺序与请求顺序一致，所以第 id 条请求就是第 id-1 次转发。
	if len(bridge.calls) != len(wantMethod) {
		t.Fatalf("expected %d forwarded calls, got %d", len(wantMethod), len(bridge.calls))
	}
	for id, method := range wantMethod {
		if toolFailed(t, messages, id) {
			t.Fatalf("工具 id %v 不该失败: %s", id, toolText(t, messages, id))
		}
		if got := bridge.calls[int(id)-1].method; got != method {
			t.Fatalf("工具 id %v 转发到 %s，应为 %s", id, got, method)
		}
	}

	if got := bridge.calls[2].args; got["limit"] != float64(50) {
		t.Fatalf("scan window did not reach the IPC call: %v", got)
	}
	if got := bridge.calls[4].args; len(got) != 0 {
		t.Fatalf("an absent window must not become a limit argument: %v", got)
	}
	if got := bridge.calls[5].args; got["cdpPort"] != float64(31999) {
		t.Fatalf("cdpPort did not reach engine.start: %v", got)
	}
	// options 是被重放的 wx API 的整个 options 对象，必须原样到达。
	options, _ := bridge.calls[25].args["options"].(map[string]any)
	if options["url"] != "https://example.test" {
		t.Fatalf("replay options did not survive the forward: %v", bridge.calls[25].args)
	}
}

// 直通表的 Args 是白名单：未声明的键必须被丢掉，而不是原样透到 IPC。桥本身能到达
// 每一个已注册方法，所以这张白名单是「工具不会退化成通用调用器」的唯一支点。
func TestPassThroughDropsUndeclaredArguments(t *testing.T) {
	bridge := &paramBridge{}
	messages := exchange(t, Deps{AppBridge: bridge}, strings.Join([]string{
		// traffic_stats 一个参数都不声明。
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"traffic_stats","arguments":{"keepRecords":5,"appid":"wxone"}}}`,
		// engine_start 只声明 cdpPort。
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"engine_start","arguments":{"cdpPort":31999,"exec":"calc.exe"}}}`,
		// miniapp_switch 只声明 id。
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"miniapp_switch","arguments":{"id":3,"locked":false}}}`,
	}, "\n")+"\n")

	for id := range map[float64]bool{1: true, 2: true, 3: true} {
		if toolFailed(t, messages, id) {
			t.Fatalf("合法调用不该失败: %s", toolText(t, messages, id))
		}
	}
	if len(bridge.calls) != 3 {
		t.Fatalf("expected 3 forwarded calls, got %d", len(bridge.calls))
	}
	for _, call := range bridge.calls {
		for _, forbidden := range []string{"keepRecords", "appid", "exec", "locked"} {
			if _, present := call.args[forbidden]; present {
				t.Fatalf("%s 把未声明的参数 %q 透到了 IPC：%v", call.method, forbidden, call.args)
			}
		}
	}
	if _, present := bridge.calls[1].args["cdpPort"]; !present {
		t.Fatalf("声明过的参数必须透传：%v", bridge.calls[1].args)
	}
}

// Required 里的键必须在到达 IPC 之前就被拦下：好几个方法对缺参的回答是一个看起来
// 正常的空成功（code.expandDir 会回 {children:[]}），agent 会读成「这个目录是空的」
// 而不是「你少给了参数」。
func TestPassThroughRequiresItsDeclaredArguments(t *testing.T) {
	bridge := &paramBridge{}
	messages := exchange(t, Deps{AppBridge: bridge}, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"code_list_dir","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"sessionkey_scan_disk","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"hook_inject","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"miniapp_switch","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"wxapi_replay","arguments":{"options":{}}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"ak_verify","arguments":{"mode":"mini"}}}`,
	}, "\n")+"\n")

	for id := range map[float64]bool{1: true, 2: true, 3: true, 4: true, 5: true, 6: true} {
		if !toolFailed(t, messages, id) {
			t.Fatalf("缺必填参数必须失败: %s", toolText(t, messages, id))
		}
		if !strings.Contains(toolText(t, messages, id), "missing required argument") {
			t.Fatalf("失败原因要指明缺哪个参数: %s", toolText(t, messages, id))
		}
	}
	if len(bridge.calls) != 0 {
		t.Fatalf("缺参的调用不得触达 IPC：%v", bridge.calls)
	}
}

// replayBridge answers every call with the next page-side payload.
type replayBridge struct {
	payloads []map[string]any
	calls    int
}

func (b *replayBridge) Call(_ context.Context, _ string, _ map[string]any) (any, error) {
	payload := b.payloads[b.calls]
	if b.calls < len(b.payloads)-1 {
		b.calls++
	}
	return payload, nil
}

// 重放是「测试」，把结果读反就会把结论读反：页面侧对「表达式跑通了但被调 API 失败」
// 回的是 {ok:true,status:"fail"}，MCP 边界上必须让 ok 表示「重放成功」，同时把
// status / error 原样留下。注意这里**不**把失败翻成 isError —— 被拒的未授权请求正是
// 一次有价值的测试结果，不是工具故障。
func TestReplayNormalizesTheFailShape(t *testing.T) {
	bridge := &replayBridge{payloads: []map[string]any{
		{"ok": true, "status": "fail", "error": "request:fail timeout"},
		{"ok": true, "status": "success", "result": map[string]any{"data": "ok"}},
		{"ok": false, "status": "fail", "reason": "调用超时"},
	}}
	messages := exchange(t, Deps{AppBridge: bridge}, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wxapi_replay","arguments":{"api_name":"request"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"wxapi_replay","arguments":{"api_name":"request"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"wxapi_replay","arguments":{"api_name":"request"}}}`,
	}, "\n")+"\n")

	failed := toolText(t, messages, 1)
	if !strings.Contains(failed, `"ok":false`) {
		t.Fatalf("被调 API 失败时 ok 必须为 false：%s", failed)
	}
	if !strings.Contains(failed, `"status":"fail"`) || !strings.Contains(failed, "request:fail timeout") {
		t.Fatalf("归一化不得丢掉 status / error：%s", failed)
	}
	success := toolText(t, messages, 2)
	if !strings.Contains(success, `"ok":true`) || !strings.Contains(success, `"status":"success"`) {
		t.Fatalf("成功的重放必须原样通过：%s", success)
	}
	timeout := toolText(t, messages, 3)
	if !strings.Contains(timeout, `"ok":false`) || !strings.Contains(timeout, "调用超时") {
		t.Fatalf("超时要保留 reason 与 ok:false：%s", timeout)
	}
}

// schema 广告的 required / properties 必须与直通表声明的一致：两边不一致时，客户端
// 照 schema 校验会放行一个必然被工具拒掉的调用，或看不到一个能用的开关 —— 那时
// agent 读到的契约就是假的。
func TestPassThroughSchemasMatchTheTable(t *testing.T) {
	advertised := map[string]map[string]any{}
	for _, tool := range New(Deps{ToolProfile: "all"}).tools() {
		advertised[tool.Name] = tool.InputSchema
	}
	if len(advertised) == 0 {
		t.Fatal("tools() 为空")
	}
	for name, spec := range PassThroughTools {
		schema, ok := advertised[name]
		if !ok {
			t.Fatalf("直通工具 %s 没有被 tools/list 列出", name)
		}
		properties := stringSet(schema["properties"])
		declared := stringSet(schema["required"])
		want := map[string]bool{}
		for _, key := range spec.Args {
			if !properties[key] {
				t.Fatalf("%s 的 Args 里有 %q，schema 的 properties 里却没有 —— 客户端看不到这个开关", name, key)
			}
		}
		for _, key := range spec.Required {
			want[key] = true
		}
		for key := range want {
			if !declared[key] {
				t.Fatalf("%s 的表里声明 %q 必填，schema 却没标 required", name, key)
			}
		}
		for key := range declared {
			if !want[key] {
				t.Fatalf("%s 的 schema 把 %q 标成 required，直通表却没声明 —— 客户端以为必填、工具却不拦", name, key)
			}
		}
	}
}

// 多方法工具的选择子必须在 schema 里声明成 required + enum，且枚举值与表里的可接受
// 集合一致：schema 少一个取值时，那个取值对客户端就等于不存在；多一个取值时，客户端
// 会照它发一个必然被拒的调用。
func TestSelectorToolSchemasMatchTheTable(t *testing.T) {
	advertised := map[string]map[string]any{}
	for _, tool := range New(Deps{ToolProfile: "all"}).tools() {
		advertised[tool.Name] = tool.InputSchema
	}
	if len(advertised) == 0 {
		t.Fatal("tools() 为空")
	}
	if len(SelectorTools) == 0 {
		t.Fatal("多方法工具表为空")
	}
	for name, spec := range SelectorTools {
		schema, ok := advertised[name]
		if !ok {
			t.Fatalf("多方法工具 %s 没有被 tools/list 列出", name)
		}
		properties, _ := schema["properties"].(map[string]any)
		field, _ := properties[spec.Selector].(map[string]any)
		if field == nil {
			t.Fatalf("%s 的 schema 没有声明选择子 %q", name, spec.Selector)
		}
		if !stringSet(schema["required"])[spec.Selector] {
			t.Fatalf("%s 的 schema 没有把选择子 %q 标成 required", name, spec.Selector)
		}
		declared := stringSet(field["enum"])
		for value := range spec.Methods {
			if !declared[value] {
				t.Fatalf("%s 的 schema 枚举里没有 %q，但表里接受它 —— 客户端看不到这个取值", name, value)
			}
		}
		for value := range declared {
			if _, ok := spec.Methods[value]; !ok {
				t.Fatalf("%s 的 schema 枚举里有 %q，表里却不接受 —— 客户端会照它发一个必然被拒的调用", name, value)
			}
		}
	}
}

// 广告的工具必须真的实现。在此之前这条不变量只靠人工维护：tools() 里加一条而
// callTool 没跟上时，agent 会在调用那一刻才收到 "unknown tool"，而既有测试都只断言
// 「某个名字在清单里」，拦不住这件事。
// 全 nil 的 Deps 下每个工具都应给出工具级错误（…unavailable 之类）或正常结果，
// 但**绝不能**是 unknown tool。
func TestEveryAdvertisedToolIsImplemented(t *testing.T) {
	tools := New(Deps{}).tools()
	if len(tools) == 0 {
		t.Fatal("tools() 为空，这条断言就失去意义")
	}
	lines := make([]string, 0, len(tools))
	for index, tool := range tools {
		// 空参数：这些调用不得产生副作用（唯一的出网工具 miniapp_http_request
		// 在空 url 上会直接构造失败）。
		lines = append(lines, fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":{}}}`,
			index+1, tool.Name))
	}
	messages := exchange(t, Deps{}, strings.Join(lines, "\n")+"\n")

	for index, tool := range tools {
		if text := toolText(t, messages, float64(index+1)); strings.Contains(text, "unknown tool") {
			t.Errorf("工具 %s 被广告了但没有实现：%s", tool.Name, text)
		}
	}
}

// 一工具落多方法的两处：选择子非法时必须当场拒绝，且不得把请求透到 IPC ——
// 非法值不该变成一次「问我猜的那个方法」的调用。
func TestMultiMethodToolsRejectUnknownSelectors(t *testing.T) {
	bridge := &paramBridge{}
	messages := exchange(t, Deps{AppBridge: bridge}, strings.Join([]string{
		// 只有 wxapi / cloud 有记录流，也就只有它们有丢弃计数。
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hook_stats","arguments":{"name":"console"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"hook_stats","arguments":{"name":"navigator"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"navigator_guard","arguments":{"action":"erase"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"navigator_guard","arguments":{}}}`,
	}, "\n")+"\n")
	for id := range map[float64]bool{1: true, 2: true, 3: true, 4: true} {
		if !toolFailed(t, messages, id) {
			t.Fatalf("非法选择子必须被拒: %s", toolText(t, messages, id))
		}
	}
	if len(bridge.calls) != 0 {
		t.Fatalf("被拒的选择子不得触达 IPC：%v", bridge.calls)
	}
}

// 合法选择子要落到对应的那条方法。
func TestMultiMethodToolsRouteBySelector(t *testing.T) {
	bridge := &paramBridge{}
	messages := exchange(t, Deps{AppBridge: bridge}, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hook_stats","arguments":{"name":"wxapi"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"hook_stats","arguments":{"name":"cloud"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"navigator_guard","arguments":{"action":"enable"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"navigator_guard","arguments":{"action":"disable"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"navigator_guard","arguments":{"action":"state"}}}`,
	}, "\n")+"\n")

	want := []string{"wxapi.stats", "cloud.stats", "navigator.enableRedirectGuard", "navigator.disableRedirectGuard", "navigator.guardState"}
	for index, method := range want {
		if toolFailed(t, messages, float64(index+1)) {
			t.Fatalf("%s 不该失败: %s", method, toolText(t, messages, float64(index+1)))
		}
	}
	if len(bridge.calls) != len(want) {
		t.Fatalf("expected %d forwarded calls, got %d", len(want), len(bridge.calls))
	}
	for index, method := range want {
		if bridge.calls[index].method != method {
			t.Fatalf("第 %d 次转发到 %s，应为 %s", index+1, bridge.calls[index].method, method)
		}
	}
}

// miniapp_scan_sensitive scans the configured decompile output directory, so
// the tool must not advertise a package-source knob its handler never reads.
func TestMiniappScanSensitiveSchemaHasNoPackagesDir(t *testing.T) {
	messages := exchange(t, Deps{Traffic: fakeTraffic{}, Core: &fakeCore{}}, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n")
	tools := byID(messages, 1)["result"].(map[string]any)["tools"].([]any)
	for _, tool := range tools {
		entry := tool.(map[string]any)
		if entry["name"] != "miniapp_scan_sensitive" {
			continue
		}
		props := entry["inputSchema"].(map[string]any)["properties"].(map[string]any)
		if _, ok := props["packages_dir"]; ok {
			t.Fatalf("miniapp_scan_sensitive advertises an unread packages_dir: %v", props)
		}
		if _, ok := props["appid"]; !ok {
			t.Fatalf("miniapp_scan_sensitive must advertise appid: %v", props)
		}
		return
	}
	t.Fatal("miniapp_scan_sensitive tool not listed")
}

func (fakeCore) HookClear(context.Context, string) error { return nil }

func (fakeCore) DebugState(_ context.Context, _ bool) (map[string]any, error) {
	return map[string]any{
		"enabled": true, "paused": false, "pausedSeq": float64(0), "resumedSeq": float64(0),
		"callFrames": []any{}, "scripts": []any{
			map[string]any{"scriptId": "1", "url": "appservice/pages/index.js"},
		}, "scriptsTruncated": float64(0),
	}, nil
}

func (fakeCore) CloudScan(context.Context) ([]map[string]any, error) {
	return []map[string]any{{"name": "hello", "type": "function", "appId": "wxfake"}}, nil
}

func (fakeCore) CDPCommand(_ context.Context, method string, params map[string]any, _ int) (map[string]any, error) {
	if method == "Page.captureScreenshot" {
		return map[string]any{"result": map[string]any{"data": "aW1hZ2U="}}, nil
	}
	if method == "Runtime.evaluate" {
		// 带 contextId 的求值按真目标作答：工具靠它挑对执行上下文。
		if frame, ok := answerContextProbe(stringArg(params, "expression", ""), params, defaultCDPContexts); ok {
			return frame, nil
		}
		return map[string]any{"result": map[string]any{"result": map[string]any{"type": "string", "value": `{"ok":true}`}}}, nil
	}
	if method == "Debugger.getScriptSource" {
		return map[string]any{"result": map[string]any{"scriptSource": "// script source"}}, nil
	}
	return map[string]any{"result": map[string]any{}}, nil
}

type fakeAppBridge struct{}

func (fakeAppBridge) Call(_ context.Context, method string, params map[string]any) (any, error) {
	switch method {
	case "navigator.pages":
		return struct {
			Pages        []string `json:"pages"`
			TabBarPages  []string `json:"tab_bar_pages"`
			CurrentRoute string   `json:"current_route"`
		}{Pages: []string{"pages/index"}, TabBarPages: []string{}, CurrentRoute: "pages/index"}, nil
	case "navigator.pageStack":
		return map[string]any{"stack": []map[string]any{{"route": "pages/index"}}, "current": "pages/index"}, nil
	case "navigator.currentRoute":
		return map[string]any{"route": "pages/index"}, nil
	case "settings.getPaths":
		return map[string]any{"outputDir": "C:/output"}, nil
	case "extract.packages":
		return map[string]any{"packages": []map[string]any{
			{"appid": "wxone", "name": "main.wxapkg", "size": 12, "path": "C:/pkg/main.wxapkg"},
			{"appid": "wxone", "name": "sub.wxapkg", "size": 8, "path": "C:/pkg/sub.wxapkg"},
		}}, nil
	default:
		return map[string]any{"ok": true, "method": method, "params": params}, nil
	}
}

type recordingAppBridge struct{ decompiled []string }

func (r *recordingAppBridge) Call(_ context.Context, method string, params map[string]any) (any, error) {
	switch method {
	case "extract.packages":
		return map[string]any{"packages": []map[string]any{
			{"appid": "wxone", "path": "C:/pkg/main.wxapkg"},
			{"appid": "wxone", "path": "C:/pkg/sub.wxapkg"},
		}}, nil
	case "extract.decompile":
		path, _ := params["path"].(string)
		r.decompiled = append(r.decompiled, path)
		return map[string]any{"output_dir": "C:/output/wxone", "files_count": 4, "packages_processed": 2}, nil
	default:
		return map[string]any{}, nil
	}
}

func TestToolCallTrafficListAndBody(t *testing.T) {
	messages := exchange(t, Deps{Traffic: fakeTraffic{}, Core: &fakeCore{}}, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"traffic_list","arguments":{"apiType":"wx.request"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"traffic_get_body","arguments":{"id":"a1","part":"response"}}}`,
	}, "\n")+"\n")

	list := byID(messages, 1)
	if list == nil {
		t.Fatal("traffic_list response missing")
	}
	content := list["result"].(map[string]any)["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "api.test.com") {
		t.Fatalf("traffic_list result missing url: %s", text)
	}
	// The settled duration rides along: an agent auditing slow calls would
	// otherwise have to fetch each record's body to learn how long it took.
	if !strings.Contains(text, `"durationMs":1830`) {
		t.Fatalf("traffic_list result missing durationMs: %s", text)
	}

	body := byID(messages, 2)
	if body == nil {
		t.Fatal("traffic_get_body response missing")
	}
	text = body["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "LTAI5tABCDEFghijklmnop") {
		t.Fatalf("body result missing secret: %s", text)
	}
}

func TestToolCallEngineStatusAndHookDrain(t *testing.T) {
	messages := exchange(t, Deps{Traffic: fakeTraffic{}, Core: &fakeCore{}}, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"engine_status","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"hook_drain","arguments":{"name":"wxapi","afterSeq":0,"limit":100}}}`,
	}, "\n")+"\n")

	status := byID(messages, 1)
	if status == nil {
		t.Fatal("engine_status response missing")
	}
	text := status["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"frida":true`) {
		t.Fatalf("engine_status result wrong: %s", text)
	}

	drain := byID(messages, 2)
	if drain == nil {
		t.Fatal("hook_drain response missing")
	}
	text = drain["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"nextSeq":1`) {
		t.Fatalf("hook_drain result wrong: %s", text)
	}
	// Settled updates must reach the agent: without them a slow async call
	// looks permanently pending in the drain stream.
	if !strings.Contains(text, `"updates":[`) || !strings.Contains(text, `wx.request-wxone-1-1`) {
		t.Fatalf("hook_drain result missing settled updates: %s", text)
	}
}

// hook_drain 的更新流必须像记录流一样可分页：agent 传回上一页的 nextUpdateSeq，
// 否则每次都从 0 取，只会反复看到最旧的那一窗落定帧。
func TestHookDrainPagesTheUpdateStream(t *testing.T) {
	// 第一次调用：显式带上游标，core 必须收到 7（而不是每轮从 0 取最旧的那一窗）
	core := &fakeCore{}
	first := exchange(t, Deps{Traffic: fakeTraffic{}, Core: core},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hook_drain","arguments":{"name":"wxapi","afterSeq":0,"limit":100,"afterUpdateSeq":7,"updateLimit":3}}}`+"\n")
	if core.lastUpdateSeq != 7 {
		t.Fatalf("afterUpdateSeq was not forwarded: core saw %d, want 7", core.lastUpdateSeq)
	}
	text := byID(first, 1)["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"nextUpdateSeq":8`) {
		t.Fatalf("result must carry the next update cursor so the caller can page: %s", text)
	}

	// 缺省调用：不传游标时仍取一小窗（不是 0，否则等于「不要更新」）
	plain := &fakeCore{}
	_ = exchange(t, Deps{Traffic: fakeTraffic{}, Core: plain},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hook_drain","arguments":{"name":"wxapi"}}}`+"\n")
	if plain.lastUpdateSeq != 0 {
		t.Fatalf("a default call starts at the beginning of the window, got %d", plain.lastUpdateSeq)
	}
	// 显式 updateLimit 0 = 只要记录流（Core 侧同一语义）
	recordsOnly := &fakeCore{}
	_ = exchange(t, Deps{Traffic: fakeTraffic{}, Core: recordsOnly},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hook_drain","arguments":{"name":"wxapi","updateLimit":0,"afterUpdateSeq":5}}}`+"\n")
	if recordsOnly.lastUpdateLimit != 0 {
		t.Fatalf("updateLimit 0 must reach the core as records-only, got %d", recordsOnly.lastUpdateLimit)
	}
}

func TestMiniappToolsDispatch(t *testing.T) {
	deps := Deps{Core: &fakeCore{}, AppBridge: fakeAppBridge{}}
	messages := exchange(t, deps, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_screenshot","arguments":{"format":"png"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"miniapp_cloud_scan","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"miniapp_get_routes","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"miniapp_list_packages","arguments":{"packages_dir":"C:/pkg"}}}`,
	}, "\n")+"\n")

	shot := byID(messages, 1)["result"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if shot["type"] != "image" || shot["mimeType"] != "image/png" || shot["data"] != "aW1hZ2U=" {
		t.Fatalf("screenshot content: %#v", shot)
	}
	cloudText := byID(messages, 2)["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(cloudText, `"name":"hello"`) {
		t.Fatalf("cloud scan content: %s", cloudText)
	}
	routesText := byID(messages, 3)["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(routesText, `"total":1`) || !strings.Contains(routesText, "pages/index") {
		t.Fatalf("routes content: %s", routesText)
	}
	packagesText := byID(messages, 4)["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(packagesText, `"total_packages":2`) || !strings.Contains(packagesText, `"wxone"`) || !strings.Contains(packagesText, `"details"`) {
		t.Fatalf("packages content: %s", packagesText)
	}
}

func TestMiniappDecompileProcessesEveryMatchedPackageOnce(t *testing.T) {
	bridge := &recordingAppBridge{}
	messages := exchange(t, Deps{AppBridge: bridge},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_decompile","arguments":{"appid":"wxone","packages_dir":"C:/pkg"}}}`+"\n")
	text := byID(messages, 1)["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if len(bridge.decompiled) != 1 || bridge.decompiled[0] != "" {
		t.Fatalf("decompile delegation: %#v", bridge.decompiled)
	}
	if !strings.Contains(text, `"packages_processed":2`) || !strings.Contains(text, `"files_count":4`) {
		t.Fatalf("decompile content: %s", text)
	}
}

func TestEveryMiniappToolHasCallableDispatch(t *testing.T) {
	// The file/search tools stat their argument before delegating, so the
	// dispatch sweep needs real paths on every platform.
	codeRoot := t.TempDir()
	codeRootFile := filepath.Join(codeRoot, "app.js")
	if err := os.WriteFile(codeRootFile, []byte("const x = 1;"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(codeRoot, "wxone"), 0o750); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer httpServer.Close()
	deps := Deps{
		Core: &fakeCore{}, AppBridge: pathsBridge{outputDir: codeRoot},
		AllowedCodeRoot: codeRoot,
		Scan: func(context.Context, string) (ScanResult, error) {
			return ScanResult{FilesScanned: 1, Summary: map[string]int{"secret": 1}, Report: json.RawMessage(`{"result":{"secret":["x"]}}`)}, nil
		},
	}
	cases := []struct {
		name string
		args map[string]any
	}{
		{"miniapp_screenshot", map[string]any{"format": "png"}},
		{"miniapp_click", map[string]any{"x": 1, "y": 2}},
		{"miniapp_type", map[string]any{"text": "a"}},
		{"miniapp_navigate", map[string]any{"route": "pages/index", "method": "navigateTo"}},
		{"miniapp_get_routes", map[string]any{}},
		{"miniapp_get_current_route", map[string]any{}},
		{"miniapp_scroll", map[string]any{"x": 0, "y": 10}},
		{"miniapp_list_contexts", map[string]any{}},
		{"miniapp_evaluate", map[string]any{"expression": "1+1"}},
		{"miniapp_console_log", map[string]any{}},
		{"miniapp_get_info", map[string]any{}},
		{"miniapp_get_storage", map[string]any{}},
		{"miniapp_get_storage_key", map[string]any{"key": "token"}},
		{"miniapp_call_cloud", map[string]any{"name": "hello", "data": map[string]any{}}},
		{"miniapp_cloud_captures", map[string]any{}},
		{"miniapp_cloud_scan", map[string]any{}},
		{"miniapp_http_request", map[string]any{"method": "GET", "url": httpServer.URL}},
		{"miniapp_decompile", map[string]any{"appid": "wxone", "packages_dir": "C:/pkg"}},
		{"miniapp_scan_sensitive", map[string]any{"appid": "wxone"}},
		{"miniapp_read_file", map[string]any{"path": codeRootFile}},
		{"miniapp_search_code", map[string]any{"root": codeRoot, "query": "x"}},
		{"miniapp_list_packages", map[string]any{"packages_dir": "C:/pkg"}},
		{"miniapp_get_skills", map[string]any{}},
		{"miniapp_set_breakpoint", map[string]any{"url": "app.js", "line": 1}},
		{"miniapp_remove_breakpoint", map[string]any{"breakpoint_id": "bp1"}},
		{"miniapp_get_source", map[string]any{"url_pattern": "appservice"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, _ := json.Marshal(tc.args)
			line := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tc.name + `","arguments":` + string(args) + `}}` + "\n"
			messages := exchange(t, deps, line)
			response := byID(messages, 1)
			if response == nil {
				t.Fatal("response missing")
			}
			result := response["result"].(map[string]any)
			if result["isError"] == true {
				content := result["content"].([]any)[0].(map[string]any)["text"]
				t.Fatalf("tool returned error: %v", content)
			}
		})
	}
}

func TestUnknownToolYieldsToolError(t *testing.T) {
	messages := exchange(t, Deps{Traffic: fakeTraffic{}, Core: &fakeCore{}},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope","arguments":{}}}`+"\n")

	call := byID(messages, 1)
	if call == nil {
		t.Fatal("response missing")
	}
	result := call["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("unknown tool must be a tool error: %v", result)
	}
}

func TestMalformedLineIsSkipped(t *testing.T) {
	messages := exchange(t, Deps{Traffic: fakeTraffic{}, Core: &fakeCore{}},
		"not json at all\n"+`{"jsonrpc":"2.0","id":7,"method":"tools/list"}`+"\n")
	if byID(messages, 7) == nil {
		t.Fatalf("valid request after malformed line lost: %v", messages)
	}
}

// miniapp_scan_sensitive joins the caller's appid into the output root; an
// appid containing path segments would scan arbitrary directories.
func TestMiniappScanSensitiveRejectsAppIDPathEscape(t *testing.T) {
	var scanned []string
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "wxone"), 0o750); err != nil {
		t.Fatal(err)
	}
	deps := Deps{
		AppBridge: pathsBridge{outputDir: root},
		Scan: func(_ context.Context, path string) (ScanResult, error) {
			scanned = append(scanned, path)
			return ScanResult{}, nil
		},
	}

	for _, appid := range []string{"../../..", "..", "wx/../../etc", ""} {
		payload, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      77,
			"method":  "tools/call",
			"params": map[string]any{
				"name":      "miniapp_scan_sensitive",
				"arguments": map[string]any{"appid": appid},
			},
		})
		messages := exchange(t, deps, string(payload)+"\n")
		text := byID(messages, 77)["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
		if !strings.Contains(text, "appid") {
			t.Fatalf("appid %q should be rejected, got %s", appid, text)
		}
	}
	if len(scanned) != 0 {
		t.Fatalf("scanner ran for rejected appids: %#v", scanned)
	}

	// A well-formed appid still scans the app's output directory.
	messages := exchange(t, deps,
		`{"jsonrpc":"2.0","id":78,"method":"tools/call","params":{"name":"miniapp_scan_sensitive","arguments":{"appid":"wxone"}}}`+"\n")
	text := byID(messages, 78)["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if strings.Contains(text, "appid") && strings.Contains(text, "invalid") {
		t.Fatalf("valid appid rejected: %s", text)
	}
	if len(scanned) != 1 || !strings.HasSuffix(scanned[0], "wxone") {
		t.Fatalf("valid appid should scan its output dir: %#v", scanned)
	}
}

// realCodeBridge routes code.* to the real IPC helpers so the MCP file tools
// exercise production read/search behavior instead of a canned reply.
type realCodeBridge struct{}

// callToolWithCodeRoot runs one tools/call against the real code bridge with
// the code-root pin set to root — the production shape of the file tools.
func callToolWithCodeRoot(t *testing.T, root string, id int, name string, args map[string]any) map[string]any {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params":  map[string]any{"name": name, "arguments": args},
	})
	messages := exchange(t, Deps{AppBridge: realCodeBridge{}, AllowedCodeRoot: root}, string(payload)+"\n")
	call := byID(messages, float64(id))
	if call == nil {
		t.Fatalf("%s produced no response", name)
	}
	return call["result"].(map[string]any)
}

func (realCodeBridge) Call(_ context.Context, method string, params map[string]any) (any, error) {
	switch method {
	case "code.readFile":
		path, _ := params["path"].(string)
		return ipc.ReadCodeFile(path), nil
	case "code.search":
		root, _ := params["root"].(string)
		query, _ := params["query"].(string)
		useRegex, _ := params["regex"].(bool)
		hits, truncated, err := ipc.SearchCode(root, query, useRegex)
		if err != nil {
			return map[string]any{"results": []ipc.SearchHit{}, "truncated": false, "error": err.Error()}, nil
		}
		if hits == nil {
			hits = []ipc.SearchHit{}
		}
		return map[string]any{"results": hits, "truncated": truncated}, nil
	}
	return fakeAppBridge{}.Call(context.Background(), method, params)
}

func callToolWithBridge(t *testing.T, bridge AppBridge, id int, name string, args map[string]any) map[string]any {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params":  map[string]any{"name": name, "arguments": args},
	})
	// 代码浏览工具被钉在反编译输出树里：测试的临时目录都在系统 Temp 下，
	// 用它当允许根即可覆盖所有用例。
	messages := exchange(t, Deps{AppBridge: bridge, AllowedCodeRoot: os.TempDir()}, string(payload)+"\n")
	call := byID(messages, float64(id))
	if call == nil {
		t.Fatalf("%s produced no response", name)
	}
	return call["result"].(map[string]any)
}

func toolResultText(t *testing.T, result map[string]any) string {
	t.Helper()
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("tool result has no content: %v", result)
	}
	return content[0].(map[string]any)["text"].(string)
}

// A missing path answers with {"error": "File not found: <path>"}; an
// empty success would let an MCP client read a missing file as an empty one.
func TestMiniappReadFileReportsMissingPath(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "nope.js")
	result := callToolWithCodeRoot(t, root, 1, "miniapp_read_file", map[string]any{"path": missing})
	if result["isError"] != true {
		t.Fatalf("missing file must be a tool error, got %v", result)
	}
	text := toolResultText(t, result)
	if !strings.Contains(text, "File not found") || !strings.Contains(text, missing) {
		t.Fatalf("missing file message: %s", text)
	}
}

func TestMiniappReadFileReadsExistingFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "app.js")
	if err := os.WriteFile(path, []byte("const a = 1;"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := callToolWithCodeRoot(t, root, 2, "miniapp_read_file", map[string]any{"path": path})
	if result["isError"] == true {
		t.Fatalf("existing file rejected: %v", result)
	}
	text := toolResultText(t, result)
	if !strings.Contains(text, "const a = 1;") || !strings.Contains(text, `"size":12`) {
		t.Fatalf("read content: %s", text)
	}
}

// max_length is a byte budget, but cutting mid-rune would hand the client a
// 图片在 code.readFile 里带内联 data URL：几 MB 的 base64 进 MCP 结果只会占满
// 上下文，而这个工具读的是源码。kind 必须留下，否则空 content 读起来像空文件。
func TestMiniappReadFileStripsImageDataURL(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "logo.png")
	if err := os.WriteFile(path, append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...), 0o600); err != nil {
		t.Fatal(err)
	}
	result := callToolWithCodeRoot(t, root, 9, "miniapp_read_file", map[string]any{"path": path})
	if result["isError"] == true {
		t.Fatalf("image read rejected: %v", result)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(toolResultText(t, result)), &data); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if data["kind"] != "image" {
		t.Fatalf("kind: %v", data["kind"])
	}
	if _, ok := data["dataUrl"]; ok {
		t.Fatalf("dataUrl must not reach the MCP client")
	}
	if content, _ := data["content"].(string); !strings.Contains(content, "图片") {
		t.Fatalf("empty content would read as an empty file: %q", content)
	}
}

// max_length is a byte budget, but cutting mid-rune would hand the client a
// replacement character at the tail instead of the text it asked for.
func TestMiniappReadFileTruncatesOnRuneBoundary(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "cn.js")
	// 每个汉字 3 字节：4 字节的上限正好落在第二个字的中间
	if err := os.WriteFile(path, []byte("代码浏览"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := callToolWithCodeRoot(t, root, 6, "miniapp_read_file", map[string]any{"path": path, "max_length": 4})
	if result["isError"] == true {
		t.Fatalf("read failed: %v", result)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(toolResultText(t, result)), &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	content, _ := payload["content"].(string)
	if payload["truncated"] != true {
		t.Fatalf("4 bytes must count as truncated: %v", payload)
	}
	if !utf8.ValidString(content) || content != "代" {
		t.Fatalf("content cut mid-rune: %q", content)
	}
}

func TestMiniappReadFileRejectsDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	result := callToolWithCodeRoot(t, root, 3, "miniapp_read_file", map[string]any{"path": filepath.Join(root, "sub")})
	if result["isError"] != true {
		t.Fatalf("directory must be a tool error, got %v", result)
	}
}

func TestMiniappSearchCodeReportsMissingRoot(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "nope")
	result := callToolWithCodeRoot(t, root, 4, "miniapp_search_code", map[string]any{"root": missing, "query": "token"})
	if result["isError"] != true {
		t.Fatalf("missing root must be a tool error, got %v", result)
	}
	if text := toolResultText(t, result); !strings.Contains(text, "Directory not found") {
		t.Fatalf("missing root message: %s", text)
	}
}

func TestMiniappSearchCodeSurfacesInvalidRegex(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app.js"), []byte("token=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := callToolWithCodeRoot(t, root, 5, "miniapp_search_code", map[string]any{"root": root, "query": "[", "regex": true})
	if result["isError"] != true {
		t.Fatalf("invalid regex must be a tool error, got %v", result)
	}
}

func TestMiniappSearchCodeFindsHits(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app.js"), []byte("let token = 1;\nlet other = 2;"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := callToolWithCodeRoot(t, root, 6, "miniapp_search_code", map[string]any{"root": root, "query": "token"})
	if result["isError"] == true {
		t.Fatalf("search failed: %v", result)
	}
	text := toolResultText(t, result)
	if !strings.Contains(text, `"total":1`) || !strings.Contains(text, "token = 1") {
		t.Fatalf("search content: %s", text)
	}
}

// pathsBridge answers settings.getPaths with a caller-chosen output root.
type pathsBridge struct{ outputDir string }

func (b pathsBridge) Call(_ context.Context, method string, params map[string]any) (any, error) {
	if method == "settings.getPaths" {
		return map[string]any{"outputDir": b.outputDir}, nil
	}
	return fakeAppBridge{}.Call(context.Background(), method, params)
}

// noPackagesBridge reports "no packages directory detected" like a machine
// without WeChat installed.
type noPackagesBridge struct{}

func (noPackagesBridge) Call(_ context.Context, method string, params map[string]any) (any, error) {
	if method == "extract.defaultDir" {
		return map[string]any{"dir": ""}, nil
	}
	return fakeAppBridge{}.Call(context.Background(), method, params)
}

// Scanning an appid whose output directory does not exist yet is refused;
// reporting a clean 0-file scan would hide a missing decompile step.
func TestMiniappScanSensitiveReportsMissingDecompiledOutput(t *testing.T) {
	root := t.TempDir()
	scanned := []string{}
	deps := Deps{
		AppBridge: pathsBridge{outputDir: root},
		Scan: func(_ context.Context, path string) (ScanResult, error) {
			scanned = append(scanned, path)
			return ScanResult{}, nil
		},
	}
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 90, "method": "tools/call",
		"params": map[string]any{"name": "miniapp_scan_sensitive", "arguments": map[string]any{"appid": "wxone"}},
	})
	messages := exchange(t, deps, string(payload)+"\n")
	result := byID(messages, 90)["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("missing output dir must be a tool error, got %v", result)
	}
	if text := toolResultText(t, result); !strings.Contains(text, "Decompiled output not found") {
		t.Fatalf("missing output message: %s", text)
	}
	if len(scanned) != 0 {
		t.Fatalf("scanner ran without decompiled output: %#v", scanned)
	}
}

// Answer with "Cannot find packages directory..." when neither the
// caller nor the platform default supplied one.
func TestMiniappListPackagesReportsMissingPackagesDir(t *testing.T) {
	result := callToolWithBridge(t, noPackagesBridge{}, 91, "miniapp_list_packages", map[string]any{})
	if result["isError"] != true {
		t.Fatalf("missing packages dir must be a tool error, got %v", result)
	}
	if text := toolResultText(t, result); !strings.Contains(text, "Cannot find packages directory") {
		t.Fatalf("missing packages message: %s", text)
	}
}

// Decompiling without a package source directory is refused instead of
// reporting an empty match list.
func TestMiniappDecompileReportsMissingPackagesDir(t *testing.T) {
	result := callToolWithBridge(t, noPackagesBridge{}, 92, "miniapp_decompile", map[string]any{"appid": "wxone"})
	if result["isError"] != true {
		t.Fatalf("missing packages dir must be a tool error, got %v", result)
	}
	if text := toolResultText(t, result); !strings.Contains(text, "Cannot find wxapkg packages directory") {
		t.Fatalf("missing packages message: %s", text)
	}
}

// miniapp_get_skills exposes the skill directory to MCP clients: only the
// top-level .md files, README excluded, in name order.
func TestReadSkillsListsSkillFilesOnly(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"b-skill.md":                          "b-marker",
		"a-skill.md":                          "a-marker",
		"README.md":                           "readme-marker",
		"notes.txt":                           "note-marker",
		filepath.Join("nested", "c-skill.md"): "nested-marker",
	}
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	result, err := readSkills(dir)
	if err != nil {
		t.Fatalf("readSkills: %v", err)
	}
	payload := objectMap(result)
	text, _ := payload["skills"].(string)
	if payload["skills_dir"] != dir {
		t.Fatalf("skills_dir: %v", payload["skills_dir"])
	}
	for _, want := range []string{"a-marker", "b-marker"} {
		if !strings.Contains(text, want) {
			t.Fatalf("skill %q missing from %q", want, text)
		}
	}
	for _, unwanted := range []string{"readme-marker", "note-marker", "nested-marker"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("%q must not be listed: %q", unwanted, text)
		}
	}
	if strings.Index(text, "a-marker") > strings.Index(text, "b-marker") {
		t.Fatalf("skills must be sorted by file name: %q", text)
	}
}

// The skills the release ships are the ones miniapp_get_skills has to serve.
func TestShippedSkillsLoad(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "resources", "skills")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read shipped skills: %v", err)
	}
	listed := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") || strings.EqualFold(entry.Name(), "README.md") {
			continue
		}
		listed++
	}
	if listed == 0 {
		t.Fatalf("shipped skills missing under %s", dir)
	}

	result, err := readSkills(dir)
	if err != nil {
		t.Fatalf("readSkills: %v", err)
	}
	text, _ := objectMap(result)["skills"].(string)
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") || strings.EqualFold(entry.Name(), "README.md") {
			continue
		}
		if !strings.Contains(text, "["+entry.Name()+"]") {
			t.Fatalf("shipped skill %s was not served", entry.Name())
		}
	}
}

// The MCP surface is what external clients script against, so a tool that
// disappears is a feature regression even when nothing in this repo calls it.
// The fixture is the frozen v0.1.0 tool inventory.
func TestCompatMcpToolsStayExposed(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "compat-surface.json"))
	if err != nil {
		t.Fatalf("read compat surface fixture: %v", err)
	}
	var surface struct {
		MCPTools []string `json:"mcpTools"`
	}
	if err := json.Unmarshal(data, &surface); err != nil {
		t.Fatalf("parse compat surface fixture: %v", err)
	}
	if len(surface.MCPTools) == 0 {
		t.Fatal("compat surface fixture has no MCP tools")
	}

	exposed := map[string]bool{}
	for _, tool := range New(Deps{ToolProfile: "all"}).tools() {
		if exposed[tool.Name] {
			t.Errorf("MCP tool %q is served twice", tool.Name)
		}
		exposed[tool.Name] = true
	}
	// "keep serving" 的语义是可调用，不是广播：lean 目录收敛后，旧名字仍要
	// 能被 tools/call 调用（不回答 unknown tool），只是不再出现在 tools/list。
	lines := make([]string, 0, len(surface.MCPTools))
	for index, name := range surface.MCPTools {
		lines = append(lines, fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":{}}}`,
			index+1, name))
	}
	dispatch := exchange(t, Deps{Traffic: fakeTraffic{}, Core: &fakeCore{}, AppBridge: fakeAppBridge{}}, strings.Join(lines, "\n")+"\n")
	for index, name := range surface.MCPTools {
		if text := toolText(t, dispatch, float64(index+1)); strings.Contains(text, "unknown tool") {
			t.Errorf("compat MCP tool %q is no longer served: %s", name, text)
		}
	}
	for _, name := range surface.MCPTools {
		if !exposed[name] {
			t.Errorf("compat MCP tool %q disappeared from the all profile", name)
		}
	}
	// These additions sit on top of the frozen surface; losing one is
	// just as much a regression as losing a pinned tool. A rename keeps
	// TestEveryAdvertisedToolIsImplemented green, so this list is what actually
	// catches it.
	for _, name := range []string{
		"engine_status", "hook_start", "hook_stop", "hook_drain",
		"traffic_list", "traffic_get_body", "scan_dir",
		"traffic_stats", "navigator_page_stack",
		"sessionkey_scan_traffic", "sessionkey_scan_decompiled",
		// 建立会话
		"wechat_status", "node_status", "engine_start", "engine_stop",
		"miniapp_list", "miniapp_switch", "targets_list",
		// 采集健康 / 驱动 / 验证
		"hook_stats", "wxapi_replay",
		"navigator_auto_visit", "navigator_auto_visit_state",
		"navigator_stop_auto_visit", "navigator_guard", "navigator_blocked_redirects",
		// 代码 / 凭据闭环
		"code_projects", "code_list_dir", "paths_get", "ak_verify",
		"sessionkey_users", "sessionkey_scan_disk", "extract_inventory",
		"hook_list", "hook_inject",
	} {
		if !exposed[name] {
			t.Errorf("added MCP tool %q is missing", name)
		}
	}
}

// captureBridge records the desktop IPC methods the MCP layer routed to.
type captureBridge struct{ calls []string }

func (b *captureBridge) Call(_ context.Context, method string, _ map[string]any) (any, error) {
	b.calls = append(b.calls, method)
	return map[string]any{"ok": true}, nil
}

// 采集开关走桌面壳已注册的 IPC（wxapi.start / cloud.stop 等），MCP 不自己实现一套：
// agent 只有显式开口之后才有东西可 drain —— 连接小程序本身不记录任何东西，所以
// 「连上就 drain」再也不是一条隐式采集通道。
func TestHookStartStopRouteToTheCaptureIPC(t *testing.T) {
	bridge := &captureBridge{}
	deps := Deps{Core: &fakeCore{}, AppBridge: bridge}
	messages := exchange(t, deps, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hook_start","arguments":{"name":"wxapi"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"hook_stop","arguments":{"name":"cloud"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"hook_start","arguments":{"name":"navigator"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"hook_stop","arguments":{"name":"console"}}}`,
	}, "\n")+"\n")

	for _, id := range []float64{1, 2} {
		result := byID(messages, id)["result"].(map[string]any)
		if result["isError"] == true {
			t.Fatalf("工具 %v 不该报错: %v", id, result["content"])
		}
	}
	if len(bridge.calls) != 2 || bridge.calls[0] != "wxapi.start" || bridge.calls[1] != "cloud.stop" {
		t.Fatalf("采集开关必须落到已注册的 IPC：%v", bridge.calls)
	}

	// navigator 是控制类钩子（没有记录流）、console 连接即常开：两者都不该由 agent
	// 启停，更不该把请求透到 IPC。
	for _, id := range []float64{3, 4} {
		if result := byID(messages, id)["result"].(map[string]any); result["isError"] != true {
			t.Fatalf("工具 %v 必须拒绝不可采集的钩子: %v", id, result)
		}
	}
	if len(bridge.calls) != 2 {
		t.Fatalf("被拒的参数不得触达 IPC：%v", bridge.calls)
	}
}
