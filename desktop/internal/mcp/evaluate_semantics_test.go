package mcp

import (
	"strings"
	"testing"
)

// WMPF 对不存在的 contextId 回 {error:{...}} 信封。Go 层的兜底检查必须把它转成
// 工具错误，否则 agent 看到的是 {"type":"","value":null}，与真空返回无法区分。
func TestEvaluateRejectsAnErrorEnvelope(t *testing.T) {
	core := &recordingCore{errorEnvelope: "context not found"}
	failed, text := callTool(t, Deps{Core: core, AppBridge: fakeAppBridge{}}, "miniapp_evaluate", `{"expression":"1+1","context_id":99}`)
	if !failed {
		t.Fatalf("error 信封必须读作失败: %s", text)
	}
	if !strings.Contains(text, "context not found") {
		t.Fatalf("错误应保留 CDP 原因: %s", text)
	}
}

// 语句级输入在 WMPF 的「表达式位置」编译下必抛 SyntaxError（实测嵌套函数声明
// 必炸）；Go 侧用 IIFE 重试一次，取值走显式 return。
func TestEvaluateWrapsStatementInputsOnSyntaxError(t *testing.T) {
	core := &recordingCore{syntaxFailures: 1}
	failed, text := callTool(t, Deps{Core: core}, "miniapp_evaluate", `{"expression":"var a = 1; return a + 1"}`)
	if failed {
		t.Fatalf("IIFE 重试后不该失败: %s", text)
	}
	if len(core.expressions) != 2 {
		t.Fatalf("应恰好求值两次（原式 + IIFE 重试）: %v", core.expressions)
	}
	if !strings.Contains(core.expressions[1], "(function(){") {
		t.Fatalf("重试应包进 IIFE: %s", core.expressions[1])
	}
}

// 已经写成 IIFE 的不再包：双重包装会把返回值吃成 undefined。求值异常沿用该
// 工具的既有契约（{error,exception} 数据形状而非工具失败），关键是「不重试」。
func TestEvaluateDoesNotDoubleWrapAnIIFE(t *testing.T) {
	core := &recordingCore{syntaxFailures: 100}
	_, text := callTool(t, Deps{Core: core}, "miniapp_evaluate", `{"expression":"(function(){return 1})()"}`)
	if len(core.expressions) != 1 {
		t.Fatalf("IIFE 输入不该重试: %v", core.expressions)
	}
	if !strings.Contains(text, "SyntaxError") {
		t.Fatalf("应如实回原异常: %s", text)
	}
}

// 超时的命令早已下发给小程序（副作用已发生），迟到响应在 Core 桥被丢弃——
// 只回失败会把 agent 引去怀疑「没写进去」，失败信息必须带上这一事实。
func TestEvaluateTimeoutNotesTheSideEffect(t *testing.T) {
	core := &recordingCore{evaluateTimeout: true}
	failed, text := callTool(t, Deps{Core: core}, "miniapp_evaluate", `{"expression":"1+1"}`)
	if !failed {
		t.Fatalf("超时必须失败: %s", text)
	}
	if !strings.Contains(text, "may have taken effect") {
		t.Fatalf("超时错误应注明副作用可能已发生: %s", text)
	}
}
