package mcp

import (
	"strings"
	"testing"
)

// The two context probes the miniapp tools issue, matched by the exact text the
// resolver builds. Being this specific matters: miniapp_list_contexts asks
// about getApp too, and a looser marker would hijack its answer.
const (
	probeAppServiceMarker = `typeof wx==='object'&&typeof getApp==='function'`
	probePageMarker       = `getAttribute('is')`

	// defaultAppServiceCtxID / defaultPageCtxID are the context ids the fakes
	// expose, mirroring a real target: the appservice is low and the page
	// webviews follow. The route must match what fakeAppBridge reports for
	// navigator.currentRoute.
	defaultAppServiceCtxID = 1
	defaultPageCtxID       = 2
)

// cdpContexts is the execution-context layout a fake target exposes.
type cdpContexts struct {
	appService int            // the logic layer (wx + getApp)
	pages      map[int]string // page webview id -> the route on its body[is]
}

var defaultCDPContexts = cdpContexts{
	appService: defaultAppServiceCtxID,
	pages:      map[int]string{defaultPageCtxID: "pages/index"},
}

// isContextProbe reports whether an expression is one of the two context probes
// the resolvers build.
func isContextProbe(expression string) bool {
	return strings.Contains(expression, probeAppServiceMarker) ||
		strings.Contains(expression, probePageMarker)
}

// answerContextProbe answers a Runtime.evaluate that carries a contextId the
// way a real target does: the right context answers "yes", every other one
// answers "no". It reports false when the params carry no contextId at all, so
// the caller falls back to its own canned payload.
func answerContextProbe(expression string, params map[string]any, layout cdpContexts) (map[string]any, bool) {
	raw, present := params["contextId"]
	if !present {
		return nil, false
	}
	// 只有探针由这里作答。带 contextId 的普通求值（工具真正要跑的那条表达式）
	// 必须落到调用方自己的载荷上，否则测试里工具拿到的是探针的 "no"，针对载荷的
	// 断言就成了空转。
	if !isContextProbe(expression) {
		return nil, false
	}
	if layout.appService == 0 && len(layout.pages) == 0 {
		layout = defaultCDPContexts
	}
	id := probeContextID(raw)
	answer := "no"
	switch {
	case strings.Contains(expression, probeAppServiceMarker):
		if id == layout.appService {
			answer = contextProbeYes
		}
	case strings.Contains(expression, probePageMarker):
		// The probe embeds the route it is looking for, so a webview only
		// matches when its body[is] carries that same route.
		if route, ok := layout.pages[id]; ok && strings.Contains(expression, route) {
			answer = contextProbeYes
		}
	}
	return evaluateStringResult(answer), true
}

// evaluateStringResult is the wire shape of Runtime.evaluate returning a string.
func evaluateStringResult(value string) map[string]any {
	return map[string]any{"result": map[string]any{"result": map[string]any{"type": "string", "value": value}}}
}

// probeContextID reads the contextId out of Runtime.evaluate params. The
// resolver sets it as a Go int, so a float64-only assertion would read every
// probe as id 0.
func probeContextID(raw any) int {
	switch value := raw.(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	}
	return 0
}

// contextCall records one Runtime.evaluate that carried a contextId: which
// expression the tool aimed at which execution context.
type contextCall struct {
	ID   int
	Expr string
}

// contextIDFor returns the id of the context that received an expression
// containing marker, or -1 when no such call happened.
func contextIDFor(calls []contextCall, marker string) int {
	for _, call := range calls {
		if strings.Contains(call.Expr, marker) {
			return call.ID
		}
	}
	return -1
}

// callTool is the one-request exchange every case below is built from.
func callTool(t *testing.T, deps Deps, name, arguments string) (bool, string) {
	t.Helper()
	messages := exchange(t, deps,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+arguments+`}}`+"\n")
	return toolFailed(t, messages, 1), toolText(t, messages, 1)
}

