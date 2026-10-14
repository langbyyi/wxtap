package mcp

// 打码契约：动态读面的默认打码与 redact:false 出原值。共享同一份 payload 的
// 近邻工具必须同口径，否则旧名/兄弟工具成为旁路。

import (
	"context"
	"strings"
	"testing"
)

// sensitiveCore 回一条带敏感头的采集记录。
type sensitiveCore struct{ fakeCore }

func (sensitiveCore) HookDrain(_ context.Context, _ string, afterSeq int64, _ int, _ int64, _ int) (DrainPage, error) {
	return DrainPage{
		Records: []DrainedRecord{{Seq: 1, Record: map[string]any{
			"name": "wx.request",
			"data": map[string]any{"header": map[string]any{"Authorization": "Bearer abcdefghijklmnop"}},
		}}},
		NextSeq: 1,
	}, nil
}

// hook_drain / hook_wait / cloud_captures 同一口径：默认掩，redact:false 出原值。
func TestHookDrainMasksSensitiveHeadersByDefault(t *testing.T) {
	core := sensitiveCore{}
	messages := exchange(t, Deps{Core: core}, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hook_drain","arguments":{"name":"wxapi"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"hook_drain","arguments":{"name":"wxapi","redact":false}}}`,
	}, "\n")+"\n")
	masked := toolText(t, messages, 1)
	if strings.Contains(masked, "Bearer abcdefghijklmnop") {
		t.Fatalf("默认必须打码: %s", masked)
	}
	if !strings.Contains(masked, "Be…op") || !strings.Contains(masked, `"redacted":true`) {
		t.Fatalf("掩码应保留形状并标注 redacted: %s", masked)
	}
	raw := toolText(t, messages, 2)
	if !strings.Contains(raw, "Bearer abcdefghijklmnop") {
		t.Fatalf("redact:false 必须出原值: %s", raw)
	}
}

// traffic_get_body：JSON 体默认打码，redact:false 出库里的原文。
func TestTrafficGetBodyMasksByDefault(t *testing.T) {
	messages := exchange(t, Deps{Traffic: fakeTraffic{}}, strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"traffic_get_body","arguments":{"id":"a1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"traffic_get_body","arguments":{"id":"a1","redact":false}}}`,
	}, "\n")+"\n")
	masked := toolText(t, messages, 1)
	if strings.Contains(masked, "LTAI5tABCDEFghijklmnop") || !strings.Contains(masked, "LT…op") {
		t.Fatalf("默认必须打码且保留形状: %s", masked)
	}
	raw := toolText(t, messages, 2)
	if !strings.Contains(raw, "LTAI5tABCDEFghijklmnop") {
		t.Fatalf("redact:false 必须出原值: %s", raw)
	}
}

// traffic_records 的体预览同一口径。
func TestTrafficRecordsMaskBodiesByDefault(t *testing.T) {
	messages := exchange(t, Deps{Traffic: fakeTraffic{}},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"traffic_records","arguments":{"apiType":"wx.request"}}}`+"\n")
	text := toolText(t, messages, 1)
	if strings.Contains(text, "LTAI5tABCDEFghijklmnop") {
		t.Fatalf("聚合页的体预览必须默认打码: %s", text)
	}
	if !strings.Contains(text, `"redacted":true`) {
		t.Fatalf("应标注 redacted: %s", text)
	}
}

// traffic_curl：回给 agent 的命令行打码（重放报文不经此路径）。
func TestTrafficCurlMasksSensitiveHeaders(t *testing.T) {
	bridge := &curlBridge{}
	messages := exchange(t, Deps{AppBridge: bridge},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"traffic_curl","arguments":{"id":"a1"}}}`+"\n")
	text := toolText(t, messages, 1)
	if strings.Contains(text, "Bearer abcdefghijklmnop") {
		t.Fatalf("curl 行的敏感头必须打码: %s", text)
	}
	if !strings.Contains(text, "Content-Type: application/json") {
		t.Fatalf("非敏感头必须原样: %s", text)
	}
}

type curlBridge struct{}

