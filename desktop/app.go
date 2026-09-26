// Wails desktop shell: binds the engine client and the traffic API to the
// Vue frontend. All lifecycle glue lives here; logic lives in internal/*.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/langbyyi/wxtap/desktop/internal/api"
	"github.com/langbyyi/wxtap/desktop/internal/api/ipc"
	"github.com/langbyyi/wxtap/desktop/internal/cloudapi"
	"github.com/langbyyi/wxtap/desktop/internal/engine"
	"github.com/langbyyi/wxtap/desktop/internal/mcp"
	"github.com/langbyyi/wxtap/desktop/internal/rpc"
	"github.com/langbyyi/wxtap/desktop/internal/traffic"
)

// App is the Wails-bound application.
type App struct {
	ctx           context.Context
	wailsReady    bool
	pollerCancel  context.CancelFunc
	consoleCancel context.CancelFunc
	mu            sync.Mutex
	// storageUnavailableWarned 记录降级模式（repo == nil）下那条「存储不可用」
	// warn 是否已经发过。ingestHookBatch 每个 drain tick 被两个 feeder 各调一次，
	// 逐批 warn 会刷爆日志面板，所以进程生命周期内只提示一次；置位后不重置，
	// 同一次运行里存储始终是不可用状态。由 a.mu 保护（emitLog 会经 wailsContext
	// 再拿 a.mu，因此必须在锁外发事件）。
	storageUnavailableWarned bool
	bg                       sync.WaitGroup
	engine                   *engine.Client
	repo                     *traffic.Repository
	trafficAPI               *api.TrafficAPI
	tasks                    *ipc.TaskTracker
	consoleLog               *consoleLog
	runtimeLog               *runtimeLog

	router        *ipc.Router
	wxapiFeeder   *ipc.HookFeeder
	cloudFeeder   *ipc.HookFeeder
	consoleFeeder *ipc.HookFeeder
	navigatorCtl  *ipc.NavigatorCtl
	configStore   *ipc.ConfigStore
	hookStoreOnce *ipc.HookStore
	audit         *ipc.AuditService
	dataBase      string
	logger        *operationLog
	cloudAPI      *cloudapi.Server
	mcpSSE        *mcp.SSEServer

	// injectedScripts remembers which user hook scripts were injected into the
	// current page realm. The registry is dropped whenever the realm is
	// rebuilt (generation change): the injected JS died with it, and a marker
	// kept across that boundary would be a lie.
	injectedScriptsMu sync.Mutex
	injectedScripts   map[string]struct{}

	// hookRuns remembers how each user script's last injection ended (verdict,
	// value summary, duration). Unlike injectedScripts it survives a realm
	// rebuild: 「已注入」随 realm 作废，但「上次注入发生了什么」仍然是事实，
	// 用户正是靠它判断脚本跑没跑、返回了什么 —— 没有它，注入完只看得到一个小标签。
	hookRunsMu sync.Mutex
	hookRuns   map[string]hookRun

	// updateRunning serialises the download. The button disables itself, but the
	// IPC surface is also reachable from MCP and from a future caller, and two
	// concurrent stages would rebuild each other's staging tree.
	updateRunningMu sync.Mutex
	updateRunning   bool
}

// NewApp constructs the application with default storage.
func NewApp() *App {
	return &App{consoleLog: &consoleLog{}, runtimeLog: &runtimeLog{}}
}

// goBackground runs a finite async task (scan, export, vConsole toggle) in a
// tracked goroutine. Handlers spawn the task before returning, so a caller
// that waits after the RPC observes every write the task performs — tests
// rely on that to keep TempDir cleanup from racing report/output writes.
// Long-lived loops (status poller, auto-visit) stay untracked: they end with
// the app context, not with a single request.
func (a *App) goBackground(fn func()) {
	a.bg.Add(1)
	go func() {
		defer a.bg.Done()
		fn()
	}()
}

// waitBackground blocks until every tracked async task has finished.
func (a *App) waitBackground() {
	a.bg.Wait()
}

