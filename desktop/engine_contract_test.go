package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/engine"
)

// The engine.* handlers are the 状态页's whole surface, and its failure paths
// matter as much as the happy one: a missing Core runtime must surface as a
// typed error (not a panic, not a false "started"), and wechat.status must
// answer with a payload the UI can render instead of an error envelope.
func TestEngineHandlersDegradeWhenTheCoreRuntimeIsMissing(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	app.setupIPC()
	t.Setenv("WXTAP_CORE_CMD", "wxtap-missing-node-xyz")

	ctx := context.Background()

	for _, method := range []string{"engine.start", "engine.stop"} {
		t.Run(method, func(t *testing.T) {
			_, err := app.router.Call(ctx, method, json.RawMessage(`{}`))
			if err == nil {
				t.Fatalf("%s must fail loudly when the Core cannot be spawned", method)
			}
			// A pinned override is the operator's own choice, so the error has to
			// name the command that failed rather than advice about installing a
			// Node they may already have.
			if !strings.Contains(err.Error(), "wxtap-missing-node-xyz") {
				t.Fatalf("%s error should name the failing runtime: %v", method, err)
			}
		})
	}

	// wechat.status is polled once a second by the status page: it must degrade
	// to a renderable payload, never an error envelope.
	result, err := app.router.Call(ctx, "wechat.status", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("wechat.status must not fail without a Core: %v", err)
	}
	payload, ok := result.(map[string]any)
	if !ok || payload["running"] != false {
		t.Fatalf("offline wechat.status must report not running: %#v", result)
	}
	note, _ := payload["error"].(string)
	if !strings.Contains(note, "微信状态检测不可用") {
		t.Fatalf("offline wechat.status must explain itself: %#v", result)
	}
}

