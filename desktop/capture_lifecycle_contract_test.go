package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 捕获生命周期契约：连接只连接，「开启捕获」才是记录的开始；「停止捕获」之后
// 既不记也不展示。
//
// 这些用例跑在真实的 Core 子进程链路上（startFakeCoreApp 用测试二进制冒充 Core），
// 断言的是 shell 实际发给 Core 的调用序列而不是实现细节：假 Core 把每条请求追加进
// FAKE_CORE_CALL_LOG，用例读它；realm 重建用 FAKE_CORE_GENERATION_FILE 驱动。

// coreCall is one request the shell sent to Core.
type coreCall struct {
	Method string
	Params string
}

// installsHook reports whether this call installed the named page-side hook.
func (c coreCall) installsHook(name string) bool {
	return c.Method == "hook.install" && strings.Contains(c.Params, `"name":"`+name+`"`)
}

// mentions reports whether this call addressed the named hook.
func (c coreCall) mentions(name string) bool {
	return strings.Contains(c.Params, `"name":"`+name+`"`)
}

func readCoreCalls(t *testing.T, path string) []coreCall {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read core call log %s: %v", path, err)
	}
	var calls []coreCall
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		method, params, _ := strings.Cut(line, "\t")
		calls = append(calls, coreCall{Method: method, Params: params})
	}
	return calls
}

func countHookInstalls(calls []coreCall, name string) int {
	count := 0
	for _, call := range calls {
		if call.installsHook(name) {
			count++
		}
	}
	return count
}

// startCaptureApp boots the shell against the fake Core with the request log and
// the generation knob wired, starts the engine, and returns the log path plus the
// generation file the test can rewrite to simulate a page reload.
func startCaptureApp(t *testing.T) (app *App, logPath, generationPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "core-calls.log")
	generationPath = filepath.Join(dir, "generation")
	t.Setenv("FAKE_CORE_CALL_LOG", logPath)
	t.Setenv("FAKE_CORE_GENERATION_FILE", generationPath)
	app = startFakeCoreApp(t)
	if err := app.EngineStart(62000); err != nil {
		t.Fatalf("engine.start: %v", err)
	}
	return app, logPath, generationPath
}

// writeGeneration simulates the page realm being rebuilt.
func writeGeneration(t *testing.T, path string, value int64) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strconv.FormatInt(value, 10)), 0o600); err != nil {
		t.Fatalf("write generation: %v", err)
	}
}

// waitForConnectInjection waits until the status poller has run its one
// connect-time injection. console / navigator are the hooks that are installed
// unconditionally, so their install is the observable proof it happened.
func waitForConnectInjection(t *testing.T, logPath string) {
	t.Helper()
	waitFor(t, 15*time.Second, "连接后的常开钩子安装", func() bool {
		calls := readCoreCalls(t, logPath)
		return countHookInstalls(calls, "console") > 0 && countHookInstalls(calls, "navigator") > 0
	})
}

// 连接不得开始采集。装钩子意味着页内从那一刻起就在攒记录，而 shell 侧的投递要等用户
// 点「开启捕获」—— 第一次点击于是把连接以来的积压当新流量一次性补投，这正是使用者
// 看到的「一点下去，历史的流量信息也一起冒出来」。console / navigator 是常开的，
// 不受这条影响。
func TestConnectInstallsOnlyTheAlwaysOnHooks(t *testing.T) {
	app, logPath, _ := startCaptureApp(t)
	defer app.shutdown(context.Background())

	waitForConnectInjection(t, logPath)

	for _, call := range readCoreCalls(t, logPath) {
		for _, hook := range []string{"wxapi", "cloud"} {
			if call.installsHook(hook) {
				t.Fatalf("连接不是捕获决定：%s 钩子在连接时就被装了（%s）", hook, call.Params)
			}
			if call.Method == "hook.drain" && call.mentions(hook) {
				t.Fatalf("连接后不该有人在 drain %s 流（%s）", hook, call.Params)
			}
		}
	}
}

