package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// debugCore simulates the Core side of a debug session: Debugger.pause flips
// the snapshot to paused, resume/step clear it, and script listing reads the
// captured script stream.
type debugCore struct {
	fakeCore
	paused    bool
	pausedSeq int64
}

func (c *debugCore) DebugState(_ context.Context, _ bool) (map[string]any, error) {
	frames := []any{}
	if c.paused {
		frames = append(frames, map[string]any{
			"callFrameId":  "frame:1:0",
			"functionName": "onLoad",
			"url":          "appservice/app.js",
			"lineNumber":   float64(9),
			"columnNumber": float64(2),
			"scopes": []any{
				map[string]any{"type": "local", "objectId": "obj:1"},
				map[string]any{"type": "global", "objectId": "obj:2"},
			},
		})
	}
	return map[string]any{
		"enabled": true, "paused": c.paused, "pausedSeq": float64(c.pausedSeq),
		"resumedSeq": float64(0), "reason": "other", "pausedAt": float64(1234),
		"callFrames": frames,
		"scripts": []any{
			map[string]any{"scriptId": "12", "url": "appservice/pages/index.js"},
			map[string]any{"scriptId": "13", "url": "appservice/utils/request.js"},
		},
		"scriptsTruncated": float64(0),
	}, nil
}

func (c *debugCore) CDPCommand(_ context.Context, method string, params map[string]any, _ int) (map[string]any, error) {
	switch method {
	case "Debugger.pause":
		c.paused = true
		c.pausedSeq++
	case "Debugger.resume", "Debugger.stepOver", "Debugger.stepInto", "Debugger.stepOut":
		c.paused = false
	case "Debugger.setPauseOnExceptions":
		state, _ := params["state"].(string)
		if state == "" {
			return nil, errors.New("state required")
		}
	case "Debugger.setBreakpointByUrl":
		return map[string]any{"result": map[string]any{"breakpointId": "bp:1", "locations": []any{}}}, nil
	case "Debugger.getScriptSource":
		return map[string]any{"result": map[string]any{"scriptSource": "// script source"}}, nil
	case "Debugger.evaluateOnCallFrame":
		return map[string]any{"result": map[string]any{
			"result": map[string]any{"type": "string", "value": "secret-value"},
		}}, nil
	case "Runtime.getProperties":
		return map[string]any{"result": map[string]any{"result": []any{
			map[string]any{"name": "token", "value": map[string]any{"type": "string", "value": "abc"}},
			map[string]any{"name": "cfg", "value": map[string]any{"type": "object", "description": "{...}"}},
		}}}, nil
	}
	return map[string]any{"result": map[string]any{}}, nil
}

// emptyScriptCore 的脚本清单是空的：这正是刚附加、Debugger 域的 scriptParsed
// 事件还没到达时的状态。
type emptyScriptCore struct{ debugCore }

func (c *emptyScriptCore) DebugState(context.Context, bool) (map[string]any, error) {
	return map[string]any{"enabled": true, "scripts": []any{}, "scriptsTruncated": float64(0)}, nil
}

// 查找型工具找不到目标必须报错。以前 miniapp_get_source 匹配不到时把 pattern
// 当 scriptId 去取正文，于是「没匹配到」变成「成功但正文为空」——读起来像
// 「这个脚本是空的」，而 agent 会据此得出错误结论。
func TestGetSourceReportsAMissingScript(t *testing.T) {
	messages := exchange(t, Deps{Core: &debugCore{}}, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_get_source","arguments":{"url_pattern":"utils/request.js"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"miniapp_get_source","arguments":{"url_pattern":"does/not/exist.js"}}}`,
	}, "\n")+"\n")

	if toolFailed(t, messages, 1) {
		t.Fatalf("匹配到脚本时不该失败: %s", toolText(t, messages, 1))
	}
	if !strings.Contains(toolText(t, messages, 1), "script source") {
		t.Fatalf("应返回正文: %s", toolText(t, messages, 1))
	}
	if !toolFailed(t, messages, 2) {
		t.Fatalf("匹配不到必须报错，不能回空正文的成功: %s", toolText(t, messages, 2))
	}
	if !strings.Contains(toolText(t, messages, 2), "does/not/exist.js") {
		t.Fatalf("错误里应点出没匹配上的 pattern: %s", toolText(t, messages, 2))
	}
}