// startup is called by Wails when the frontend is ready.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.wailsReady = true

	// Diagnostics first: Core stderr and lifecycle events must survive the
	// process so a failed attach can be investigated afterwards.
	//
	// Both failures below used to be println-only, which a -H windowsgui binary
	// and a Finder-launched .app both discard: a read-only install directory
	// (Program Files, a mounted dmg, a folder restored from backup) then looked
	// like the app working while nothing was being written at all. recordLog
	// still reaches the in-memory ring and therefore the log panel, even with no
	// logger on disk, so the user is told.
	base, baseErr := userBaseDir()
	if baseErr != nil {
		println("data dir unavailable:", baseErr.Error())
		a.recordLog("error", "数据目录不可用（配置、日志、数据库都无法落盘）："+baseErr.Error())
	} else if logger, logErr := openOperationLog(base); logErr == nil {
		a.logger = logger
	} else {
		println("log init failed:", logErr.Error())
		a.recordLog("error", "日志初始化失败（日志不会落盘，仅本面板可见）："+logErr.Error())
	}
	// 启动行进文件与回放环，但不广播：EventsEmit 需要前端就绪。webview 订阅
	// 后由 log.list 回放补上，所以面板里不会缺这一行。
	a.recordLog("info", "WxTap 启动")
	// The swap happens before Wails starts, so a relaunch reports itself here:
	// this process is the new build, and the record describes the update that
	// put it there.
	logApplyRecord(a.logger, dataDir())

	repo, err := traffic.Open(resolveDBPath(), resolveMigrationsDir(exeDir()))
	if err != nil {
		// Storage is degraded, not fatal: the IPC router and the rest of
		// the surface keep working, only the traffic API reports errors.
		a.recordLog("error", "存储初始化失败: "+err.Error())
		println("storage init failed:", err.Error())
	} else {
		a.repo = repo
		a.trafficAPI = api.NewTrafficAPI(traffic.NewService(repo))
	}

	a.setupIPC()
}

// wailsContext returns the lifecycle context Wails accepts for EventsEmit.
// Only startup() installs it: runtime.EventsEmit fatals on any other context,
// so tests (and any non-Wails caller) must get nil and skip emission.
func (a *App) wailsContext() context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.wailsReady {
		return nil
	}
	return a.ctx
}

// Call dispatches one desktop IPC method and returns the WxTap IPC
// envelope: {"result": ...} on success, {"error": "..."} on failure. Bound
// methods carry no context.Context: the Wails v2 binder treats it as a
// regular input, so the frontend call would mismatch argument counts.
func (a *App) Call(method string, paramsJSON string) string {
	params := json.RawMessage(paramsJSON)
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	result, err := a.router.Call(context.Background(), method, params)
	if err != nil {
		message := err.Error()
		code := "BACKEND_ERROR"
		retryable := ipcErrorRetryable(err)
		if strings.Contains(message, "unsupported backend method") {
			code = "UNSUPPORTED_METHOD"
			retryable = false
		}
		envelope, _ := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": message, "retryable": retryable}})
		return string(envelope)
	}
	if result == nil {
		return `{"result": {"ok": true}}`
	}
	envelope, _ := json.Marshal(map[string]any{"result": result})
	return string(envelope)
}

// ipcErrorRetryable decides whether the IPC envelope tells the UI to offer
// 「重试」 for this failure. A typed Core error answers for itself: Retryable is
// set where the cause is actually known (the Core is gone, the call timed out,
// the connection dropped), and the message it renders as —
// "core error 1002: core call timed out: wxapi.start" — matches none of the
// wording rules below. That is the difference between "the engine died, restart
// it" and a permanent failure, so guessing from the text used to drop exactly
// the errors the retry button exists for. Untyped failures still fall back to
// the wording heuristic; unrecognised text means "not retryable".
func ipcErrorRetryable(err error) bool {
	var coreErr *rpc.CoreError
	if errors.As(err, &coreErr) {
		return coreErr.Retryable
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "timeout") ||
		strings.Contains(message, "timed out") ||
		strings.Contains(message, "deadline exceeded") ||
		strings.Contains(message, "端口") ||
		strings.Contains(message, "not running")
}

