package ipc

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hookScriptHarness runs one generated expression in Node with a stub console,
// so the wrapper's real behaviour is checked instead of its text. 包装层是手写的
// JS：前缀、完成值、异常传播三件事都只有跑一遍才算验证过，逐字断言挡不住
// 「字符串都在、语义已经错了」。__CONSOLE__ 让每个用例决定 realm 里有哪些级别。
const hookScriptHarness = `
const fs = require("node:fs");
const logged = [];
const make = (level) => (...args) => logged.push([level, args]);
globalThis.console = __CONSOLE__;
globalThis.wx = { answer: 42 };
let value, error;
try { value = eval(fs.readFileSync(process.argv[2], "utf8")); }
catch (thrown) { error = String((thrown && thrown.message) || thrown); }
const isPromise = !!value && typeof value === "object" && typeof value.then === "function";
process.stdout.write(JSON.stringify({ logged, value: isPromise ? null : value, error, isPromise }));
`

// 五个级别齐全的 console（与 core/hooks/console.js 的 LEVELS 一致）。
const consoleStubAll = `{ log: make("log"), info: make("info"), warn: make("warn"), error: make("error"), debug: make("debug") }`

// 少了 debug 的 console：缺级别不能变成用户脚本里的 TypeError。
const consoleStubWithoutDebug = `{ log: make("log"), info: make("info"), warn: make("warn"), error: make("error") }`

type harnessResult struct {
	Logged    [][2]any `json:"logged"`
	Value     any      `json:"value"`
	Error     string   `json:"error"`
	IsPromise bool     `json:"isPromise"`
}

func runHookScript(t *testing.T, filename, source string) harnessResult {
	t.Helper()
	return runHookScriptWithConsole(t, filename, source, consoleStubAll)
}

func runHookScriptWithConsole(t *testing.T, filename, source, consoleStub string) harnessResult {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available: the wrapper's semantics cannot be checked here")
	}
	dir := t.TempDir()
	expressionPath := filepath.Join(dir, "expression.js")
	if err := os.WriteFile(expressionPath, []byte(HookScriptExpression(filename, source)), 0o600); err != nil {
		t.Fatal(err)
	}
	harnessPath := filepath.Join(dir, "harness.js")
	harness := strings.Replace(hookScriptHarness, "__CONSOLE__", consoleStub, 1)
	if err := os.WriteFile(harnessPath, []byte(harness), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(node, harnessPath, expressionPath).Output()
	if err != nil {
		t.Fatalf("node run: %v", err)
	}
	var result harnessResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("harness output %q: %v", output, err)
	}
	return result
}

func TestHookScriptExpressionPrefixesConsoleOutput(t *testing.T) {
	result := runHookScript(t, "trace.js", "console.log('hi', 1); console.warn('careful')")
	if len(result.Logged) != 2 {
		t.Fatalf("logged = %#v", result.Logged)
	}
	first, _ := json.Marshal(result.Logged[0])
	if !strings.Contains(string(first), "[trace.js] ") {
		t.Fatalf("first line lost the prefix: %s", first)
	}
	// 级别必须转发到真 console 的同名方法，否则 Console 页把它们全记成 log。
	if !strings.Contains(string(mustJSON(t, result.Logged[1])), `"warn"`) {
		t.Fatalf("level not preserved: %#v", result.Logged[1])
	}
}

// 完成值只能靠直接 eval 带回来：包成普通函数调用会让手敲 wx.getStorageSync('k')
// 这类脚本恒返回 undefined，界面上就是「无返回值」。
func TestHookScriptExpressionKeepsTheCompletionValue(t *testing.T) {
	result := runHookScript(t, "trace.js", "wx.answer")
	if result.Error != "" {
		t.Fatalf("unexpected error: %s", result.Error)
	}
	if result.Value != float64(42) {
		t.Fatalf("completion value = %#v, want 42", result.Value)
	}
}

// 脚本抛错时包装层要把带前缀的一行写进真 console（否则页内报错在小程序日志里
// 没有主人），并且必须把异常继续抛出去 —— 吞掉它，注入就会「成功」。
func TestHookScriptExpressionRethrowsWithAPrefixedLine(t *testing.T) {
	result := runHookScript(t, "trace.js", "throw new Error('boom')")
	if !strings.Contains(result.Error, "boom") {
		t.Fatalf("error must propagate: %#v", result)
	}
	last := string(mustJSON(t, result.Logged[len(result.Logged)-1]))
	if !strings.Contains(last, "[trace.js] ") || !strings.Contains(last, "boom") {
		t.Fatalf("error line = %s, want a prefixed line", last)
	}
}

// 最后一条表达式是 Promise（async 写法）时，包装层必须原样把它交给 CDP 的
// awaitPromise，而不是把它序列化成 null。
func TestHookScriptExpressionHandsBackAPromise(t *testing.T) {
	result := runHookScript(t, "trace.js", "Promise.resolve(7)")
	if !result.IsPromise {
		t.Fatalf("promise must survive the wrapper: %#v", result)
	}
}

// 包装层遮蔽 console 会顺带改变作用域语义：脚本里的 var/function 留在包装层的
// 作用域里，不再变成全局。这是有意的（页面上写明了），但也必须钉住，免得哪天
// 有人以为「跟手敲一样」而漏掉这条差异。
func TestHookScriptExpressionKeepsDeclarationsOutOfTheGlobalScope(t *testing.T) {
	result := runHookScript(t, "trace.js", "var leaked = 1; function leakFn(){}; typeof globalThis.leaked")
	if result.Value != "undefined" {
		t.Fatalf("declarations must stay scoped to the wrapper: %#v", result.Value)
	}
}

// realm 里少一个级别（比如没有 console.debug）不能变成用户脚本里的 TypeError：
// 那会把「实现里少一个方法」记成「用户的脚本写错了」。
func TestHookScriptExpressionSurvivesAMissingConsoleLevel(t *testing.T) {
	result := runHookScriptWithConsole(t, "trace.js", "console.debug('x')", consoleStubWithoutDebug)
	if result.Error != "" {
		t.Fatalf("a missing level must not throw inside the script: %s", result.Error)
	}
	if len(result.Logged) != 1 {
		t.Fatalf("logged = %#v", result.Logged)
	}
	line := string(mustJSON(t, result.Logged[0]))
	if !strings.Contains(line, "[trace.js] ") || !strings.Contains(line, `"log"`) {
		t.Fatalf("a missing level must fall back to log: %s", line)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
