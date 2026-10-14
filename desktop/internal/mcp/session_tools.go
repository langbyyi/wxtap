package mcp

// 会话组合工具：把引导顺序（node → wechat → engine_start → 选目标 →
// hook_start）收成一次调用，把「状态快照」收成一次调用。这是 agent 效率的
// 最大杠杆点：引导顺序是 agent 最容易出错的地方（连接不等于采集），组合工具
// 把顺序固化在服务端，每步失败都带归因。

import (
	"context"
	"encoding/json"
	"fmt"
)

// sessionTools lists the composite session tools the dispatcher routes here.
var sessionTools = map[string]bool{
	"session_start": true, "session_status": true,
}

func (s *Server) callSessionTool(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	if !sessionTools[name] {
		return nil, nil
	}
	args := map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("invalid tool arguments: %w", err)
		}
	}
	if name == "session_start" {
		return s.sessionStart(ctx, args)
	}
	return s.sessionStatus(ctx)
}

// sessionStart runs the bootstrap in order and stops at the first hard
// failure, returning everything gathered so far plus the failing step — the
// caller sees WHY it stopped, not just an error string.
func (s *Server) sessionStart(ctx context.Context, args map[string]any) (any, error) {
	out := map[string]any{}

	// 1. 前置：两个诊断工具从不报错，原样带上。
	out["node"], _ = s.appCall(ctx, "node.status", map[string]any{})
	wechat, wechatErr := s.appCall(ctx, "wechat.status", map[string]any{})
	out["wechat"] = wechat
	if wechatErr != nil {
		out["wechat"] = map[string]any{"error": wechatErr.Error()}
	}

	// 2. 附加引擎（幂等）。失败即停：没有引擎后面全是空转。
	if _, err := s.appCall(ctx, "engine.start", map[string]any{"cdpPort": intArg(args, "cdpPort", 0)}); err != nil {
		out["ok"] = false
		out["step"] = "engine_start"
		out["error"] = err.Error()
		return out, nil
	}

	// 3. 选目标：显式 id > 唯一连接自动锁定 > 多开未指定（返回清单让调用者挑）。
	listed, err := s.appCall(ctx, "miniapp.list", map[string]any{})
	if err != nil {
		out["ok"] = false
		out["step"] = "miniapp_list"
		out["error"] = err.Error()
		return out, nil
	}
	// IPC 实际返回 {"list":[...]}；后两种形状是给测试/未来改动留的兜底。
	miniapps := anySlice(objectMap(listed)["list"])
	if len(miniapps) == 0 {
		miniapps = anySlice(objectMap(listed)["miniapps"])
	}
	if len(miniapps) == 0 {
		miniapps = anySlice(listed)
	}
	entries := make([]map[string]any, 0, len(miniapps))
	for _, item := range miniapps {
		entries = append(entries, objectMap(item))
	}
	out["available"] = entries
	// IPC 层把引擎侧故障放在 error 字段（见 miniapp.list 注册处）：零目标时
	// 带上它归因，调用方才知道该处理引擎还是去开小程序窗口。
	engineError, _ := objectMap(listed)["error"].(string)
	if engineError != "" {
		out["engine_error"] = engineError
	}

	lockedID := 0
	for _, entry := range entries {
		if objectMap(entry)["locked"] == true {
			lockedID = entryID(objectMap(entry))
			break
		}
	}
	// target 与当前锁定一致时不再 switch：重跑 switch 会重置页面 generation 与
	// feeder ack，等于把一个正在工作的会话拆了重来（实机观测 generation 97→1、
	// 钩子断线就是这么来的）。只有真的换目标才 switch，并强制重装两个钩子——
	// 换上的 realm 里没有本进程装的东西。
	targetID := intArg(args, "target", 0)
	forcedReinstall := false
	switch {
	case targetID > 0:
		if targetID != lockedID {
			switched, err := s.appCall(ctx, "miniapp.switch", map[string]any{"id": targetID})
			if err != nil {
				out["ok"] = false
				out["step"] = "miniapp_switch"
				out["error"] = err.Error()
				return out, nil
			}
			if objectMap(switched)["ok"] == false {
				out["ok"] = false
				out["step"] = "miniapp_switch"
				out["error"] = fmt.Sprintf("unknown miniapp id %d (see available)", targetID)
				return out, nil
			}
			forcedReinstall = true
		}
	case len(entries) == 1:
		// 唯一连接：引擎默认就锁它，无需显式切换。
	default:
		out["ok"] = false
		out["step"] = "target"
		// default 兜住「0 个」和「≥2 个」两种情况，文案必须分开：零连接时
		// 说「多个小程序」会把调用方引去找一个不存在的 target:<id>。
		if len(entries) == 0 {
			if engineError != "" {
				out["error"] = fmt.Sprintf("no miniapp connected and the engine reports: %s — fix the engine (its connection may have been torn down; session_start respawns it) before assuming no mini program is open", engineError)
			} else {
				out["error"] = "no miniapp connected — open a mini program in WeChat (keep its window open) and retry; engine.miniapp in session_status flips to true once it attaches"
			}
		} else {
			out["error"] = "multiple mini programs connected and no target given — pass target:<id> from available"
		}
		return out, nil
	}

	// 4. 开采集：显式动作，默认只开 wxapi；显式 [] 表示本轮不采集。
	hooks := []string{"wxapi"}
	if rawHooks, ok := args["hooks"].([]any); ok {
		hooks = hooks[:0]
		for _, hook := range rawHooks {
			if name, ok := hook.(string); ok {
				hooks = append(hooks, name)
			}
		}
	}
	capture := map[string]any{}
	for _, hook := range hooks {
		if hook != "wxapi" && hook != "cloud" {
			out["ok"] = false
			out["step"] = "hook_start"
			out["error"] = fmt.Sprintf("hooks must contain only wxapi / cloud, got %q", hook)
			return out, nil
		}
		started, err := s.ensureHook(ctx, hook, forcedReinstall)
		if err != nil {
			out["ok"] = false
			out["step"] = "hook_start"
			out["error"] = err.Error()
			return out, nil
		}
		capture[hook] = started
	}
	out["capture"] = capture

	// 5. 汇总快照。
	return s.assembleSnapshot(ctx, out)
}

