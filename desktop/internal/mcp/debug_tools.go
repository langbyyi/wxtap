package mcp

// 调试器工具：把 DevTools 的断点会话交给 agent。CDP 事件（Debugger.paused /
// scriptParsed）不会作为 MCP 通知推送，所以整族工具围绕 Core 的调试快照工作：
// enable / pause / step 先改目标状态，再轮询快照等待事件落地，读类工具直接读
// 快照。暂停会冻结整个小程序运行时——这一点写进了每个写类工具的描述里。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// debuggerToolNames lists the tools callDebuggerTool implements, so the
// dispatcher routes by table instead of by trying this handler and falling
// through on nil.
var debuggerToolNames = map[string]bool{
	"debugger_enable": true, "debugger_state": true,
	"debugger_pause": true, "debugger_resume": true,
	"debugger_step_over": true, "debugger_step_into": true, "debugger_step_out": true,
	"debugger_call_stack": true, "debugger_list_scripts": true,
	"debugger_get_scopes": true, "debugger_evaluate_on_call_frame": true,
	"debugger_pause_on_exceptions": true,
	// 合并形态（lean 目录广播的是这三个）。
	"debugger_control": true, "debugger_breakpoint": true, "debugger_inspect": true,
}

// debuggerControlActions maps debugger_control's action enum onto the
// single-purpose tools' behavior. One table, so the enum and the dispatch
// cannot drift.
var debuggerControlActions = map[string]func(s *Server, ctx context.Context) (any, error){
	"pause":     func(s *Server, ctx context.Context) (any, error) { return s.debuggerPause(ctx) },
	"resume":    func(s *Server, ctx context.Context) (any, error) { return s.debuggerResume(ctx) },
	"step_over": func(s *Server, ctx context.Context) (any, error) { return s.debuggerStep(ctx, "Debugger.stepOver") },
	"step_into": func(s *Server, ctx context.Context) (any, error) { return s.debuggerStep(ctx, "Debugger.stepInto") },
	"step_out":  func(s *Server, ctx context.Context) (any, error) { return s.debuggerStep(ctx, "Debugger.stepOut") },
}

// debuggerStepCommands maps the step tools onto their CDP commands.
var debuggerStepCommands = map[string]string{
	"debugger_step_over": "Debugger.stepOver",
	"debugger_step_into": "Debugger.stepInto",
	"debugger_step_out":  "Debugger.stepOut",
}

const (
	// debugPauseDeadline 是 pause / step 等待 paused 事件落地的上限；超过即
	// 如实回报当前快照，不谎报成功。
	debugPauseDeadline = 5 * time.Second
	debugResumeDelay   = 2 * time.Second
	debugPollInterval  = 100 * time.Millisecond
)

