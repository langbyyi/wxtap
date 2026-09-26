package ipc

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestHookScriptExpressionCarriesScriptAndPrefix(t *testing.T) {
	expression := HookScriptExpression("trace.js", "console.log('hi')")

	// 前缀必须出现在表达式里：脚本的输出要能在 Console 页认出主人。
	if !strings.Contains(expression, `"[trace.js] "`) {
		t.Fatalf("filename prefix missing: %s", expression)
	}
	if !strings.Contains(expression, "console.log('hi')") {
		t.Fatalf("script source missing: %s", expression)
	}
	// 完成值只能靠直接 eval 带回来；换成普通函数调用会让「无返回值」成为常态。
	if !strings.Contains(expression, "eval(__wxTapSource)") {
		t.Fatalf("script must run through a direct eval: %s", expression)
	}
	if !strings.Contains(expression, "sourceURL=wxtap-user-script/trace.js") {
		t.Fatalf("sourceURL missing: %s", expression)
	}
	// 严格模式会改变宽松脚本的语义（foo = 1 会变成 ReferenceError）。
	if strings.Contains(expression, "use strict") {
		t.Fatalf("wrapper must stay sloppy to keep Runtime.evaluate semantics: %s", expression)
	}
}

// 用户脚本是文本，不是代码拼接的材料：引号、反斜杠、换行都必须留在字符串里，
// 否则一个带引号的脚本就能改掉包装层、把 console 前缀绕过去。
func TestHookScriptExpressionEscapesHostileSource(t *testing.T) {
	source := "\"}); console.log('hijacked'); (function(){"
	expression := HookScriptExpression("evil.js", source)

	body, err := json.Marshal(source + "\n//# sourceURL=wxtap-user-script/evil.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(expression, string(body)) {
		t.Fatalf("source must be embedded as a JSON string literal: %s", expression)
	}
	if strings.Count(expression, "console.log('hijacked')") != 1 {
		t.Fatalf("hostile source escaped its string literal: %s", expression)
	}
}

func TestHookRunSummaryRendersWhatTheUserWillSee(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  string
	}{
		// undefined 在 CDP 里没有 value 字段，解出来就是 nil。
		{"undefined", nil, "无返回值"},
		{"string", "  wx.setStorageSync 已替换  ", "wx.setStorageSync 已替换"},
		{"number", float64(3), "3"},
		{"bool", true, "true"},
		{"object", map[string]any{"ok": true}, `{"ok":true}`},
		// 本地排查工具：< > & 要看原文，不要 HTML 转义。
		{"html chars", map[string]any{"q": "a<b>&c"}, `{"q":"a<b>&c"}`},
	}
	for _, testCase := range cases {
		if got := HookRunSummary(testCase.value); got != testCase.want {
			t.Fatalf("%s: HookRunSummary = %q, want %q", testCase.name, got, testCase.want)
		}
	}
}

func TestHookRunSummaryTruncatesLongValues(t *testing.T) {
	got := HookRunSummary(strings.Repeat("字", HookRunSummaryLimit+50))
	if !strings.HasSuffix(got, "…（已截断）") {
		t.Fatalf("long value must be truncated: %q", got)
	}
	if runes := []rune(strings.TrimSuffix(got, "…（已截断）")); len(runes) != HookRunSummaryLimit {
		t.Fatalf("truncated to %d runes, want %d", len(runes), HookRunSummaryLimit)
	}
}