// 清单还是空的时候要说清原因，而不是把它混同于「没有脚本匹配这个 pattern」。
func TestGetSourceExplainsAnEmptyScriptRing(t *testing.T) {
	messages := exchange(t, Deps{Core: &emptyScriptCore{}}, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_get_source","arguments":{"url_pattern":"appservice"}}}`+"\n")
	if !toolFailed(t, messages, 1) {
		t.Fatal("脚本清单为空时必须报错")
	}
	if !strings.Contains(toolText(t, messages, 1), "脚本清单还是空的") {
		t.Fatalf("错误应说明清单为空: %s", toolText(t, messages, 1))
	}
}

// 直接按 scriptId 取正文仍然可用（工具契约：script id 或 url pattern）。
func TestGetSourceStillAcceptsABareScriptID(t *testing.T) {
	messages := exchange(t, Deps{Core: &debugCore{}}, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_get_source","arguments":{"url_pattern":"12"}}}`+"\n")
	if toolFailed(t, messages, 1) {
		t.Fatalf("按 scriptId 取正文不该失败: %s", toolText(t, messages, 1))
	}
	if !strings.Contains(toolText(t, messages, 1), `"scriptId":"12"`) {
		t.Fatalf("应回传 scriptId: %s", toolText(t, messages, 1))
	}
}

// pause / resume 是异步事件：工具返回前必须等到快照确认，不能发了命令就说成
// 功。
func TestDebuggerPauseResumeFlow(t *testing.T) {
	core := &debugCore{}
	messages := exchange(t, Deps{Core: core}, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"debugger_pause","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"debugger_state","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"debugger_resume","arguments":{}}}`,
	}, "\n")+"\n")

	paused := toolJSON(t, messages, 1)
	if paused["paused"] != true || len(paused["callFrames"].([]any)) != 1 {
		t.Fatalf("debugger_pause 应返回已暂停快照: %v", paused)
	}
	if paused["pausedSeq"].(float64) != 1 {
		t.Fatalf("pausedSeq 应推进: %v", paused)
	}

	state := toolJSON(t, messages, 2)
	if state["paused"] != true || state["scriptCount"].(float64) != 2 {
		t.Fatalf("debugger_state 应含帧与脚本计数: %v", state)
	}

	resumed := toolJSON(t, messages, 3)
	if resumed["paused"] != false {
		t.Fatalf("debugger_resume 后应未暂停: %v", resumed)
	}
}

// 未暂停时单步当场报错：把「猜测没有发生」变成一句可读的指引。
func TestDebuggerStepRequiresPause(t *testing.T) {
	core := &debugCore{}
	messages := exchange(t, Deps{Core: core}, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"debugger_step_over","arguments":{}}}`+"\n")
	if !toolFailed(t, messages, 1) {
		t.Fatalf("未暂停时单步必须报错: %s", toolText(t, messages, 1))
	}
}