func (curlBridge) Call(_ context.Context, method string, _ map[string]any) (any, error) {
	if method == "traffic.curl" {
		return map[string]any{
			"ok":   true,
			"bash": `curl -k 'https://x.y/' -H 'Content-Type: application/json' -H 'Authorization: Bearer abcdefghijklmnop'`,
			"cmd":  `curl -k "https://x.y/" -H "Content-Type: application/json" -H "Authorization: Bearer abcdefghijklmnop"`,
		}, nil
	}
	return map[string]any{"ok": true}, nil
}

// storage：默认打码；mode:"keys" 只回键名与体长，值根本不离开小程序。
func TestStorageMasksByDefaultAndKeysModeOmitsValues(t *testing.T) {
	deps := Deps{Core: &recordingCore{
		errorPayload:  `{"keys":["token"],"data":{"token":"LTAI5tABCDEFghijklmnop"},"currentSize":24}`,
		payloadMarker: "r[k[n]]",
	}, AppBridge: fakeAppBridge{}}
	messages := exchange(t, deps,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_get_storage","arguments":{}}}`+"\n")
	full := toolText(t, messages, 1)
	if strings.Contains(full, "LTAI5tABCDEFghijklmnop") || !strings.Contains(full, "LT…op") {
		t.Fatalf("storage 值必须默认打码: %s", full)
	}
	keysCore := &recordingCore{
		errorPayload:  `{"keys":["token"],"sizes":{"token":214},"currentSize":24}`,
		payloadMarker: "s[k[n]]",
	}
	messages = exchange(t, Deps{Core: keysCore, AppBridge: fakeAppBridge{}},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_get_storage","arguments":{"mode":"keys"}}}`+"\n")
	keys := toolText(t, messages, 1)
	if strings.Contains(keys, "LTAI") || !strings.Contains(keys, `"mode":"keys"`) {
		t.Fatalf("keys 模式不得带值: %s", keys)
	}
	if !strings.Contains(keys, `"sizes"`) || !strings.Contains(keys, `"token"`) {
		t.Fatalf("keys 模式应回键名与体长: %s", keys)
	}
}

// 旧名单键读同口径：不是绕开打码的口子。
func TestStorageLegacyKeyReadMasksToo(t *testing.T) {
	core := &recordingCore{
		errorPayload:  `{"key":"token","value":"LTAI5tABCDEFghijklmnop","type":"string"}`,
		payloadMarker: "wx.getStorageSync",
	}
	messages := exchange(t, Deps{Core: core, AppBridge: fakeAppBridge{}},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_get_storage_key","arguments":{"key":"token"}}}`+"\n")
	if text := toolText(t, messages, 1); strings.Contains(text, "LTAI5tABCDEFghijklmnop") {
		t.Fatalf("旧名单键读必须同口径打码: %s", text)
	}
}

// page_stack：query 值默认打码（键名保留）。
func TestPageStackMasksQueryValues(t *testing.T) {
	bridge := &queryStackBridge{}
	messages := exchange(t, Deps{AppBridge: bridge},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_page_stack","arguments":{}}}`+"\n")
	text := toolText(t, messages, 1)
	if strings.Contains(text, "st-ticket-value") {
		t.Fatalf("query 实值必须打码: %s", text)
	}
	if !strings.Contains(text, "ticket") || !strings.Contains(text, `"redacted":true`) {
		t.Fatalf("键名必须保留且标注 redacted: %s", text)
	}
}

type queryStackBridge struct{}

func (queryStackBridge) Call(_ context.Context, method string, _ map[string]any) (any, error) {
	switch method {
	case "navigator.pageStack":
		return map[string]any{"stack": []any{map[string]any{
			"route": "pages/cert/verify", "params": map[string]any{"ticket": "st-ticket-value", "from": "index"},
		}}, "current": "pages/cert/verify"}, nil
	case "navigator.pages":
		return map[string]any{"pages": []any{"pages/cert/verify"}, "tab_bar_pages": []any{}, "current_route": "pages/cert/verify"}, nil
	}
	return map[string]any{"ok": true}, nil
}