// shutdown releases the engine process and the database. In-flight async
// tasks (scan, export, vConsole toggle) are drained first: they write reports
// and outputs, so closing storage under them would race or truncate results.
func (a *App) shutdown(context.Context) {
	// Stop the status poller first: it may spawn tracked work (auto hook
	// injection), and a WaitGroup add must not race shutdown's drain.
	a.mu.Lock()
	cancelPoller := a.pollerCancel
	a.pollerCancel = nil
	cancelConsole := a.consoleCancel
	a.consoleCancel = nil
	sse := a.mcpSSE
	cloudAPI := a.cloudAPI
	a.mu.Unlock()
	if cancelPoller != nil {
		cancelPoller()
	}
	if cancelConsole != nil {
		cancelConsole()
	}
	// Stop the inbound surfaces BEFORE draining tracked work: an in-flight
	// MCP/HTTP request entering a handler during the drain would goBackground
	// into a draining WaitGroup — Add-during-Wait is documented panic
	// territory. cloudapi.Stop waits for its handlers, so after it returns no
	// new goBackground can come from that surface.
	if sse != nil {
		sse.Stop()
	}
	if cloudAPI != nil {
		cloudAPI.Stop()
	}
	// 审计扫描/资产构建是可取消的跟踪任务：先取消再排空，重放战役不会对着
	// 正在关闭的数据库继续打请求。
	if a.audit != nil {
		a.audit.Close()
	}
	a.waitBackground()
	if a.wxapiFeeder != nil {
		a.wxapiFeeder.Stop()
	}
	if a.cloudFeeder != nil {
		a.cloudFeeder.Stop()
	}
	if a.consoleFeeder != nil {
		a.consoleFeeder.Stop()
	}
	a.mu.Lock()
	current := a.engine
	a.mu.Unlock()
	if current != nil {
		current.Shutdown()
	}
	if a.repo != nil {
		_ = a.repo.Close()
	}
	if a.logger != nil {
		_ = a.logger.Close()
	}
}

// EngineStatus returns the Core engine status; an offline Core yields a
// typed retryable error rather than taking the app down.
func (a *App) EngineStatus() (engine.EngineStatus, error) {
	client, err := a.currentEngine()
	if err != nil {
		return engine.EngineStatus{}, err
	}
	return client.Status(context.Background())
}

// EngineStart spawns the Core (if needed) and starts the debug engine.
func (a *App) EngineStart(cdpPort int) error {
	client, err := a.currentEngine()
	if err != nil {
		return err
	}
	return client.Start(context.Background(), cdpPort)
}

// EngineStop stops the debug engine (the Core process stays alive).
func (a *App) EngineStop() error {
	client, err := a.currentEngine()
	if err != nil {
		return err
	}
	return client.Stop(context.Background())
}

// TrafficList forwards the frontend's traffic.list call.
func (a *App) TrafficList(params api.TrafficListParams) (traffic.TrafficPage, error) {
	if a.trafficAPI == nil {
		return traffic.TrafficPage{}, fmt.Errorf("存储初始化失败，流量记录不可用")
	}
	return a.trafficAPI.TrafficList(context.Background(), params)
}

// TrafficGetBody forwards the frontend's traffic.getBody call.
func (a *App) TrafficGetBody(params api.TrafficGetBodyParams) ([]byte, error) {
	if a.trafficAPI == nil {
		return nil, fmt.Errorf("存储初始化失败，流量记录不可用")
	}
	return a.trafficAPI.TrafficGetBody(context.Background(), params)
}

// IngestTraffic stores a drained Hook batch and publishes traffic.available.
func (a *App) IngestTraffic(batch []traffic.Record) (int, error) {
	return a.ingestTraffic(context.Background(), batch)
}