// 开启捕获才装钩子，且游标必须与页内 seq 空间同起点：install 注入的钩子从 1 重新
// 计数，沿用旧 ack 会让之后的每条记录都被静默跳过（drain 的 entry.seq <= afterSeq
// 全过滤）。所以「开始记录」的第一个可观测事实是 hook.drain 带 afterSeq:0。
func TestCaptureStartInstallsTheAuditHookAndDrainsFromZero(t *testing.T) {
	app, logPath, _ := startCaptureApp(t)
	defer app.shutdown(context.Background())

	waitForConnectInjection(t, logPath)
	if count := countHookInstalls(readCoreCalls(t, logPath), "wxapi"); count != 0 {
		t.Fatalf("点「开启捕获」之前不该有 wxapi 钩子：装了 %d 次", count)
	}

	if envelope := app.Call("wxapi.start", `{}`); strings.Contains(envelope, `"error"`) {
		t.Fatalf("wxapi.start: %s", envelope)
	}
	if count := countHookInstalls(readCoreCalls(t, logPath), "wxapi"); count != 1 {
		t.Fatalf("开启捕获应恰好装一次 wxapi 钩子：%d 次", count)
	}

	waitFor(t, 10*time.Second, "开启捕获后从 0 开始的 drain", func() bool {
		for _, call := range readCoreCalls(t, logPath) {
			if call.Method == "hook.drain" && call.mentions("wxapi") && strings.Contains(call.Params, `"afterSeq":0`) {
				return true
			}
		}
		return false
	})
	waitFor(t, 10*time.Second, "三条记录进入 poll 缓冲", func() bool {
		return wxapiPending(t, app) == 3
	})

	// 「从此刻起记录」在起点上强制：页内可能还留着上一段的残留（引擎停止时卸载钩子
	// 的 CDP 到不了页面），所以本次开始的清缓冲必须先于第一次 drain。
	clear, drain := -1, -1
	for index, call := range readCoreCalls(t, logPath) {
		if clear < 0 && call.Method == "hook.clear" && call.mentions("wxapi") {
			clear = index
		}
		if drain < 0 && call.Method == "hook.drain" && call.mentions("wxapi") {
			drain = index
		}
	}
	if clear < 0 || drain < 0 {
		t.Fatalf("开启捕获必须清页内残留并开始 drain: hook.clear@%d hook.drain@%d", clear, drain)
	}
	if clear > drain {
		t.Fatal("开启捕获必须先清页内缓冲再 drain：否则上一段的残留会被当成新记录投出来")
	}
}

// 停止 = 从此不再记、也不再展示：页内缓冲里最后半个 tick 的记录留着的话，下次开启会
// 被当作新流量补投（「我明明停过，怎么又冒出来几条」）；shell 侧 pending 留着的话，
// 下次开启会把上一段捕获的记录重新灌进实时列表。顺序与 wxapi.clear 的 R10b 一致：
// 先清页面（一次 CDP 往返），再卸载钩子 —— 反过来的窗口里 tick 会把已清的记录重新
// drain 回 pending。
func TestCaptureStopClearsThePageBufferBeforeUninstalling(t *testing.T) {
	app, logPath, _ := startCaptureApp(t)
	defer app.shutdown(context.Background())

	waitForConnectInjection(t, logPath)
	if envelope := app.Call("wxapi.start", `{}`); strings.Contains(envelope, `"error"`) {
		t.Fatalf("wxapi.start: %s", envelope)
	}
	waitFor(t, 10*time.Second, "三条记录进入 poll 缓冲", func() bool {
		return wxapiPending(t, app) == 3
	})

	if envelope := app.Call("wxapi.stop", `{}`); strings.Contains(envelope, `"error"`) {
		t.Fatalf("wxapi.stop: %s", envelope)
	}
	stats := app.Call("wxapi.stats", `{}`)
	if !strings.Contains(stats, `"running":false`) || !strings.Contains(stats, `"pending":0`) {
		t.Fatalf("停止后必须既不在跑也没有待投递记录: %s", stats)
	}
	if records := wxapiPoll(t, app, `{}`); len(records) != 0 {
		t.Fatalf("停止后 poll 必须为空: %#v", records)
	}

	// 停止自己的清理：先清页面缓冲（一次 CDP 往返），再卸载钩子 —— 反过来的窗口里
	// tick 会把已清的记录重新 drain 回 pending。所以这一清必须晚于最后一次 drain。
	lastDrain, lastClear, uninstall := -1, -1, -1
	for index, call := range readCoreCalls(t, logPath) {
		switch {
		case call.Method == "hook.drain" && call.mentions("wxapi"):
			lastDrain = index
		case call.Method == "hook.clear" && call.mentions("wxapi"):
			lastClear = index
		case call.Method == "hook.uninstall" && call.mentions("wxapi"):
			uninstall = index
		}
	}
	if uninstall < 0 || lastClear < 0 {
		t.Fatalf("停止必须清页面缓冲并卸载钩子: hook.clear@%d hook.uninstall@%d", lastClear, uninstall)
	}
	if lastClear > uninstall {
		t.Fatal("停止必须先 hook.clear 再 hook.uninstall：反过来的窗口内，tick 会把已清记录重新 drain 回 pending")
	}
	if lastDrain > lastClear {
		t.Fatal("停止的清理必须晚于最后一次 drain：否则那次窗口里的记录留在了页内，下次开启会被补投")
	}
}

