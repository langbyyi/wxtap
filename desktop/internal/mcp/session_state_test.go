package mcp

// session_start / hook_start 的幂等矩阵与断线自愈：target 未变不重跑 switch、
// feeder 自认在跑但页内钩子已亡时强制重装、健康时 no-op（already）、采集断线
// 以 capture_health 浮出。IPC 形状镜像真壳：id/stats 的字段与 JSON 解码后一致。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// stateBridge models the desktop IPC a session needs: a locked target marker
// and feeder running flags that follow start/stop the way the real feeder does.
type stateBridge struct {
	calls   []string
	locked  float64
	running bool
}

func (b *stateBridge) Call(_ context.Context, method string, _ map[string]any) (any, error) {
	b.calls = append(b.calls, method)
	switch method {
	case "miniapp.list":
		return map[string]any{"list": []any{
			map[string]any{"id": b.locked, "appid": "wxaaa", "name": "一号", "locked": true},
		}}, nil
	case "wxapi.stats", "cloud.stats":
		return map[string]any{"running": b.running}, nil
	case "navigator.pageStack":
		return map[string]any{"stack": []any{}, "current": "pages/index"}, nil
	case "wxapi.start", "cloud.start":
		b.running = true
	case "wxapi.stop", "cloud.stop":
		b.running = false
	}
	return map[string]any{"ok": true}, nil
}

// stateCore serves a configurable generation, records HookClear, and answers
// the hook-alive probe with "yes" (the page-side audit object is installed).
type stateCore struct {
	recordingCore
	clears []string
	gen    int64
}

func (c *stateCore) Status(_ context.Context) (Status, error) {
	return Status{Frida: true, Miniapp: true, Generation: c.gen}, nil
}

func (c *stateCore) HookClear(_ context.Context, name string) error {
	c.clears = append(c.clears, name)
	return nil
}

func (c *stateCore) CDPCommand(ctx context.Context, method string, params map[string]any, timeout int) (map[string]any, error) {
	if method == "Runtime.evaluate" {
		if expr, _ := params["expression"].(string); strings.Contains(expr, "Audit==='undefined'") {
			return evaluateStringResult("yes"), nil
		}
	}
	return c.recordingCore.CDPCommand(ctx, method, params, timeout)
}