// callDebuggerTool handles one debugger_* tool. It returns (nil, nil) for
// names it does not own so the dispatcher can keep falling through.
func (s *Server) callDebuggerTool(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	if !debuggerToolNames[name] {
		return nil, nil
	}
	if s.deps.Core == nil {
		return nil, fmt.Errorf("core engine unavailable")
	}
	args := map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("invalid tool arguments: %w", err)
		}
	}

	switch name {
	case "debugger_control":
		action := stringArg(args, "action", "")
		handler, known := debuggerControlActions[action]
		if !known {
			return nil, fmt.Errorf("action must be one of: pause, resume, step_into, step_out, step_over")
		}
		return handler(s, ctx)
	case "debugger_breakpoint":
		return s.debuggerBreakpoint(ctx, args)
	case "debugger_inspect":
		if expression := stringArg(args, "expression", ""); expression != "" {
			return s.debuggerFrameEvaluate(ctx, expression, intArg(args, "frame_index", 0))
		}
		return s.debugScopes(ctx, intArg(args, "frame_index", 0))
	case "debugger_enable":
		return s.debugSnapshot(ctx, true)
	case "debugger_state":
		return s.debugSnapshot(ctx, false)
	case "debugger_pause":
		return s.debuggerPause(ctx)
	case "debugger_resume":
		return s.debuggerResume(ctx)
	case "debugger_step_over", "debugger_step_into", "debugger_step_out":
		return s.debuggerStep(ctx, debuggerStepCommands[name])
	case "debugger_call_stack":
		state, err := s.deps.Core.DebugState(ctx, false)
		if err != nil {
			return nil, err
		}
		if state["paused"] != true {
			return nil, errors.New("not paused — the call stack only exists between a pause (or breakpoint hit) and debugger_resume")
		}
		snapshot := debugSnapshotOf(state)
		return map[string]any{"paused": true, "reason": state["reason"], "frames": snapshot["callFrames"]}, nil
	case "debugger_list_scripts":
		state, err := s.deps.Core.DebugState(ctx, true)
		if err != nil {
			return nil, err
		}
		filter := strings.ToLower(stringArg(args, "url_filter", ""))
		limit := intArg(args, "limit", 200)
		if limit <= 0 {
			limit = 200
		}
		all := anySlice(state["scripts"])
		matched := make([]any, 0, len(all))
		for _, script := range all {
			entry := objectMap(script)
			if filter != "" && !strings.Contains(strings.ToLower(stringArg(entry, "url", "")), filter) {
				continue
			}
			matched = append(matched, entry)
		}
		// Eval 出的脚本（WMPF 的业务包正是这么加载的）url 为空串，且在环形
		// 缓冲里占绝对多数。不排序的话 limit 窗口几乎全被空 url 淹没，而断点
		// 和 get_source 只能作用于有 url 的那几个 —— 命名的排前面。
		sort.SliceStable(matched, func(i, j int) bool {
			return stringArg(objectMap(matched[i]), "url", "") != "" && stringArg(objectMap(matched[j]), "url", "") == ""
		})
		shown := matched
		truncated := false
		if len(matched) > limit {
			shown = matched[:limit]
			truncated = true
		}
		return map[string]any{
			"scripts": shown, "total": len(matched), "shown": len(shown), "truncated": truncated,
			"scriptsTruncated": state["scriptsTruncated"],
		}, nil
	case "debugger_get_scopes":
		return s.debugScopes(ctx, intArg(args, "call_frame_index", 0))
	case "debugger_evaluate_on_call_frame":
		expression := stringArg(args, "expression", "")
		if expression == "" {
			return nil, fmt.Errorf("missing required argument \"expression\"")
		}
		frameID := stringArg(args, "call_frame_id", "")
		if frameID == "" {
			frame, err := s.pausedFrame(ctx, 0)
			if err != nil {
				return nil, err
			}
			frameID = stringArg(frame, "callFrameId", "")
			if frameID == "" {
				return nil, errors.New("not paused — no live call frame to evaluate in")
			}
		}
		resp, err := s.cdp(ctx, "Debugger.evaluateOnCallFrame", map[string]any{
			"callFrameId": frameID, "expression": expression, "returnByValue": true,
		}, 10000)
		if err != nil {
			return nil, err
		}
		result, _ := nested(resp, "result").(map[string]any)
		if details, ok := result["exceptionDetails"].(map[string]any); ok {
			exception, _ := nested(details, "exception", "description").(string)
			if exception == "" {
				exception, _ = details["text"].(string)
			}
			return map[string]any{"exception": exception, "callFrameId": frameID}, nil
		}
		inner, _ := result["result"].(map[string]any)
		return map[string]any{
			"callFrameId": frameID,
			"type":        inner["type"],
			"value":       valuePreview(inner),
			"description": stringArg(inner, "description", ""),
		}, nil
	case "debugger_pause_on_exceptions":
		state := stringArg(args, "state", "")
		if state != "none" && state != "uncaught" && state != "all" {
			return nil, fmt.Errorf("state must be one of: all, none, uncaught")
		}
		if _, err := s.deps.Core.DebugState(ctx, true); err != nil {
			return nil, err
		}
		if _, err := s.cdp(ctx, "Debugger.setPauseOnExceptions", map[string]any{"state": state}, 5000); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "state": state}, nil
	}
	return nil, nil
}

// debugSnapshot fetches the Core debug state and shapes it for an agent:
// 1-based frame positions, bounded script info (a count, not the whole list —
// debugger_list_scripts pages that). enable controls whether Debugger.enable
// is sent first.
func (s *Server) debugSnapshot(ctx context.Context, enable bool) (map[string]any, error) {
	state, err := s.deps.Core.DebugState(ctx, enable)
	if err != nil {
		return nil, err
	}
	return debugSnapshotOf(state), nil
}

