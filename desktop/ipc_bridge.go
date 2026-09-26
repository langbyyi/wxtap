// IPC bridge wiring: registers the desktop method surface on the
// ipc.Router and adapts it to the Go domain packages, emitting the event
// names the Vue frontend listens for.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	apiinternal "github.com/langbyyi/wxtap/desktop/internal/api"
	"github.com/langbyyi/wxtap/desktop/internal/api/ipc"
	"github.com/langbyyi/wxtap/desktop/internal/cloud"
	"github.com/langbyyi/wxtap/desktop/internal/cloudapi"
	"github.com/langbyyi/wxtap/desktop/internal/devtools"
	"github.com/langbyyi/wxtap/desktop/internal/engine"
	"github.com/langbyyi/wxtap/desktop/internal/extract"
	"github.com/langbyyi/wxtap/desktop/internal/mcp"
	"github.com/langbyyi/wxtap/desktop/internal/navigator"
	"github.com/langbyyi/wxtap/desktop/internal/traffic"
	"github.com/langbyyi/wxtap/desktop/internal/update"
)

// navigatorAdapter adapts navigator.Navigator to ipc.pageReader.
type navigatorAdapter struct {
	nav *navigator.Navigator
}

// Backend event names. The Vue frontend listens for these literal strings in
// desktop/frontend/src/api/bridge.ts (supportedEvents), so an implementation
// and its contract test share one constant instead of pinning the emit call by
// source text - a rename now fails the constant assertion, not a text match.
const (
	wxapiCaptureEvent = "wxapi_capture"
	wxapiUpdateEvent  = "wxapi_update"
	cloudCaptureEvent = "cloud_capture"
	cloudUpdateEvent  = "cloud_update"
)

func (a navigatorAdapter) FetchConfig(ctx context.Context) (ipc.PageConfig, error) {
	config, err := a.nav.FetchConfig(ctx)
	if err != nil {
		return ipc.PageConfig{}, err
	}
	// The navigator always answers with arrays: a mini program whose window.nav is
	// not ready yet must not surface null page lists to consumers.
	pages := config.Pages
	if pages == nil {
		pages = []string{}
	}
	tabBar := config.TabBar
	if tabBar == nil {
		tabBar = []string{}
	}
	return ipc.PageConfig{Pages: pages, TabBarPages: tabBar,
		AppID: config.AppID, Entry: config.Entry, Name: config.Name}, nil
}

func (a navigatorAdapter) NavigateTo(ctx context.Context, route string) error {
	return a.nav.NavigateTo(ctx, route)
}

func (a navigatorAdapter) RelaunchTo(ctx context.Context, route string) error {
	return a.nav.RelaunchTo(ctx, route)
}

func (a navigatorAdapter) RedirectTo(ctx context.Context, route string) error {
	return a.nav.RedirectTo(ctx, route)
}

func (a navigatorAdapter) SwitchTab(ctx context.Context, route string) error {
	return a.nav.SwitchTab(ctx, route)
}

func (a navigatorAdapter) NavigateBack(ctx context.Context, delta int) error {
	return a.nav.NavigateBack(ctx, delta)
}

func (a navigatorAdapter) RefreshPage(ctx context.Context) (ipc.RefreshResult, error) {
	result, err := a.nav.RefreshPage(ctx)
	return ipc.RefreshResult(result), err
}

func (a navigatorAdapter) GetCurrentRoute(ctx context.Context) (string, error) {
	return a.nav.GetCurrentRoute(ctx)
}

func (a navigatorAdapter) EnableRedirectGuard(ctx context.Context) (ipc.GuardResult, error) {
	result, err := a.nav.EnableRedirectGuard(ctx)
	return ipc.GuardResult(result), err
}

func (a navigatorAdapter) DisableRedirectGuard(ctx context.Context) error {
	return a.nav.DisableRedirectGuard(ctx)
}

func (a navigatorAdapter) GetPageStack(ctx context.Context) ([]ipc.PageEntry, error) {
	stack, err := a.nav.GetPageStack(ctx)
	if err != nil {
		return nil, err
	}
	entries := make([]ipc.PageEntry, 0, len(stack))
	for _, entry := range stack {
		params := entry.Params
		if params == nil {
			params = map[string]string{}
		}
		entries = append(entries, ipc.PageEntry{Route: entry.Route, Params: params})
	}
	return entries, nil
}

func (a navigatorAdapter) GuardOn(ctx context.Context) (bool, error) {
	return a.nav.GuardOn(ctx)
}

func (a navigatorAdapter) GetBlockedRedirects(ctx context.Context) ([]map[string]any, error) {
	return a.nav.GetBlockedRedirects(ctx)
}

func (a navigatorAdapter) Invalidate() {
	a.nav.Invalidate()
}

func (a navigatorAdapter) Pages() []string {
	return a.nav.Pages()
}

// engineBridge adapts the App's engine access to ipc.Core and navigator.Core.
type engineBridge struct{ app *App }

// EvaluateAwait runs an expression that returns a Promise and resolves it in the
// page before returning, so callers see the settled value (the navigator uses it
// to learn whether wx actually navigated).
func (b *engineBridge) EvaluateAwait(ctx context.Context, expression string, timeoutMs int) (any, error) {
	client, err := b.app.currentEngine()
	if err != nil {
		return nil, err
	}
	return client.EvaluateAwait(ctx, expression, timeoutMs)
}

// InstallHookReport returns the page-side install() report verbatim: the hook
// feeder needs the {ok:false} verdict, which InstallHook collapses into a nil
// error.
func (b *engineBridge) InstallHookReport(ctx context.Context, name string) (map[string]any, error) {
	client, err := b.app.currentEngine()
	if err != nil {
		return nil, err
	}
	return client.HookInstall(ctx, name)
}

// InstallHook keeps the report-blind install for the navigator and the
// existing callers: the install failed only when the RPC itself failed.
func (b *engineBridge) InstallHook(ctx context.Context, name string) error {
	_, err := b.InstallHookReport(ctx, name)
	return err
}

func (b *engineBridge) HookDrain(ctx context.Context, name string, afterSeq int64, limit int, afterUpdateSeq int64, updateLimit int) (ipc.DrainPage, error) {
	client, err := b.app.currentEngine()
	if err != nil {
		return ipc.DrainPage{}, err
	}
	page, err := client.HookDrain(ctx, name, afterSeq, limit, afterUpdateSeq, updateLimit)
	if err != nil {
		return ipc.DrainPage{}, err
	}
	out := ipc.DrainPage{
		NextSeq: page.NextSeq, NextUpdateSeq: page.NextUpdateSeq, HasMore: page.HasMore,
		// R12：页面侧的累计丢弃数原样透传，feeder 只复述最近一次读数。
		DroppedRecords: page.DroppedRecords,
		DroppedUpdates: page.DroppedUpdates,
	}
	for _, record := range page.Records {
		out.Records = append(out.Records, ipc.DrainedRecord{Seq: record.Seq, Record: record.Record})
	}
	for _, update := range page.Updates {
		out.Updates = append(out.Updates, ipc.DrainedUpdate{Seq: update.Seq, Update: update.Update})
	}
	return out, nil
}

func (b *engineBridge) Evaluate(ctx context.Context, expression string, timeoutMs int) (any, error) {
	client, err := b.app.currentEngine()
	if err != nil {
		return nil, err
	}
	return client.Evaluate(ctx, expression, timeoutMs)
}

func (b *engineBridge) Engine() (*engine.Client, error) {
	return b.app.currentEngine()
}

// setupIPC builds the router and its collaborators at startup.
func (a *App) setupIPC() {
	a.router = ipc.New()
	a.tasks = ipc.NewTaskTracker(func(state ipc.TaskState) { a.emit("task", state) })

	core := &engineBridge{app: a}
	ingest := func(ctx context.Context, batch []ipc.DrainedRecord) error {
		return a.ingestHookBatch(ctx, "wxapi", batch)
	}
	cloudIngest := func(ctx context.Context, batch []ipc.DrainedRecord) error {
		return a.ingestHookBatch(ctx, "cloud", batch)
	}

	// wxapi and cloud both have a settled-record update stream: an async call
	// that lands after its record was drained still has to reach storage and the
	// panel. Both hooks deliver one batch per tick (F2): the payload is the
	// tick's whole array, not one event per record. cloud used to fan its batch
	// back out to one event per record / per update frame, which coupled the
	// event rate to the record rate - a 200-record tick meant 200 EventsEmit
	// calls, 200 JSON serializations and 200 webview dispatches. The frontend
	// accepts an array or a single object, so one array per stream per tick is
	// what both panels see now.
	a.wxapiFeeder = ipc.NewHookFeederWithUpdates(core, "wxapi",
		func(records []map[string]any) { a.emit(wxapiCaptureEvent, records) }, ingest,
		func(ctx context.Context, updates []ipc.DrainedUpdate) error {
			return a.applyHookUpdates(ctx, "wxapi", updates)
		},
		func(updates []map[string]any) { a.emit(wxapiUpdateEvent, updates) })
	a.cloudFeeder = ipc.NewHookFeederWithUpdates(core, "cloud",
		func(records []map[string]any) { a.emit(cloudCaptureEvent, records) }, cloudIngest,
		func(ctx context.Context, updates []ipc.DrainedUpdate) error {
			return a.applyHookUpdates(ctx, "cloud", updates)
		},
		func(updates []map[string]any) { a.emit(cloudUpdateEvent, updates) })
	// Console records are UI-only: they are not traffic, so the feeder has no
	// ingest step. The console collector moves them into a bounded ring that
	// the panel polls by sequence number; there is no live event, because a
	// second delivery path would duplicate rows in the log view.
	a.consoleFeeder = ipc.NewHookFeeder(core, "console", nil, nil)
	// WMPF 锁住 console 时页内 install() 回 {ok:false}，但错误钩子（onerror /
	// unhandledrejection）已挂上，drain 循环必须照常拉起，否则那部分错误永远
	// 留在页面缓冲里。install 报错（页面不在、realm 死了）仍然算失败。
	a.consoleFeeder.AcceptAnyInstallReport()

	nav := navigator.New(core, "")
	a.navigatorCtl = ipc.NewNavigatorCtl(navigatorAdapter{nav: nav},
		func(step ipc.AutoVisitStep) {
			percent := 0
			if step.Total > 0 {
				percent = step.Done * 100 / step.Total
			}
			a.emit("navigate_progress", map[string]any{
				"progress": percent, "current": step.Route, "done": step.Done >= step.Total,
				"total": step.Total, "failed": step.Failed,
			})
		})

	if base, err := userBaseDir(); err == nil {
		a.configStore = ipc.NewConfigStore(base)
		a.dataBase = base
	}
	// 审计面（assets.* / traffic.curl、replay、exportHar）：在 handler
	// 注册前装配，注册即引用（见 ipc_audit.go）。
	a.audit = a.newAuditService()
	a.registerIPCHandlers()
	a.startStatusPoller()
	a.startConsoleCollector()
}

// startConsoleCollector moves drained console records from the hook feeder
// into the bounded ring the console panel reads. It runs for the life of the
// app: console records have no other consumer (no live event, no ingest), so
// without the collector they would simply pile up in the feeder's pending
// buffer - bounded at maxPendingRecords like every other feeder, with the
// oldest silently dropped and counted.
func (a *App) startConsoleCollector() {
	if a.ctx == nil || a.consoleFeeder == nil {
		// No Wails runtime (tests): the collector would only spin.
		return
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.mu.Lock()
	a.consoleCancel = cancel
	a.mu.Unlock()
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		// 页内 console 缓冲（1000 条）溢出时，记录还没被 drain 就没了：feeder 会把
		// 页面的累计读数带回来，这里在增量出现时写一条运行日志，别让丢弃无声。
		// clear（用户清空）会把页内计数归零，读数回落只跟随、不告警。
		lastPageDropped := int64(0)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			// Epoch 在拉取前取：期间发生的 console.clear 必须让这批记录作废，
			// 否则用户刚清掉的行会以新序号复活。
			epoch := a.consoleLog.Epoch()
			records, err := a.consoleFeeder.Poll()
			if err != nil {
				continue
			}
			a.consoleLog.Append(records, epoch)
			if stats := a.consoleFeeder.Stats(); stats.PageDroppedRecords != lastPageDropped {
				if stats.PageDroppedRecords > lastPageDropped {
					a.emitLog("warn", fmt.Sprintf("页内 console 缓冲溢出：已丢弃 %d 条（页面产生日志的速度超过了采集跟随速度）", stats.PageDroppedRecords-lastPageDropped))
				}
				lastPageDropped = stats.PageDroppedRecords
			}
		}
	}()
}

// ingestHookBatch converts drained hook records to traffic records and stores
// them (idempotent by captured_at + seq).
func (a *App) ingestHookBatch(ctx context.Context, hookName string, batch []ipc.DrainedRecord) error {
	if a.repo == nil {
		// 存储初始化失败的降级模式：实时捕获照常，历史库一条不进，前端警示
		// pill 之外日志流里也必须留下这条线索，否则排障时看不出「实时有记录、
		// 历史库没数据」是存储没起来。两个 feeder 每 500ms 各调一次本函数，
		// 逐批 warn 会刷爆日志，所以只在第一次进入本分支时发一条，进程生命周期
		// 内不重置。emitLog 内部会经 wailsContext 再拿 a.mu，必须先解锁再发。
		a.mu.Lock()
		first := !a.storageUnavailableWarned
		a.storageUnavailableWarned = true
		a.mu.Unlock()
		if first {
			a.emitLog("warn", "存储不可用：捕获记录不会写入历史库（本次运行仅提示一次）")
		}
		return nil
	}
	if len(batch) == 0 {
		return nil
	}
	records := make([]traffic.Record, 0, len(batch))
	for _, drained := range batch {
		records = append(records, cloud.Convert(hookName, engine.HookDrainedRecord{
			Seq:    drained.Seq,
			Record: drained.Record,
		}))
	}
	_, err := a.ingestTraffic(ctx, records)
	return err
}

// applyHookUpdates overwrites the settled fields of records that are already in
// storage: a slow call is ingested while it is still pending, and its status,
// response and duration arrive later, on the update stream. Both record hooks
// with an update stream (wxapi, cloud) share it; hookName only decides the
// API-type fallback of a frame that carries no type of its own.
func (a *App) applyHookUpdates(ctx context.Context, hookName string, updates []ipc.DrainedUpdate) error {
	if a.repo == nil || len(updates) == 0 {
		return nil
	}
	records := make([]traffic.Record, 0, len(updates))
	for _, update := range updates {
		record, ok := settledTrafficRecordFor(hookName, update.Update)
		if !ok {
			continue // no rid: no stored row can be matched
		}
		records = append(records, record)
	}
	// Rows that were never stored (page buffer ahead of the ingest, old
	// database without the duration column) simply do not match.
	updated, err := a.repo.ApplyUpdates(ctx, records)
	if err != nil {
		return err
	}
	if updated > 0 {
		// 落定写库既不是插入、也不产生新行，但历史记录页必须知道有行从「等待中」变成了
		// 已落定：否则慢调用会一直显示等待中、也不显示耗时，直到下一条记录入库才顺带刷新
		// （在此之前唯一的 traffic:available 来自 ingestTraffic 的插入分支）。前端把这条
		// 事件当刷新触发器用，自带 2s 节流；一条都没改动就不发，空事件是纯开销。
		a.emit("traffic:available", map[string]any{"updated": updated})
	}
	return nil
}