// sessionStatus is the one-call snapshot: prerequisites, engine, target,
// pages, capture health and store size. Read-only.
func (s *Server) sessionStatus(ctx context.Context) (any, error) {
	out := map[string]any{}
	return s.assembleSnapshot(ctx, out)
}

// assembleSnapshot fills out with the read-only state every session tool
// reports: node/wechat/engine, locked target, page stack, capture health,
// store size. Each read is best-effort — a failed read is reported as an
// {error} entry instead of failing the whole snapshot.
func (s *Server) assembleSnapshot(ctx context.Context, out map[string]any) (any, error) {
	if _, has := out["node"]; !has {
		out["node"], _ = s.appCall(ctx, "node.status", map[string]any{})
	}
	if _, has := out["wechat"]; !has {
		wechat, err := s.appCall(ctx, "wechat.status", map[string]any{})
		if err != nil {
			out["wechat"] = map[string]any{"error": err.Error()}
		} else {
			out["wechat"] = wechat
		}
	}

	if s.deps.Core != nil {
		engine, err := s.deps.Core.Status(ctx)
		if err != nil {
			out["engine"] = map[string]any{"error": err.Error()}
		} else {
			out["engine"] = engine
		}
	} else {
		// 缺键会被客户端读成「无此信息」而不是「引擎不可用」——快照里每个
		// 读不到的面都以 {error} 内联，这里保持同一口径。
		out["engine"] = map[string]any{"error": "core engine unavailable"}
	}

	// 页面栈：一个调用覆盖配置页面、实际栈与当前路由。
	pages, err := s.appCall(ctx, "navigator.pages", map[string]any{})
	if err != nil {
		out["pages"] = map[string]any{"error": err.Error()}
	} else {
		out["pages"] = pages
	}

	// 采集健康：两个钩子的丢弃计数。结论可信度前提，随快照常驻。
	// session_start 已填过 capture（含 already 标注）时不覆盖——它记录的是
	// 这次调用对钩子做了什么，比再读一次 stats 更有价值；丢弃计数走 session_status。
	if _, has := out["capture"]; !has {
		stats := map[string]any{}
		for _, hook := range []string{"wxapi", "cloud"} {
			result, err := s.appCall(ctx, hook+".stats", map[string]any{})
			if err != nil {
				stats[hook] = map[string]any{"error": err.Error()}
				continue
			}
			stats[hook] = result
		}
		out["capture"] = stats
	}

	if s.deps.Traffic != nil {
		if store, err := s.appCall(ctx, "traffic.stats", map[string]any{}); err == nil {
			out["store"] = store
		}
	}

	if out["ok"] == nil {
		out["ok"] = true
	}
	return out, nil
}