func debugSnapshotOf(state map[string]any) map[string]any {
	return map[string]any{
		"enabled":          state["enabled"],
		"paused":           state["paused"],
		"pausedSeq":        state["pausedSeq"],
		"resumedSeq":       state["resumedSeq"],
		"pausedAt":         state["pausedAt"],
		"reason":           state["reason"],
		"callFrames":       debugFramesOf(state),
		"scriptCount":      len(anySlice(state["scripts"])),
		"scriptsTruncated": state["scriptsTruncated"],
	}
}

// debugFramesOf maps the snapshot's call frames for an agent: an explicit
// index (what debugger_get_scopes takes), 1-based line/column (CDP reports
// 0-based), and scope types without the raw object ids (those belong to
// debugger_get_scopes).
func debugFramesOf(state map[string]any) []map[string]any {
	frames := anySlice(state["callFrames"])
	out := make([]map[string]any, 0, len(frames))
	for index, frame := range frames {
		entry := objectMap(frame)
		scopes := anySlice(entry["scopes"])
		scopeTypes := make([]string, 0, len(scopes))
		for _, scope := range scopes {
			if kind := stringArg(objectMap(scope), "type", ""); kind != "" {
				scopeTypes = append(scopeTypes, kind)
			}
		}
		out = append(out, map[string]any{
			"index":        index,
			"callFrameId":  entry["callFrameId"],
			"functionName": entry["functionName"],
			"url":          entry["url"],
			"line":         intOf(entry["lineNumber"]) + 1,
			"column":       intOf(entry["columnNumber"]) + 1,
			"scopes":       scopeTypes,
		})
	}
	return out
}

// awaitPause polls the snapshot until a pause newer than beforeSeq lands, so
// pause / step return the paused state instead of a promise an MCP client
// cannot subscribe to.
func (s *Server) awaitPause(ctx context.Context, beforeSeq int, withNote bool) (any, error) {
	deadline := time.Now().Add(debugPauseDeadline)
	for {
		state, err := s.deps.Core.DebugState(ctx, false)
		if err != nil {
			return nil, err
		}
		if state["paused"] == true && intOf(state["pausedSeq"]) > beforeSeq {
			return debugSnapshotOf(state), nil
		}
		if time.Now().After(deadline) {
			snapshot := debugSnapshotOf(state)
			if withNote {
				snapshot["note"] = "pause/step did not confirm within 5s — execution may already be paused (pausedSeq unchanged), idle, or the target went away; re-check debugger_state"
			}
			return snapshot, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(debugPollInterval):
		}
	}
}

func (s *Server) awaitResume(ctx context.Context) (any, error) {
	deadline := time.Now().Add(debugResumeDelay)
	for {
		state, err := s.deps.Core.DebugState(ctx, false)
		if err != nil {
			return nil, err
		}
		if state["paused"] != true {
			return debugSnapshotOf(state), nil
		}
		if time.Now().After(deadline) {
			return debugSnapshotOf(state), nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(debugPollInterval):
		}
	}
}

// pausedFrame returns one live frame of the current pause.
func (s *Server) pausedFrame(ctx context.Context, index int) (map[string]any, error) {
	state, err := s.deps.Core.DebugState(ctx, false)
	if err != nil {
		return nil, err
	}
	if state["paused"] != true {
		return nil, errors.New("not paused — frame inspection needs a held pause (debugger_pause or a breakpoint first)")
	}
	frames := anySlice(state["callFrames"])
	if len(frames) == 0 {
		return nil, errors.New("paused snapshot carries no call frames — the pause may already have been released; re-check debugger_state")
	}
	if index < 0 || index >= len(frames) {
		return nil, fmt.Errorf("call frame index %d out of range (0..%d)", index, len(frames)-1)
	}
	return objectMap(frames[index]), nil
}