// Storage must run in the appservice. The host page has no wx, which is exactly
// how the storage tools used to answer "wx is not defined".
func TestStorageToolsTargetTheAppServiceContext(t *testing.T) {
	core := &recordingCore{}
	failed, text := callTool(t, Deps{Core: core, AppBridge: fakeAppBridge{}}, "miniapp_get_storage", `{}`)
	if failed {
		t.Fatalf("读存储不该失败: %s", text)
	}
	if got := contextIDFor(core.contextCalls, "getStorageInfoSync"); got != defaultAppServiceCtxID {
		t.Fatalf("存储求值打到了上下文 %d，应显式为 appservice %d（-1 = 根本没指定上下文）", got, defaultAppServiceCtxID)
	}
}

// The mini program's DOM only exists in the current page's webview; the host
// page has no wx-view elements, so a selector there never matches.
func TestDOMToolsTargetTheCurrentPageContext(t *testing.T) {
	core := &recordingCore{}
	failed, text := callTool(t, Deps{Core: core, AppBridge: fakeAppBridge{}}, "miniapp_click", `{"selector":"#go"}`)
	if failed {
		t.Fatalf("选择器点击不该失败: %s", text)
	}
	if got := contextIDFor(core.contextCalls, "document.querySelector"); got != defaultPageCtxID {
		t.Fatalf("选择器求值打到了上下文 %d，应为当前页 webview %d", got, defaultPageCtxID)
	}
}

// switchTab no longer evaluates wx.switchTab directly: that landed in the host
// page context (no wx) and bypassed the page-side navigator, so with the
// redirect guard on the tool's own navigation was intercepted and still
// reported success.
func TestNavigateSwitchTabGoesThroughThePageNavigator(t *testing.T) {
	core := &recordingCore{}
	failed, text := callTool(t, Deps{Core: core, AppBridge: fakeAppBridge{}}, "miniapp_navigate", `{"route":"pages/index/index","method":"switchTab"}`)
	if failed {
		t.Fatalf("switchTab 不该失败: %s", text)
	}
	for _, expr := range core.expressions {
		if strings.Contains(expr, "wx.switchTab") {
			t.Fatalf("switchTab 不该再直接求值 wx.switchTab: %s", expr)
		}
	}
}

// No matching context must be an error. Falling back to the default context is
// how the tools silently produced "wx is not defined" in the first place.
func TestContextResolutionRefusesToGuess(t *testing.T) {
	// appService 0 = 没有任何上下文是逻辑层；pages 非空是为了绕过替身的缺省
	// 布局回退，否则这条用例会退化成「一切正常」。
	core := &recordingCore{}
	core.contexts = cdpContexts{pages: map[int]string{8: "pages/other"}}
	failed, text := callTool(t, Deps{Core: core, AppBridge: fakeAppBridge{}}, "miniapp_get_storage", `{}`)
	if !failed {
		t.Fatal("没有 appservice 上下文时必须报错")
	}
	if !strings.Contains(text, "logic layer not found") {
		t.Fatalf("错误应指明逻辑层未找到: %s", text)
	}
}

// A live webview whose route is not the current one must not be used: clicking
// there would land on a page the mini program is not showing.
func TestPageContextRequiresTheCurrentRoute(t *testing.T) {
	core := &recordingCore{}
	core.contexts = cdpContexts{appService: defaultAppServiceCtxID, pages: map[int]string{7: "pages/stale"}}
	failed, text := callTool(t, Deps{Core: core, AppBridge: fakeAppBridge{}}, "miniapp_click", `{"selector":"#go"}`)
	if !failed {
		t.Fatal("没有当前路由对应的 webview 时必须报错")
	}
	if !strings.Contains(text, "page webview for pages/index not found") {
		t.Fatalf("错误应指明当前页 webview 未找到: %s", text)
	}
}

// 切页时会有一个短暂窗口：appservice 报的还是上一页，而上一页的 webview 已经销毁。
// 这正是一次真机验证里观察到的失败。解析器必须重试到两边一致，而不是当场报错。
func TestPageContextRetriesWhileAPageTransitionSettles(t *testing.T) {
	// 第一轮扫描的 24 个探针全部落空，第二轮才看得到页面 webview。
	core := &recordingCore{pageMisses: contextProbeMaxID}
	failed, text := callTool(t, Deps{Core: core, AppBridge: fakeAppBridge{}}, "miniapp_click", `{"selector":"#go"}`)
	if failed {
		t.Fatalf("切页期间的选择器点击不该失败: %s", text)
	}
	if got := contextIDFor(core.contextCalls, "document.querySelector"); got != defaultPageCtxID {
		t.Fatalf("重试后仍应落到当前页 webview %d，实际 %d", defaultPageCtxID, got)
	}
}

