package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/api"
	"github.com/langbyyi/wxtap/desktop/internal/traffic"
)

// TestRealAttachCaptureLifecycle 是捕获生命周期的实机门禁：连接小程序本身不记录
// 任何东西，第一次「开启捕获」从那一刻开始记，「停止捕获」之后既不再记、也不再补投。
//
// 它和 TestRealAttachAndDeepLink 一样是 opt-in（WXTAP_REAL_ATTACH=1）——往使用者已
// 登录的微信里附加调试器不是测试套件能自己决定的事；全部可写路径都指向临时目录
// （WXTAP_DATA_DIR / WXTAP_TRAFFIC_DB），真实数据目录与真实配置不受影响。
//
// 离线用例（capture_lifecycle_contract_test.go）证明的是 shell 发给 Core 的调用序列；
// 这一条证明它在真实 WMPF 宿主上确实成立：页内钩子的安装时机、真实 rid 的产生，以及
// 「没开启捕获时页面一条都不记」。
func TestRealAttachCaptureLifecycle(t *testing.T) {
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

	dir := t.TempDir()
	t.Setenv("WXTAP_DATA_DIR", dir)
	t.Setenv("WXTAP_TRAFFIC_DB", filepath.Join(dir, "traffic.db"))
	t.Setenv("WXTAP_CORE_CMD", nodePath)
	t.Setenv("WXTAP_CORE_SCRIPT", script)
	t.Setenv("WXTAP_CORE_DIR", filepath.Dir(filepath.Dir(script)))

	app := NewApp()
	app.ctx = context.Background()
	app.dataBase = dir
	// startup() 是 Wails 专属路径，这里手工镜像它的存储接线：没有 repository 的
	// drain 路径无处入库，记录也就到不了 poll 缓冲。
	repo, err := traffic.Open(resolveDBPath(), resolveMigrationsDir(exeDir()))
	if err != nil {
		t.Fatalf("open traffic db: %v", err)
	}
	app.repo = repo
	app.trafficAPI = api.NewTrafficAPI(traffic.NewService(repo))
	app.setupIPC()
	defer func() {
		t.Logf("engine.stop -> %s", app.Call("engine.stop", "{}"))
		// 生产关停而不只是 engine.stop：停止引擎只解除 Frida 附加，Core 进程仍握着
		// 9421/31415，要等 stdio 管道关闭；连着跑两次会撞端口。
		app.shutdown(context.Background())
		t.Logf("core 已关停，端口已释放")
	}()

	// 引擎附加的是 WMPF 宿主（WeChatAppEx.exe），所以「微信里已打开一个小程序」是
	// 调用方的前置条件：它不满足时应当读成指引，而不是一条红色用例（微信 4.x 是自绘
	// UI，「打开一个小程序」这一步只能人工完成）。
	started := app.Call("engine.start", `{"cdp_port":62000}`)
	t.Logf("engine.start -> %s", started)
	if strings.Contains(started, "WeChatAppEx") {
		t.Skipf("前置未满足（先在微信里打开一个小程序，再重跑）：%s", started)
	}
	if strings.Contains(started, `"error"`) {
		t.Fatalf("engine.start 失败: %s", started)
	}

	// 附加成功不等于小程序可注入：9421 上要有一个真的拨进来的小程序页面，否则页内
	// 钩子无处可装（求值会回 no miniapp connected）。等一会儿再判，给调用方留出
	// 刚打开小程序就重跑的余地；WXTAP_REAL_WAIT 可延长。
	wait := 20 * time.Second
	if seconds, err := strconv.Atoi(os.Getenv("WXTAP_REAL_WAIT")); err == nil && seconds > 0 {
		wait = time.Duration(seconds) * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		status := callJSON2(t, app, "engine.status", nil)
		if miniapp, _ := status["miniapp"].(bool); miniapp {
			t.Logf("小程序已连接: %v", status["appInfo"])
			break
		}
		if time.Now().After(deadline) {
			t.Skipf("等待 %v 仍没有小程序连上 9421：请在微信里打开一个小程序后重跑本用例（WXTAP_REAL_WAIT 可延长）", wait)
		}
		time.Sleep(500 * time.Millisecond)
	}

	client, err := app.currentEngine()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}

	// 驱动一次真实的 wx.* 调用：getSystemInfoSync 是只读 API（不改变小程序状态），
	// 又在 wxapi 钩子的包裹名单里（wx.device），所以开启捕获后必然产生记录。
	const drivenAPI = "getSystemInfoSync"
	drive := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		value, err := client.Evaluate(ctx, "typeof wx", 8000)
		if err != nil {
			t.Fatalf("页面求值失败（realm 不可用？）: %v", err)
		}
		if text, _ := value.(string); text != "object" {
			t.Skipf("当前页面没有 wx 对象（typeof wx = %v）：请确认小程序页面已加载完", value)
		}
		if _, err := client.Evaluate(ctx,
			"(function(){try{wx.getSystemInfoSync();return 'ok'}catch(e){return 'err:'+e.message}})()", 8000); err != nil {
			t.Fatalf("驱动 wx.%s 失败: %v", drivenAPI, err)
		}
	}
	drove := func(records []map[string]any) bool {
		for _, record := range records {
			if record["name"] == drivenAPI {
				return true
			}
		}
		return false
	}

	// 1. 连接不采集：驱动调用后等过两个 drain 周期（tick 500ms），缓冲必须一直是空的。
	drive()
	time.Sleep(1500 * time.Millisecond)
	if stats := app.Call("wxapi.stats", `{}`); !strings.Contains(stats, `"running":false`) || !strings.Contains(stats, `"pending":0`) {
		t.Fatalf("连接后不该采集: %s", stats)
	}
	if records := wxapiPoll(t, app, `{}`); len(records) != 0 {
		t.Fatalf("连接后不该有任何记录: %#v", records)
	}

	// 2. 开启捕获之后才记：驱动一次调用，它必须带着真实 rid 进缓冲。
	if envelope := app.Call("wxapi.start", `{}`); strings.Contains(envelope, `"error"`) {
		t.Fatalf("wxapi.start: %s", envelope)
	}
	drive()
	waitFor(t, 20*time.Second, "开启捕获后取到驱动的调用", func() bool {
		return drove(wxapiPoll(t, app, `{"limit":200}`))
	})
	for _, record := range wxapiPoll(t, app, `{"limit":200}`) {
		if record["name"] != drivenAPI {
			continue
		}
		rid, _ := record["rid"].(string)
		t.Logf("捕获到 wx.%s（rid=%s, status=%v）", drivenAPI, rid, record["status"])
		// rid = <type>-<appId>-<ts>-<seq>，就是 traffic_records.id 主键。
		if fields := strings.Split(rid, "-"); len(fields) < 4 || fields[0] != "wx.device" {
			t.Fatalf("rid 形状不对（应为 <type>-<appId>-<ts>-<seq>）: %q", rid)
		}
		break
	}

	// 3. 停止之后既不再记、也不补投：清完之后再驱动调用，不得有任何记录回来。
	if envelope := app.Call("wxapi.stop", `{}`); strings.Contains(envelope, `"error"`) {
		t.Fatalf("wxapi.stop: %s", envelope)
	}
	if stats := app.Call("wxapi.stats", `{}`); !strings.Contains(stats, `"running":false`) || !strings.Contains(stats, `"pending":0`) {
		t.Fatalf("停止后必须既不在跑也没有待投递记录: %s", stats)
	}
	if records := wxapiPoll(t, app, `{}`); len(records) != 0 {
		t.Fatalf("停止后 poll 必须为空: %#v", records)
	}
	drive()
	drive()
	time.Sleep(1500 * time.Millisecond)
	if stats := app.Call("wxapi.stats", `{}`); !strings.Contains(stats, `"pending":0`) {
		t.Fatalf("停止后仍在采集: %s", stats)
	}
	if records := wxapiPoll(t, app, `{}`); len(records) != 0 {
		t.Fatalf("停止后的调用不得再产生记录: %#v", records)
	}
}
