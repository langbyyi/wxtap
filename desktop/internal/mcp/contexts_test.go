package mcp

// miniapp_list_contexts 的双路径：事件注册表（权威）与有界探测（回退）。

import (
	"context"
	"strings"
	"testing"
)

// contextsCore serves an event-fed context registry in the shape cdp.contexts
// answers.
type contextsCore struct {
	fakeCore
	registry []map[string]any
}

func (c *contextsCore) Contexts(context.Context) ([]map[string]any, error) {
	return c.registry, nil
}

// 事件路径：注册表非空 → 每项分类（appservice 探针 / webview 读 body[is]），
// source 标注 events。
func TestListContextsUsesTheEventRegistry(t *testing.T) {
	core := &contextsCore{registry: []map[string]any{
		{"id": float64(1), "name": "appservice"},
		{"id": float64(2), "name": "page-frame"},
	}}
	messages := exchange(t, Deps{Core: core, AppBridge: fakeAppBridge{}},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_list_contexts","arguments":{}}}`+"\n")
	text := toolText(t, messages, 1)
	if toolFailed(t, messages, 1) {
		t.Fatalf("list_contexts 不该失败: %s", text)
	}
	if !strings.Contains(text, `"kind":"appservice"`) || !strings.Contains(text, `"kind":"webview"`) {
		t.Fatalf("应按探针分出两类: %s", text)
	}
	if !strings.Contains(text, `"source":"events"`) {
		t.Fatalf("应标注事件来源: %s", text)
	}
}

// 回退路径：注册表为空（shim 不发事件）→ 有界探测照旧，source 标注 probe。
func TestListContextsFallsBackToProbing(t *testing.T) {
	core := &recordingCore{}
	messages := exchange(t, Deps{Core: core, AppBridge: fakeAppBridge{}},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_list_contexts","arguments":{}}}`+"\n")
	text := toolText(t, messages, 1)
	if toolFailed(t, messages, 1) {
		t.Fatalf("回退路径不该失败: %s", text)
	}
	if !strings.Contains(text, `"source":"probe"`) {
		t.Fatalf("应标注探测来源: %s", text)
	}
}