// ingestTraffic is the context-carrying variant the drain loops call.
func (a *App) ingestTraffic(ctx context.Context, batch []traffic.Record) (int, error) {
	if a.repo == nil {
		return 0, fmt.Errorf("存储初始化失败，流量记录不可用")
	}
	inserted, err := traffic.Ingest(ctx, a.repo, batch)
	if err != nil {
		return inserted, err
	}
	if inserted > 0 {
		a.emit("traffic:available", map[string]any{"inserted": inserted})
	}
	return inserted, nil
}

func (a *App) currentEngine() (*engine.Client, error) {
	client, created, err := a.ensureEngine()
	if err != nil {
		return nil, err
	}
	if !created {
		return client, nil
	}
	// Pushed every engine logger line to the control view; the Core
	// logs on stderr, so each line becomes a `log` event. The Core has
	// no level protocol — everything lands on stderr — so the level is
	// guessed from the tags its own code prints (coreLogLevel).
	//
	// Registration must happen outside a.mu: OnLog synchronously replays
	// the stderr lines captured before it ran, and its callback (emitLog
	// → wailsContext) takes a.mu again — registering under the lock would
	// self-deadlock the moment a line was captured in the spawn window.
	client.OnLog(func(line string) { a.emitLog(coreLogLevel(line), line) })
	// 控制台记录由 Core 经 CDP 事件推送（页内 hook 包不上小程序的 console），
	// 这里放进 shell 的环形缓冲，Console 页照旧按序号拉取。OnEvent 每次注册
	// 都会新起一个事件消费协程，所以只在刚拉起的进程上注册这一次。
	client.OnEvent(a.handleCoreEvent)
	return client, nil
}

// ensureEngine returns a live Core client, spawning the process on first use
// or after an unexpected exit. The spawn and the a.engine swap happen under
// a.mu; callback registration deliberately happens in currentEngine, outside
// the lock (see there). created tells the caller whether this client was just
// spawned and still needs its callbacks wired.
func (a *App) ensureEngine() (*engine.Client, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.engine != nil {
		dropped := false
		select {
		case <-a.engine.WaitExited():
			dropped = true
		case <-a.engine.Broken():
			dropped = true
		default:
			return a.engine, false, nil
		}
		if dropped {
			// The Broken branch is the zombie: connection torn (a cancelled
			// write hit the wedge guard) while the process lives on, pinned by
			// its listening servers. Kill it — a lingering Core would hold the
			// CDP port and the Frida attach against the replacement.
			victim := a.engine
			a.engine = nil
			go victim.Shutdown()
		}
	}
	cmd, err := coreCommand()
	if err != nil {
		return nil, false, err
	}
	client, err := engine.StartProcess(cmd)
	if err != nil {
		return nil, false, err
	}
	a.engine = client
	return client, true, nil
}

// coreLogLevel guesses a display level for one Core stderr line. The tags
// below are the only signal: [core] lines from the runtime itself, [hook:error]
// from Frida script failures, and Node's own crash/warning formats. Everything
// else stays info — mislabeling a real error as info is worse than noise, but
// so is painting every Core note red.
func coreLogLevel(line string) string {
	trimmed := strings.TrimSpace(line)
	switch {
	case strings.Contains(line, "[hook:error]"),
		strings.Contains(line, "[error]"),
		strings.HasPrefix(trimmed, "Error:"),
		strings.HasPrefix(trimmed, "FATAL"),
		strings.Contains(line, "FATAL ERROR"):
		return "error"
	case strings.Contains(line, "[warn]"),
		strings.Contains(line, "Warning"),
		strings.Contains(line, "DeprecationWarning"):
		return "warn"
	default:
		return "info"
	}
}

// engineStatusForTest reads the live engine status for the opt-in live checks
// (the router's engine.status degrades to an offline payload, which cannot tell
// whether a mini program is attached).
func (a *App) engineStatusForTest(ctx context.Context) (engine.EngineStatus, error) {
	client, err := a.currentEngine()
	if err != nil {
		return engine.EngineStatus{}, err
	}
	return client.Status(ctx)
}

