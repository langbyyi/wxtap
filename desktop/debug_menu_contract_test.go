package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// 前端的端口常量与 Go/Core 必须一致：9421 曾在三处各写一遍、31415 有两份，
// 谁改一处都不会有测试发现。这里解析前端与 Core 的源码，把常量钉在一起。
func TestFrontendPortConstantsMatchTheBackend(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	readConstant := func(rel, name string) float64 {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		match := regexp.MustCompile(name + `\s*=\s*(\d+)`).FindStringSubmatch(string(data))
		if match == nil {
			t.Fatalf("%s: constant %s not found", rel, name)
		}
		value := 0.0
		for _, digit := range match[1] {
			value = value*10 + float64(digit-'0')
		}
		return value
	}

	if got := readConstant("desktop/frontend/src/ports.ts", "DEFAULT_CDP_PORT"); got != float64(defaultCDPPort) {
		t.Errorf("frontend DEFAULT_CDP_PORT = %v, want %d (desktop/ports.go)", got, defaultCDPPort)
	}
	wmpfFrontend := readConstant("desktop/frontend/src/ports.ts", "WMPF_DEBUG_PORT")
	wmpfCore := readConstant("core/src/engine/wmpf-frida-runtime.ts", "DEBUG_PORT")
	if wmpfFrontend != wmpfCore {
		t.Errorf("frontend WMPF_DEBUG_PORT = %v, want %v (core DEBUG_PORT)", wmpfFrontend, wmpfCore)
	}
}

// pinElectronDetection replaces the well-known-location list for one test, the
// way pinNodeDetection does for Node.
//
// Clearing PATH and the Windows env vars is not enough on macOS: its candidates
// are absolute paths (/Applications/Electron.app, $HOME/Applications/
// Electron.app) that no environment variable can hide, so a Mac with Electron
// installed would find it and fail the "nothing is installed" assertion below.
func pinElectronDetection(t *testing.T, candidates []string) {
	t.Helper()
	original := electronRuntimeCandidates
	electronRuntimeCandidates = func() []string { return candidates }
	t.Cleanup(func() { electronRuntimeCandidates = original })
}

// 调试菜单里那些不依赖引擎的路由：失败必须以可读原因回来，而不是伪装成空结果。
func TestDebugMenuRoutesDegradeHonestlyOffline(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	app.dataBase = t.TempDir()
	app.setupIPC()
	t.Setenv("WXTAP_CORE_CMD", "wxtap-missing-node-xyz")

	ctx := context.Background()

	// targets.list：空数组会被界面读成「尚未检测到小程序」，必须带上原因。
	result, err := app.router.Call(ctx, "targets.list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("targets.list must not fail without a Core: %v", err)
	}
	payload, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("targets.list payload: %#v", result)
	}
	targets, ok := payload["targets"].([]any)
	if !ok || len(targets) != 0 {
		t.Fatalf("offline targets must be an empty array: %#v", payload["targets"])
	}
	if note, _ := payload["error"].(string); !strings.Contains(note, "调试目标不可用") {
		t.Fatalf("offline targets.list must explain itself: %#v", result)
	}

	// shell.openDevtoolsWindow：机器上找不到 Electron 时必须报清楚，而不是静默
	// 什么都不做。留空 electron_path 现在是「自动解析」（设置 → PATH → 常见安装
	// 位置），所以要把这几处一并清空才真的构成「找不到」——常见安装位置在 macOS
	// 上是绝对路径，清环境变量清不掉，得走测试接缝把它也钉空。
	t.Setenv("WXTAP_DATA_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())
	pinElectronDetection(t, nil)
	if _, err := app.router.Call(ctx, "shell.openDevtoolsWindow", json.RawMessage(`{"cdp_port":31415,"electron_path":""}`)); err == nil {
		t.Fatal("opening Electron with no Electron installed anywhere must fail")
	} else if !strings.Contains(err.Error(), "Electron") {
		t.Fatalf("error should name Electron: %v", err)
	}

	// engine.vconsole：没有引擎时必须同步报错（异步路径会把失败推成事件）。
	if _, err := app.router.Call(ctx, "engine.vconsole", json.RawMessage(`{"enable":true}`)); err == nil {
		t.Fatal("engine.vconsole must fail without a Core")
	}
}