// realm 重建（页面重载 / 切换小程序）会把页内缓冲与 seq 空间一起清掉，所以**正在捕获**
// 的钩子必须在连接时重装并重置游标，否则重建后每条新记录都被静默跳过。反过来，
// 没在捕获时同样的重载不得装审计钩子 —— 连接的副作用里没有「开始采集」这一项。
func TestRealmRebuildRehooksOnlyARunningCapture(t *testing.T) {
	app, logPath, generationPath := startCaptureApp(t)
	defer app.shutdown(context.Background())

	waitForConnectInjection(t, logPath)
	if envelope := app.Call("wxapi.start", `{}`); strings.Contains(envelope, `"error"`) {
		t.Fatalf("wxapi.start: %s", envelope)
	}
	waitFor(t, 10*time.Second, "三条记录进入 poll 缓冲", func() bool {
		return wxapiPending(t, app) == 3
	})

	writeGeneration(t, generationPath, 2)
	waitFor(t, 15*time.Second, "realm 重建后重装 wxapi 钩子", func() bool {
		return countHookInstalls(readCoreCalls(t, logPath), "wxapi") >= 2
	})
	if stats := app.Call("wxapi.stats", `{}`); !strings.Contains(stats, `"running":true`) {
		t.Fatalf("realm 重建不该结束捕获: %s", stats)
	}

	if envelope := app.Call("wxapi.stop", `{}`); strings.Contains(envelope, `"error"`) {
		t.Fatalf("wxapi.stop: %s", envelope)
	}
	installsBefore := countHookInstalls(readCoreCalls(t, logPath), "wxapi")
	consoleBefore := countHookInstalls(readCoreCalls(t, logPath), "console")

	writeGeneration(t, generationPath, 3)
	// 常开钩子的重装证明轮询器确实看到了新的 generation；审计钩子的计数必须不变。
	waitFor(t, 15*time.Second, "未捕获时的 realm 重建", func() bool {
		return countHookInstalls(readCoreCalls(t, logPath), "console") > consoleBefore
	})
	if installsAfter := countHookInstalls(readCoreCalls(t, logPath), "wxapi"); installsAfter != installsBefore {
		t.Fatalf("未捕获时的 realm 重建不得装 wxapi 钩子：%d → %d 次", installsBefore, installsAfter)
	}
}

// 引擎停止就是捕获结束：前端在 engine.stop 时把「捕获中」置回未捕获，后端若还说自己
// 跑着，下次连接时 autoInjectHooks 就会以为用户还在捕获 —— 没人点「开启捕获」，记录
// 却开始进库。待投递记录同样不留：它们已经入库，历史记录里查得到。
func TestEngineStopEndsTheCapture(t *testing.T) {
	app, _, _ := startCaptureApp(t)
	defer app.shutdown(context.Background())

	if envelope := app.Call("wxapi.start", `{}`); strings.Contains(envelope, `"error"`) {
		t.Fatalf("wxapi.start: %s", envelope)
	}
	waitFor(t, 10*time.Second, "三条记录进入 poll 缓冲", func() bool {
		return wxapiPending(t, app) == 3
	})

	if envelope := app.Call("engine.stop", `{}`); strings.Contains(envelope, `"error"`) {
		t.Fatalf("engine.stop: %s", envelope)
	}
	if stats := app.Call("wxapi.stats", `{}`); !strings.Contains(stats, `"running":false`) {
		t.Fatalf("引擎停止后捕获必须结束: %s", stats)
	}
	if records := wxapiPoll(t, app, `{}`); len(records) != 0 {
		t.Fatalf("引擎停止后不得留下待投递记录: %#v", records)
	}
}