// hookAuditObjects maps a record-hook name onto the page-side audit object the
// injected script installs (core/src/app.ts HOOK_SURFACES). Keep in sync with
// that table.
var hookAuditObjects = map[string]string{"wxapi": "wxApiAudit", "cloud": "cloudAudit"}

// entryID reads the numeric id of a miniapp.list entry (IPC JSON makes them
// float64).
func entryID(entry map[string]any) int {
	id, _ := entry["id"].(float64)
	return int(id)
}

// currentGeneration reads the page generation from the Core; a failed read is
// 0, which only ever compares equal to another 0.
func (s *Server) currentGeneration(ctx context.Context) int64 {
	if s.deps.Core == nil {
		return 0
	}
	status, err := s.deps.Core.Status(ctx)
	if err != nil {
		return 0
	}
	return status.Generation
}

// recordInstall pins when this process last (re)installed a hook: the current
// page generation at that moment.
func (s *Server) recordInstall(ctx context.Context, hook string) {
	s.installMu.Lock()
	s.installGen[hook] = s.currentGeneration(ctx)
	s.installMu.Unlock()
}

// hookAlive probes the page side directly: the feeder claiming running says
// nothing about whether the realm it installed into still exists — an engine
// restart or a page rebuild silently takes the hook with it (observed live as
// a wxapi hook that ran while recording nothing).
func (s *Server) hookAlive(ctx context.Context, hook string) bool {
	object, ok := hookAuditObjects[hook]
	if !ok || s.deps.Core == nil {
		return false
	}
	contextID, err := s.appServiceContextID(ctx)
	if err != nil {
		return false
	}
	answer, err := s.evaluateJSON(ctx,
		`(function(){return typeof window.`+object+`==='undefined'?'no':'yes'})()`,
		false, contextID, 3000)
	return err == nil && answer == "yes"
}

// hookCurrent reports whether the running hook is the one this process
// installed on the current realm. Generation alone cannot decide it: an engine
// restart rebuilds the bridge and the generation counts from zero again, which
// can collide with a stale install-time value — the page-side probe is the
// backstop, not an optimization.
func (s *Server) hookCurrent(ctx context.Context, hook string) bool {
	s.installMu.Lock()
	installGen, recorded := s.installGen[hook]
	s.installMu.Unlock()
	if !recorded || installGen != s.currentGeneration(ctx) {
		return false
	}
	return s.hookAlive(ctx, hook)
}