// 调用栈行号是 1-based（CDP 是 0-based），这是工具和 agent 之间的约定，测试
// 钉住它。
func TestDebuggerCallStackIsOneBased(t *testing.T) {
	core := &debugCore{paused: true, pausedSeq: 3}
	messages := exchange(t, Deps{Core: core}, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"debugger_call_stack","arguments":{}}}`+"\n")
	payload := toolJSON(t, messages, 1)
	frames := payload["frames"].([]any)
	if len(frames) != 1 {
		t.Fatalf("应有 1 帧: %v", payload)
	}
	frame := frames[0].(map[string]any)
	if frame["line"] != float64(10) || frame["column"] != float64(3) {
		t.Fatalf("行号应转成 1-based: %v", frame)
	}
	if frame["callFrameId"] != "frame:1:0" || frame["functionName"] != "onLoad" {
		t.Fatalf("帧字段不完整: %v", frame)
	}
}

func TestDebuggerListScriptsFilters(t *testing.T) {
	core := &debugCore{}
	messages := exchange(t, Deps{Core: core}, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"debugger_list_scripts","arguments":{"url_filter":"utils"}}}`+"\n")
	payload := toolJSON(t, messages, 1)
	scripts := payload["scripts"].([]any)
	if payload["total"].(float64) != 1 || len(scripts) != 1 {
		t.Fatalf("过滤后应剩 1 个脚本: %v", payload)
	}
	if scripts[0].(map[string]any)["scriptId"] != "13" {
		t.Fatalf("应命中 request.js: %v", scripts)
	}
}

func TestDebuggerGetScopesReadsVariables(t *testing.T) {
	core := &debugCore{paused: true, pausedSeq: 2}
	messages := exchange(t, Deps{Core: core}, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"debugger_get_scopes","arguments":{}}}`+"\n")
	payload := toolJSON(t, messages, 1)
	scopes := payload["scopes"].([]any)
	if len(scopes) != 2 {
		t.Fatalf("应读到两个作用域: %v", payload)
	}
	local := scopes[0].(map[string]any)
	variables := local["variables"].([]any)
	if len(variables) != 2 {
		t.Fatalf("local 作用域应有 2 个变量: %v", local)
	}
	first := variables[0].(map[string]any)
	if first["name"] != "token" || first["value"] != "abc" {
		t.Fatalf("变量读取不对: %v", first)
	}
	// 对象值走 description 预览，不把整个对象灌进结果。
	second := variables[1].(map[string]any)
	if second["value"] != "{...}" {
		t.Fatalf("对象应保留 description 预览: %v", second)
	}
}

func TestDebuggerEvaluateOnCallFrame(t *testing.T) {
	core := &debugCore{paused: true, pausedSeq: 2}
	messages := exchange(t, Deps{Core: core}, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"debugger_evaluate_on_call_frame","arguments":{"expression":"token"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"debugger_evaluate_on_call_frame","arguments":{}}}`,
	}, "\n")+"\n")

	payload := toolJSON(t, messages, 1)
	if payload["value"] != "secret-value" || payload["callFrameId"] != "frame:1:0" {
		t.Fatalf("帧上求值应落到顶帧并返回值: %v", payload)
	}
	if !toolFailed(t, messages, 2) {
		t.Fatal("缺 expression 必须报错")
	}
}

func TestDebuggerPauseOnExceptionsValidates(t *testing.T) {
	core := &debugCore{}
	messages := exchange(t, Deps{Core: core}, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"debugger_pause_on_exceptions","arguments":{"state":"sometimes"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"debugger_pause_on_exceptions","arguments":{"state":"uncaught"}}}`,
	}, "\n")+"\n")
	if !toolFailed(t, messages, 1) {
		t.Fatal("非法 state 必须报错")
	}
	payload := toolJSON(t, messages, 2)
	if payload["ok"] != true || payload["state"] != "uncaught" {
		t.Fatalf("合法 state 应成功: %v", payload)
	}
}

// 没有 Core 时调试工具报「引擎不可用」，而不是 unknown tool。
func TestDebuggerToolsWithoutCoreAnswerUnavailable(t *testing.T) {
	messages := exchange(t, Deps{}, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"debugger_state","arguments":{}}}`+"\n")
	text := toolText(t, messages, 1)
	if !toolFailed(t, messages, 1) || !strings.Contains(text, "core engine unavailable") {
		t.Fatalf("无 Core 应报引擎不可用: %s", text)
	}
}

// toolJSON decodes one tools/call result's JSON payload into a map.
func toolJSON(t *testing.T, messages []map[string]any, id float64) map[string]any {
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
	text, _ := content[0].(map[string]any)["text"].(string)
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("decode tool payload %q: %v", text, err)
	}
	return payload
}