// serveOn runs tool calls against ONE server instance: the install bookkeeping
// session_start keeps must survive across calls, which the per-call exchange()
// helper (a fresh server each time) cannot model.
func serveOn(t *testing.T, server *Server, input string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := server.Serve(strings.NewReader(input), &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	var messages []map[string]any
	scanner := bufio.NewScanner(&out)
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

func callToolOn(t *testing.T, server *Server, id int, name, arguments string) (bool, string) {
	t.Helper()
	messages := serveOn(t, server,
		fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`+"\n", id, name, arguments))
	return toolFailed(t, messages, float64(id)), toolText(t, messages, float64(id))
}

func sessionStateServer(bridge *stateBridge, core *stateCore) *Server {
	return New(Deps{Core: core, AppBridge: bridge})
}

// 同 target 不得重跑 switch：那会重置 generation 与 feeder ack，把正在工作的
// 会话拆了重来（实机观测 generation 97→1 由此而来）。
func TestSessionStartSkipsSwitchWhenTargetAlreadyLocked(t *testing.T) {
	bridge := &stateBridge{locked: 1}
	server := sessionStateServer(bridge, &stateCore{gen: 7})
	if failed, text := callToolOn(t, server, 1, "session_start", `{"target":1}`); failed {
		t.Fatalf("session_start 不该失败: %s", text)
	}
	for _, call := range bridge.calls {
		if call == "miniapp.switch" {
			t.Fatalf("target 已锁定时不得重跑 switch: %v", bridge.calls)
		}
	}
}

// 首次启动：feeder 没在跑 → 直接 start，并清一次页内缓冲（采集窗口从现在起算）。
func TestSessionStartFreshInstallClearsThePageBuffer(t *testing.T) {
	bridge := &stateBridge{locked: 1}
	core := &stateCore{gen: 7}
	server := sessionStateServer(bridge, core)
	if failed, text := callToolOn(t, server, 1, "session_start", `{}`); failed {
		t.Fatalf("session_start 不该失败: %s", text)
	}
	if !contains(bridge.calls, "wxapi.start") {
		t.Fatalf("应启动 wxapi 钩子: %v", bridge.calls)
	}
	if len(core.clears) == 0 || !contains(core.clears, "wxapi") {
		t.Fatalf("新窗口必须清页内缓冲: %v", core.clears)
	}
}

// 健康复用：第二次 session_start 是 no-op（already），不再 start/stop，也不再
// 清缓冲——窗口不被拆掉。
func TestSessionStartReusesAHealthyHook(t *testing.T) {
	bridge := &stateBridge{locked: 1}
	core := &stateCore{gen: 7}
	server := sessionStateServer(bridge, core)
	if failed, text := callToolOn(t, server, 1, "session_start", `{}`); failed {
		t.Fatalf("第一次 session_start 不该失败: %s", text)
	}
	failed, text := callToolOn(t, server, 2, "session_start", `{}`)
	if failed {
		t.Fatalf("第二次 session_start 不该失败: %s", text)
	}
	firstHook := 0
	for _, call := range bridge.calls {
		if call == "wxapi.start" {
			firstHook++
		}
		if firstHook > 1 {
			t.Fatalf("健康钩子不得重启: %v", bridge.calls)
		}
	}
	if len(core.clears) != 1 {
		t.Fatalf("缓冲只该在首启时清一次: %v", core.clears)
	}
	var payload struct {
		Capture map[string]map[string]any `json:"capture"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Capture["wxapi"]["already"] != true {
		t.Fatalf("健康复用应标注 already: %s", text)
	}
}

// feeder 自认在跑但页内钩子已亡（installGen 未记账）：stop + start 重装。
func TestSessionStartRestartsAHookNobodyRecorded(t *testing.T) {
	bridge := &stateBridge{locked: 1, running: true}
	server := sessionStateServer(bridge, &stateCore{gen: 3})
	if failed, text := callToolOn(t, server, 1, "session_start", `{}`); failed {
		t.Fatalf("session_start 不该失败: %s", text)
	}
	if !contains(bridge.calls, "wxapi.stop") || !contains(bridge.calls, "wxapi.start") {
		t.Fatalf("无记账的在跑钩子必须重装: %v", bridge.calls)
	}
}

// generation 换代（引擎重启/切页重建）而页内钩子已亡：重装。
func TestSessionStartReinstallsAfterAGenerationMove(t *testing.T) {
	bridge := &stateBridge{locked: 1}
	core := &stateCore{gen: 3}
	server := sessionStateServer(bridge, core)
	if failed, text := callToolOn(t, server, 1, "session_start", `{}`); failed {
		t.Fatalf("session_start 不该失败: %s", text)
	}
	core.gen = 9 // realm 重建：页内钩子随之消失
	if failed, text := callToolOn(t, server, 2, "session_start", `{}`); failed {
		t.Fatalf("换代后的 session_start 不该失败: %s", text)
	}
	if !contains(bridge.calls, "wxapi.stop") {
		t.Fatalf("换代后必须重装: %v", bridge.calls)
	}
}

// 换代但 autoInject 已把页内钩子装回去（探测 yes）：不算断线，记账刷新即可。
func TestCaptureHealthAcceptsAnExternallyReinstalledHook(t *testing.T) {
	bridge := &stateBridge{locked: 1}
	core := &stateCore{gen: 3}
	server := sessionStateServer(bridge, core)
	if failed, text := callToolOn(t, server, 1, "session_start", `{}`); failed {
		t.Fatalf("session_start 不该失败: %s", text)
	}
	core.gen = 9
	bridge.running = true // feeder 还在跑；页内探针仍答 yes（autoInject 重装过）
	failed, text := callToolOn(t, server, 2, "miniapp_page_stack", `{}`)
	if failed {
		t.Fatalf("page_stack 不该失败: %s", text)
	}
	if strings.Contains(text, "capture_health") {
		t.Fatalf("外部已重装的健康会话不得报断线: %s", text)
	}
	if contains(bridge.calls, "wxapi.stop") {
		t.Fatalf("健康会话不得被重装: %v", bridge.calls)
	}
}

// 采集断线只在异常时浮出：feeder 停了 → 会话类工具回包带 capture_health；
// 健康时不得戳这个字段。
func TestCaptureHealthStampsOnlyBrokenReplies(t *testing.T) {
	bridge := &stateBridge{locked: 1}
	core := &stateCore{gen: 3}
	server := sessionStateServer(bridge, core)
	if failed, text := callToolOn(t, server, 1, "session_start", `{}`); failed {
		t.Fatalf("session_start 不该失败: %s", text)
	}
	failed, text := callToolOn(t, server, 2, "miniapp_page_stack", `{}`)
	if failed {
		t.Fatalf("page_stack 不该失败: %s", text)
	}
	if strings.Contains(text, "capture_health") {
		t.Fatalf("健康会话不得带 capture_health: %s", text)
	}
	bridge.running = false // feeder 断了（引擎重启、GUI 手动停都算）
	failed, text = callToolOn(t, server, 3, "miniapp_page_stack", `{}`)
	if failed {
		t.Fatalf("page_stack 不该失败: %s", text)
	}
	if !strings.Contains(text, "capture_health") {
		t.Fatalf("断线必须在回包浮出: %s", text)
	}
	var payload struct {
		CaptureHealth struct {
			Hooks map[string]struct {
				Running bool `json:"running"`
			} `json:"hooks"`
		} `json:"capture_health"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.CaptureHealth.Hooks["wxapi"].Running {
		t.Fatalf("断线钩子应如实标注 running=false: %s", text)
	}
}

// hook_stop 撤销记账：之后 UI 工具不再背书采集状态。
func TestHookStopUnwatchesCapture(t *testing.T) {
	bridge := &stateBridge{locked: 1}
	core := &stateCore{gen: 3}
	server := sessionStateServer(bridge, core)
	if failed, text := callToolOn(t, server, 1, "session_start", `{}`); failed {
		t.Fatalf("session_start 不该失败: %s", text)
	}
	if failed, text := callToolOn(t, server, 2, "hook_stop", `{"name":"wxapi"}`); failed {
		t.Fatalf("hook_stop 不该失败: %s", text)
	}
	failed, text := callToolOn(t, server, 3, "miniapp_page_stack", `{}`)
	if strings.Contains(text, "capture_health") {
		t.Fatalf("hook_stop 之后不得再背书采集状态: %s", text)
	}
	_ = failed
}

// 从未由 MCP 启动过的采集（GUI 自己开的）不归 capture_health 管。
func TestCaptureHealthStaysSilentWithoutMCPCapture(t *testing.T) {
	bridge := &stateBridge{locked: 1, running: false}
	server := sessionStateServer(bridge, &stateCore{gen: 3})
	failed, text := callToolOn(t, server, 1, "miniapp_page_stack", `{}`)
	if strings.Contains(text, "capture_health") {
		t.Fatalf("MCP 没开过采集就不得报断线: %s", text)
	}
	_ = failed
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
