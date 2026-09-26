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

	targetID := intArg(args, "target", 0)
	switch {
	case targetID > 0:
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
		result, err := s.appCall(ctx, hook+".start", map[string]any{})
		if err != nil {
			out["ok"] = false
			out["step"] = "hook_start"
			out["error"] = fmt.Sprintf("%s: %v", hook, err)
			return out, nil
		}
		capture[hook] = objectMap(result)
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
