package main

import (
	"embed"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"github.com/langbyyi/wxtap/desktop/internal/update"
)

//go:embed all:frontend/dist
var assets embed.FS

// relaunchWaitFlag marks a process that exists only to restart the app after
// the instance it replaces has exited. Windows will not let a running
// executable be replaced, so the swap needs a second process to perform it.
const relaunchWaitFlag = "-relaunch-wait"

// relaunchWaitTimeout bounds that wait, so a stale process id can never wedge
// the launch.
const relaunchWaitTimeout = 30 * time.Second

func main() {
	mcpMode := flag.Bool("mcp", false, "run as an MCP stdio server instead of the desktop app")
	mcpTools := flag.String("tools", "lean", "MCP tools/list profile: lean (consolidated catalog) or all (every tool incl. compat names)")
	waitPID := flag.Int("relaunch-wait", 0, "wait for this process id to exit before starting (internal)")
	flag.Parse()

	if *mcpMode {
		// A self-update mid-session would replace the binary the stdio client
		// already connected to.
		os.Exit(runMCPStdio(*mcpTools))
	}

	if *waitPID > 0 {
		waitForProcess(*waitPID)
	}
	applyStagedUpdate()

	app := NewApp()

	err := wails.Run(&options.App{
		Title:     "WxTap — 微信小程序安全调试",
		Width:     1280,
		Height:    832,
		MinWidth:  1024,
		MinHeight: 640,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		OnStartup:  app.startup,
		OnShutdown: app.shutdown,
		Bind: []interface{}{
			app,
		},
		Windows: &windows.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
			WebviewUserDataPath:  webviewUserDataDir(),
			// Wails' defaults are English, and this dialog is the first thing a
			// Windows user without the runtime sees — in an otherwise Chinese-only
			// app. It also depends on reaching go.microsoft.com, which an offline
			// or proxied machine has to be told about before it waits on a
			// download that will not happen.
			Messages: &windows.Messages{
				InstallationRequired: "缺少 WebView2 运行时，界面无法显示。按「确定」将联网（go.microsoft.com）静默下载并安装，请稍候；离线环境请先手动安装 WebView2 Runtime 再启动本程序。",
				UpdateRequired:       "WebView2 运行时需要更新。按「确定」将联网（go.microsoft.com）静默下载并安装，请稍候。",
				MissingRequirements:  "WxTap 缺少运行组件",
				Webview2NotInstalled: "未安装 WebView2 运行时",
				Error:                "错误",
				FailedToInstall:      "WebView2 运行时安装失败。离线环境请手动安装 WebView2 Runtime 后重试。",
				DownloadPage:         "本程序需要 WebView2 运行时。按「确定」打开下载页。最低版本要求：",
				PressOKToInstall:     "按「确定」开始安装。",
				ContactAdmin:         "本程序需要 WebView2 运行时。请联系系统管理员安装后重试。",
			},
		},
	})
	if err != nil {
		// A GUI build has no console, so printing alone is not enough: the user
		// would just see the app not appear. This is the path a non-writable
		// install takes, because the WebView2 profile directory lives under it.
		println("Error:", err.Error())
		showFatalMessage("WxTap 启动失败", err.Error())
	}
}

// webviewUserDataDir follows the data directory, not the executable: on macOS
// the executable lives inside the .app bundle, which is not a writable place.
// Windows is where this path is actually used (Wails' WebviewUserDataPath).
func webviewUserDataDir() string {
	return filepath.Join(dataDir(), "webview")
}

// waitForProcess blocks until pid exits, then lets the launch continue.
func waitForProcess(pid int) {
	process, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	exited := make(chan struct{})
	go func() {
		_, _ = process.Wait()
		close(exited)
	}()
	select {
	case <-exited:
	case <-time.After(relaunchWaitTimeout):
	}
}

// applyStagedUpdate swaps a downloaded version into the install directory and
// restarts into it. It runs before Wails starts, and the restart is part of the
// same step rather than optional, because the process image is the previous
// build: continuing would run the old shell against a new Core.
//
// A failure here must still leave a launchable app, so every outcome is
// recorded for the log rather than raised.
func applyStagedUpdate() {
	executable, err := os.Executable()
	if err != nil {
		// The install directory is derived from this rather than from exeDir(),
		// whose "." fallback would scatter an install into the working
		// directory. Leaving the staged version for a launch that can locate
		// itself is the only safe answer.
		println("update skipped: cannot locate the running executable:", err.Error())
		return
	}
	// ApplyPending derives the install root from this directory plus the
	// platform's layout: the executable's own directory on Windows, the
	// enclosing .app bundle on macOS.
	install := filepath.Dir(executable)
	root := dataDir()
	record := update.ApplyPending(install, root)
	if record.Version == "" {
		// Nothing is staged, so any leftovers belong to an update that has
		// already taken effect and is running right now.
		update.CleanupAfterLaunch(install, root)
		return
	}
	if !record.Applied {
		println("update apply failed:", record.Error)
		return
	}
	relaunch(executable)
	os.Exit(0)
}

// relaunch starts the freshly installed build. A failure is fatal to this
// launch: the install on disk no longer matches the running process image.
func relaunch(executable string) {
	command := exec.Command(executable)
	configureGUIProcess(command)
	if err := command.Start(); err != nil {
		println("update applied but relaunch failed:", err.Error())
		os.Exit(1)
	}
}
