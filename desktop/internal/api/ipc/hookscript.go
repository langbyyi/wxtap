package ipc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// HookRunSummaryLimit caps the one-line summary the GUI shows for a script's
// settled value: 一个返回巨大对象的脚本不该在每次 hook.list 里推几百 KB 的 JSON。
const HookRunSummaryLimit = 400

// hookScriptTemplate is the expression the shell evaluates to inject one user
// hook script. __WXTAP_JSON_PREFIX__ / __WXTAP_JSON_SOURCE__ are replaced with
// JSON string literals (so the user's code can never break out of the string
// and rewrite the wrapper).
//
// 两件事在这里发生，且只在这里发生：
//
//  1. 脚本跑在一个 `console` 被遮蔽的词法作用域里。WMPF 把全局 console 做成了
//     configurable:false 的访问器（见 core/hooks/console.js），全局包装不上，
//     只有遮蔽一条路。每个级别转发给真 console 并加上 [文件名] 前缀 —— CDP 的
//     Runtime.consoleAPICalled 照旧收得到，Console 页就能把脚本输出和小程序自己
//     的日志分开，否则用户脚本的 console.log 混在几百行里没有主人。
//  2. 脚本经由**直接 eval** 执行。直接调用一个函数体会丢掉最后一条表达式的值，
//     用户手敲 wx.getStorageSync('k') 就只会看到「无返回值」；eval 把完成值带回来。
//     收尾的 sourceURL 让页内报错在堆栈里仍然指向脚本文件名。
//
// 刻意不写 "use strict"：Runtime.evaluate 默认是宽松模式，加了严格模式会让
// `foo = 1` 这类今天能跑的脚本直接抛 ReferenceError。
const hookScriptTemplate = `(function(){
if (typeof eval !== "function") { throw new Error("该 realm 禁用了 eval，无法注入用户脚本"); }
var __wxTapLevels = ["log","info","warn","error","debug"];
// 必须从 globalThis 取真 console：下面那个 var console 会被提升到整个函数作用域，
// 写 console 读到的就是提升后的 undefined（这一步踩过，node 侧的行为测试才抓出来）。
var __wxTapReal = (typeof globalThis !== "undefined" && globalThis.console) || (typeof window !== "undefined" && window.console);
if (!__wxTapReal) { throw new Error("该 realm 里找不到 console，无法注入用户脚本"); }
var __wxTapPrefix = __WXTAP_JSON_PREFIX__;
// 缺级别就往 log 落，不要在用户脚本里抛 TypeError：console.debug 这类方法在某些
// 实现里可以缺席，而报错会算到用户脚本头上。
var __wxTapWrite = function(level, args){ var fn = __wxTapReal[level] || __wxTapReal.log; if (fn) { fn.apply(__wxTapReal, args); } };
var __wxTapShim = {};
for (var __wxTapIndex = 0; __wxTapIndex < __wxTapLevels.length; __wxTapIndex++) {
(function(level){ __wxTapShim[level] = function(){ var args = [__wxTapPrefix]; args.push.apply(args, arguments); __wxTapWrite(level, args); }; })(__wxTapLevels[__wxTapIndex]);
}
var console = (function(){
try {
return new Proxy(__wxTapReal, { get: function(target, key){ if (__wxTapLevels.indexOf(key) >= 0) { return __wxTapShim[key]; } var value = target[key]; return typeof value === "function" ? value.bind(target) : value; } });
} catch (__wxTapNoProxy) { return __wxTapShim; }
})();
var __wxTapSource = __WXTAP_JSON_SOURCE__;
try { return eval(__wxTapSource); }
catch (__wxTapError) { __wxTapWrite("error", [__wxTapPrefix + ((__wxTapError && __wxTapError.stack) || String(__wxTapError))]); throw __wxTapError; }
})()`

// HookScriptExpression builds the injected expression for one user hook script.
func HookScriptExpression(filename, source string) string {
	prefix, err := json.Marshal("[" + filename + "] ")
	if err != nil {
		prefix = []byte(`"[] "`)
	}
	// sourceURL 只在字符串里生效：eval 出来的代码在堆栈里没有名字，加上它，
	// 报错与断点都还认得这个脚本。
	body, err := json.Marshal(source + "\n//# sourceURL=wxtap-user-script/" + filename)
	if err != nil {
		body = []byte(`""`)
	}
	expression := strings.Replace(hookScriptTemplate, "__WXTAP_JSON_PREFIX__", string(prefix), 1)
	return strings.Replace(expression, "__WXTAP_JSON_SOURCE__", string(body), 1)
}

// HookRunSummary renders a script's settled value as the one line the GUI and
// the shell log show. `undefined`/`null` and anything unserialisable degrade to
// an honest sentence instead of an empty cell.
func HookRunSummary(value any) string {
	switch typed := value.(type) {
	case nil:
		return "无返回值"
	case string:
		return truncateRunes(strings.TrimSpace(typed), HookRunSummaryLimit)
	case bool, float64:
		return fmt.Sprint(typed)
	default:
		var buffer bytes.Buffer
		encoder := json.NewEncoder(&buffer)
		// 默认的 HTML 转义会把尖括号和 & 换成转义序列：这是排查工具的输出，
		// 原文才看得出问题。
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(typed); err != nil {
			return truncateRunes(fmt.Sprint(typed), HookRunSummaryLimit)
		}
		return truncateRunes(strings.TrimSpace(buffer.String()), HookRunSummaryLimit)
	}
}

func truncateRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit]) + "…（已截断）"
}
