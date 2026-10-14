package mcp

// miniapp_navigate 的事后核验：回调说 ok 不算数，以调用之后的真实页面栈为准。
// fakeAppBridge 是静态的，这里的替身栈会随导航翻面。

import (
	"context"
	"strings"
	"testing"
	"time"
)

func init() {
	// 核验按真实时钟轮询；全文件把窗口缩到最小。
	navigationVerifyTimeout, navigationVerifyPoll = 60*time.Millisecond, 5*time.Millisecond
}

// navBridge models a page stack that flips when a navigation dispatches.
type navBridge struct {
	calls []string
	route string
	depth int
}

func (b *navBridge) Call(_ context.Context, method string, params map[string]any) (any, error) {
	b.calls = append(b.calls, method)
	switch method {
	case "navigator.pageStack":
		stack := []any{}
		for i := 0; i < b.depth; i++ {
			route := b.route
			if i < b.depth-1 {
				route = "pages/below"
			}
			stack = append(stack, map[string]any{"route": route})
		}
		return map[string]any{"stack": stack, "current": b.route}, nil
	case "navigator.currentRoute":
		return map[string]any{"route": b.route}, nil
	case "navigator.navigate":
		route, _ := params["route"].(string)
		if params["method"] == "navigateBack" {
			delta := 1
			// 进程内直调拿到的 delta 是 Go int（真 IPC 是 JSON float64），两种都收。
			switch d := params["delta"].(type) {
			case float64:
				if int(d) > 0 {
					delta = int(d)
				}
			case int:
				if d > 0 {
					delta = d
				}
			}
			b.depth -= delta
			if b.depth < 1 {
				b.depth = 1
			}
			b.route = "pages/below"
			return map[string]any{"ok": true}, nil
		}
		b.route = route
		b.depth++
		return map[string]any{"ok": true}, nil
	}
	return map[string]any{"ok": true}, nil
}

// navCore answers the routeless body[is] read (bodyRoute, double-quoted so it
// never collides with the resolver's single-quoted probePageMarker) with the
// landing route — the fill-in path for a stack layer whose route reads "".
type navCore struct{ fakeCore }

func (navCore) CDPCommand(_ context.Context, method string, params map[string]any, _ int) (map[string]any, error) {
	if method == "Runtime.evaluate" {
		if expr, _ := params["expression"].(string); strings.Contains(expr, `getAttribute("is")`) {
			return evaluateStringResult("pages/payment/pay"), nil
		}
	}
	return map[string]any{"result": map[string]any{"result": map[string]any{"type": "string", "value": `{"ok":true}`}}}, nil
}

func navigateTestDeps(bridge AppBridge, core *navCore) Deps {
	return Deps{Core: core, AppBridge: bridge}
}