// TestLiveEngineStartStopDrivesThePorts is the 状态页 contract on a real device:
// engine.start attaches to the logged-in WeChat and brings 9421 + the CDP port
// up; engine.stop releases both. It is opt-in because it injects a Frida hook
// into a real WeChat process — it must never run on a CI leg.
//
//	WXTAP_LIVE_WECHAT=1 go test ./ -run TestLiveEngineStartStopDrivesThePorts -v
func TestLiveEngineStartStopDrivesThePorts(t *testing.T) {
	if os.Getenv("WXTAP_LIVE_WECHAT") != "1" {
		t.Skip("opt-in: set WXTAP_LIVE_WECHAT=1 to attach to the running WeChat")
	}
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH")
	}
	script := resolveCoreScript(exeDir())
	if _, err := os.Stat(script); err != nil {
		t.Skipf("core bundle not built (%s): %v", script, err)
	}
	const cdpPort = 31517
	if portListening(t, cdpPort) || portListening(t, 9421) {
		t.Skipf("port %d or 9421 is already in use; close the running engine first", cdpPort)
	}

	app := NewApp()
	app.ctx = context.Background()
	app.dataBase = t.TempDir()
	app.setupIPC()
	t.Setenv("WXTAP_CORE_CMD", nodePath)
	t.Setenv("WXTAP_CORE_SCRIPT", script)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if _, err := app.router.Call(ctx, "engine.start", json.RawMessage(`{"cdp_port":`+strconv.Itoa(cdpPort)+`}`)); err != nil {
		t.Fatalf("engine.start against the real Core: %v", err)
	}
	defer func() {
		if _, err := app.router.Call(context.Background(), "engine.stop", json.RawMessage(`{}`)); err != nil {
			t.Errorf("engine.stop must release the engine: %v", err)
		}
		if portListening(t, cdpPort) || portListening(t, 9421) {
			t.Errorf("ports must be released after engine.stop")
		}
	}()

	if !portListening(t, 9421) || !portListening(t, cdpPort) {
		t.Fatalf("engine.start must listen on 9421 and %d", cdpPort)
	}

	status, err := app.router.Call(ctx, "engine.status", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("engine.status after start: %v", err)
	}
	if payload, ok := status.(map[string]any); !ok || payload["frida"] != true {
		t.Fatalf("engine.status must report Frida attached: %#v", status)
	}

	// The handler answers with the typed engine.WeChatStatus (the frontend sees
	// its JSON shape), so decode rather than asserting a map.
	// 控制台采集：小程序的 console 在当前 WMPF 版本里是 configurable:false 的
	// 访问器（页内 hook 包不上），所以记录由 Core 经 CDP 事件推给 shell。这里触发
	// 一次页面内的 console.log，并确认它落进 shell 的环形缓冲（Console 页读的就是
	// 这个缓冲）。
	var liveStatus engine.EngineStatus
	{
		waitCtx, cancelWait := context.WithTimeout(ctx, 180*time.Second)
		for liveStatus.Miniapp != true {
			if waitCtx.Err() != nil {
				cancelWait()
				t.Skip("no miniapp connected: open a mini program, then re-run this live check")
			}
			liveStatus, err = app.engineStatusForTest(waitCtx)
			if err != nil {
				cancelWait()
				t.Fatalf("engine.status: %v", err)
			}
			time.Sleep(time.Second)
		}
		cancelWait()
	}
	client, err := app.currentEngine()
	if err != nil {
		t.Fatalf("engine client: %v", err)
	}
	probe := "wxtap-live-console-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err := client.Evaluate(ctx, "console.log("+strconv.Quote(probe)+")", 5000); err != nil {
		t.Fatalf("evaluate console.log: %v", err)
	}
	found := false
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline) && !found; {
		result, err := app.router.Call(ctx, "console.list", json.RawMessage(`{"tail":50}`))
		if err != nil {
			t.Fatalf("console.list: %v", err)
		}
		data, _ := json.Marshal(result)
		if strings.Contains(string(data), probe) {
			found = true
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !found {
		t.Errorf("a page-side console.log must reach the shell console ring")
	}

	// 注入脚本页：注入 → 后端登记「已注入」→ 标记全局 → realm 重建后自动注入。
	// 这一步需要你在测试运行期间重载一次小程序（提示会打在输出里）。
	hooks := app.hookStore()
	if hooks == nil {
		t.Fatal("hook store unavailable")
	}
	scriptName := "wxtap-live-probe.js"
	if err := os.MkdirAll(hooks.ScriptsDir(), 0o750); err != nil {
		t.Fatalf("scripts dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hooks.ScriptsDir(), scriptName), []byte("console.log('[WxTap] live probe injected');"), 0o644); err != nil {
		t.Fatalf("write probe script: %v", err)
	}
	if _, err := app.router.Call(ctx, "hook.inject", json.RawMessage(`{"filename":"`+scriptName+`"}`)); err != nil {
		t.Fatalf("hook.inject: %v", err)
	}
	if injected := hookScriptState(t, app, scriptName); !injected {
		t.Errorf("hook.inject must record the script as injected for the current realm")
	}
	if _, err := app.router.Call(ctx, "hook.setGlobal", json.RawMessage(`{"filename":"`+scriptName+`","global":true}`)); err != nil {
		t.Fatalf("hook.setGlobal: %v", err)
	}

	// realm 重建（页面重载/小程序重启）会清空注入登记，随后应重新自动注入全局脚本。
	before, err := app.engineStatusForTest(ctx)
	if err != nil {
		t.Fatalf("engine.status before reload: %v", err)
	}
	t.Logf("请在 %d 秒内重载一次小程序（关掉再打开），以验证全局脚本自动注入", 120)
	reloaded := false
	for deadline := time.Now().Add(120 * time.Second); time.Now().Before(deadline); {
		time.Sleep(2 * time.Second)
		current, err := app.engineStatusForTest(ctx)
		if err != nil {
			continue
		}
		if current.Generation > before.Generation {
			reloaded = true
			// 自动注入在 generation 变化后异步执行，给它一点时间。
			time.Sleep(3 * time.Second)
			break
		}
	}
	if !reloaded {
		t.Log("没有观察到 realm 重建：全局脚本自动注入这一次未验证（其余检查不受影响）")
	} else if !hookScriptState(t, app, scriptName) {
		t.Errorf("a global script must be injected again after the realm is rebuilt")
	}
	// 收尾：撤掉全局标记与探针脚本，别污染用户目录。
	_, _ = app.router.Call(ctx, "hook.setGlobal", json.RawMessage(`{"filename":"`+scriptName+`","global":false}`))
	_ = os.Remove(filepath.Join(hooks.ScriptsDir(), scriptName))

	host, err := app.router.Call(ctx, "wechat.status", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("wechat.status while attached: %v", err)
	}
	encoded, err := json.Marshal(host)
	if err != nil {
		t.Fatalf("marshal wechat.status: %v", err)
	}
	var described engine.WeChatStatus
	if err := json.Unmarshal(encoded, &described); err != nil {
		t.Fatalf("decode wechat.status: %v", err)
	}
	if !described.Running {
		t.Fatalf("wechat.status must report the host running: %s", encoded)
	}
	if described.Version <= 0 {
		t.Fatalf("wechat.status must name the verified build: %s", encoded)
	}
}

// portListening reports whether something accepts TCP connections on a loopback port.
func portListening(t *testing.T, port int) bool {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// hookScriptState 读 hook.list，判断某个脚本是否被登记为「已注入」。
func hookScriptState(t *testing.T, app *App, filename string) bool {
	t.Helper()
	result, err := app.router.Call(context.Background(), "hook.list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("hook.list: %v", err)
	}
	data, _ := json.Marshal(result)
	var decoded struct {
		Scripts []struct {
			Filename string `json:"filename"`
			Injected bool   `json:"injected"`
		} `json:"scripts"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode hook.list: %v", err)
	}
	for _, script := range decoded.Scripts {
		if script.Filename == filename {
			return script.Injected
		}
	}
	return false
}
