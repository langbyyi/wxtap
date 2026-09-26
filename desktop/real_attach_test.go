package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRealAttachAndDeepLink is the only check that exercises the live
// navigation path: attach to the local WeChat, read the running mini program's
// page list, and deep-link into it through the same IPC the 「页面路由」 view
// uses.
//
// It is opt-in (WXTAP_REAL_ATTACH=1) because attaching a debugger to the user's
// logged-in WeChat is not something a test suite may do on its own. It also
// writes nothing to the real data directory: WXTAP_DATA_DIR points at a temp
// dir, so the user's config and traffic database are untouched.
//
// Everything it proves cannot be proven offline: Frida attach, the bridge's
// page list, and that goTo() actually moves the running mini program.
func TestRealAttachAndDeepLink(t *testing.T) {
	if os.Getenv("WXTAP_REAL_ATTACH") != "1" {
		t.Skip("set WXTAP_REAL_ATTACH=1 to attach to the local WeChat install")
	}
	if runtime.GOOS != "windows" {
		t.Skip("the local attach check is written for the Windows WeChat install")
	}
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node is not on PATH: %v", err)
	}
	script := resolveCoreScript(exeDir())
	if _, err := os.Stat(script); err != nil {
		t.Skipf("core bundle not built (%s): %v", script, err)
	}

	// Isolate every writable path this run touches.
	t.Setenv("WXTAP_DATA_DIR", t.TempDir())
	t.Setenv("WXTAP_CORE_CMD", nodePath)
	t.Setenv("WXTAP_CORE_SCRIPT", script)
	t.Setenv("WXTAP_CORE_DIR", filepath.Dir(filepath.Dir(script)))

	app := NewApp()
	app.setupIPC()
	defer func() {
		t.Logf("engine.stop -> %s", app.Call("engine.stop", "{}"))
		// Production shutdown, not just engine.stop: stopping the engine
		// detaches Frida but leaves the Core process holding 9421/31415 until
		// its stdio pipe closes, so two runs in a row collide on the ports.
		app.shutdown(context.Background())
		t.Logf("core 已关停，端口已释放")
	}()

	// 1. Attach. The engine attaches to the WMPF host (WeChatAppEx.exe), so a
	// mini program must already be open: that precondition belongs to the
	// caller, and its absence must read as guidance rather than as a red test.
	started := app.Call("engine.start", `{"cdp_port":62000}`)
	t.Logf("engine.start -> %s", started)
	var startEnvelope map[string]any
	if err := json.Unmarshal([]byte(started), &startEnvelope); err != nil {
		t.Fatalf("engine.start did not answer with an envelope: %s", started)
	}
	if _, ok := startEnvelope["error"]; ok {
		if strings.Contains(started, "WeChatAppEx") {
			t.Skipf("前置未满足（先在微信里打开一个小程序，再重跑）：%s", started)
		}
		t.Fatalf("engine.start 失败: %s", started)
	}

	status := callJSON2(t, app, "engine.status", nil)
	t.Logf("engine.status -> %v", status)
	if frida, _ := status["frida"].(bool); !frida {
		t.Fatalf("frida did not attach: %#v", status)
	}
	t.Logf("目标小程序: %v", status["appInfo"])

	// 2. The state a user is in before opening a mini program. The view turns
	// either outcome into its own guidance, but only if the backend answers
	// rather than crashing, and only if "no target" really means "no appid" —
	// the view tells "nothing attached" from "attached but empty" off that
	// field.
	probe := app.Call("navigator.pages", "{}")
	t.Logf("无目标时 navigator.pages -> %s", probe)
	var probed map[string]any
	if err := json.Unmarshal([]byte(probe), &probed); err != nil {
		t.Fatalf("navigator.pages did not answer with an envelope: %s", probe)
	}
	if result, ok := probed["result"].(map[string]any); ok {
		if appID, _ := result["appid"].(string); appID != "" {
			t.Fatalf("no mini program is attached but navigator.pages reported appid %q", appID)
		}
	} else if _, ok := probed["error"]; !ok {
		t.Fatalf("unexpected envelope: %s", probe)
	}

	// The same state for the jump itself: the view guards on the running appid,
	// but a stale one would reach the backend, so the error a user could see
	// must be readable too.
	navigate := app.Call("navigator.navigate", `{"route":"pages/index/index","method":"navigateTo"}`)
	t.Logf("无目标时 navigator.navigate -> %s", navigate)
	var navigateEnvelope map[string]any
	if err := json.Unmarshal([]byte(navigate), &navigateEnvelope); err != nil {
		t.Fatalf("navigator.navigate did not answer with an envelope: %s", navigate)
	}
	if _, failed := navigateEnvelope["error"]; !failed {
		t.Fatalf("no mini program is attached but navigate reported success: %s", navigate)
	}

	// 3. Which mini programs are running (WMPF exposes one bridge per target).
	//
	// The Core *listens* on 9421 and a mini program dials in once the Frida
	// hook has enabled its debug channel, so a target only appears after this
	// attach and after the mini program is opened. Waiting here is what makes
	// the check usable: run it, then open a mini program.
	//
	// Opening one for the user is not possible from here, and that was
	// measured rather than assumed:
	//
	//   - WeChat's URL scheme needs a ticket generated with a valid AK; the
	//     appid-only form is silently ignored (no new WeChatAppEx process, no
	//     9421 handshake) — with the engine attached or not.
	//   - The panel scheme (weixin://dl/business/) does spawn WMPF hosts, but
	//     windowless ones with no debug target: measured over a 40s attach
	//     window, miniapp.list stayed at 0.
	//   - WeChat 4.x draws its own UI (MMUIRenderSubWindowHW): its window
	//     exposes 52 automation nodes with no named controls, so there is
	//     nothing to invoke by name.
	//   - Clicking the self-drawn surface by coordinate could land on a chat or
	//     a send button, so it is not attempted.
	//
	// So the mini program is the caller's step.
	wait := 30 * time.Second
	if seconds, err := strconv.Atoi(os.Getenv("WXTAP_REAL_WAIT")); err == nil && seconds > 0 {
		wait = time.Duration(seconds) * time.Second
	}
	deadline := time.Now().Add(wait)
	var items []any
	for {
		list := callJSON2(t, app, "miniapp.list", nil)
		items, _ = list["list"].([]any)
		if len(items) > 0 || time.Now().After(deadline) {
			break
		}
		t.Logf("等待小程序目标…（还剩 %v）", time.Until(deadline).Round(time.Second))
		time.Sleep(3 * time.Second)
	}
	t.Logf("miniapp.list -> %d 个目标", len(items))
	if len(items) == 0 {
		// The CDP-side view can disagree with the bridge view: with several
		// WeChatAppEx hosts alive, the engine may sit on an idle one while a
		// real target is advertised through the proxy. Log both so a "no
		// target" verdict says which layer was empty.
		t.Logf("targets.list -> %s", app.Call("targets.list", "{}"))
		t.Logf("engine.status -> %s", app.Call("engine.status", "{}"))
		// Explain the failure instead of only reporting it: a rejected 9421
		// handshake, a missing hook, or no mini program at all look identical
		// from the outside, and the Core's stderr tells them apart.
		if client, err := app.currentEngine(); err == nil {
			if lines := client.RecentStderr(); len(lines) > 0 {
				tail := lines
				if len(tail) > 8 {
					tail = tail[len(tail)-8:]
				}
				t.Logf("Core stderr 尾部:\n%s", strings.Join(tail, "\n"))
			}
		}
		t.Skipf("等待 %v 仍没有小程序目标：请在微信里打开一个小程序后重跑本用例（WXTAP_REAL_WAIT 可延长等待）", wait)
	}
	defer func() {
		// Leave the desktop as found: the target we opened, we close.
		closed := app.Call("runtime.evaluate", `{"expression":"typeof wx!=='undefined'&&wx.exitMiniProgram?String(wx.exitMiniProgram()):'no-exit'"}`)
		t.Logf("关闭小程序 -> %s", closed)
	}()

	// 4. The running mini program's page list.
	pages := callJSON2(t, app, "navigator.pages", nil)
	pageList, _ := pages["pages"].([]any)
	t.Logf("navigator.pages -> appid=%v 当前=%v 页面数=%d", pages["appid"], pages["current_route"], len(pageList))
	if len(pageList) == 0 {
		t.Fatalf("running mini program reported no pages: %#v", pages)
	}

	// 5. Deep-link: jump the running mini program to a page the caller did not
	// open.
	current, _ := pages["current_route"].(string)
	tabBar := map[string]bool{}
	if tabs, ok := pages["tab_bar_pages"].([]any); ok {
		for _, item := range tabs {
			if name, ok := item.(string); ok {
				tabBar[name] = true
			}
		}
	}
	target := ""
	for _, item := range pageList {
		name, _ := item.(string)
		if name == "" || name == current || tabBar[name] {
			continue
		}
		target = strings.TrimPrefix(name, "/")
		break
	}
	if target == "" {
		t.Skip("只有一个页面或全是 tabBar 页面，跳不出可验证的差异")
	}

	result := callJSON2(t, app, "navigator.navigate", map[string]any{"route": target, "method": "navigateTo"})
	t.Logf("navigator.navigate(%s) -> %v", target, result)
	after := callJSON2(t, app, "navigator.currentRoute", nil)
	arrived, _ := after["route"].(string)
	t.Logf("navigator.currentRoute -> %v (期望 %s)", arrived, target)
	if strings.TrimPrefix(arrived, "/") != target {
		t.Fatalf("直达没有生效：当前 %q，期望 %q", arrived, target)
	}

	// 6. Put the user's session back where it was.
	if current != "" && strings.TrimPrefix(current, "/") != target {
		_ = callJSON2(t, app, "navigator.navigate", map[string]any{"route": current, "method": "reLaunch"})
		back := callJSON2(t, app, "navigator.currentRoute", nil)
		t.Logf("已回到 %v", back["route"])
	}
}

// callJSON2 mirrors the frontend's call path (App.Call -> router) and fails the
// test on a structured error, which is what the UI would render.
func callJSON2(t *testing.T, app *App, method string, params map[string]any) map[string]any {
	t.Helper()
	raw := "{}"
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			t.Fatalf("marshal %s: %v", method, err)
		}
		raw = string(encoded)
	}
	envelope := app.Call(method, raw)
	var decoded map[string]any
	if err := json.Unmarshal([]byte(envelope), &decoded); err != nil {
		t.Fatalf("%s: 非 JSON 响应 %s: %v", method, envelope, err)
	}
	if failure, ok := decoded["error"]; ok {
		t.Fatalf("%s 失败: %v", method, failure)
	}
	result, ok := decoded["result"].(map[string]any)
	if !ok {
		t.Fatalf("%s: 响应缺少 result: %s", method, envelope)
	}
	return result
}