// 分发成功且栈真的落向目标路由 → ok:true，route_observed 是实际落点。
func TestNavigateVerifiesALandingRoute(t *testing.T) {
	bridge := &navBridge{route: "pages/index", depth: 1}
	messages := exchange(t, navigateTestDeps(bridge, &navCore{}),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_navigate","arguments":{"route":"pages/payment/pay","method":"navigateTo"}}}`+"\n")
	text := toolText(t, messages, 1)
	if toolFailed(t, messages, 1) || !strings.Contains(text, `"ok":true`) {
		t.Fatalf("落地成功不该失败: %s", text)
	}
	if !strings.Contains(text, `"route_observed":"pages/payment/pay"`) {
		t.Fatalf("应回报实际落点路由: %s", text)
	}
}

// staticNavBridge never moves: dispatches "succeed" but the stack stays put —
// the false-ok shape observed live on switchTab/navigateBack.
type staticNavBridge struct{ navBridge }

func (b *staticNavBridge) Call(ctx context.Context, method string, params map[string]any) (any, error) {
	if method == "navigator.navigate" {
		b.calls = append(b.calls, method)
		return map[string]any{"ok": true}, nil
	}
	return b.navBridge.Call(ctx, method, params)
}

// 回调说 ok 而栈纹丝不动 → ok:false + 观测到的栈顶。实测 switchTab/navigateBack
// 都出过这种「假 ok」。
func TestNavigateReportsAFalseOkWhenTheStackNeverMoves(t *testing.T) {
	static := &staticNavBridge{navBridge: navBridge{route: "pages/index", depth: 1}}
	messages := exchange(t, navigateTestDeps(static, &navCore{}),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_navigate","arguments":{"route":"pages/payment/pay","method":"navigateTo"}}}`+"\n")
	text := toolText(t, messages, 1)
	if toolFailed(t, messages, 1) {
		t.Fatalf("假 ok 是数据不是工具错误: %s", text)
	}
	if !strings.Contains(text, `"ok":false`) || !strings.Contains(text, "did not land") {
		t.Fatalf("栈未落地必须回 ok:false 并带归因: %s", text)
	}
	if !strings.Contains(text, `"route_observed":"pages/index"`) {
		t.Fatalf("route_observed 应是观测到的栈顶: %s", text)
	}
}

// 路由为空串的新层（webview 尚在挂载，实测出现过）：用该页 webview 的 body[is] 补上。
func TestNavigateFillsAnEmptyRouteFromBodyMarker(t *testing.T) {
	bridge := &navBridge{route: "", depth: 2}
	messages := exchange(t, navigateTestDeps(bridge, &navCore{}),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_navigate","arguments":{"route":"pages/payment/pay","method":"navigateTo"}}}`+"\n")
	text := toolText(t, messages, 1)
	if toolFailed(t, messages, 1) || !strings.Contains(text, `"ok":true`) {
		t.Fatalf("body[is] 补齐后不该失败: %s", text)
	}
	if !strings.Contains(text, `"route_observed":"pages/payment/pay"`) {
		t.Fatalf("空路由应由 body[is] 补齐: %s", text)
	}
}

// navigateBack 以栈深收敛判定：深度真的减了才算落地。
func TestNavigateBackVerifiesTheDepthChange(t *testing.T) {
	bridge := &navBridge{route: "pages/top", depth: 3}
	messages := exchange(t, navigateTestDeps(bridge, &navCore{}),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_navigate","arguments":{"method":"navigateBack","delta":2}}}`+"\n")
	text := toolText(t, messages, 1)
	if toolFailed(t, messages, 1) || !strings.Contains(text, `"ok":true`) {
		t.Fatalf("回退落地不该失败: %s", text)
	}
	if bridge.depth != 1 {
		t.Fatalf("delta=2 应把栈深从 3 减到 1，实际 %d", bridge.depth)
	}
}

// 栈深不减的假 ok 回退 → ok:false。
func TestNavigateBackReportsAFalseOkWhenDepthNeverShrinks(t *testing.T) {
	static := &staticNavBridge{navBridge: navBridge{route: "pages/top", depth: 3}}
	messages := exchange(t, navigateTestDeps(static, &navCore{}),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_navigate","arguments":{"method":"navigateBack"}}}`+"\n")
	text := toolText(t, messages, 1)
	if !strings.Contains(text, `"ok":false`) || !strings.Contains(text, "did not shrink") {
		t.Fatalf("深度不减必须回 ok:false: %s", text)
	}
}

// 宿主侧分发被拒（插件页前台）时，navigateBack 回退到页 webview 里调。
func TestNavigateBackRetriesInsideThePageWebview(t *testing.T) {
	core := &recordingCore{}
	bridge := &refusingNavBridge{navBridge: navBridge{route: "pages/index", depth: 1}}
	messages := exchange(t, Deps{Core: core, AppBridge: bridge},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"miniapp_navigate","arguments":{"method":"navigateBack"}}}`+"\n")
	text := toolText(t, messages, 1)
	if toolFailed(t, messages, 1) || !strings.Contains(text, `"ok":true`) {
		t.Fatalf("页 webview 回退成功不该失败: %s", text)
	}
	if !strings.Contains(text, "page webview") {
		t.Fatalf("应注明走了页 webview 回退: %s", text)
	}
	found := false
	for _, expr := range core.expressions {
		if strings.Contains(expr, "wx.navigateBack") {
			found = true
		}
	}
	if !found {
		t.Fatalf("应在页 webview 里求值 wx.navigateBack: %v", core.expressions)
	}
}

// refusingNavBridge refuses appservice-side dispatch the way a foreground
// plugin page does; the page webview fallback (real wx) then goes through.
type refusingNavBridge struct{ navBridge }

func (b *refusingNavBridge) Call(ctx context.Context, method string, params map[string]any) (any, error) {
	if method == "navigator.navigate" {
		b.calls = append(b.calls, method)
		return nil, context.DeadlineExceeded
	}
	return b.navBridge.Call(ctx, method, params)
}