// settledTrafficRecord maps one wxapi update frame. It is the frozen single
// argument entry point desktop/wxapi_contract_test.go pins; the shared logic
// lives in settledTrafficRecordFor.
func settledTrafficRecord(update map[string]any) (traffic.Record, bool) {
	return settledTrafficRecordFor("wxapi", update)
}

// settledTrafficRecordFor maps one update frame of a named hook onto the fields
// ApplyUpdates writes. The frame is fed back through cloud.Convert as a hook
// record so the rid → Record.ID formula and the result/error → response body
// mapping stay in that one place; only the settled fields exist here, which is
// exactly what ApplyUpdates writes (the request side and the capture time stay
// untouched).
//
// The frame must name a row (rid) and a settled status. cloud.Convert
// defaults any other status - a missing field, a typo, a page that reports
// something else - to pending, and ApplyUpdates would then overwrite an
// already settled row back into "waiting", so an unknown status is refused
// here instead.
func settledTrafficRecordFor(hookName string, update map[string]any) (traffic.Record, bool) {
	rid := stringField(update, "rid")
	if rid == "" {
		return traffic.Record{}, false // no rid: no stored row can be matched
	}
	status := stringField(update, "status")
	if status != string(traffic.StatusSuccess) && status != string(traffic.StatusFail) {
		return traffic.Record{}, false // only a settled status may overwrite a row
	}
	return cloud.Convert(hookName, engine.HookDrainedRecord{Record: map[string]any{
		"rid":        rid,
		"status":     status,
		"result":     update["result"],
		"error":      update["error"],
		"durationMs": update["durationMs"],
	}}), true
}

// stringField reads a string field of a hook payload ("" when absent).
func stringField(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return value
}

// emitSink, when non-nil, receives every backend event instead of the Wails
// runtime. Only tests install one: EventsEmit needs the Wails lifecycle
// context and log.Fatals on any other, so a test binary cannot observe the
// event stream otherwise. Nil in production, where emit goes to Wails.
var emitSink func(name string, payload any)

func (a *App) emit(name string, payload any) {
	if emitSink != nil {
		emitSink(name, payload)
		return
	}
	ctx := a.wailsContext()
	if ctx == nil {
		return
	}
	wailsruntime.EventsEmit(ctx, name, payload)
}

// ipcDispatch routes a cloudapi HTTP call back through this router so the
// HTTP surface and the GUI share one implementation.
func (a *App) ipcDispatch(method string, params map[string]any) (any, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return a.router.Call(ctx, method, raw)
}

// resourceProbeRoots returns candidate roots that may host frida/,
// skills/, and devtools_* — preferring resources/
// (repo + release layout) while still accepting a flat sibling layout.
func resourceProbeRoots() []string {
	exeDir := ""
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	wd, _ := os.Getwd()
	return resourceProbeRootsFor(exeDir, wd)
}

// resourceProbeRootsFor lists candidate roots that may host frida/,
// skills/ and devtools_* — exe-adjacent resources/ first
// (Windows release + dev layouts), then the macOS bundle Resources directory
// (WxTap.app/Contents/Resources), the cwd and its parent (the wails dev
// checkout root), and finally ".".
func resourceProbeRootsFor(exeDir, wd string) []string {
	var roots []string
	seen := map[string]bool{}
	add := func(base string) {
		if base == "" || seen[base] {
			return
		}
		seen[base] = true
		roots = append(roots, filepath.Join(base, "resources"), base)
	}
	if exeDir != "" {
		add(exeDir)
		// macOS app bundle: Contents/MacOS/WxTap → Contents/Resources.
		add(filepath.Join(exeDir, "..", "Resources"))
	}
	if wd != "" {
		add(wd)
		// `wails dev` runs desktop/build/bin/WxTap-dev.exe with the working
		// directory set to desktop/, so the checkout's resources/ sits one
		// level above the cwd. Core derives the same root from
		// core/dist/cli.js; without it the two disagree in development and
		// frida/config and skills/ are unreachable.
		add(filepath.Dir(wd))
	}
	add(".")
	return roots
}

// devtoolsBaseDirs lookup order: resources/ next to the
// executable first, then flat exe/cwd roots used in local checkouts.
func devtoolsBaseDirs() []string {
	return resourceProbeRoots()
}

// appVersion is the desktop shell version reported by update.checkVersion.
// It is derived from wails.json in version.go, not written here: a second
// literal in this file is exactly how the MCP handshake ended up announcing a
// different version than the shell.

// resolveSyncBaseDir picks the writable asset root for OSS syncs: the first
// probe root that already hosts a frida/ tree (resources/ preferred), else
// exeDir/resources (release) or ./resources (dev).
func resolveSyncBaseDir() string {
	for _, root := range resourceProbeRoots() {
		if info, err := os.Stat(filepath.Join(root, "frida")); err == nil && info.IsDir() {
			return root
		}
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "resources")
	}
	return filepath.Join(".", "resources")
}

// resolveSkillsDir is the agent skill directory under the asset root.
func resolveSkillsDir() string {
	return skillsDirIn(resolveSyncBaseDir())
}

// skillsDirIn prefers skills/, falling back to the pre-rename mcp_skills/ so an
// installation that has not run the OSS sync since upgrading keeps serving its
// skills. Both paths are judged by content, not existence: a missing or empty
// directory yields an empty skill list rather than an error, and the sync
// creates the directory before it lists the remote prefix, so a failed listing
// leaves an empty skills/ that would otherwise shadow a populated legacy tree.
func skillsDirIn(base string) string {
	current := filepath.Join(base, "skills")
	legacy := filepath.Join(base, "mcp_skills")
	if hasSkillDocs(current) {
		return current
	}
	if hasSkillDocs(legacy) {
		return legacy
	}
	return current
}

// hasSkillDocs reports whether dir holds at least one readable skill file: a
// markdown file other than README, the same set the skill readers serve.
func hasSkillDocs(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() || strings.EqualFold(entry.Name(), "README.md") {
			continue
		}
		if strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			return true
		}
	}
	return false
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func extractBrowseDialogOptions(dir string) wailsruntime.OpenDialogOptions {
	options := wailsruntime.OpenDialogOptions{Title: "选择小程序包目录"}
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		options.DefaultDirectory = dir
	}
	return options
}

// registerIPCHandlers wires every IPC method the frontend may call. The
// handlers are grouped by domain into the register*Handlers helpers below so
// each surface stays reviewable on its own.
func (a *App) registerIPCHandlers() {
	r := a.router
	a.registerEngineHandlers(r)
	a.registerWxapiHandlers(r)
	a.registerCloudHandlers(r)
	a.registerNavigatorHandlers(r)
	a.registerExtractHandlers(r)
	a.registerSettingsHandlers(r)
	a.registerDevtoolsHandlers(r)
	a.registerLocalServiceHandlers(r)
	a.registerMiscHandlers(r)
	a.registerMiniappHandlers(r)
	a.registerHookHandlers(r)
	a.registerConsoleHandlers(r)
	a.registerLogHandlers(r)
	a.registerSessionKeyHandlers(r)
	a.registerExploitHandlers(r)
	a.registerTrafficHandlers(r)
	a.registerAuditHandlers(r)

	// AK 管理：显式凭据验活，不涉及批量枚举或业务操作。
	r.Register("ak.verify", ipc.VerifyAK)
}

// registerTrafficHandlers exposes the paginated history surface through the
// same generic Call envelope used by the Vue bridge.
func (a *App) registerTrafficHandlers(r *ipc.Router) {
	r.Register("traffic.list", func(ctx context.Context, params json.RawMessage) (any, error) {
		var args apiinternal.TrafficListParams
		if err := json.Unmarshal(params, &args); err != nil {
			return nil, err
		}
		if a.trafficAPI == nil {
			return nil, fmt.Errorf("存储初始化失败，流量记录不可用")
		}
		return a.trafficAPI.TrafficList(ctx, args)
	})
	r.Register("traffic.getBody", func(ctx context.Context, params json.RawMessage) (any, error) {
		var args apiinternal.TrafficGetBodyParams
		if err := json.Unmarshal(params, &args); err != nil {
			return nil, err
		}
		if a.trafficAPI == nil {
			return nil, fmt.Errorf("存储初始化失败，流量记录不可用")
		}
		return a.trafficAPI.TrafficGetBody(ctx, args)
	})
	r.Register("traffic.appids", func(ctx context.Context, _ json.RawMessage) (any, error) {
		if a.trafficAPI == nil {
			return nil, fmt.Errorf("存储初始化失败，流量记录不可用")
		}
		stats, err := a.trafficAPI.TrafficAppIDs(ctx)
		if err != nil {
			return nil, err
		}
		// 库里只有 appid；把连接时记住的小程序名回填上，下拉才读得出来。
		if names := a.appNames(); len(names) > 0 {
			for i := range stats {
				if stats[i].Name == "" {
					stats[i].Name = names[stats[i].AppID]
				}
			}
		}
		return stats, nil
	})
	r.Register("traffic.stats", func(ctx context.Context, _ json.RawMessage) (any, error) {
		if a.trafficAPI == nil {
			return nil, fmt.Errorf("存储初始化失败，流量记录不可用")
		}
		stats, err := a.trafficAPI.TrafficStats(ctx)
		if err != nil {
			return nil, err
		}
		// R12 过载计数的冻结求和口径（字段名 droppedRecords / droppedUpdates 不可改）：
		//   droppedRecords = 两个 feeder 的 shell 侧 pending 溢出 + 两个页面侧记录缓冲溢出
		//   droppedUpdates = 两个页面侧更新缓冲溢出（shell 侧没有更新缓冲，无对应项）
		// 两侧互补不重叠，但含义不同：shell 的 dropped 是「未进轮询缓冲」——这些记录已经作为
		// capture 事件投给过界面、也已入库，只是 poll 路径再也取不回（ack 已越过它们，drain 不会
		// 重投）；页面侧的 pageDropped* 才是「永久丢失」——页面缓冲溢出时还没人读过，库里也没有。
		// 前端据此分开展示（「已丢弃」= 永久丢失，「未进列表」= 仍可在历史记录里查到），不得合成
		// 一个数字。feeder 未初始化（捕获没开）
		// 按 0 处理 —— 历史记录页的「过载提示」不能因为没在抓包就让统计整体失败。
		for _, feeder := range []*ipc.HookFeeder{a.wxapiFeeder, a.cloudFeeder} {
			if feeder == nil {
				continue
			}
			feederStats := feeder.Stats()
			stats.DroppedRecords += feederStats.Dropped + feederStats.PageDroppedRecords
			stats.DroppedUpdates += feederStats.PageDroppedUpdates
		}
		return stats, nil
	})
	r.Register("traffic.delete", func(ctx context.Context, params json.RawMessage) (any, error) {
		var args apiinternal.TrafficDeleteParams
		if err := json.Unmarshal(params, &args); err != nil {
			return nil, fmt.Errorf("traffic.delete 参数无效: %w", err)
		}
		if a.trafficAPI == nil {
			return nil, fmt.Errorf("存储初始化失败，流量记录不可用")
		}
		result, err := a.trafficAPI.TrafficDelete(ctx, args)
		if err != nil && result.Deleted == 0 {
			// 一条都没删：删除事务本身失败。这种情况必须报错。
			return nil, err
		}
		if result.ReclamationFailed {
			// 行已经删掉并提交了，失败的只是空间回收（checkpoint / VACUUM）：报成失败会让
			// 用户以为记录还在，只记日志、由 result 的 reclamationFailed 字段上报
			// （见 traffic.DeleteResult）。
			a.emitLog("error", "流量删除的空间回收失败")
		}
		return result, nil
	})
	r.Register("traffic.clear", func(ctx context.Context, _ json.RawMessage) (any, error) {
		if a.trafficAPI == nil {
			return nil, fmt.Errorf("存储初始化失败，流量记录不可用")
		}
		result, err := a.trafficAPI.TrafficClear(ctx)
		if err != nil && result.Deleted == 0 {
			// 一条都没删：清空事务本身失败。这种情况必须报错。
			return nil, err
		}
		if result.ReclamationFailed {
			// 同上：记录已经删掉并提交，失败的只是空间回收，不能报成清空失败。
			a.emitLog("error", "流量清空的空间回收失败")
		}
		return result, nil
	})
}

// registerEngineHandlers wires the engine.* lifecycle surface.
func (a *App) registerEngineHandlers(r *ipc.Router) {
	core := &engineBridge{app: a}

	// Engine.
	r.Register("engine.status", func(ctx context.Context, _ json.RawMessage) (any, error) {
		client, err := core.Engine()
		if err != nil {
			return offlineEngineStatus(), nil
		}
		status, err := client.Status(ctx)
		if err != nil {
			return offlineEngineStatus(), nil
		}
		return engineStatusPayload(status), nil
	})
	r.Register("wechat.status", func(ctx context.Context, _ json.RawMessage) (any, error) {
		client, err := core.Engine()
		if err != nil {
			return map[string]any{"running": false, "error": "微信状态检测不可用：" + err.Error()}, nil
		}
		status, err := client.WeChatStatus(ctx)
		if err != nil {
			return map[string]any{"running": false, "error": "微信状态检测不可用：" + err.Error()}, nil
		}
		return status, nil
	})
	r.Register("engine.start", func(ctx context.Context, params json.RawMessage) (any, error) {
		// The frontend sends cdp_port (snake case); older builds
		// sent cdpPort. Accept both and fall back to the default.
		var args struct {
			CDPPort      int `json:"cdpPort"`
			CDPPortSnake int `json:"cdp_port"`
		}
		_ = json.Unmarshal(params, &args)
		if args.CDPPort == 0 {
			args.CDPPort = args.CDPPortSnake
		}
		if args.CDPPort == 0 {
			args.CDPPort = defaultCDPPort
		}
		client, err := core.Engine()
		if err != nil {
			a.emitLog("error", "启动失败: "+err.Error())
			return nil, err
		}
		if err := client.Start(ctx, args.CDPPort); err != nil {
			a.emitLog("error", "启动失败: "+err.Error())
			return nil, err
		}
		a.emitLog("info", "引擎已启动")
		return map[string]any{"ok": true}, nil
	})
	r.Register("engine.stop", func(ctx context.Context, _ json.RawMessage) (any, error) {
		client, err := core.Engine()
		if err != nil {
			return nil, err
		}
		if err := client.Stop(ctx); err != nil {
			return map[string]any{"ok": true}, err
		}
		// 引擎停止就是捕获结束：前端在 engine.stop 时把「捕获中」置回未捕获
		// （stores/engine.ts），后端不能继续说自己还在跑 —— 那个 running 是下次
		// 连接时 autoInjectHooks 判断要不要重装钩子的依据，留着它就会出现「没人点
		// 开启捕获，记录却开始进库」。
		a.stopAuditCapture()
		a.emitLog("info", "引擎已停止")
		return map[string]any{"ok": true}, nil
	})
	// vConsole toggle: async — fires vconsole_result with the page-side
	// verdict (the expression settles its Promise on the success/fail
	// callback). Prefetch page config so the navigator hook is live first.
	r.Register("engine.vconsole", func(ctx context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Enable *bool `json:"enable"`
		}
		_ = json.Unmarshal(params, &args)
		enable := args.Enable == nil || *args.Enable
		client, err := core.Engine()
		if err != nil {
			return nil, err
		}
		expression := ipc.VConsoleExpression(enable)
		a.goBackground(func() {
			vctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := a.navigatorCtl.Pages(vctx); err != nil {
				a.emit("vconsole_result", map[string]any{"ok": false, "error": err.Error()})
				return
			}
			result, err := client.EvaluateAwait(vctx, expression, 5000)
			if err != nil {
				a.emit("vconsole_result", map[string]any{"ok": false, "error": err.Error()})
				return
			}
			// 页面回执是最终裁决：no wxFrame / setEnableDebug fail 都是失败，
			// 只看 RPC error 会把「发起过调用」当成「开关已生效」。
			var outcome struct {
				Ok  bool   `json:"ok"`
				Err string `json:"err"`
			}
			if raw, ok := result.(string); ok {
				_ = json.Unmarshal([]byte(raw), &outcome)
			}
			if reason := strings.TrimSpace(outcome.Err); reason != "" || !outcome.Ok {
				if reason == "" {
					reason = "页面未确认 setEnableDebug 生效"
				}
				a.emit("vconsole_result", map[string]any{"ok": false, "error": reason})
				return
			}
			a.emit("vconsole_result", map[string]any{"ok": true, "enable": enable})
		})
		return map[string]any{"ok": true, "async": true}, nil
	})

}

