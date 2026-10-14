package mcp

// miniapp_call：appservice 执行 + 结果摘要化。回包只有形状（类型/长度/键名），
// 实值——令牌在内——不出小程序。

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// jsonString is a tiny helper for containment assertions over summaries.
func jsonString(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// 摘要形状：字符串只留长度、长数字只留位数、对象留键名+子摘要、数组留长度
// +前 3 项、敏感键整体折叠。
func TestSummarizeCallValueShapes(t *testing.T) {
	value := map[string]any{
		"code":    float64(0),
		"msg":     "ok",
		"token":   "Bearer abcdefghijklmnop",
		"list":    []any{"a", "b", "c", "d"},
		"orderNo": float64(13812345678),
	}
	summary := summarizeCallValue(value, 0).(map[string]any)
	if summary["code"] != float64(0) {
		t.Fatalf("短数字应原样: %v", summary["code"])
	}
	if msg, ok := summary["msg"].(map[string]any); !ok || msg["type"] != "string" || msg["len"] != 2 {
		t.Fatalf("字符串摘要应只剩类型与长度: %v", summary["msg"])
	}
	if summary["token"] != "Be…op" {
		t.Fatalf("敏感键必须掩掉实值（保留头尾形状）: %v", summary["token"])
	}
	list := summary["list"].(map[string]any)
	if list["len"] != 4 || len(list["items"].([]any)) != 3 {
		t.Fatalf("数组应报长度并取前 3 项: %v", list)
	}
	phone, ok := summary["orderNo"].(map[string]any)
	if !ok || phone["type"] != "number" || phone["digits"] != 11 {
		t.Fatalf("长数字只报位数: %v", summary["orderNo"])
	}
}

// 宽度上限：超过 64 键的对象按名排序取前 64，其余计数标注；排序保证摘要确定。
func TestSummarizeCallValueBreadthCap(t *testing.T) {
	value := map[string]any{}
	for i := 0; i < 80; i++ {
		value[fmt.Sprintf("k%03d", i)] = i
	}
	summary := summarizeCallValue(value, 0).(map[string]any)
	if len(summary) != callSummaryKeys+1 { // 64 键 + …keys_omitted
		t.Fatalf("应只展开 64 键并计数省略: %d 项", len(summary))
	}
	if summary["…keys_omitted"] != 16 {
		t.Fatalf("省略计数应为 16: %v", summary["…keys_omitted"])
	}
	if _, ok := summary["k000"]; !ok {
		t.Fatalf("排序后最小键名应在内: %v", summary)
	}
	if _, ok := summary["k079"]; ok {
		t.Fatalf("排序后最大键名应被省略: %v", summary)
	}
}

// 深度 4 截断：更深的嵌套只剩容器大小。
func TestSummarizeCallValueDepthCap(t *testing.T) {
	deep := map[string]any{}
	layer := deep
	for i := 0; i < 6; i++ {
		next := map[string]any{}
		layer["child"] = next
		layer = next
	}
	layer["leaf"] = "bottom"
	summary := summarizeCallValue(deep, 0)
	// 走满 callSummaryDepth 层后应只剩 {"type":"object","keys":1} 形状的截断。
	current := summary
	for i := 0; i < callSummaryDepth-1; i++ {
		current = current.(map[string]any)["child"]
	}
	tail := current.(map[string]any)["child"].(map[string]any)
	if tail["type"] != "object" || tail["keys"] != 1 {
		t.Fatalf("深度截断后应只剩容器大小: %v", tail)
	}
	if strings.Contains(jsonString(summary), "bottom") {
		t.Fatalf("深层叶子不得出现在摘要里: %v", summary)
	}
}

// 工具路径：表达式打到 appservice 上下文，回包是摘要。
func TestMiniappCallRunsInTheAppServiceAndSummarizes(t *testing.T) {
	core := &recordingCore{
		errorPayload:  `{"code":0,"data":{"token":"LTAI5tABCDEFghijklmnop"}}`,
		payloadMarker: "store.dispatch",
	}
	messages := exchange(t, Deps{Core: core, AppBridge: fakeAppBridge{}},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_call","arguments":{"expression":"store.dispatch('cart/load')"}}}`+"\n")
	if toolFailed(t, messages, 1) {
		t.Fatalf("miniapp_call 不该失败: %s", toolText(t, messages, 1))
	}
	if got := contextIDFor(core.contextCalls, "store.dispatch"); got != defaultAppServiceCtxID {
		t.Fatalf("业务调用必须打在 appservice %d，实际 %d", defaultAppServiceCtxID, got)
	}
	text := toolText(t, messages, 1)
	if strings.Contains(text, "LTAI5tABCDEFghijklmnop") {
		t.Fatalf("摘要不得携带实值: %s", text)
	}
	if !strings.Contains(text, `"summary"`) || !strings.Contains(text, `"token":"LT…op"`) {
		t.Fatalf("回包应为形状摘要且敏感键掩掉实值: %s", text)
	}
}