// 页内 try/catch 的 {error} 必须变成工具错误。直接当成功转发，一次失败的读会读成
// 一次空的读 —— 读路径以前就是这样漏掉的（写路径早修过）。
func TestStorageReadTurnsAnInlinePageErrorIntoAFailure(t *testing.T) {
	core := &recordingCore{
		errorPayload:  `{"error":"getStorageInfoSync is not a function"}`,
		payloadMarker: "getStorageInfoSync",
	}
	failed, text := callTool(t, Deps{Core: core, AppBridge: fakeAppBridge{}}, "miniapp_get_storage", `{}`)
	if !failed {
		t.Fatalf("页内返回 error 时必须读作失败: %s", text)
	}
	if !strings.Contains(text, "getStorageInfoSync is not a function") {
		t.Fatalf("错误应保留页内原因: %s", text)
	}
}

// 单键读走同一条转换（合并形态与旧单键形态共用 evaluateStorage）。
func TestStorageReadKeyTurnsAnInlinePageErrorIntoAFailure(t *testing.T) {
	core := &recordingCore{
		errorPayload:  `{"error":"getStorageSync failed"}`,
		payloadMarker: "wx.getStorageSync",
	}
	failed, text := callTool(t, Deps{Core: core, AppBridge: fakeAppBridge{}}, "miniapp_get_storage", `{"key":"token"}`)
	if !failed {
		t.Fatalf("页内返回 error 时必须读作失败: %s", text)
	}
	if !strings.Contains(text, "getStorageSync failed") {
		t.Fatalf("错误应保留页内原因: %s", text)
	}
}

// 滚动必须派发真实滚轮事件。WMPF 页面由合成器滚动，改 scrollTop 或调
// window.scrollBy 都不会让页面动（真机实测两者都恒为 0），而工具此前回 ok。
func TestScrollDispatchesARealWheelEvent(t *testing.T) {
	core := &recordingCore{}
	failed, text := callTool(t, Deps{Core: core, AppBridge: fakeAppBridge{}}, "miniapp_scroll", `{"x":0,"y":400}`)
	if failed {
		t.Fatalf("滚动不该失败: %s", text)
	}
	if core.lastInputParams == nil {
		t.Fatalf("滚动必须派发滚轮事件，实际命令: %v", core.methods)
	}
	if got := core.lastInputParams["type"]; got != "mouseWheel" {
		t.Fatalf("事件类型应为 mouseWheel，实际 %v", got)
	}
	if got, ok := core.lastInputParams["deltaY"].(float64); !ok || got != 400 {
		t.Fatalf("deltaY 应原样透传 400，实际 %v", core.lastInputParams["deltaY"])
	}
	for _, expr := range core.expressions {
		if strings.Contains(expr, "window.scrollBy") || strings.Contains(expr, "scrollTop+=") {
			t.Fatalf("不该再用 DOM 滚动: %s", expr)
		}
	}
	// 落点必须在当前页 webview 里取，宿主页的尺寸与小程序的可见区域无关。
	if got := contextIDFor(core.contextCalls, "innerWidth"); got != defaultPageCtxID {
		t.Fatalf("取落点应打在当前页 webview %d，实际 %d", defaultPageCtxID, got)
	}
}

// 重试必须有界：一直找不到就报错，不能转圈。
func TestPageContextGivesUpAfterBoundedRetries(t *testing.T) {
	core := &recordingCore{pageMisses: 1 << 20}
	failed, text := callTool(t, Deps{Core: core, AppBridge: fakeAppBridge{}}, "miniapp_click", `{"selector":"#go"}`)
	if !failed {
		t.Fatal("始终找不到页面 webview 时必须报错")
	}
	if !strings.Contains(text, "after 3 attempts") {
		t.Fatalf("错误应说明重试已用尽: %s", text)
	}
}