// stopAuditCapture ends both audit captures when the engine goes away (explicit
// stop, or shutdown). The caller has already stopped the engine, so there is no
// page left to clear — the page-side buffers die with it. The shell-side pending
// is dropped because a later capture start must not flood the panel with the
// previous session's undelivered records: those are already in storage and stay
// reachable from the history view.
func (a *App) stopAuditCapture() {
	for _, feeder := range []*ipc.HookFeeder{a.wxapiFeeder, a.cloudFeeder} {
		if feeder == nil {
			continue
		}
		feeder.Stop()
		feeder.Clear()
	}
}

// captureStats 是 wxapi.stats / cloud.stats 的响应：feeder 自己的缓冲与游标读数
// 之外，再补一个 shell 侧的存储可用性。存储不是 feeder 的职责（repo 属于 App，
// feeder 只管 drain），所以不往 ipc.FeederStats 上加字段，而是包一层。
// StorageAvailable 绝不能加 omitempty：前端把「字段缺失」当 true 处理，若 false
// 被吞掉，存储初始化失败的降级模式（startup() 里 traffic.Open 失败 → repo == nil，
// 实时面板照常收到记录、历史库一条不进）对用户就完全隐形了。
type captureStats struct {
	ipc.FeederStats
	StorageAvailable bool `json:"storageAvailable"`
}

// registerWxapiHandlers wires the wxapi.* audit capture surface.
func (a *App) registerWxapiHandlers(r *ipc.Router) {
	// wxapi audit.
	r.Register("wxapi.start", func(ctx context.Context, _ json.RawMessage) (any, error) {
		if a.wxapiFeeder.Running() {
			// 重复点「开启捕获」不该动当前窗口：既不改游标也不清缓冲。
			return map[string]any{"ok": true}, nil
		}
		// 游标必须与页内 seq 空间同起点：install 注入的钩子在（新 realm 里）从 1
		// 重新计数，沿用旧 ack 会把之后的每条记录都静默跳过。
		a.wxapiFeeder.ResetAck()
		if err := a.wxapiFeeder.Start(); err != nil {
			return map[string]any{"ok": true}, err
		}
		// 「从此刻起记录」在边界上强制一次：页内可能还留着上一段的残留（最典型的是
		// engine.stop —— 引擎已经没了，卸载钩子那次 CDP 到不了页面，页内缓冲连同
		// 钩子一起活到下一次连接）。不清它，这批会在本次开始的第一个 tick 被当成新
		// 记录补投，使用者又会看到「刚点下去就冒出一堆」。
		a.mu.Lock()
		client := a.engine
		a.mu.Unlock()
		if client != nil {
			_ = client.HookClear(ctx, "wxapi")
		}
		return map[string]any{"ok": true}, nil
	})
	r.Register("wxapi.stop", func(ctx context.Context, _ json.RawMessage) (any, error) {
		a.wxapiFeeder.Stop()
		a.mu.Lock()
		client := a.engine
		a.mu.Unlock()
		if client != nil {
			// 顺序同 R10b（先页面、后 shell）：只卸载不清页面缓冲的话，停捕前最后半个
			// tick 记下的记录会留在页内，下次「开启捕获」时被当成新流量补投 —— 使用者
			// 看到的是「我明明停过，怎么又冒出来几条」。已入库的记录不受影响，仍可在
			// 历史记录里查到。
			_ = client.HookClear(ctx, "wxapi")
			_ = client.HookUninstall(ctx, "wxapi")
		}
		a.wxapiFeeder.Clear()
		return map[string]any{"ok": true}, nil
	})
	// wxapi.poll answers one bounded page per call (F3): the panel asks for 500
	// records by default, so a burst never arrives as one multi-megabyte
	// payload. An absent limit means the default; an explicit value is clamped
	// to 1..2000 by the feeder. The response is still the same array in the
	// same order - only how much of it comes back per call changed.
	r.Register("wxapi.poll", func(_ context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Limit *int `json:"limit"`
		}
		_ = json.Unmarshal(params, &args)
		limit := ipc.PollDefaultLimit
		if args.Limit != nil {
			limit = *args.Limit
		}
		return a.wxapiFeeder.PollLimit(limit)
	})
	// wxapi.stats reports the capture state the panel cannot read from the
	// records alone: whether the feeder is running, how much is buffered, how
	// much was dropped and how far both streams are acknowledged. It also says
	// whether the shell can persist at all (storageAvailable)：repo 为 nil 的降级
	// 模式下 feeder 照常 capture，面板若不读这个字段就发现不了历史库在悄悄丢数据。
	r.Register("wxapi.stats", func(context.Context, json.RawMessage) (any, error) {
		if a.wxapiFeeder == nil {
			return nil, fmt.Errorf("捕捉器未初始化")
		}
		return captureStats{FeederStats: a.wxapiFeeder.Stats(), StorageAvailable: a.repo != nil}, nil
	})
	// wxapi.clear 的顺序是先清页面缓冲、再清 shell 侧的 pending（R10b）：HookClear
	// 是一次 CDP 往返，这段时间里 500ms 的 drain tick 会把页面缓冲里还没被清掉的
	// 记录重新 drain 回 pending —— 用户刚清空的面板立刻又冒出旧记录。反过来先清
	// pending 再清页面，同一窗口内的 tick 正好能把这些记录捞回来。
	r.Register("wxapi.clear", func(ctx context.Context, _ json.RawMessage) (any, error) {
		a.mu.Lock()
		client := a.engine
		a.mu.Unlock()
		if client != nil {
			_ = client.HookClear(ctx, "wxapi")
		}
		a.wxapiFeeder.Clear()
		return map[string]any{"ok": true}, nil
	})
	r.Register("wxapi.replay", func(ctx context.Context, params json.RawMessage) (any, error) {
		var args struct {
			APIName string         `json:"api_name"`
			Options map[string]any `json:"options"`
		}
		_ = json.Unmarshal(params, &args)
		// The feeder builds the replay expression with JS-string escaping;
		// interpolating the raw name would let an api_name terminate the
		// expression.
		return a.wxapiFeeder.Replay(ctx, args.APIName, args.Options)
	})

}