// 「已注入」登记是 shell 侧的事实，realm 重建后必须整批作废。
func TestInjectedHookScriptRegistryResetsOnRealmRebuild(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	app.dataBase = t.TempDir()
	app.setupIPC()

	if app.hookScriptInjected("trace.js") {
		t.Fatal("a fresh shell must not claim any script was injected")
	}
	app.markHookScriptInjected("trace.js")
	app.markHookScriptInjected("other.js")
	if !app.hookScriptInjected("trace.js") || !app.hookScriptInjected("other.js") {
		t.Fatal("injection must be remembered for the current realm")
	}

	// generation 变化（页面 realm 重建）会调用它：注入的 JS 已经随旧 realm 消失。
	app.resetInjectedHookScripts()
	if app.hookScriptInjected("trace.js") || app.hookScriptInjected("other.js") {
		t.Fatal("a rebuilt realm must clear every injected marker")
	}
}

// hook.list 必须带上「上次注入的结局」与「文件是否又改过」：注入完只回一个
// 「已注入」标签，用户既不知道脚本跑没跑成、也不知道返回了什么、更不知道改完
// 文件要不要重注 —— 这三件事都是壳侧的事实，只能由这里给。
func TestHookListCarriesTheLastRunAndStaleness(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	dataDir := t.TempDir()
	// userBaseDir 读环境变量；setupIPC 会用它的结果覆盖 dataBase，所以两者必须指到
	// 同一个目录，否则 hookStore 读的是真实用户目录，测试看到的是空列表。
	t.Setenv("WXTAP_DATA_DIR", dataDir)
	app.dataBase = dataDir
	app.setupIPC()

	scriptPath := filepath.Join(dataDir, "hook_scripts", "trace.js")
	if err := os.MkdirAll(filepath.Dir(scriptPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scriptPath, []byte("console.log('hi')"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatal(err)
	}

	// 先记一次成功的注入，时间钉在文件修改之后。
	app.markHookScriptInjected("trace.js")
	app.recordHookScriptRun("trace.js", true, "wx.setStorageSync 已替换", 12*time.Millisecond)
	run, ok := app.hookScriptRun("trace.js")
	if !ok {
		t.Fatal("the run registry must keep the attempt")
	}
	app.hookRunsMu.Lock()
	run.At = info.ModTime().Unix() + 60
	app.hookRuns["trace.js"] = run
	app.hookRunsMu.Unlock()

	result, err := app.router.Call(context.Background(), "hook.list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("hook.list: %v", err)
	}
	payload := result.(map[string]any)["scripts"].([]map[string]any)
	if len(payload) != 1 {
		t.Fatalf("scripts = %#v", payload)
	}
	script := payload[0]
	if script["filename"] != "trace.js" || script["injected"] != true {
		t.Fatalf("script entry = %#v", script)
	}
	if script["mtime"] != info.ModTime().Unix() {
		t.Fatalf("mtime = %v, want %d", script["mtime"], info.ModTime().Unix())
	}
	if script["stale"] == true {
		t.Fatal("a run made after the last write must not be reported as stale")
	}
	lastRun, ok := script["lastRun"].(map[string]any)
	if !ok {
		t.Fatalf("lastRun missing: %#v", script)
	}
	if lastRun["ok"] != true || lastRun["summary"] != "wx.setStorageSync 已替换" || lastRun["durationMs"] != int64(12) {
		t.Fatalf("lastRun = %#v", lastRun)
	}
}

// 文件在注入之后又被改过时要报 stale，否则用户改完脚本点一次注入、看到上次的结果，
// 会以为改动生效了。
func TestHookListMarksScriptsChangedSinceTheLastRun(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	dataDir := t.TempDir()
	t.Setenv("WXTAP_DATA_DIR", dataDir)
	app.dataBase = dataDir
	app.setupIPC()

	scriptPath := filepath.Join(dataDir, "hook_scripts", "trace.js")
	if err := os.MkdirAll(filepath.Dir(scriptPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scriptPath, []byte("// v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	app.markHookScriptInjected("trace.js")
	app.recordHookScriptRun("trace.js", true, "无返回值", 0)
	// 注入之后再改文件。
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	app.hookRunsMu.Lock()
	run := app.hookRuns["trace.js"]
	run.At = info.ModTime().Unix() - 60
	app.hookRuns["trace.js"] = run
	app.hookRunsMu.Unlock()

	result, err := app.router.Call(context.Background(), "hook.list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("hook.list: %v", err)
	}
	script := result.(map[string]any)["scripts"].([]map[string]any)[0]
	if script["stale"] != true {
		t.Fatalf("a script edited after its last run must be stale: %#v", script)
	}
}

// 失败的注入也要留档：用户改脚本时最需要知道的是「上次为什么没生效」。
func TestHookRunRegistryKeepsFailuresAcrossRealmRebuilds(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()

	app.recordHookScriptRun("trace.js", false, "ReferenceError: wx is not defined", 3*time.Millisecond)
	app.markHookScriptInjected("trace.js")
	app.resetInjectedHookScripts()

	if app.hookScriptInjected("trace.js") {
		t.Fatal("realm rebuild must clear the injected marker")
	}
	run, ok := app.hookScriptRun("trace.js")
	if !ok || run.OK {
		t.Fatalf("the failure must survive the realm rebuild: %+v ok=%v", run, ok)
	}
	if !strings.Contains(run.Summary, "ReferenceError") {
		t.Fatalf("failure summary lost: %+v", run)
	}
}

// 耗时未知记成负数：文件读不出来时脚本根本没跑过，记 0 会让界面显示「0ms」，
// 看起来像跑得飞快。
func TestHookRunRegistryMarksAnUnknownDuration(t *testing.T) {
	app := NewApp()
	app.recordHookScriptRun("trace.js", false, "script not found", -1)
	run, ok := app.hookScriptRun("trace.js")
	if !ok {
		t.Fatal("the attempt must be recorded")
	}
	if run.DurationMs != -1 {
		t.Fatalf("DurationMs = %d, want the -1 sentinel", run.DurationMs)
	}
}

// 用户放进去的脚本会被应用更新覆盖。
func TestSettingsHookScriptsPointsAtTheWritableDirectory(t *testing.T) {
	app := NewApp()
	app.ctx = context.Background()
	dataDir := t.TempDir()
	// userBaseDir 读环境变量/可执行文件目录，生产里 app.dataBase 就是它；测试里
	// 把两者指到同一个临时目录，断言才有意义。
	t.Setenv("WXTAP_DATA_DIR", dataDir)
	app.dataBase = dataDir
	app.setupIPC()

	result, err := app.router.Call(context.Background(), "settings.getPaths", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("settings.getPaths: %v", err)
	}
	payload, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("payload: %#v", result)
	}
	hookScripts, _ := payload["hook_scripts"].(string)
	if hookScripts != filepath.Join(dataDir, "hook_scripts") {
		t.Fatalf("hook_scripts = %q, want the data dir under %q", hookScripts, dataDir)
	}

	// 与 HookStore 实际使用的目录一致：脚本放进这个目录才能被 hook.list 列出来。
	store := app.hookStore()
	if store == nil {
		t.Fatal("hook store unavailable")
	}
	if got := store.ScriptsDir(); got != hookScripts {
		t.Fatalf("HookStore writes %q but the UI opens %q", got, hookScripts)
	}
}