// debugScopes reads one paused frame's scope chain through
// Runtime.getProperties: each scope's variables with a short value preview.
func (s *Server) debugScopes(ctx context.Context, index int) (any, error) {
	frame, err := s.pausedFrame(ctx, index)
	if err != nil {
		return nil, err
	}
	scopes := anySlice(frame["scopes"])
	out := make([]map[string]any, 0, len(scopes))
	for _, scope := range scopes {
		entry := objectMap(scope)
		kind := stringArg(entry, "type", "")
		objectID := stringArg(entry, "objectId", "")
		variables := []map[string]any{}
		if objectID != "" {
			resp, err := s.cdp(ctx, "Runtime.getProperties", map[string]any{"objectId": objectID}, 5000)
			if err != nil {
				return nil, fmt.Errorf("reading %s scope: %w", kind, err)
			}
			variables = scopeVariables(nested(resp, "result", "result"))
		}
		out = append(out, map[string]any{"type": kind, "variables": variables})
	}
	return map[string]any{"frame": debugFramesOf(map[string]any{"callFrames": []any{frame}})[0], "scopes": out}, nil
}

// scopeVariables shapes a Runtime.getProperties result list; unmapped values
// (functions, big objects) keep their description, everything is preview-bounded.
func scopeVariables(value any) []map[string]any {
	properties := anySlice(value)
	out := make([]map[string]any, 0, len(properties))
	for _, property := range properties {
		entry := objectMap(property)
		if name := stringArg(entry, "name", ""); name != "" {
			inner := objectMap(entry["value"])
			out = append(out, map[string]any{
				"name":        name,
				"type":        inner["type"],
				"value":       valuePreview(inner),
				"description": stringArg(inner, "description", ""),
			})
		}
	}
	return out
}

// valuePreview renders a CDP remote-object value for JSON output: primitives
// pass through, everything structured falls back to its description so one
// huge object cannot flood a tool result.
func valuePreview(inner map[string]any) any {
	switch inner["type"] {
	case "object", "function", "symbol":
		if description := stringArg(inner, "description", ""); description != "" {
			return truncateText(description, 300)
		}
		return nil
	default:
		value := inner["value"]
		if text, ok := value.(string); ok {
			return truncateText(text, 2000)
		}
		return value
	}
}