// registerCloudHandlers wires the cloud.* audit and manual-call surface.
func (a *App) registerCloudHandlers(r *ipc.Router) {
	core := &engineBridge{app: a}

	// Cloud audit.
	r.Register("cloud.start", func(ctx context.Context, _ json.RawMessage) (any, error) {
		if a.cloudFeeder.Running() {
			return map[string]any{"ok": true}, nil
		}
		// 同 wxapi.start：游标与页内 seq 空间同起点，并在边界上清掉页内残留。
		a.cloudFeeder.ResetAck()
		if err := a.cloudFeeder.Start(); err != nil {
			return map[string]any{"ok": true}, err
		}
		a.mu.Lock()
		client := a.engine
		a.mu.Unlock()
		if client != nil {
			_ = client.HookClear(ctx, "cloud")
		}
		return map[string]any{"ok": true}, nil
	})
	r.Register("cloud.stop", func(ctx context.Context, _ json.RawMessage) (any, error) {
		a.cloudFeeder.Stop()
		a.mu.Lock()
		client := a.engine
		a.mu.Unlock()
		if client != nil {
			// 顺序同 wxapi.stop（也是 R10b 的同一顺序）：先清页面缓冲，再卸载钩子。
			_ = client.HookClear(ctx, "cloud")
			_ = client.HookUninstall(ctx, "cloud")
		}
		a.cloudFeeder.Clear()
		return map[string]any{"ok": true}, nil
	})
	// cloud.poll answers one bounded page per call, exactly like wxapi.poll: an
	// absent limit means 500 records, an explicit value is clamped to 1..2000 by
	// the feeder, and the response is still the same array in the same order.
	r.Register("cloud.poll", func(_ context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Limit *int `json:"limit"`
		}
		_ = json.Unmarshal(params, &args)
		limit := ipc.PollDefaultLimit
		if args.Limit != nil {
			limit = *args.Limit
		}
		return a.cloudFeeder.PollLimit(limit)
	})
	// cloud.stats reports the same capture state wxapi.stats does (running,
	// buffered, dropped, both acknowledgements) for the cloud feeder, plus the
	// same storageAvailable（与 wxapi.stats 同一语义、同一必须显式输出的要求）。
	r.Register("cloud.stats", func(context.Context, json.RawMessage) (any, error) {
		if a.cloudFeeder == nil {
			return nil, fmt.Errorf("捕捉器未初始化")
		}
		return captureStats{FeederStats: a.cloudFeeder.Stats(), StorageAvailable: a.repo != nil}, nil
	})
	// cloud.clear 的顺序与 wxapi.clear 一致（R10b，路线图 §2.1 对云函数同样适用）：
	// 先清页面缓冲（HookClear 是一次 CDP 往返），再清 shell 侧的 pending。反过来的
	// 话，这段往返窗口里 500ms 一次的 drain tick 会把页面缓冲里还没被清掉的记录重新
	// drain 回 pending —— 用户刚清空的面板立刻又冒出旧记录。
	r.Register("cloud.clear", func(ctx context.Context, _ json.RawMessage) (any, error) {
		a.mu.Lock()
		client := a.engine
		a.mu.Unlock()
		if client != nil {
			_ = client.HookClear(ctx, "cloud")
		}
		a.cloudFeeder.Clear()
		return map[string]any{"ok": true}, nil
	})
	r.Register("cloud.scan", func(ctx context.Context, _ json.RawMessage) (any, error) {
		// No engine reads as an empty scan result (handler returned {items: []}
		// whenever the auditor was absent); a failed scan is a real error.
		client, err := core.Engine()
		if err != nil {
			return map[string]any{"items": []any{}}, nil
		}
		items, err := client.CloudScan(ctx)
		if err != nil {
			return nil, err
		}
		if items == nil {
			items = []map[string]any{}
		}
		return map[string]any{"items": items}, nil
	})
	// Manual cloud calls: evaluated with awaitPromise so the injected hook's
	// Promise settles in-page (one round trip instead of 0.5s polling).
	r.Register("cloud.call", func(ctx context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Name string          `json:"name"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(params, &args); err != nil {
			return nil, err
		}
		client, err := core.Engine()
		if err != nil {
			return nil, err
		}
		expression, err := ipc.CloudCallExpression(args.Name, args.Data)
		if err != nil {
			return nil, err
		}
		return client.EvaluateAwait(ctx, expression, 15000)
	})
	r.Register("cloud.call_container", func(ctx context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Path   string          `json:"path"`
			Method string          `json:"method"`
			Header json.RawMessage `json:"header"`
			Data   json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(params, &args); err != nil {
			return nil, err
		}
		client, err := core.Engine()
		if err != nil {
			return nil, err
		}
		expression, err := ipc.CloudContainerExpression(args.Path, args.Method, args.Header, args.Data)
		if err != nil {
			return nil, err
		}
		value, err := client.EvaluateAwait(ctx, expression, 15000)
		if err != nil {
			return nil, err
		}
		return ipc.NormalizeCloudContainerResult(value), nil
	})
	r.Register("cloud.export", func(ctx context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(params, &args); err != nil {
			return nil, err
		}
		if len(args.Items) == 0 {
			return map[string]any{"error": "没有数据可导出"}, nil
		}
		savePath, err := wailsruntime.SaveFileDialog(a.ctx, wailsruntime.SaveDialogOptions{
			DefaultFilename: "cloud_audit_report.xlsx",
			Filters:         []wailsruntime.FileFilter{{DisplayName: "Excel Files (*.xlsx)", Pattern: "*.xlsx"}},
		})
		if err != nil {
			return nil, err
		}
		if savePath == "" {
			return map[string]any{"ok": false, "reason": "用户取消"}, nil
		}
		if !strings.HasSuffix(savePath, ".xlsx") {
			savePath += ".xlsx"
		}
		items, total := args.Items, len(args.Items)
		var task ipc.TaskState
		taskDone := false
		if a.tasks != nil {
			task = a.tasks.Start("cloud.export", "导出云调用记录", total)
		}
		a.emit("export_progress", map[string]any{"status": "working", "current": 0, "total": total})
		a.goBackground(func() {
			if task.ID != "" {
				defer func() {
					if !taskDone {
						_, _ = a.tasks.Finish(task.ID, "done", "导出完成", "")
					}
				}()
			}
			if err := ipc.ExportCloudXLSX(items, savePath, func(current, total int) {
				if task.ID != "" {
					_, _ = a.tasks.Update(task.ID, "running", "正在导出", current, total)
				}
				a.emit("export_progress", map[string]any{"status": "working", "current": current, "total": total})
			}); err != nil {
				if task.ID != "" {
					taskDone = true
					_, _ = a.tasks.Finish(task.ID, "failed", "导出失败", err.Error())
				}
				a.emit("export_progress", map[string]any{"status": "error", "message": err.Error()})
				return
			}
			a.emit("export_progress", map[string]any{"status": "done", "path": savePath})
		})
		return map[string]any{"ok": true, "async": true}, nil
	})
	// Progress actually rides the export_progress event; this endpoint stays
	// registered only because the v0.1.0 frozen surface pins it
	// (testdata/compat-surface.json).
	r.Register("cloud.export.progress", func(context.Context, json.RawMessage) (any, error) {
		return map[string]any{"ok": true}, nil
	})
}

// registerNavigatorHandlers wires the navigator.* routing surface.
func (a *App) registerNavigatorHandlers(r *ipc.Router) {
	// Navigator.
	r.Register("navigator.pages", func(ctx context.Context, _ json.RawMessage) (any, error) {
		return a.navigatorCtl.Pages(ctx)
	})
	r.Register("navigator.navigate", func(ctx context.Context, params json.RawMessage) (any, error) {
		var req ipc.NavigateRequest
		_ = json.Unmarshal(params, &req)
		return map[string]any{"ok": true}, a.navigatorCtl.Navigate(ctx, req)
	})
	r.Register("navigator.currentRoute", a.currentRouteHandler)
	r.Register("navigator.getCurrentRoute", a.currentRouteHandler)
	r.Register("navigator.pageStack", func(ctx context.Context, _ json.RawMessage) (any, error) {
		return a.navigatorCtl.PageStack(ctx)
	})
	r.Register("navigator.guardState", func(ctx context.Context, _ json.RawMessage) (any, error) {
		return a.navigatorCtl.GuardState(ctx)
	})
	r.Register("navigator.autoVisit", func(ctx context.Context, _ json.RawMessage) (any, error) {
		// started=false 表示后端已经有一轮在走：界面据此提示「已在遍历中」，
		// 而不是谎报「已开始遍历」。
		started, err := a.navigatorCtl.AutoVisit(ctx, 2*time.Second)
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "started": started}, nil
	})
	r.Register("navigator.autoVisitState", func(context.Context, json.RawMessage) (any, error) {
		// progress 是 MCP 侧唯一的遍历进度来源（事件通道它收不到）：没有它，
		// "还在走" 与 "早已走完但全部失败" 对调用方无法区分。
		step := a.navigatorCtl.AutoVisitProgress()
		return map[string]any{
			"visiting": a.navigatorCtl.AutoVisitRunning(),
			"progress": map[string]any{"done": step.Done, "total": step.Total, "failed": step.Failed, "route": step.Route},
		}, nil
	})
	r.Register("navigator.stopAutoVisit", func(context.Context, json.RawMessage) (any, error) {
		a.navigatorCtl.StopAutoVisit()
		return map[string]any{"ok": true}, nil
	})
	r.Register("navigator.enableRedirectGuard", func(ctx context.Context, _ json.RawMessage) (any, error) {
		return a.navigatorCtl.EnableRedirectGuard(ctx)
	})
	r.Register("navigator.disableRedirectGuard", func(ctx context.Context, _ json.RawMessage) (any, error) {
		return map[string]any{"ok": true}, a.navigatorCtl.DisableRedirectGuard(ctx)
	})
	r.Register("navigator.getBlockedRedirects", func(ctx context.Context, _ json.RawMessage) (any, error) {
		return a.navigatorCtl.GetBlockedRedirects(ctx)
	})

}

// registerExtractHandlers wires the extract.* packages/decompile/report surface.
func (a *App) registerExtractHandlers(r *ipc.Router) {
	// Extract.
	r.Register("extract.packages", func(ctx context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Dir string `json:"dir"`
		}
		_ = json.Unmarshal(params, &args)
		dir := args.Dir
		if dir == "" {
			dir = ipc.DefaultPackagesDir()
		}
		packages := ipc.FindPackages(dir)
		if packages == nil {
			// A missing/unreadable directory must read as an empty list, not null.
			packages = []ipc.PkgFile{}
		}
		known := make(map[string]bool, len(packages))
		for i := range packages {
			outDir := a.decompileOutputDir(packages[i].AppID)
			packages[i].OutputDir = outDir
			if info, err := os.Stat(outDir); err == nil && info.IsDir() {
				packages[i].Decompiled = true
			}
			known[packages[i].AppID] = true
		}
		// 产物目录同样是小程序的来源：原始包被清掉或换过包目录后，已经
		// 反编译过的小程序不能从列表里消失。
		for _, project := range ipc.DecompiledProjects(a.extractOutputRoot()) {
			if known[project.AppID] {
				continue
			}
			packages = append(packages, ipc.PkgFile{
				AppID:      project.AppID,
				Name:       project.Name,
				Mtime:      project.Mtime,
				Decompiled: true,
				OutputDir:  project.Path,
			})
		}
		return map[string]any{"packages": packages}, nil
	})
	r.Register("extract.inventory", func(_ context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Dir     string `json:"dir"`
			UserDir string `json:"user_dir"`
		}
		_ = json.Unmarshal(params, &args)
		dir := args.Dir
		if dir == "" {
			dir = ipc.DefaultPackagesDir()
		}
		items, summary := ipc.BuildInventory(args.UserDir, ipc.FindPackages(dir), ipc.DecompiledProjects(a.extractOutputRoot()))
		return map[string]any{"items": items, "summary": summary}, nil
	})
	r.Register("extract.defaultDir", func(context.Context, json.RawMessage) (any, error) {
		return map[string]any{"dir": ipc.DefaultPackagesDir()}, nil
	})
	r.Register("extract.candidateDirs", func(context.Context, json.RawMessage) (any, error) {
		return map[string]any{"dirs": ipc.PackageDirCandidates(), "default": ipc.DefaultPackagesDir()}, nil
	})
	r.Register("extract.browse", func(_ context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Dir string `json:"dir"`
		}
		_ = json.Unmarshal(params, &args)
		dir, err := wailsruntime.OpenDirectoryDialog(a.ctx, extractBrowseDialogOptions(args.Dir))
		if err != nil {
			return nil, err
		}
		return map[string]any{"dir": dir}, nil
	})
	r.Register("extract.decompile", func(ctx context.Context, params json.RawMessage) (any, error) {
		var task ipc.TaskState
		taskDone := false
		if a.tasks != nil {
			task = a.tasks.Start("extract.decompile", "准备反编译", 0)
			defer func() {
				if task.ID != "" && !taskDone {
					_, _ = a.tasks.Finish(task.ID, "done", "反编译完成", "")
				}
			}()
		}
		var args struct {
			Dir   string `json:"dir"`
			AppID string `json:"appid"`
			Path  string `json:"path"`
		}
		_ = json.Unmarshal(params, &args)
		if err := ipc.ValidateAppID(args.AppID); err != nil {
			if task.ID != "" {
				taskDone = true
				_, _ = a.tasks.Finish(task.ID, "failed", "参数校验失败", err.Error())
			}
			return nil, err
		}
		dir := args.Dir
		if dir == "" {
			dir = ipc.DefaultPackagesDir()
		}
		var packagePaths []string
		if args.Path != "" {
			packagePaths = []string{args.Path}
		} else {
			packages := ipc.FindPackages(dir)
			// The appid route is what the page and every compat caller use; an
			// explicit path stays the deliberate escape hatch.
			if ipc.UnsupportedAppIDs(packages)[args.AppID] {
				if task.ID != "" {
					taskDone = true
					_, _ = a.tasks.Finish(task.ID, "failed", "反编译失败", ipc.UnsupportedAppReason)
				}
				return nil, errors.New(ipc.UnsupportedAppReason)
			}
			for i := range packages {
				if packages[i].AppID == args.AppID {
					packagePaths = append(packagePaths, packages[i].Path)
				}
			}
		}
		if len(packagePaths) == 0 {
			if task.ID != "" {
				taskDone = true
				_, _ = a.tasks.Finish(task.ID, "failed", "未找到安装包", "未找到 wxapkg 文件")
			}
			return nil, fmt.Errorf("未找到 %s 的 wxapkg 文件", args.AppID)
		}
		outDir := a.decompileOutputDir(args.AppID)
		a.emit("extract_progress", map[string]any{"percent": 10, "text": "正在反编译 " + args.AppID})
		files, err := ipc.DecompilePackages(packagePaths, outDir, args.AppID)
		if err != nil {
			if task.ID != "" {
				taskDone = true
				_, _ = a.tasks.Finish(task.ID, "failed", "反编译失败", err.Error())
			}
			a.emit("extract_log", map[string]any{"level": "error", "message": err.Error()})
			return nil, err
		}
		name := ipc.ReadMiniAppName(outDir)
		a.emit("extract_progress", map[string]any{"percent": 100, "text": "反编译完成"})
		a.emit("extract_done", map[string]any{"ok": true, "path": outDir, "appid": args.AppID})
		return map[string]any{"ok": true, "files_count": len(files), "name": name, "dir": outDir, "output_dir": outDir, "packages_processed": len(packagePaths)}, nil
	})
	r.Register("extract.decompileAll", func(ctx context.Context, params json.RawMessage) (any, error) {
		var task ipc.TaskState
		taskDone := false
		if a.tasks != nil {
			task = a.tasks.Start("extract.decompileAll", "准备批量反编译", 0)
			defer func() {
				if task.ID != "" && !taskDone {
					_, _ = a.tasks.Finish(task.ID, "done", "批量反编译完成", "")
				}
			}()
		}
		var args struct {
			Dir string `json:"dir"`
		}
		_ = json.Unmarshal(params, &args)
		dir := strings.TrimSpace(args.Dir)
		if dir == "" {
			dir = ipc.DefaultPackagesDir()
		}
		if dir == "" {
			return nil, fmt.Errorf("未找到微信小程序包目录，请显式指定 dir")
		}

		grouped := map[string][]string{}
		found := ipc.FindPackages(dir)
		for _, pkg := range found {
			grouped[pkg.AppID] = append(grouped[pkg.AppID], pkg.Path)
		}
		// Apps whose templates this tool cannot restore are not part of the batch:
		// they would fail, and the succeeded/failed counts are meant to describe
		// what is actually decompilable on this machine.
		unsupported := ipc.UnsupportedAppIDs(found)
		appIDs := make([]string, 0, len(grouped))
		for appID := range grouped {
			if unsupported[appID] {
				continue
			}
			appIDs = append(appIDs, appID)
		}
		sort.Strings(appIDs)
		if len(appIDs) == 0 {
			// An agent reading an empty batch must not take it for an empty
			// directory: everything here may simply be outside the scope.
			a.emit("extract_log", map[string]any{"level": "log", "message": "该目录没有可反编译的小程序"})
		}

		results := make([]map[string]any, 0, len(appIDs))
		succeeded := 0
		failed := 0
		for _, appID := range appIDs {
			if task.ID != "" {
				_, _ = a.tasks.Update(task.ID, "running", "正在反编译 "+appID, succeeded+failed, len(appIDs))
			}
			// 页面自己的进度条不走 task 事件：每开一个小程序推一帧，避免 0% 直跳 100%。
			a.emit("extract_progress", map[string]any{
				"percent": (succeeded + failed) * 100 / len(appIDs),
				"text":    "正在反编译 " + appID,
			})
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			outDir := a.decompileOutputDir(appID)
			files, err := ipc.DecompilePackages(grouped[appID], outDir, appID)
			entry := map[string]any{
				"appid":              appID,
				"dir":                outDir,
				"output_dir":         outDir,
				"packages_processed": len(grouped[appID]),
			}
			if err != nil {
				failed++
				entry["ok"] = false
				entry["error"] = err.Error()
				a.emit("extract_log", map[string]any{"level": "error", "message": appID + ": " + err.Error()})
			} else {
				succeeded++
				entry["ok"] = true
				entry["files_count"] = len(files)
				entry["name"] = ipc.ReadMiniAppName(outDir)
			}
			results = append(results, entry)
		}
		a.emit("extract_progress", map[string]any{"percent": 100, "text": "全部反编译完成"})
		a.emit("extract_done", map[string]any{"ok": failed == 0, "total": len(appIDs), "succeeded": succeeded, "failed": failed})
		return map[string]any{"results": results, "total": len(appIDs), "succeeded": succeeded, "failed": failed}, nil
	})
	r.Register("extract.scan", func(ctx context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Dir   string `json:"dir"`
			AppID string `json:"appid"`
			Path  string `json:"path"`
		}
		_ = json.Unmarshal(params, &args)
		if err := ipc.ValidateAppID(args.AppID); err != nil {
			return nil, err
		}
		scanDir := args.Path
		if scanDir == "" {
			scanDir = a.decompileOutputDir(args.AppID)
		}
		if info, err := os.Stat(scanDir); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("请先反编译此小程序")
		}
		appID := args.AppID
		var task ipc.TaskState
		taskDone := false
		if a.tasks != nil {
			task = a.tasks.Start("extract.scan", "扫描反编译产物", 0)
		}
		a.goBackground(func() {
			if task.ID != "" {
				defer func() {
					if !taskDone {
						_, _ = a.tasks.Finish(task.ID, "done", "扫描完成", "")
					}
				}()
			}
			a.emit("extract_progress", map[string]any{"percent": 10, "text": "正在扫描 " + appID})
			report, err := ipc.Scan(scanDir, appID, "", a.customPatterns())
			if err != nil {
				if task.ID != "" {
					taskDone = true
					_, _ = a.tasks.Finish(task.ID, "failed", "扫描失败", err.Error())
				}
				a.emit("extract_log", map[string]any{"level": "error", "message": err.Error()})
				a.emit("extract_scan_done", map[string]any{"appid": appID, "error": err.Error()})
				return
			}
			a.emit("extract_scan_done", map[string]any{
				"appid":          appID,
				"js_count":       report.JSCount,
				"findings_count": len(report.Findings),
				"output_dir":     a.decompileOutputDir(appID),
				"findings":       report.Findings,
				"results":        report.Result,
				"result":         map[string]any{"files_scanned": report.JSCount, "total_size": report.TotalSize},
			})
		})
		return map[string]any{"ok": true, "async": true}, nil
	})
	r.Register("extract.delete", func(ctx context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Dir   string `json:"dir"`
			AppID string `json:"appid"`
			Path  string `json:"path"`
		}
		if err := json.Unmarshal(params, &args); err != nil {
			return nil, fmt.Errorf("extract.delete 参数无效: %w", err)
		}
		if args.Path != "" {
			if err := safeRemoveAll(args.Path); err != nil {
				return nil, err
			}
			return map[string]any{"ok": true}, nil
		}
		if err := validateAppID(args.AppID); err != nil {
			return nil, err
		}
		dir := strings.TrimSpace(args.Dir)
		if dir == "" {
			dir = ipc.DefaultPackagesDir()
		}
		if strings.TrimSpace(dir) == "" {
			return nil, fmt.Errorf("未找到微信小程序包目录，请显式指定 dir")
		}
		// Raw packages: {dir}/{appid} plus any {appid}-prefixed sibling.
		if err := safeRemoveAll(filepath.Join(dir, args.AppID)); err != nil {
			return nil, err
		}
		if entries, err := os.ReadDir(dir); err == nil {
			for _, entry := range entries {
				if entry.IsDir() && strings.HasPrefix(entry.Name(), args.AppID) {
					if err := safeRemoveAll(filepath.Join(dir, entry.Name())); err != nil {
						return nil, err
					}
				}
			}
		}
		// Decompiled output.
		if err := safeRemoveAll(a.decompileOutputDir(args.AppID)); err != nil {
			return nil, err
		}

		return map[string]any{"ok": true}, nil
	})
	r.Register("extract.builtinPatterns", func(context.Context, json.RawMessage) (any, error) {
		labels := map[string]string{}
		for _, info := range ipc.BuiltinPatterns() {
			labels[info.Key] = info.Label
		}
		out := map[string]string{}
		for key, regex := range extract.PatternStrings() {
			name := key
			if label := labels[key]; label != "" {
				name = label
			}
			out[name] = regex
		}
		return map[string]any{"patterns": out}, nil
	})
	r.Register("extract.clearOutput", func(ctx context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Type string `json:"type"`
			Dir  string `json:"dir"`
			Path string `json:"path"`
		}
		if err := json.Unmarshal(params, &args); err != nil {
			return nil, fmt.Errorf("extract.clearOutput 参数无效: %w", err)
		}
		switch args.Type {
		case "decompiled":
			// All decompiled output.
			if err := safeRemoveAll(a.extractOutputRoot()); err != nil {
				return nil, err
			}

		case "applet":
			// Wipe the package directory's contents, keeping the directory.
			dir := strings.TrimSpace(args.Dir)
			if dir == "" {
				dir = ipc.DefaultPackagesDir()
			}
			if strings.TrimSpace(dir) == "" {
				return nil, fmt.Errorf("未找到微信小程序包目录，请显式指定 dir")
			}
			// The directory itself stays (existing semantics); only its
			// children are removed, each through the guard.
			if err := requireDeletableDir(dir); err != nil {
				return nil, err
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				return nil, fmt.Errorf("读取包目录失败 %s: %w", dir, err)
			}
			for _, entry := range entries {
				if err := safeRemoveAll(filepath.Join(dir, entry.Name())); err != nil {
					return nil, err
				}
			}
		default:
			// Passthrough: delete an explicit path when given.
			if strings.TrimSpace(args.Path) != "" {
				if err := safeRemoveAll(args.Path); err != nil {
					return nil, err
				}
			}
		}
		return map[string]any{"ok": true}, nil
	})
	r.Register("extract.openDir", func(ctx context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Path string `json:"path"`
			Dir  string `json:"dir"`
		}
		_ = json.Unmarshal(params, &args)
		target := args.Path
		if target == "" {
			target = args.Dir
		}
		if target == "" {
			target = a.extractOutputRoot()
		}
		return map[string]any{"ok": true}, openInExplorer(target)
	})

}

// registerSettingsHandlers wires config.*, settings.getPaths and the shell.* helpers.
func (a *App) registerSettingsHandlers(r *ipc.Router) {
	// Config & settings.
	r.Register("config.load", func(context.Context, json.RawMessage) (any, error) {
		if a.configStore == nil {
			return map[string]any{"cdp_port": defaultCDPPort}, nil
		}
		config, err := a.configStore.Load()
		if err != nil {
			return nil, err
		}
		// Migrate the implicit default. Explicit values other than
		// 62000 remain untouched so user-selected ports are preserved.
		if value, ok := config["cdp_port"]; !ok {
			config["cdp_port"] = defaultCDPPort
		} else {
			switch number := value.(type) {
			case float64:
				if number == 62000 {
					config["cdp_port"] = defaultCDPPort
				}
			case string:
				if number == "62000" {
					config["cdp_port"] = defaultCDPPort
				}
			}
		}
		return config, nil
	})
	r.Register("config.save", func(ctx context.Context, params json.RawMessage) (any, error) {
		if a.configStore == nil {
			return nil, fmt.Errorf("config store unavailable")
		}
		patch := map[string]any{}
		if err := json.Unmarshal(params, &patch); err != nil {
			// Saving must never guess: a partially decoded patch would merge
			// garbage into the persisted config.
			return nil, fmt.Errorf("config.save 参数无效: %w", err)
		}
		return map[string]any{"ok": true}, a.configStore.Save(ctx, patch)
	})
	// node.status answers which Node runs Core; node.detect probes the
	// well-known install locations. They are separate calls because detection
	// spawns one Node per candidate and is only worth doing on request, while
	// status is read every time the settings page loads.
	r.Register("node.status", func(context.Context, json.RawMessage) (any, error) {
		return resolveNodeRuntime(), nil
	})
	r.Register("node.detect", func(_ context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Path string `json:"path"`
		}
		if len(params) > 0 {
			_ = json.Unmarshal(params, &args)
		}
		// A path means "check this one", which is what the user just typed;
		// without one the caller is asking what this machine has.
		if path := strings.TrimSpace(args.Path); path != "" {
			return map[string]any{"candidates": []NodeRuntime{probeNode(path, nodeSourceConfig)}}, nil
		}
		candidates := []NodeRuntime{}
		seen := map[string]bool{}
		consider := func(candidate, source string) {
			probed := probeNode(candidate, source)
			if probed.Error != "" {
				return
			}
			key := strings.ToLower(probed.Path)
			if seen[key] {
				return
			}
			seen[key] = true
			candidates = append(candidates, probed)
		}
		// PATH 上的那个排在最前：它就是 `node` 真正会跑的那个。漏掉它会让面板
		// 写着「正在用 PATH 上的 node」而检测提示说「没找到」，两处互相打脸。
		if fromPath, err := exec.LookPath("node"); err == nil {
			consider(fromPath, nodeSourcePath)
		}
		for _, candidate := range detectNodeCandidates() {
			consider(candidate, nodeSourceDetected)
		}
		return map[string]any{"candidates": candidates}, nil
	})
	// electron.status answers which Electron opens the DevTools window, and
	// electron.detect lists every one this machine has. Detection only stats
	// candidates, but the two stay separate so both pages read the same shape as
	// node.status / node.detect.
	r.Register("electron.status", func(context.Context, json.RawMessage) (any, error) {
		return resolveElectronRuntime(), nil
	})
	r.Register("electron.detect", func(context.Context, json.RawMessage) (any, error) {
		return map[string]any{"candidates": probeElectronCandidates()}, nil
	})
	r.Register("settings.getPaths", func(context.Context, json.RawMessage) (any, error) {
		// The folders the settings view shortcuts into, plus the paths agents
		// read through paths_get.
		base := resolveSyncBaseDir()
		logDir := ""
		hookScripts := filepath.Join(base, "hook_scripts")
		if dir, err := userBaseDir(); err == nil {
			logDir = filepath.Join(dir, "logs")
			// HookStore 读写的是数据目录下的 hook_scripts；设置页与注入脚本页的
			// 「脚本目录」必须指向那里，否则用户放进随包资源目录的脚本会被应用
			// 更新覆盖。
			hookScripts = filepath.Join(dir, "hook_scripts")
		}
		return map[string]any{
			"outputDir": a.extractOutputRoot(),
			"dbPath":    resolveDBPath(),
			// hook_scripts 是可写目录：用户自己放脚本的地方。
			"hook_scripts": hookScripts,
			"logDir":       logDir,
			"frida_config": filepath.Join(base, "frida", "config"),
			"skills":       resolveSkillsDir(),
		}, nil
	})

	// Shell helpers.
	r.Register("shell.openUrl", func(_ context.Context, params json.RawMessage) (any, error) {
		var args struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(params, &args)
		if args.URL == "" {
			return nil, fmt.Errorf("url is required")
		}
		wailsruntime.BrowserOpenURL(a.ctx, args.URL)
		return map[string]any{"ok": true}, nil
	})
	r.Register("shell.openFolder", func(_ context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(params, &args)
		return map[string]any{"ok": true}, openInExplorer(args.Path)
	})
}

// registerDevtoolsHandlers wires the Electron DevTools window surface.
func (a *App) registerDevtoolsHandlers(r *ipc.Router) {
	// openDevtoolsWindow launches a configured Electron on the devtools:// URL.
	// Electron 是唯一的窗口打开方式：把 devtools:// 作为启动参数交给
	// Chrome/Edge 会被丢弃（实测 2026-09 Chrome 155，独立 profile 也一样），
	// 结果只是一个新标签页。
	r.Register("shell.openDevtoolsWindow", func(_ context.Context, params json.RawMessage) (any, error) {
		var args struct {
			CDPPort      int    `json:"cdp_port"`
			ElectronPath string `json:"electron_path"`
			TargetID     string `json:"target_id"`
		}
		_ = json.Unmarshal(params, &args)
		if args.CDPPort == 0 {
			args.CDPPort = defaultCDPPort
		}
		pageURL := devtoolsInspectorURL(args.CDPPort, args.TargetID)
		// An empty path means "use whichever Electron this machine has": the
		// Settings page still wins when it holds one, and the resolution (PATH,
		// npm global, well-known locations) is the backend's business either way.
		electronPath := strings.TrimSpace(args.ElectronPath)
		if electronPath == "" {
			resolved := resolveElectronRuntime()
			if resolved.Error != "" {
				return nil, fmt.Errorf("打不开 DevTools 窗口：%s", resolved.Error)
			}
			electronPath = resolved.Path
		}
		script, err := devtools.ElectronScript(devtoolsBaseDirs())
		if err != nil {
			return nil, fmt.Errorf("打不开 DevTools 窗口：%w", err)
		}
		name, argv, err := electronLaunch(runtime.GOOS, electronPath, script, pageURL)
		if err != nil {
			return nil, fmt.Errorf("打不开 DevTools 窗口：%w", err)
		}
		cmd := exec.Command(name, argv...)
		configureGUIProcess(cmd)
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("打不开 DevTools 窗口：%w", err)
		}
		go func() { _ = cmd.Wait() }()
		return map[string]any{"ok": true, "method": "electron", "url": pageURL}, nil
	})

}

// registerLocalServiceHandlers wires the loopback HTTP services (cloudapi, MCP/SSE).
func (a *App) registerLocalServiceHandlers(r *ipc.Router) {
	core := &engineBridge{app: a}

	// Local cloud-function HTTP forwarder (fixed default 127.0.0.1:27182 /call).
	// Start requires a running engine bridge; every /call
	// request re-enters this router so manual and HTTP calls share one path.
	r.Register("cloudapi.start", func(_ context.Context, params json.RawMessage) (any, error) {
		if _, err := core.Engine(); err != nil {
			return nil, err
		}
		var args struct {
			Port int `json:"port"`
		}
		_ = json.Unmarshal(params, &args)
		if args.Port == 0 {
			args.Port = defaultCloudAPIPort
		}
		// The pointer is reached from independent Wails goroutines; create
		// and start under the app lock so a double-start cannot leak a
		// second, untracked listener.
		a.mu.Lock()
		if a.cloudAPI == nil {
			a.cloudAPI = cloudapi.New(a.ipcDispatch)
		}
		if err := a.cloudAPI.Start(args.Port); err != nil {
			a.mu.Unlock()
			return nil, err
		}
		a.mu.Unlock()
		return map[string]any{"ok": true, "port": args.Port}, nil
	})
	r.Register("cloudapi.stop", func(context.Context, json.RawMessage) (any, error) {
		a.mu.Lock()
		server := a.cloudAPI
		a.mu.Unlock()
		if server != nil {
			server.Stop()
		}
		return map[string]any{"ok": true}, nil
	})
	r.Register("cloudapi.status", func(context.Context, json.RawMessage) (any, error) {
		a.mu.Lock()
		server := a.cloudAPI
		a.mu.Unlock()
		if server == nil {
			return map[string]any{"kind": "cloudapi", "state": "stopped", "available": false, "running": false, "port": 0}, nil
		}
		running := server.Running()
		state := "stopped"
		if running {
			state = "running"
		}
		return map[string]any{"kind": "cloudapi", "state": state, "available": running, "running": running, "port": server.Port()}, nil
	})

	// MCP: stdio mode stays a separate `WxTap.exe -mcp` launch; HTTP/SSE mode
	// serves the same tool surface on the loopback (mcp.start).
	r.Register("mcp.start", func(_ context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Port  int    `json:"port"`
			Tools string `json:"tools"`
		}
		_ = json.Unmarshal(params, &args)
		if args.Port == 0 {
			args.Port = defaultMCPPort
		}
		if args.Tools != "all" {
			args.Tools = "lean"
		}
		a.mu.Lock()
		if a.mcpSSE != nil && a.mcpSSE.Running() {
			port := a.mcpSSE.Port()
			a.mu.Unlock()
			return map[string]any{"ok": true, "already_running": true, "port": port, "url": mcpHTTPURL(port)}, nil
		}
		if a.mcpSSE == nil {
			a.mcpSSE = mcp.NewSSE(buildAppMCPServer(a, args.Tools))
		}
		if err := a.mcpSSE.Start(args.Port); err != nil {
			a.mu.Unlock()
			return nil, err
		}
		port := a.mcpSSE.Port()
		a.mu.Unlock()
		return map[string]any{"ok": true, "port": port, "url": mcpHTTPURL(port)}, nil
	})
	r.Register("mcp.stop", func(context.Context, json.RawMessage) (any, error) {
		a.mu.Lock()
		server := a.mcpSSE
		a.mu.Unlock()
		if server != nil {
			server.Stop()
		}
		return map[string]any{"ok": true}, nil
	})
	r.Register("mcp.status", func(context.Context, json.RawMessage) (any, error) {
		status := map[string]any{
			"kind":      "mcp",
			"state":     "stopped",
			"available": false,
			"running":   false,
			"mode":      "stdio",
			"command":   mcpCommandHint(),
		}
		a.mu.Lock()
		server := a.mcpSSE
		a.mu.Unlock()
		if server != nil && server.Running() {
			port := server.Port()
			status["running"] = true
			status["available"] = true
			status["state"] = "running"
			status["mode"] = "http+sse"
			status["port"] = port
			status["url"] = mcpHTTPURL(port)
			status["sseUrl"] = fmt.Sprintf("http://127.0.0.1:%d/sse", port)
		}
		return status, nil
	})

}

// registerMiscHandlers wires the remaining one-off surface and update.*.
func (a *App) registerMiscHandlers(r *ipc.Router) {
	// Remaining IPC surface: explicit, typed failures.
	// fetch.md: plain GET with the desktop UA, 10s budget, {text} response.
	r.Register("fetch.md", func(ctx context.Context, params json.RawMessage) (any, error) {
		var args struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(params, &args)
		if args.URL == "" {
			return nil, fmt.Errorf("missing url")
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, args.URL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "WxTap")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		// Markdown is rendered locally; a runaway response must not allocate
		// unbounded memory just because the endpoint omits Content-Length.
		const maxFetchMarkdownBytes = 8 << 20
		text, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchMarkdownBytes+1))
		if err != nil {
			return nil, err
		}
		if len(text) > maxFetchMarkdownBytes {
			return nil, fmt.Errorf("markdown response exceeds %d bytes", maxFetchMarkdownBytes)
		}
		return map[string]any{"text": string(text)}, nil
	})

	// update.*: the self-update chain reads one published manifest for both
	// "is there a newer version" and "where do its bytes come from", so the two
	// answers can no longer drift apart the way an OSS version file and a
	// GitHub release did.
	//
	// The asset sync below reads the repository the project is published in —
	// the WMPF address tables and the MCP skills are committed under
	// resources/, so refreshing them is a read of the newest revision, not a
	// second content channel with its own credentials.
	checker := update.New(update.AssetBase)
	r.Register("update.syncWMPF", func(context.Context, json.RawMessage) (any, error) {
		return checker.SyncWMPF(resolveSyncBaseDir()), nil
	})
	r.Register("update.syncSkills", func(context.Context, json.RawMessage) (any, error) {
		result := checker.SyncSkills(resolveSyncBaseDir())
		return map[string]any{"updated": result.Updated, "unchanged": result.Unchanged, "error": nilIfEmpty(result.Error)}, nil
	})
	// update.checkVersion / update.downloadRelease keep their v0.1.0 names —
	// testdata/compat-surface.json freezes them — but read the same
	// published manifest, so "is there a newer version" and "where do its bytes
	// come from" can no longer drift apart the way an OSS version file and a
	// GitHub release did.
	r.Register("update.checkVersion", func(ctx context.Context, _ json.RawMessage) (any, error) {
		manifest, err := update.FetchManifest(ctx, update.ManifestURLs)
		if err != nil {
			// A failed check is not an IPC failure: it is the expected outcome
			// on a hostile network, and the caller only needs to know that no
			// update was found. Reporting it as an error would make the silent
			// startup check noisy.
			return map[string]any{"current": appVersion, "latest": nil, "has_update": false, "error": err.Error()}, nil
		}
		return map[string]any{
			"current":    appVersion,
			"latest":     manifest.Version,
			"notes":      manifest.Notes,
			"total_size": manifest.TotalSize(),
			"has_update": update.HasUpdate(appVersion, manifest.Version),
		}, nil
	})
	r.Register("update.status", func(context.Context, json.RawMessage) (any, error) {
		state, staged := update.StagedVersion(dataDir())
		if !staged {
			return map[string]any{"staged": false}, nil
		}
		return map[string]any{"staged": true, "version": state.Version, "staged_at": state.StagedAt}, nil
	})
	r.Register("update.downloadRelease", func(ctx context.Context, _ json.RawMessage) (any, error) {
		if a.tasks == nil {
			return nil, fmt.Errorf("任务跟踪未初始化")
		}
		manifest, err := update.FetchManifest(ctx, update.ManifestURLs)
		if err != nil {
			return nil, err
		}
		if !update.HasUpdate(appVersion, manifest.Version) {
			return map[string]any{"staged": false, "version": manifest.Version, "reason": "当前已是最新版本"}, nil
		}
		if state, staged := update.StagedVersion(dataDir()); staged && state.Version == manifest.Version {
			return map[string]any{"staged": true, "version": state.Version}, nil
		}
		if !a.claimUpdateSlot() {
			return nil, fmt.Errorf("已有更新任务在进行中")
		}
		root := dataDir()
		// The download outlives this request, so it follows the app lifecycle
		// rather than the caller's context: quitting mid-transfer aborts it and
		// leaves no partial file behind.
		lifecycle := a.ctx
		if lifecycle == nil {
			lifecycle = context.Background()
		}
		task := a.tasks.Start("update.download", "准备下载更新", int(manifest.TotalSize()))
		a.goBackground(func() {
			defer a.releaseUpdateSlot()
			_, err := update.StageVersion(lifecycle, manifest, root, func(done, total int64) {
				_, _ = a.tasks.Update(task.ID, "running", "正在下载更新", int(done), int(total))
			})
			if err != nil {
				_, _ = a.tasks.Finish(task.ID, "failed", "下载更新失败", err.Error())
				return
			}
			_, _ = a.tasks.Finish(task.ID, "done", "更新已就绪，重启后生效", "")
		})
		return map[string]any{"staged": false, "version": manifest.Version, "async": true, "task_id": task.ID}, nil
	})
	r.Register("update.restart", func(context.Context, json.RawMessage) (any, error) {
		// The staged version is swapped in by the next process at startup, so
		// restarting means launching one and stepping aside.
		executable, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("无法定位可执行文件: %w", err)
		}
		var args []string
		if update.NeedsSecondProcess() {
			// Windows cannot replace a running executable, so the replacement has
			// to wait for this process to exit before it can swap — which also
			// stops two instances sharing one WebView2 user data directory.
			// macOS replaces a running bundle in place, so it restarts immediately.
			args = append(args, relaunchWaitFlag, strconv.Itoa(os.Getpid()))
		}
		command := exec.Command(executable, args...)
		configureGUIProcess(command)
		if err := command.Start(); err != nil {
			return nil, fmt.Errorf("无法启动新进程: %w", err)
		}
		go func() { _ = command.Wait() }()
		// Quit needs the lifecycle context, the same one EventsEmit requires.
		if ctx := a.wailsContext(); ctx != nil {
			wailsruntime.Quit(ctx)
		}
		return map[string]any{"restarting": true}, nil
	})

}

// registerMiniappHandlers wires the miniapp management and CDP targets surface.
func (a *App) registerMiniappHandlers(r *ipc.Router) {
	// Miniapp management domain: multi-open list and debug lock switching.
	// Engine-side failures are surfaced in an `error` field instead of being
	// swallowed into an empty list: 「引擎挂了」和「没打开小程序」对调用方是
	// 两种完全不同的处置，空列表会让前一种被误读成后一种。
	r.Register("miniapp.list", func(ctx context.Context, _ json.RawMessage) (any, error) {
		client, err := a.currentEngine()
		if err != nil {
			return map[string]any{"list": []any{}, "error": err.Error()}, nil
		}
		list, err := client.MiniappList(ctx)
		if err != nil {
			return map[string]any{"list": []any{}, "error": err.Error()}, nil
		}
		return map[string]any{"list": list}, nil
	})
	r.Register("miniapp.switch", func(ctx context.Context, params json.RawMessage) (any, error) {
		var args struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal(params, &args)
		client, err := a.currentEngine()
		if err != nil {
			return nil, fmt.Errorf("engine not running")
		}
		ok, err := client.MiniappSwitch(ctx, args.ID)
		if err != nil {
			return nil, err
		}
		if ok {
			// The switched-to miniapp has its own page-local sequence space:
			// restart the drain from 0 or its early records are skipped.
			a.wxapiFeeder.ResetAck()
			a.cloudFeeder.ResetAck()
		}
		return map[string]any{"ok": ok}, nil
	})
	r.Register("miniapp.setLock", func(ctx context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Enabled *bool `json:"enabled"`
		}
		_ = json.Unmarshal(params, &args)
		client, err := a.currentEngine()
		if err != nil {
			return nil, fmt.Errorf("engine not running")
		}
		enabled := true
		if args.Enabled != nil {
			enabled = *args.Enabled
		}
		if err := client.MiniappSetLock(ctx, enabled); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	})
	r.Register("miniapp.getLock", func(ctx context.Context, _ json.RawMessage) (any, error) {
		client, err := a.currentEngine()
		if err != nil {
			return map[string]any{"enabled": true}, nil
		}
		enabled, err := client.MiniappGetLock(ctx)
		if err != nil {
			return map[string]any{"enabled": true}, nil
		}
		return map[string]any{"enabled": enabled}, nil
	})

	// Targets domain: CDP target enumeration and attachment.
	r.Register("targets.list", func(ctx context.Context, _ json.RawMessage) (any, error) {
		targets := []any{}
		client, err := a.currentEngine()
		if err != nil {
			// 空数组会被界面读成「尚未检测到小程序」，与「引擎没起来 / CDP 查询
			// 失败」完全是两回事，所以把原因一起带回去。
			return map[string]any{"targets": targets, "error": "调试目标不可用：" + err.Error()}, nil
		}
		resp, err := client.CDPCommand(ctx, "Target.getTargets", map[string]any{}, 8000)
		if err != nil {
			return map[string]any{"targets": targets, "error": "读取调试目标失败：" + err.Error()}, nil
		}
		if inner, ok := resp["result"].(map[string]any); ok {
			if infos, ok := inner["targetInfos"].([]any); ok {
				targets = infos
			}
		}
		return map[string]any{"targets": targets}, nil
	})
	r.Register("targets.attach", func(ctx context.Context, params json.RawMessage) (any, error) {
		var args struct {
			TargetID string `json:"target_id"`
		}
		_ = json.Unmarshal(params, &args)
		if args.TargetID == "" {
			return nil, fmt.Errorf("未指定 targetId")
		}
		client, err := a.currentEngine()
		if err != nil {
			return nil, fmt.Errorf("engine not running")
		}
		return client.CDPCommand(ctx, "Target.attachToTarget", map[string]any{"targetId": args.TargetID}, 8000)
	})

}

// registerHookHandlers wires the user hook script surface.
func (a *App) registerHookHandlers(r *ipc.Router) {
	// Hook domain: user hook scripts and the global injection list.
	hooks := a.hookStore()
	r.Register("hook.list", func(context.Context, json.RawMessage) (any, error) {
		if hooks == nil {
			return map[string]any{"scripts": []any{}}, nil
		}
		scripts, err := hooks.List()
		if err != nil {
			return nil, err
		}
		payload := make([]map[string]any, 0, len(scripts))
		for i := range scripts {
			// Injected is a property of the current page realm, tracked by the
			// shell: the page side cannot tell us what a user script did.
			scripts[i].Injected = a.hookScriptInjected(scripts[i].Filename)
			item := map[string]any{
				"filename": scripts[i].Filename,
				"global":   scripts[i].Global,
				"injected": scripts[i].Injected,
				"mtime":    scripts[i].Mtime,
			}
			if run, ok := a.hookScriptRun(scripts[i].Filename); ok {
				item["lastRun"] = map[string]any{
					"at":         run.At,
					"ok":         run.OK,
					"summary":    run.Summary,
					"durationMs": run.DurationMs,
				}
				// 文件在最近一次注入之后又被改过：界面据此提示「重新注入」，
				// 否则用户会以为改动已经生效。
				item["stale"] = scripts[i].Mtime > run.At
			}
			payload = append(payload, item)
		}
		return map[string]any{"scripts": payload}, nil
	})
	r.Register("hook.inject", func(ctx context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Filename string `json:"filename"`
		}
		_ = json.Unmarshal(params, &args)
		if hooks == nil {
			return nil, fmt.Errorf("hook store unavailable")
		}
		source, err := hooks.Inject(args.Filename)
		if err != nil {
			return nil, err
		}
		client, err := a.currentEngine()
		if err != nil {
			return nil, fmt.Errorf("engine not running")
		}
		// EvaluateAwait，而不是 Evaluate：脚本的最后一条表达式是 Promise 时
		// （async/await 写法）只有 awaitPromise 才拿得到落定值 —— 否则注入成功、
		// 界面却恒显示「无返回值」，用户会以为脚本没跑。
		started := time.Now()
		result, err := client.EvaluateAwait(ctx, ipc.HookScriptExpression(args.Filename, source), 5000)
		elapsed := time.Since(started)
		if err != nil {
			// 失败也要留档：用户改完脚本想知道「这次为什么没生效」，靠的就是这一行。
			a.recordHookScriptRun(args.Filename, false, ipc.HookRunSummary(err.Error()), elapsed)
			return nil, err
		}
		a.markHookScriptInjected(args.Filename)
		summary := ipc.HookRunSummary(result)
		a.recordHookScriptRun(args.Filename, true, summary, elapsed)
		a.emitLog("info", fmt.Sprintf("已注入 %s：%s", args.Filename, summary))
		return map[string]any{
			"ok":         true,
			"result":     result,
			"summary":    summary,
			"durationMs": elapsed.Milliseconds(),
			"injected":   true,
		}, nil
	})
	// hook.clear drops captured records of a named hook (GUI surface).
	// The user-script list has no counterpart here: JS already evaluated in the
	// page cannot be un-evaluated, which is why hook.list tracks injection
	// instead of offering a clear.
	//
	// 顺序与 wxapi.clear / cloud.clear 一致（R10b）：先清页面缓冲（HookClear 是一次 CDP
	// 往返），再清 shell 侧的 pending。反过来的话，这段往返窗口里 500ms 一次的 drain tick
	// 会把页面缓冲里还没被清掉的记录重新 drain 回 pending —— 用户刚清空的记录立刻复活。
	r.Register("hook.clear", func(ctx context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(params, &args)
		name := args.Name
		if name == "" {
			name = "wxapi"
		}
		// 先把名字验完再动页面：不认识的 hook 不该先花掉一次 CDP 往返。
		var feeder *ipc.HookFeeder
		switch name {
		case "wxapi":
			feeder = a.wxapiFeeder
		case "cloud":
			feeder = a.cloudFeeder
		case "console":
			feeder = a.consoleFeeder
		default:
			return nil, fmt.Errorf("不支持的 hook: %s", name)
		}
		a.mu.Lock()
		client := a.engine
		a.mu.Unlock()
		if client != nil {
			_ = client.HookClear(ctx, name)
		}
		feeder.Clear()
		return map[string]any{"ok": true}, nil
	})
	r.Register("hook.setGlobal", func(_ context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Filename string `json:"filename"`
			Global   bool   `json:"global"`
		}
		_ = json.Unmarshal(params, &args)
		if hooks == nil {
			return nil, fmt.Errorf("hook store unavailable")
		}
		if err := hooks.SetGlobal(args.Filename, args.Global); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	})
	r.Register("code.projects", func(context.Context, json.RawMessage) (any, error) {
		root := a.extractOutputRoot()
		projects := ipc.DecompiledProjects(root)
		// 反编译产物可能没带名字（目录里只有 appid）：用连接时记住的名字补上。
		if names := a.appNames(); len(names) > 0 {
			for i := range projects {
				if strings.TrimSpace(projects[i].Name) == "" {
					projects[i].Name = names[projects[i].AppID]
				}
			}
		}
		return map[string]any{"root": root, "projects": projects}, nil
	})
	r.Register("code.project", func(_ context.Context, params json.RawMessage) (any, error) {
		var args struct {
			AppID string `json:"appid"`
		}
		_ = json.Unmarshal(params, &args)
		if err := ipc.ValidateAppID(args.AppID); err != nil {
			return nil, err
		}
		target := a.decompileOutputDir(args.AppID)
		info, err := os.Stat(target)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("反编译产物不存在: %s", args.AppID)
		}
		tree := ipc.BuildFileTree(target, 1)
		if tree == nil {
			tree = []ipc.FileNode{}
		}
		return map[string]any{"tree": tree, "root": target}, nil
	})
	r.Register("code.openDir", func(_ context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(params, &args)
		target := strings.TrimSpace(args.Path)
		if target == "" {
			return nil, fmt.Errorf("目录不能为空")
		}
		info, err := os.Stat(target)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("目录不存在: %s", target)
		}
		return map[string]any{"tree": ipc.BuildFileTree(target, 1), "root": target}, nil
	})
	r.Register("code.expandDir", func(_ context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(params, &args)
		children := []ipc.FileNode{}
		if info, err := os.Stat(args.Path); err == nil && info.IsDir() {
			if listed := ipc.ExpandDir(args.Path); listed != nil {
				children = listed
			}
		}
		return map[string]any{"children": children}, nil
	})
	r.Register("code.readFile", func(_ context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(params, &args)
		return ipc.ReadCodeFile(args.Path), nil
	})
	r.Register("code.search", func(_ context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Root  string `json:"root"`
			Query string `json:"query"`
			Regex bool   `json:"regex"`
		}
		_ = json.Unmarshal(params, &args)
		hits, truncated, err := ipc.SearchCode(args.Root, args.Query, args.Regex)
		if err != nil {
			return map[string]any{"results": []ipc.SearchHit{}, "truncated": false, "error": err.Error()}, nil
		}
		if hits == nil {
			hits = []ipc.SearchHit{}
		}
		// truncated 让面板把「已到 500 条上限」说出来，而不是把一份不完整的结果
		// 当成完整的搜索结果。
		return map[string]any{"results": hits, "truncated": truncated}, nil
	})
	r.Register("code.formatFile", func(_ context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(params, &args)
		info, err := os.Stat(args.Path)
		if err != nil || info.IsDir() {
			return map[string]any{"error": "文件不存在"}, nil
		}
		language, ok := ipc.FormatLanguage(filepath.Ext(args.Path))
		if !ok {
			content, readErr := os.ReadFile(args.Path)
			if readErr != nil {
				return nil, readErr
			}
			return map[string]any{"content": string(content)}, nil
		}
		a.goBackground(func() { a.formatCodeFile(args.Path, language) })
		return map[string]any{"ok": true, "async": true}, nil
	})
	r.Register("code.formatAll", func(_ context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Root string `json:"root"`
		}
		_ = json.Unmarshal(params, &args)
		info, err := os.Stat(args.Root)
		if err != nil || !info.IsDir() {
			return map[string]any{"error": "目录不存在"}, nil
		}
		a.goBackground(func() { a.formatCodeDir(args.Root) })
		return map[string]any{"ok": true, "async": true}, nil
	})
}

// registerConsoleHandlers wires the miniapp console capture surface. Records
// are drained from the page-side console hook into the console feeder (see
// console_log.go for the collector), which moves them into a bounded ring the
// panel reads by sequence number. Sequence-based reads are what keep the panel
// free of duplicates: it asks for everything after the last row it already
// has, instead of merging a live event stream with a poll.
func (a *App) registerConsoleHandlers(r *ipc.Router) {
	r.Register("console.list", func(_ context.Context, params json.RawMessage) (any, error) {
		var args struct {
			AfterSeq int64 `json:"afterSeq"`
			Limit    int   `json:"limit"`
			// Tail 请求「最新 N 条」：从 0 开始的 List 在环形缓冲写满之后
			// 返回的是最旧的一批，与「最近输出」正好相反。
			Tail int `json:"tail"`
		}
		_ = json.Unmarshal(params, &args)
		if args.Tail > 0 {
			// 环形缓冲本身就只有 consoleCapacity 条：更大的 tail 只是把同一个
			// 上限再问一遍，钳住它免得调用方把无界的 N 当成合法输入。
			if args.Tail > consoleCapacity {
				args.Tail = consoleCapacity
			}
			records := a.consoleLog.Tail(args.Tail)
			nextSeq := int64(0)
			if len(records) > 0 {
				nextSeq = records[len(records)-1].Seq
			}
			return map[string]any{"records": records, "nextSeq": nextSeq, "hasMore": false, "dropped": 0}, nil
		}
		if args.Limit <= 0 {
			args.Limit = 200
		}
		if args.Limit > consoleCapacity {
			args.Limit = consoleCapacity
		}
		records, nextSeq, hasMore, dropped := a.consoleLog.List(args.AfterSeq, args.Limit)
		// dropped 是本页之前被环形缓冲淘汰的条数：前端据此提示「有 N 条已丢弃」，
		// 而不是静默显示一段缺口。
		return map[string]any{"records": records, "nextSeq": nextSeq, "hasMore": hasMore, "dropped": dropped}, nil
	})
	r.Register("console.clear", func(ctx context.Context, _ json.RawMessage) (any, error) {
		// 与 wxapi/cloud.clear 同一款 R10b 顺序：先停页面侧来源（HookClear 是
		// 一次 CDP 往返，期间 feeder 的 drain tick 会把页面缓冲里还没清掉的记录
		// 补走），再清 shell 侧——反过来刚清掉的行会立刻"复活"。
		a.mu.Lock()
		client := a.engine
		a.mu.Unlock()
		if client != nil {
			_ = client.HookClear(ctx, "console")
		}
		a.consoleFeeder.Clear()
		a.consoleLog.Clear()
		return map[string]any{"ok": true}, nil
	})
}

// formatCodeFile formats one file through the Core's code.format and writes
// it back, reporting the outcome on code_file_formatted (async shape).
func (a *App) formatCodeFile(path, language string) {
	event := func(payload map[string]any) {
		a.emit("code_file_formatted", payload)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		event(map[string]any{"path": path, "error": err.Error()})
		return
	}
	client, err := a.currentEngine()
	if err != nil {
		event(map[string]any{"path": path, "error": err.Error()})
		return
	}
	formatted, err := client.CodeFormat(context.Background(), string(content), language)
	if err != nil {
		event(map[string]any{"path": path, "error": err.Error()})
		return
	}
	if err := os.WriteFile(path, []byte(formatted), 0o600); err != nil {
		event(map[string]any{"path": path, "error": err.Error()})
		return
	}
	event(map[string]any{"path": path, "content": formatted})
}

// formatCodeDir walks root formatting every whitelisted file and reports the
// count on code_format_done (failures skip files silently).
func (a *App) formatCodeDir(root string) {
	count := 0
	client, err := a.currentEngine()
	if err == nil {
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !ipc.FormatAllExtension(filepath.Ext(path)) {
				return nil
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			language, _ := ipc.FormatLanguage(filepath.Ext(path))
			formatted, err := client.CodeFormat(context.Background(), string(content), language)
			if err != nil {
				return nil
			}
			if err := os.WriteFile(path, []byte(formatted), 0o600); err != nil {
				return nil
			}
			count++
			return nil
		})
	} else {
		a.emitLog("error", "格式化失败: "+err.Error())
	}
	a.emit("code_format_done", map[string]any{"count": count})
}

// recordLog persists one runtime log line to the operation file and the replay
// ring, and returns the line's stamp and ring sequence. emitLog broadcasts on
// top of it; startup uses recordLog directly because EventsEmit is not usable
// before the frontend lifecycle is up.
func (a *App) recordLog(level, message string) (string, int64) {
	if a.logger != nil {
		a.logger.Log(level, message)
	}
	stamp := time.Now().Format("15:04:05.000")
	return stamp, a.runtimeLog.Append(stamp, level, message)
}

// emitLog pushes the `log` event the frontend control view renders. The line
// lands in the runtime ring before the event goes out, so a replay snapshot
// always covers what was delivered, and the event's seq is what lets the
// frontend dedupe the replay against the live stream. It goes through a.emit
// like every other event: the direct EventsEmit call it used before bypassed
// the emitSink seam tests rely on.
func (a *App) emitLog(level, message string) {
	stamp, seq := a.recordLog(level, message)
	a.emit("log", map[string]any{
		"time":    stamp,
		"level":   level,
		"message": message,
		"seq":     seq,
	})
}

// registerLogHandlers exposes the runtime log ring to the frontend. The panel
// replays it once right after subscribing (log.list with tail), which is how
// lines emitted before the webview mounted still show up, and clears it so the
// 清空 button also wipes the backend copy instead of only the view.
func (a *App) registerLogHandlers(r *ipc.Router) {
	r.Register("log.list", func(_ context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Tail int `json:"tail"`
		}
		_ = json.Unmarshal(params, &args)
		records, nextSeq, dropped := a.runtimeLog.Tail(args.Tail)
		return map[string]any{"records": records, "nextSeq": nextSeq, "dropped": dropped}, nil
	})
	r.Register("log.clear", func(_ context.Context, _ json.RawMessage) (any, error) {
		a.runtimeLog.Clear()
		return map[string]any{"ok": true}, nil
	})
}

// hookStore wires the user hook script directory to the config base; nil
// when no user base directory is available. The instance is cached: each
// HookStore owns a ConfigStore over config.json, and a second instance would
// do read-modify-write with its own mutex — concurrent saves from the two
// instances overwrite each other's patch (lost update).
func (a *App) hookStore() *ipc.HookStore {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hookStoreOnce != nil {
		return a.hookStoreOnce
	}
	if a.dataBase == "" {
		return nil
	}
	// hook_scripts/ lives next to the data: it is the only place the user drops
	// scripts into, so the hook page lists exactly what that directory holds.
	a.hookStoreOnce = ipc.NewHookStore(a.dataBase)
	return a.hookStoreOnce
}

// offlineEngineStatus is what engine.status reports when the Core cannot be
// reached — returns an all-false status instead of an error, so the
// control view renders an idle engine rather than a failure banner.
func offlineEngineStatus() map[string]any {
	return map[string]any{"kind": "engine", "state": "offline", "available": false, "frida": false, "miniapp": false, "devtools": false}
}

func engineStatusPayload(status engine.EngineStatus) map[string]any {
	state := "ready"
	if !status.Frida {
		state = "starting"
	}
	if status.Generation == 0 && !status.Frida && !status.Miniapp && !status.Devtools {
		state = "stopped"
	}
	return map[string]any{"kind": "engine", "state": state, "available": status.Frida, "frida": status.Frida, "miniapp": status.Miniapp, "devtools": status.Devtools, "generation": status.Generation, "appInfo": status.AppInfo}
}

// appInfoEvent is the top-bar identity update. A cleared miniapp must emit
// empty fields; otherwise the shell keeps the previous name after disconnect.
func appInfoEvent(previous, current *engine.AppInfo) (map[string]any, bool) {
	if previous == nil && current == nil {
		return nil, false
	}
	if previous != nil && current != nil && *previous == *current {
		return nil, false
	}
	if current == nil {
		return map[string]any{"appid": "", "name": "", "icon": ""}, true
	}
	return map[string]any{"appid": current.AppID, "name": current.Name, "icon": current.Icon}, true
}

func shouldAutoInjectHooks(last, current engine.EngineStatus) bool {
	if !current.Miniapp {
		return false
	}
	if current.Generation > 0 {
		return current.Generation != last.Generation
	}
	return !last.Miniapp
}

// startStatusPoller pushes the
// `status` and `app_info` events as the engine reports them and auto-injects
// the cloud/wxapi hooks when a miniapp connects.
func (a *App) startStatusPoller() {
	if a.ctx == nil {
		// No Wails runtime (yet): events would go nowhere and the Done
		// channel below would dereference a nil ctx. Production startup
		// assigns ctx before calling setupIPC.
		return
	}
	// The poller owns a cancellable child context so shutdown can stop it
	// before draining tracked background work (a poller tick must not add to
	// the WaitGroup while shutdown waits on it).
	pollCtx, cancel := context.WithCancel(a.ctx)
	a.mu.Lock()
	a.pollerCancel = cancel
	a.mu.Unlock()
	go func() {
		var last engine.EngineStatus
		var reportedDown *engine.Client
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pollCtx.Done():
				return
			case <-ticker.C:
			}
			// Non-spawning access: a nil engine means nothing has started one yet.
			a.mu.Lock()
			current := a.engine
			a.mu.Unlock()
			if current == nil {
				continue
			}
			select {
			case <-current.WaitExited():
				// Report an unexpected Core exit once per process, with the
				// last stderr lines so the reason is observable.
				if reportedDown != current {
					reportedDown = current
					message := "Core 进程已退出，引擎不可用"
					if stderrLines := current.RecentStderr(); len(stderrLines) > 0 {
						tail := stderrLines
						if len(tail) > 5 {
							tail = tail[len(tail)-5:]
						}
						message += "：" + strings.Join(tail, " | ")
					}
					a.emitLog("error", message)
					// 界面的「运行中」pill 只由 status 事件驱动：进程死了却不下发
					// 全 false，UI 会停在运行中直到引擎被某次调用重新拉起。
					a.emit("status", engineStatusPayload(engine.EngineStatus{}))
				}
				if payload, ok := appInfoEvent(last.AppInfo, nil); ok {
					a.emit("app_info", payload)
				}
				last = engine.EngineStatus{}
				continue
			default:
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			status, err := current.Status(ctx)
			cancel()
			if err != nil {
				continue
			}
			if payload, ok := appInfoEvent(last.AppInfo, status.AppInfo); ok {
				a.emit("app_info", payload)
				// 身份首次上报（或变化）时把名字记进 config：反编译产物与流量
				// 库都只有 appid，这里是小程序名唯一的持久来源。
				a.rememberAppName(status.AppInfo)
			}
			if status.Frida != last.Frida || status.Miniapp != last.Miniapp || status.Devtools != last.Devtools {
				a.emit("status", engineStatusPayload(status))
			}
			if shouldAutoInjectHooks(last, status) {
				a.goBackground(a.autoInjectHooks)
			}
			last = status
		}
	}()
}

// autoInjectHooks runs connect-time injection: wait briefly for
// the page to settle, then install the hooks that are not a capture decision.
// Each successful install restarts the feeder's ack: the Core-side sequence
// space lives in the page context, so a reload reset it to 1 while the ack kept
// growing and would skip every new record.
//
// The audit hooks (wxapi / cloud) are gated on the feeder actually running.
// Connecting is not a capture decision: installing them unconditionally made
// the page buffer every wx.* call from the moment the miniapp connected, while
// the shell-side drain only starts when the user presses 开启捕获 - so the first
// press delivered that whole backlog as if it were new traffic. The contract is
// pinned by capture_lifecycle_contract_test.go.
//
// A generation change means the page realm was rebuilt, which is also the
// moment every piece of shell state that described the old realm has to be
// dropped: the navigator's injection cache (window.nav died with the realm, so
// keeping the cache made every navigation call fail until the user fetched the
// routes by hand), the injected-script registry, and the user scripts marked
// global, which are injected again here. A *running* capture has to be
// re-hooked here for the same reason: its page-side sequence space restarted at
// 1 with the new realm, so the ack has to follow.
func (a *App) autoInjectHooks() {
	time.Sleep(500 * time.Millisecond)
	core := &engineBridge{app: a}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// 新 realm 的页内序列空间必然从 1 重计：只要捕获还在跑，ack 就必须跟着
	// 回零——安装失败时也一样。否则之后装上（重试或下一次 generation）的记录
	// 会被旧游标过滤，面板显示"捕获中"却永远没有新记录。
	if a.cloudFeeder.Running() {
		if report, err := a.installHookForRealm(ctx, core, "cloud", ipc.InstallReportOK); err != nil {
			a.emitLog("error", "cloud 钩子安装失败："+err.Error())
		} else if !ipc.InstallReportOK(report) {
			a.emitLog("error", "cloud 钩子安装失败："+ipc.InstallFailureReason(report))
		}
		a.cloudFeeder.ResetAck()
	}
	// wxapi 要看安装结果：页面里没有可注入的 wx 时 install() 回的是
	// {ok:false, reason}，静默成功会让面板显示"捕获中"却永远没有记录。
	if a.wxapiFeeder.Running() {
		if report, err := a.installHookForRealm(ctx, core, "wxapi", ipc.InstallReportOK); err != nil {
			a.emitLog("error", "wxapi 钩子安装失败："+err.Error())
		} else if !ipc.InstallReportOK(report) {
			a.emitLog("error", "wxapi 钩子安装失败："+ipc.InstallFailureReason(report))
		}
		a.wxapiFeeder.ResetAck()
	}
	// 控制台采集要一直开着，所以这里既拉起循环也重装脚本：feeder 的 Start()
	// 只在首次真正注入（已 running 就直接返回），光靠它会让重建后的 realm
	// 没有 console hook，而 drain 循环还在轮流询一个空缓冲。
	if report, err := a.installHookForRealm(ctx, core, "console", func(map[string]any) bool { return true }); err == nil {
		// 页内 hook 只负责未捕获异常与 Promise 拒绝：当前 WMPF 把小程序的 console
		// 做成了不可配置的访问器，console.* 包不上（回执 ok:false），那部分由 Core
		// 的 CDP 事件采集兜底 —— 所以这里是 info 而不是警告，写清楚各自负责什么，
		// 免得排障时被一条「未生效」误导。
		if ok, _ := report["ok"].(bool); !ok {
			a.emitLog("info", fmt.Sprintf("页内 console 不可包装（小程序锁住了 console）：console.* 由 CDP 事件采集，页内 hook 只补未捕获异常与 Promise 拒绝：%v", report))
		}
		if startErr := a.consoleFeeder.Start(); startErr == nil {
			a.consoleFeeder.ResetAck()
		} else {
			a.emitLog("error", "console 采集循环启动失败："+startErr.Error())
		}
	} else {
		a.emitLog("error", "console 钩子安装失败："+err.Error())
		// 理由同上：安装失败也要回零游标，CDP 事件源仍在采集，页内钩子稍后
		// 装上时记录不能被旧游标挡在门外。
		a.consoleFeeder.ResetAck()
	}
	if err := core.InstallHook(ctx, "navigator"); err == nil {
		a.navigatorCtl.Invalidate()
	}
	a.resetInjectedHookScripts()
	a.injectGlobalHookScripts(ctx, core)
}

// installHookForRealm installs a hook on a freshly rebuilt realm, retrying a
// couple of times: the new realm may still be booting when the generation
// change fires, and a single blind attempt would leave a running capture dark
// until the user manually stopped and restarted it.
//
// accept decides whether an answered install report is final. wxapi/cloud use
// ipc.InstallReportOK; the console hook passes an always-true predicate — its
// {ok:false} ("the page locked the console") is a definitive answer, not a
// boot race, so retrying would just stall the feeder start by two rounds.
func (a *App) installHookForRealm(ctx context.Context, core *engineBridge, name string, accept func(map[string]any) bool) (map[string]any, error) {
	var report map[string]any
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return report, err
			case <-time.After(2 * time.Second):
			}
		}
		report, err = core.InstallHookReport(ctx, name)
		if err == nil && accept(report) {
			return report, nil
		}
	}
	return report, err
}

// injectGlobalHookScripts injects every user script marked global into the
// realm that was just built. Nothing read the global flag before this: the
// checkbox wrote a name into the config that no code ever consulted, so a
// "global" script was only ever injected when the user clicked 注入 by hand.
func (a *App) injectGlobalHookScripts(ctx context.Context, core *engineBridge) {
	hooks := a.hookStore()
	if hooks == nil {
		return
	}
	scripts, err := hooks.List()
	if err != nil {
		return
	}
	for _, script := range scripts {
		if !script.Global {
			continue
		}
		source, err := hooks.Inject(script.Filename)
		if err != nil {
			a.emitLog("warn", fmt.Sprintf("全局脚本 %s 读取失败：%v", script.Filename, err))
			a.recordHookScriptRun(script.Filename, false, ipc.HookRunSummary(err.Error()), -1)
			continue
		}
		// 与手动注入走同一条包装，但每个脚本只给 2s（手动注入是 5s）：这段循环和
		// cloud / wxapi / console / navigator 的安装共享 autoInjectHooks 的 15s 预算，
		// 一个挂在未落定 Promise 上的脚本不该把后面的脚本全饿死。失败仍会留档。
		started := time.Now()
		scriptCtx, cancelScript := context.WithTimeout(ctx, 2*time.Second)
		result, err := core.EvaluateAwait(scriptCtx, ipc.HookScriptExpression(script.Filename, source), 2000)
		cancelScript()
		elapsed := time.Since(started)
		if err != nil {
			a.emitLog("warn", fmt.Sprintf("全局脚本 %s 注入失败：%v", script.Filename, err))
			a.recordHookScriptRun(script.Filename, false, ipc.HookRunSummary(err.Error()), elapsed)
			continue
		}
		a.markHookScriptInjected(script.Filename)
		a.recordHookScriptRun(script.Filename, true, ipc.HookRunSummary(result), elapsed)
		a.emitLog("info", fmt.Sprintf("已自动注入全局脚本 %s", script.Filename))
	}
}

// markHookScriptInjected records a successful user-script injection into the
// current page realm.
func (a *App) markHookScriptInjected(filename string) {
	a.injectedScriptsMu.Lock()
	defer a.injectedScriptsMu.Unlock()
	if a.injectedScripts == nil {
		a.injectedScripts = map[string]struct{}{}
	}
	a.injectedScripts[filename] = struct{}{}
}

// hookScriptInjected reports whether the script was injected into the current
// page realm. The page side cannot answer this: what a user script does is
// invisible to the shell, so "was it injected" is a fact only the shell knows.
func (a *App) hookScriptInjected(filename string) bool {
	a.injectedScriptsMu.Lock()
	defer a.injectedScriptsMu.Unlock()
	_, ok := a.injectedScripts[filename]
	return ok
}

// hookRun is the outcome of one injection attempt, as the hook page shows it.
// DurationMs 为负表示耗时未知（脚本没跑起来，比如文件读不出来）—— 记成 0 会让
// 界面显示「0ms」，看起来像跑过一样快。
type hookRun struct {
	At         int64
	OK         bool
	Summary    string
	DurationMs int64
}

// recordHookScriptRun keeps the outcome of the latest injection attempt. 失败也留档：
// 「为什么这次没生效」是用户改脚本时最需要的信息，只留一个成功标记等于什么都没说。
// elapsed < 0 表示耗时未知。
func (a *App) recordHookScriptRun(filename string, ok bool, summary string, elapsed time.Duration) {
	durationMs := int64(-1)
	if elapsed >= 0 {
		durationMs = elapsed.Milliseconds()
	}
	a.hookRunsMu.Lock()
	defer a.hookRunsMu.Unlock()
	if a.hookRuns == nil {
		a.hookRuns = map[string]hookRun{}
	}
	a.hookRuns[filename] = hookRun{
		At:         time.Now().Unix(),
		OK:         ok,
		Summary:    summary,
		DurationMs: durationMs,
	}
}

// hookScriptRun reports the latest injection attempt for one script.
func (a *App) hookScriptRun(filename string) (hookRun, bool) {
	a.hookRunsMu.Lock()
	defer a.hookRunsMu.Unlock()
	run, ok := a.hookRuns[filename]
	return run, ok
}

// resetInjectedHookScripts drops the registry after the page realm was
// rebuilt: the injected JS died with the old realm.
//
// hookRuns 刻意不清：那是「上次注入的结局」，跨 realm 仍是事实。
func (a *App) resetInjectedHookScripts() {
	a.injectedScriptsMu.Lock()
	defer a.injectedScriptsMu.Unlock()
	a.injectedScripts = nil
}

func (a *App) currentRouteHandler(ctx context.Context, _ json.RawMessage) (any, error) {
	route, err := a.navigatorCtl.CurrentRoute(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"route": route}, nil
}

func (a *App) extractOutputRoot() string {
	if base, err := userBaseDir(); err == nil {
		return filepath.Join(base, "output")
	}
	return "output"
}

// customPatterns reads the user-defined scan patterns (name -> regex) from
// the config store; missing or malformed entries yield nil.
func (a *App) customPatterns() map[string]string {
	if a.configStore == nil {
		return nil
	}
	config, err := a.configStore.Load()
	if err != nil {
		return nil
	}
	raw, ok := config["custom_patterns"].(map[string]any)
	if !ok {
		return nil
	}
	patterns := map[string]string{}
	for name, value := range raw {
		if expr, ok := value.(string); ok && strings.TrimSpace(expr) != "" {
			patterns[name] = expr
		}
	}
	return patterns
}

func (a *App) decompileOutputDir(appID string) string {
	return filepath.Join(a.extractOutputRoot(), appID)
}

// appNames returns the remembered appid → display-name registry (config.json).
func (a *App) appNames() map[string]string {
	if a.configStore == nil {
		return nil
	}
	return a.configStore.AppNames()
}

// rememberAppName persists the display name of the connected mini program so
// pages that only know the appid (traffic history, decompiled outputs without
// embedded metadata) can still show a readable name. Best-effort: a failed
// write costs a label, nothing else.
func (a *App) rememberAppName(info *engine.AppInfo) {
	if info == nil {
		return
	}
	_ = a.configStore.RememberAppName(info.AppID, info.Name)
}

// userBaseDir is the writable root for everything the shell persists: the
// config, the logs, the traffic database, staged updates and user hook scripts.
//
// On macOS it must not be the executable's own directory. That is
// Contents/MacOS inside the .app bundle, where writing would break the code
// signature and fail outright for a read-only bundle — one run from a mounted
// .dmg, a managed Mac, or /Applications without admin rights. Apple's location
// for this is ~/Library/Application Support.
func userBaseDir() (string, error) {
	if override := os.Getenv("WXTAP_DATA_DIR"); override != "" {
		return override, nil
	}
	if runtime.GOOS == "darwin" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir := filepath.Join(home, "Library", "Application Support", "WxTap")
		// Resolved here rather than left to each writer: this is the one place
		// that knows where the directory is, and every writer needs it to exist.
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return "", err
		}
		return dir, nil
	}
	return exeDir(), nil
}

// dataDir is the writable root the update chain stages into. Staging outside
// the install directory keeps a read-only install (Program Files) updatable,
// and keeps a half-unpacked tree from being a broken install.
func dataDir() string {
	if dir, err := userBaseDir(); err == nil {
		return dir
	}
	return exeDir()
}

// claimUpdateSlot takes the single staging slot, reporting false when a
// download is already running.
func (a *App) claimUpdateSlot() bool {
	a.updateRunningMu.Lock()
	defer a.updateRunningMu.Unlock()
	if a.updateRunning {
		return false
	}
	a.updateRunning = true
	return true
}

func (a *App) releaseUpdateSlot() {
	a.updateRunningMu.Lock()
	a.updateRunning = false
	a.updateRunningMu.Unlock()
}

func openInExplorer(path string) error {
	command, args := fileManagerCommand(runtime.GOOS, path)
	cmd := exec.Command(command, args...)
	configureGUIProcess(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// fileManagerCommand picks the platform file manager: explorer on Windows,
// open on macOS, xdg-open
// elsewhere. Names are injectable so the platform mapping stays testable on
// any host.
func fileManagerCommand(goos, path string) (string, []string) {
	switch goos {
	case "darwin":
		return "open", []string{path}
	case "windows":
		return "explorer", []string{path}
	default:
		return "xdg-open", []string{path}
	}
}

// mcpHTTPURL is the Streamable HTTP endpoint MCP clients connect to. It is
// the primary URL: current clients speak /mcp, the /sse endpoint stays beside it.
func mcpHTTPURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/mcp", port)
}

// mcpCommandHint shows how to launch this build's stdio MCP server. The
// binary name differs per platform (WxTap.exe on Windows, WxTap in a mac
// bundle), so the hint follows the running executable.
func mcpCommandHint() string {
	name := filepath.Base(os.Args[0])
	if name == "" || name == "." {
		name = "WxTap"
	}
	return name + " -mcp"
}

// devtoolsInspectorURL is the Chromium inspector address. It is not a web page:
// Chrome and Edge open it from their own bundled DevTools.
func devtoolsInspectorURL(cdpPort int, targetID string) string {
	if cdpPort == 0 {
		cdpPort = defaultCDPPort
	}
	ws := fmt.Sprintf("127.0.0.1:%d", cdpPort)
	if safeDevtoolsTargetID(targetID) {
		ws += "/devtools/page/" + targetID
	}
	return "devtools://devtools/bundled/inspector.html?ws=" + ws
}

func safeDevtoolsTargetID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

func electronLaunch(goos, electronPath, script, url string) (string, []string, error) {
	resolved, err := resolveElectronPath(electronPath)
	if err != nil {
		return "", nil, err
	}
	electronPath = resolved
	// Windows and Linux exec the configured path directly; existence is the
	// caller's concern and exec reports a precise failure.
	if goos != "darwin" {
		return electronPath, []string{script, url}, nil
	}
	info, err := os.Stat(electronPath)
	if err != nil {
		return "", nil, err
	}
	if info.IsDir() {
		if strings.HasSuffix(electronPath, ".app") {
			return "open", []string{"-a", electronPath, "--args", script, url}, nil
		}
		// Accept both the bundle's Contents/MacOS directory and a directory
		// that contains one.
		for _, candidate := range []string{
			filepath.Join(electronPath, "Electron"),
			filepath.Join(electronPath, "Contents", "MacOS", "Electron"),
		} {
			if stat, err := os.Stat(candidate); err == nil && !stat.IsDir() {
				return candidate, []string{script, url}, nil
			}
		}
		return "", nil, fmt.Errorf("%s 不是可运行的 Electron：请指向 Electron.app、其 Contents/MacOS 目录或可执行文件", electronPath)
	}
	return electronPath, []string{script, url}, nil
}

// resolveElectronPath maps a Windows command script onto the binary it would
// run. electron.cmd starts cmd.exe, which is the black console window, and the
// npm-installed layouts put the real electron.exe nowhere near the shim: a
// global install keeps it under node_modules\electron\dist, a project-local one
// under the sibling package for node_modules\.bin.
func resolveElectronPath(electronPath string) (string, error) {
	lower := strings.ToLower(electronPath)
	if !strings.HasSuffix(lower, ".cmd") && !strings.HasSuffix(lower, ".bat") && !strings.HasSuffix(lower, ".ps1") {
		return electronPath, nil
	}
	dir := filepath.Dir(electronPath)
	for _, candidate := range []string{
		filepath.Join(dir, "electron.exe"),
		filepath.Join(dir, "dist", "electron.exe"),
		// npm --global: <prefix>\electron.cmd + <prefix>\node_modules\electron\dist\electron.exe
		filepath.Join(dir, "node_modules", "electron", "dist", "electron.exe"),
		// npm project-local: <proj>\node_modules\.bin\electron.cmd + <proj>\node_modules\electron\dist\electron.exe
		filepath.Join(dir, "..", "electron", "dist", "electron.exe"),
	} {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s 是命令行脚本，打开时只会弹出黑窗。请改为 electron.exe", electronPath)
}

// registerSessionKeyHandlers wires the sessionkey.* multi-user scan surface.
func (a *App) registerSessionKeyHandlers(r *ipc.Router) {
	r.Register("sessionkey.detect", func(_ context.Context, _ json.RawMessage) (any, error) {
		dir := ipc.DefaultUsersDir()
		exists := dir != ""
		if exists {
			if _, err := os.Stat(dir); err != nil {
				exists = false
			}
		}
		return map[string]any{"dir": dir, "exists": exists}, nil
	})
	r.Register("sessionkey.users", func(_ context.Context, _ json.RawMessage) (any, error) {
		dir := ipc.DefaultUsersDir()
		var users []ipc.SessionKeyUser
		if dir != "" {
			users = ipc.DetectedUsers(dir)
		}
		if users == nil {
			users = []ipc.SessionKeyUser{}
		}
		return map[string]any{"users": users, "dir": dir}, nil
	})
	r.Register("sessionkey.scan", func(_ context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Dir string `json:"dir"`
		}
		if err := json.Unmarshal(params, &args); err != nil || args.Dir == "" {
			return nil, fmt.Errorf("dir is required")
		}
		// Restrict scans to every platform-specific WeChat users tree; macOS
		// has two valid roots, so relying on DefaultUsersDir would reject one.
		if !ipc.IsUsersDir(args.Dir) {
			return nil, fmt.Errorf("dir must be within the WeChat users directory")
		}
		findings := ipc.ScanUser(args.Dir)
		if findings == nil {
			findings = []ipc.SessionKeyFinding{}
		}
		return map[string]any{"findings": findings}, nil
	})
	// sessionkey.scanTraffic pulls session_key / iv / encryptedData out of the
	// packets the hook drainers already ingested into the traffic store.
	r.Register("sessionkey.scanTraffic", func(ctx context.Context, params json.RawMessage) (any, error) {
		var args struct {
			Limit int `json:"limit"`
		}
		_ = json.Unmarshal(params, &args)
		if a.repo == nil {
			return nil, fmt.Errorf("存储初始化失败，流量记录不可用")
		}
		if ctx == nil {
			ctx = context.Background()
		}
		queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		records, err := a.repo.RecentRecordsAllApps(queryCtx, args.Limit)
		if err != nil {
			return nil, err
		}
		findings := ipc.ScanTrafficRecords(queryCtx, records)
		if findings == nil {
			findings = []ipc.SessionKeyFinding{}
		}
		return map[string]any{"findings": findings, "scanned": len(records)}, nil
	})
	// sessionkey.scanDecompiled scans the restored mini-program output tree
	// (extract output root) for crypto material left in shipped sources.
	r.Register("sessionkey.scanDecompiled", func(_ context.Context, _ json.RawMessage) (any, error) {
		findings := ipc.ScanDecompiled(a.extractOutputRoot())
		if findings == nil {
			findings = []ipc.SessionKeyFinding{}
		}
		return map[string]any{"findings": findings}, nil
	})
}