// ensureHook brings one capture hook to "running and installed on the current
// realm". Three states: not running → start; running but stale (generation
// moved, page-side object gone, or an explicit target switch forced a fresh
// realm) → stop + start; running and healthy → no-op with already:true. A
// fresh start also clears the page-side record buffer, so hook_drain's
// afterSeq=0 reads only this capture window — a stale cursor can no longer
// replay days-old records with full bodies. A healthy already-running hook is
// left untouched: the GUI may share that page buffer.
func (s *Server) ensureHook(ctx context.Context, hook string, forced bool) (map[string]any, error) {
	stats, err := s.appCall(ctx, hook+".stats", map[string]any{})
	running := err == nil && objectMap(stats)["running"] == true
	if running && !forced && s.hookCurrent(ctx, hook) {
		out := objectMap(stats)
		out["already"] = true
		return out, nil
	}
	if running {
		if _, err := s.appCall(ctx, hook+".stop", map[string]any{}); err != nil {
			return nil, fmt.Errorf("%s: %v", hook, err)
		}
	}
	result, err := s.appCall(ctx, hook+".start", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("%s: %v", hook, err)
	}
	if s.deps.Core != nil {
		_ = s.deps.Core.HookClear(ctx, hook)
	}
	s.recordInstall(ctx, hook)
	out := objectMap(result)
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// captureHealth exposes the quiet-failure signature: a hook this process
// started that is no longer running, or one whose realm moved without a
// reinstall. Generation moved but the page-side object answers → autoInject
// got there first; refresh the bookkeeping instead of crying wolf. nil means
// nothing to report (nothing watched, or everything healthy).
func (s *Server) captureHealth(ctx context.Context) map[string]any {
	s.installMu.Lock()
	watched := len(s.installGen) > 0
	watches := map[string]bool{}
	for hook := range s.installGen {
		watches[hook] = true
	}
	s.installMu.Unlock()
	if !watched || s.deps.AppBridge == nil {
		return nil
	}
	curGen := s.currentGeneration(ctx)
	hooks := map[string]any{}
	abnormal := false
	for _, hook := range []string{"wxapi", "cloud"} {
		if !watches[hook] {
			continue
		}
		stats, err := s.appCall(ctx, hook+".stats", map[string]any{})
		running := err == nil && objectMap(stats)["running"] == true
		entry := map[string]any{"running": running}
		switch {
		case !running:
			abnormal = true
		default:
			s.installMu.Lock()
			installGen := s.installGen[hook]
			s.installMu.Unlock()
			if installGen != curGen {
				if s.hookAlive(ctx, hook) {
					s.recordInstall(ctx, hook)
				} else {
					abnormal = true
					entry["reason"] = "realm rebuilt without reinstall (page-side hook missing)"
				}
			}
		}
		hooks[hook] = entry
	}
	if !abnormal {
		return nil
	}
	return map[string]any{"generation": curGen, "hooks": hooks}
}

// attachHealth stamps capture_health onto a map-shaped tool result when there
// is something to report. Healthy sessions stay unstamped — the full picture
// lives in session_status, and stamping every UI reply would be pure overhead.
// Non-map results (the screenshot image) carry no stamp.
func (s *Server) attachHealth(ctx context.Context, result any) any {
	health := s.captureHealth(ctx)
	if health == nil {
		return result
	}
	if m, ok := result.(map[string]any); ok {
		m["capture_health"] = health
	}
	return result
}

// healthWatchedTools are the session-adjacent tools whose replies get the
// capture_health stamp when capture is broken: the UI/runtime/storage readers
// an agent leans on mid-session, plus the drain readers. A stamp here means
// "the capture you started is no longer recording — re-run session_start".
var healthWatchedTools = map[string]bool{
	"miniapp_click": true, "miniapp_type": true, "miniapp_navigate": true,
	"miniapp_evaluate": true, "miniapp_console_log": true, "miniapp_page_stack": true,
	"miniapp_scroll": true, "miniapp_get_storage": true, "miniapp_get_storage_key": true,
	"miniapp_set_storage": true, "miniapp_remove_storage": true, "miniapp_clear_storage": true,
	"miniapp_cloud_captures": true, "navigator_page_stack": true,
	"miniapp_call": true,
	"hook_drain":   true, "hook_wait": true,
}