func truncateText(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	// 按字节切会劈开多字节字符（CDP 值里常见中文）；退到 UTF-8 边界再加省略号。
	cut := text[:limit]
	for i := 0; i < utf8.UTFMax && !utf8.ValidString(cut); i++ {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}

func intOf(value any) int {
	switch number := value.(type) {
	case int:
		return number
	case int64:
		return int(number)
	case float64:
		return int(number)
	default:
		return 0
	}
}

// debuggerPause / debuggerResume / debuggerStep 是 debugger_control 与旧的单
// 用途工具共用的实现：改状态 → 轮询快照等事件落地。
func (s *Server) debuggerPause(ctx context.Context) (any, error) {
	before, err := s.debugSnapshot(ctx, true)
	if err != nil {
		return nil, err
	}
	if _, err := s.cdp(ctx, "Debugger.pause", map[string]any{}, 5000); err != nil {
		return nil, err
	}
	return s.awaitPause(ctx, intOf(before["pausedSeq"]), true)
}

func (s *Server) debuggerResume(ctx context.Context) (any, error) {
	if _, err := s.cdp(ctx, "Debugger.resume", map[string]any{}, 5000); err != nil {
		return nil, err
	}
	return s.awaitResume(ctx)
}

func (s *Server) debuggerStep(ctx context.Context, cdpMethod string) (any, error) {
	state, err := s.deps.Core.DebugState(ctx, false)
	if err != nil {
		return nil, err
	}
	if state["paused"] != true {
		return nil, errors.New("not paused — steps only work while a pause is held (debugger_control pause or a breakpoint first)")
	}
	beforeSeq := intOf(state["pausedSeq"])
	if _, err := s.cdp(ctx, cdpMethod, map[string]any{}, 5000); err != nil {
		return nil, err
	}
	return s.awaitPause(ctx, beforeSeq, true)
}

// breakpointEntry is one breakpoint this server process set, for
// debugger_breakpoint {action:"list"}. CDP does not offer a breakpoint
// enumeration, so the registry is the source of truth; entries can go stale
// when the page realm rebuilds (the answer says so).
type breakpointEntry struct {
	URL       string
	Line      int
	Condition string
}

// trackBreakpoints records set/removed breakpoints per Server. Concurrent
// tools/call dispatch is serialized today, but the guard is cheap insurance.
func (s *Server) trackBreakpoint(id string, entry breakpointEntry) {
	s.breakpointMu.Lock()
	defer s.breakpointMu.Unlock()
	if s.breakpoints == nil {
		s.breakpoints = map[string]breakpointEntry{}
	}
	s.breakpoints[id] = entry
}

func (s *Server) untrackBreakpoint(id string) {
	s.breakpointMu.Lock()
	defer s.breakpointMu.Unlock()
	delete(s.breakpoints, id)
}

func (s *Server) trackedBreakpoints() map[string]breakpointEntry {
	s.breakpointMu.Lock()
	defer s.breakpointMu.Unlock()
	out := make(map[string]breakpointEntry, len(s.breakpoints))
	for id, entry := range s.breakpoints {
		out[id] = entry
	}
	return out
}

// debuggerBreakpoint unifies set / remove / list. "list" is new capability:
// CDP has no enumeration, so this reads the server-side registry.
func (s *Server) debuggerBreakpoint(ctx context.Context, args map[string]any) (any, error) {
	action := stringArg(args, "action", "")
	switch action {
	case "set":
		url := stringArg(args, "url", "")
		line := intArg(args, "line", 0)
		if url == "" || line <= 0 {
			return nil, fmt.Errorf("set requires url and line (1-based)")
		}
		params := map[string]any{"url": url, "lineNumber": line - 1}
		if condition := stringArg(args, "condition", ""); condition != "" {
			params["condition"] = condition
		}
		if _, err := s.deps.Core.DebugState(ctx, true); err != nil {
			return nil, err
		}
		resp, err := s.cdp(ctx, "Debugger.setBreakpointByUrl", params, 10000)
		if err != nil {
			return nil, err
		}
		result, _ := nested(resp, "result").(map[string]any)
		id, _ := result["breakpointId"].(string)
		if id != "" {
			s.trackBreakpoint(id, breakpointEntry{URL: url, Line: line, Condition: stringArg(params, "condition", "")})
		}
		return map[string]any{"breakpoint_id": id, "locations": result["locations"], "url": url, "line": line}, nil
	case "remove":
		id := stringArg(args, "breakpoint_id", "")
		if id == "" {
			return nil, fmt.Errorf("remove requires breakpoint_id")
		}
		_, err := s.cdp(ctx, "Debugger.removeBreakpoint", map[string]any{"breakpointId": id}, 10000)
		s.untrackBreakpoint(id)
		return map[string]any{"ok": err == nil, "removed": id}, err
	case "list":
		entries := s.trackedBreakpoints()
		list := make([]map[string]any, 0, len(entries))
		for id, entry := range entries {
			item := map[string]any{"breakpoint_id": id, "url": entry.URL, "line": entry.Line}
			if entry.Condition != "" {
				item["condition"] = entry.Condition
			}
			list = append(list, item)
		}
		return map[string]any{
			"breakpoints": list, "count": len(list),
			"note": "registry of breakpoints this server set since its start; a page realm rebuild clears the target-side breakpoints (re-set them), an engine restart clears this registry",
		}, nil
	default:
		return nil, fmt.Errorf("action must be one of: list, remove, set")
	}
}

// debuggerFrameEvaluate evaluates an expression in a paused frame picked by
// index (top frame by default) — the debugger_evaluate_on_call_frame behavior
// behind debugger_inspect's expression mode.
func (s *Server) debuggerFrameEvaluate(ctx context.Context, expression string, frameIndex int) (any, error) {
	frame, err := s.pausedFrame(ctx, frameIndex)
	if err != nil {
		return nil, err
	}
	frameID := stringArg(frame, "callFrameId", "")
	if frameID == "" {
		return nil, errors.New("not paused — no live call frame to evaluate in")
	}
	resp, err := s.cdp(ctx, "Debugger.evaluateOnCallFrame", map[string]any{
		"callFrameId": frameID, "expression": expression, "returnByValue": true,
	}, 10000)
	if err != nil {
		return nil, err
	}
	result, _ := nested(resp, "result").(map[string]any)
	if details, ok := result["exceptionDetails"].(map[string]any); ok {
		exception, _ := nested(details, "exception", "description").(string)
		if exception == "" {
			exception, _ = details["text"].(string)
		}
		return map[string]any{"exception": exception, "callFrameId": frameID}, nil
	}
	inner, _ := result["result"].(map[string]any)
	return map[string]any{
		"callFrameId": frameID,
		"type":        inner["type"],
		"value":       valuePreview(inner),
		"description": stringArg(inner, "description", ""),
	}, nil
}