// handleCoreEvent routes one Core-pushed event into shell state. The Core pushes
// console records this way: the miniapp's console object is a non-configurable
// accessor on current WMPF builds, so capture happens at the CDP layer and
// arrives as {event:"console", payload:{...}} lines.
func (a *App) handleCoreEvent(name string, payload []byte) {
	if name != "console" {
		return
	}
	var record map[string]any
	if err := json.Unmarshal(payload, &record); err != nil || len(record) == 0 {
		return
	}
	a.consoleLog.Append([]map[string]any{record}, a.consoleLog.Epoch())
}

// coreCommand assembles the Core spawn. Which Node runs it is decided in
// node_runtime.go, which fails with a user-facing message when none is usable.
func coreCommand() (*exec.Cmd, error) {
	runtimeRoot := exeDir()
	node := resolveNodeRuntime()
	if node.Error != "" {
		return nil, errors.New(node.Error)
	}
	script := resolveCoreScript(runtimeRoot)
	if _, err := os.Stat(script); err != nil {
		return nil, fmt.Errorf("未找到 Core 脚本（%s）：请在 core/ 目录执行 npm install && npm run build，或通过 WXTAP_CORE_SCRIPT 指定路径", script)
	}
	cmd := exec.Command(node.Path, script)
	configureBackgroundProcess(cmd)
	if dir := os.Getenv("WXTAP_CORE_DIR"); dir != "" {
		cmd.Dir = dir
	} else {
		cmd.Dir = filepath.Dir(script)
	}
	return cmd, nil
}

// exeDir is split out for tests.
func exeDir() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "."
}

// resolveCoreScript finds core/dist/cli.js across deployment (WxTap.exe next
// to core/) and development (desktop/build/bin → repo core/) layouts, then
// falls back to the cwd-relative candidates. WXTAP_CORE_SCRIPT always wins.
func resolveCoreScript(exeDir string) string {
	if script := os.Getenv("WXTAP_CORE_SCRIPT"); script != "" {
		return script
	}
	name := filepath.Join("dist", "cli.js")
	candidates := []string{
		filepath.Join(exeDir, "core", name),
		// macOS app bundle: Contents/Resources/core/dist/cli.js.
		filepath.Join(exeDir, "..", "Resources", "core", name),
		filepath.Join(exeDir, "..", "..", "..", "core", name),
		filepath.Join("core", name),
		filepath.Join("..", "core", name),
		name,
	}
	for _, candidate := range candidates {
		if abs, err := filepath.Abs(candidate); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs
			}
		}
	}
	abs, _ := filepath.Abs(candidates[0])
	return abs
}

// resolveDBPath puts the traffic database in the data directory rather than
// beside the executable: on macOS those differ, and the executable's directory
// is inside the .app bundle.
func resolveDBPath() string {
	if path := os.Getenv("WXTAP_TRAFFIC_DB"); path != "" {
		return path
	}
	return filepath.Join(dataDir(), "traffic.db")
}

// resolveMigrationsDir probes the exe-sibling migrations/ (release layout:
// migrations/ ships next to WxTap.exe) before the cwd-relative one (dev
// layout), like resolveCoreScript does. WXTAP_MIGRATIONS_DIR always wins.
func resolveMigrationsDir(exeDir string) string {
	if path := os.Getenv("WXTAP_MIGRATIONS_DIR"); path != "" {
		return path
	}
	candidates := []string{
		filepath.Join(exeDir, "migrations"),
		// macOS app bundle: Contents/Resources/migrations.
		filepath.Join(exeDir, "..", "Resources", "migrations"),
		"migrations",
	}
	for _, candidate := range candidates {
		if abs, err := filepath.Abs(candidate); err == nil {
			if info, err := os.Stat(abs); err == nil && info.IsDir() {
				return abs
			}
		}
	}
	abs, _ := filepath.Abs(candidates[0])
	return abs
}
