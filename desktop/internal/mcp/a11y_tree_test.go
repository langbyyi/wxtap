package mcp

// a11y 树与组件事件路径的契约：树是稳定寻址的页内 DOM 投影；click 的
// node_path 走 exparser 桥并如实标注 via；type 对组件输入走组件的 input 事件、
// 空值即失败。

import (
	"strings"
	"testing"
)

func TestA11yTreeWalksThePageWebview(t *testing.T) {
	core := &recordingCore{
		errorPayload:  `{"route":"pages/payment/pay","nodes":[{"path":"0","tag":"wx-view","rect":[0,0,10,10]},{"path":"0/1","tag":"wx-button","text":"缴款","rect":[8,40,90,28]}],"count":2,"truncated":false}`,
		payloadMarker: "childElementCount",
	}
	messages := exchange(t, Deps{Core: core, AppBridge: fakeAppBridge{}},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_a11y_tree","arguments":{}}}`+"\n")
	if toolFailed(t, messages, 1) {
		t.Fatalf("a11y 树不该失败: %s", toolText(t, messages, 1))
	}
	if got := contextIDFor(core.contextCalls, "childElementCount"); got != defaultPageCtxID {
		t.Fatalf("树必须走当前页 webview %d，实际 %d", defaultPageCtxID, got)
	}
	text := toolText(t, messages, 1)
	// 路径不带前导斜杠：miniapp_click {node_path} 的 split('/') 才能原样往返。
	if !strings.Contains(text, `"path":"0/1"`) || !strings.Contains(text, "缴款") {
		t.Fatalf("节点应带无前导斜杠的路径与文本: %s", text)
	}
}

// node_path 点击：页内重定位 + exparser 桥可达时走 tap，回包标 via。
func TestClickByNodePathDispatchesComponentTap(t *testing.T) {
	core := &recordingCore{
		errorPayload:  `{"ok":true,"x":10,"y":20,"tag":"wx-view","via":"exparser"}`,
		payloadMarker: "__wxElement",
	}
	messages := exchange(t, Deps{Core: core, AppBridge: fakeAppBridge{}},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_click","arguments":{"node_path":"0/2/1"}}}`+"\n")
	if toolFailed(t, messages, 1) {
		t.Fatalf("node_path 点击不该失败: %s", toolText(t, messages, 1))
	}
	if got := contextIDFor(core.contextCalls, "__wxElement"); got != defaultPageCtxID {
		t.Fatalf("node_path 点击必须走当前页 webview %d，实际 %d", defaultPageCtxID, got)
	}
	if text := toolText(t, messages, 1); !strings.Contains(text, `"via":"exparser"`) {
		t.Fatalf("回包应注明派发通道: %s", text)
	}
}

// node_path 落空：页内 {error} 必须读作失败，不能当成功转发。
func TestClickByNodePathReadsAMissAsAnError(t *testing.T) {
	core := &recordingCore{
		errorPayload:  `{"error":"Node not found: 9/9"}`,
		payloadMarker: "__wxElement",
	}
	failed, text := callTool(t, Deps{Core: core, AppBridge: fakeAppBridge{}}, "miniapp_click", `{"node_path":"9/9"}`)
	if !failed {
		t.Fatalf("路径落空必须失败: %s", text)
	}
	if !strings.Contains(text, "Node not found") {
		t.Fatalf("错误应保留页内原因: %s", text)
	}
}

// type 的组件路径：input 事件送达 → ok + 值长度；空值 → ok:false。
func TestTypeViaComponentInputEvent(t *testing.T) {
	core := &recordingCore{
		errorPayload:  `{"component":true,"valueLen":5}`,
		payloadMarker: "triggerEvent",
	}
	messages := exchange(t, Deps{Core: core, AppBridge: fakeAppBridge{}},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_type","arguments":{"text":"hello","selector":"wx-input"}}}`+"\n")
	text := toolText(t, messages, 1)
	if toolFailed(t, messages, 1) || !strings.Contains(text, `"value_len":5`) || !strings.Contains(text, "component-input") {
		t.Fatalf("组件输入应回值长度与通道: %s", text)
	}
}

func TestTypeViaComponentInputEventTreatsAnEmptyValueAsFailure(t *testing.T) {
	core := &recordingCore{
		errorPayload:  `{"component":true,"valueLen":0}`,
		payloadMarker: "triggerEvent",
	}
	messages := exchange(t, Deps{Core: core, AppBridge: fakeAppBridge{}},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_type","arguments":{"text":"hello","selector":"wx-input"}}}`+"\n")
	text := toolText(t, messages, 1)
	if toolFailed(t, messages, 1) {
		t.Fatalf("空值是数据不是工具错误: %s", text)
	}
	if !strings.Contains(text, `"ok":false`) || !strings.Contains(text, "empty value") {
		t.Fatalf("空值必须读作 ok:false: %s", text)
	}
}

// 组件桥不可达：回退键盘路径（Input.dispatchKeyEvent 逐字符派发）。
func TestTypeFallsBackToKeyboardWhenNoComponentBridge(t *testing.T) {
	core := &recordingCore{
		errorPayload:  `{"component":false}`,
		payloadMarker: "triggerEvent",
	}
	messages := exchange(t, Deps{Core: core, AppBridge: fakeAppBridge{}},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_type","arguments":{"text":"hi","selector":"input"}}}`+"\n")
	if toolFailed(t, messages, 1) {
		t.Fatalf("键盘回退不该失败: %s", toolText(t, messages, 1))
	}
	keyEvents := 0
	for _, method := range core.methods {
		if method == "Input.dispatchKeyEvent" {
			keyEvents++
		}
	}
	if keyEvents != 4 { // "hi" = 2 字符 × keyDown/keyUp
		t.Fatalf("键盘回退应逐字符派发 4 个按键事件，实际 %d: %v", keyEvents, core.methods)
	}
}
