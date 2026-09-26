// Package mcp implements a Model Context Protocol server over stdio that
// exposes the desktop's traffic store, engine status, hook drain and wxapkg
// scanner as MCP tools, so an external LLM client can query and drive WxTap.
//
// Transport: newline-delimited JSON-RPC 2.0 (MCP stdio), methods initialize,
// notifications/initialized, ping, tools/list and tools/call.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// Deps carries the providers the MCP server fronts. Every field is optional;
// the corresponding tool reports an unavailable error when nil.
type Deps struct {
	Traffic   Traffic
	Core      Core
	Scan      ScanFunc
	AppBridge AppBridge
	SkillsDir string
	// AllowedCodeRoot pins the agent-facing code tools (miniapp_read_file /
	// miniapp_search_code) to the decompiled output tree; empty disables
	// those tools' path use entirely.
	AllowedCodeRoot string
	// ToolProfile curates what tools/list advertises: "lean" (the default)
	// lists the consolidated catalog; "all" lists every tool including the
	// compat names kept callable for old clients. Dispatch is profile-independent —
	// unadvertised names still answer tools/call.
	ToolProfile string
	// Version is the version the MCP handshake reports. It is injected rather
	// than hardcoded here: the shell's version already lives in
	// desktop/ipc_bridge.go's appVersion, and a second copy in this package is
	// how the server came to announce 2.0.0 while the app was 1.0.0.
	Version string
}

// ListParams are the traffic_list tool arguments.
type ListParams struct {
	// Page is the 1-based page number; 0 reads as the first page. Ordering is
	// newest first, so page 1 is the most recent window.
	Page   int    `json:"page"`
	Limit  int    `json:"limit"`
	Query  string `json:"query"`
	APType string `json:"apiType"`
	Status string `json:"status"`
	// AppID filters by the miniapp that produced the record, exactly like the
	// GUI's appId filter: without it an agent can only filter by keyword and
	// would have to guess which captures belong to the app under audit.
	AppID string `json:"appId"`
}

// Item is one traffic summary row. DurationMs is filled once the call settles
// (the record is stored while it is still pending and updated in place); 0
// means "not settled yet / not recorded".
type Item struct {
	ID            string `json:"id"`
	Seq           int64  `json:"seq"`
	CapturedAt    string `json:"capturedAt"`
	APType        string `json:"apiType"`
	AppID         string `json:"appId"`
	Name          string `json:"name"`
	Method        string `json:"method"`
	URL           string `json:"url"`
	Status        string `json:"status"`
	RequestBytes  int    `json:"requestBytes"`
	ResponseBytes int    `json:"responseBytes"`
	DurationMs    int64  `json:"durationMs"`
}

// Page is one traffic_list result page. Total is how many records the same
// filter matches; an offset window over a store that is still being written to
// can shift between calls, so page numbers are for browsing — a caller that
// must not miss a record follows the drain stream's afterSeq cursor instead.
type Page struct {
	Items []Item `json:"items"`
	Total int64  `json:"total"`
}

// Traffic is the traffic store surface used by the MCP tools.
type Traffic interface {
	List(ctx context.Context, params ListParams) (Page, error)
	GetBody(ctx context.Context, id string, part string) ([]byte, error)
}

// Status is the engine status snapshot.
type Status struct {
	Frida      bool     `json:"frida"`
	Miniapp    bool     `json:"miniapp"`
	Devtools   bool     `json:"devtools"`
	Generation int64    `json:"generation,omitempty"`
	AppInfo    *AppInfo `json:"appInfo,omitempty"`
}

// AppInfo is the identity the Core probed from a connected miniapp.
type AppInfo struct {
	AppID string `json:"appid"`
	Name  string `json:"name"`
}

// DrainedRecord is one hook.drain record.
type DrainedRecord struct {
	Seq    int64          `json:"seq"`
	Record map[string]any `json:"record"`
}

// defaultUpdateLimit is the settled-update page size hook_drain uses when the
// caller sends no updateLimit. It is a const so the schema text and the handler
// agree on one number.
const defaultUpdateLimit = 200

// DrainedUpdate is one settled-record update frame of a hook's update stream:
// an async call whose outcome landed after its record was already drained. An
// agent that only reads records would otherwise see it as permanently pending.
type DrainedUpdate struct {
	Seq    int64          `json:"seq"`
	Update map[string]any `json:"update"`
}

// DrainPage is one hook.drain result page. NextUpdateSeq is the cursor to pass
// back as afterUpdateSeq: the update stream is paged like the record stream, so
// a caller that always sends 0 keeps re-reading the oldest frames instead of
// the settled calls that just landed.
type DrainPage struct {
	Records       []DrainedRecord `json:"records"`
	Updates       []DrainedUpdate `json:"updates"`
	NextSeq       int64           `json:"nextSeq"`
	NextUpdateSeq int64           `json:"nextUpdateSeq"`
	HasMore       bool            `json:"hasMore"`
}

// Core is the engine surface used by the MCP tools.
type Core interface {
	Status(ctx context.Context) (Status, error)
	HookDrain(ctx context.Context, name string, afterSeq int64, limit int, afterUpdateSeq int64, updateLimit int) (DrainPage, error)
	HookClear(ctx context.Context, name string) error
	CloudScan(ctx context.Context) ([]map[string]any, error)
	CDPCommand(ctx context.Context, method string, params map[string]any, timeoutMs int) (map[string]any, error)
	// DebugState is the debugger session snapshot (paused frames, parsed
	// scripts); enable=true also flips on Debugger.enable in the Core.
	DebugState(ctx context.Context, enable bool) (map[string]any, error)
}

// AppBridge exposes the desktop IPC router to MCP miniapp tools.
type AppBridge interface {
	Call(ctx context.Context, method string, params map[string]any) (any, error)
}

// passThrough is one tool that forwards to exactly one GUI IPC method.
type passThrough struct {
	// Method is the registered IPC method name.
	Method string
	// Args is the allow-list of argument keys forwarded verbatim. A key the
	// tool does not declare never reaches the IPC.
	Args []string
	// Required is the subset of Args the caller must supply. It exists because
	// several methods answer a missing argument with a plausible-looking empty
	// success (code.expandDir returns {children:[]}), which reads to an agent as
	// "that directory is empty" instead of "you forgot an argument".
	Required []string
}

// PassThroughTools maps a tool onto the GUI IPC method it fronts. The two names
// differ (the miniapp_* naming predates the consolidated profiles), so the pairing is data rather than
// something to retype at each call site — and the shell asserts every target is
// actually registered, because a renamed method would otherwise leave the tool
// answering "unsupported backend method" only when an agent used it.
//
// The tool name is the whole boundary. AppBridge.Call reaches every method the
// router has, so an unbounded forward would hand an agent extract.delete and
// config.save alongside the read-only tools; Args is the second half of that
// boundary. New tools go into this table — never into a general
// "call any IPC method" tool.
var PassThroughTools = map[string]passThrough{
	// 界面同名能力的直通：agent 与 GUI 走同一条 IPC 方法，两侧看到同一份实现。
	"traffic_stats": {Method: "traffic.stats"},
	// 库里有哪些小程序（appid + 记录数 + 已知名字）：traffic_list 的 appId 与
	// asset_scan 的 appid 都靠它来选，没有它 agent 只能从单条记录里猜 appid。
	"traffic_appids":             {Method: "traffic.appids"},
	"navigator_page_stack":       {Method: "navigator.pageStack"},
	"sessionkey_scan_traffic":    {Method: "sessionkey.scanTraffic", Args: []string{"limit"}},
	"sessionkey_scan_decompiled": {Method: "sessionkey.scanDecompiled"},

	// 建立会话。没有这几件，`-mcp` stdio 模式是可用的假象：currentEngine()
	// 会惰性拉起 Core 进程，而 engine.start（真正让 Frida 附加微信的那一步）
	// 够不到，于是 hook_drain / traffic_list 恒空、没有任何办法察觉原因。
	"wechat_status":  {Method: "wechat.status"},
	"node_status":    {Method: "node.status"},
	"engine_start":   {Method: "engine.start", Args: []string{"cdpPort"}},
	"engine_stop":    {Method: "engine.stop"},
	"miniapp_list":   {Method: "miniapp.list"},
	"miniapp_switch": {Method: "miniapp.switch", Args: []string{"id"}, Required: []string{"id"}},
	"targets_list":   {Method: "targets.list"},

	// 采集健康 / 驱动 / 验证。
	"wxapi_replay": {Method: "wxapi.replay", Args: []string{"api_name", "options"}, Required: []string{"api_name"}},
	// 自动遍历的进度只走 navigate_progress 事件，MCP 客户端收不到事件，所以
	// auto_visit_state 是唯一的完成判据 —— 它看着琐碎，却是必需的。
	"navigator_auto_visit":       {Method: "navigator.autoVisit"},
	"navigator_auto_visit_state": {Method: "navigator.autoVisitState"},
	"navigator_stop_auto_visit":  {Method: "navigator.stopAutoVisit"},
	// 配合 navigator_guard 使用：守住跳转后读被拦清单，有 redirect 记录的页面
	// 说明它自己做了登录跳转，没有的便是未做鉴权。
	"navigator_blocked_redirects": {Method: "navigator.getBlockedRedirects"},

	// 代码 / 凭据闭环。
	"code_projects": {Method: "code.projects"},
	// 列目录。注意它**不打开资源管理器**，IP 与界面按钮的名字有误导。
	"code_list_dir":        {Method: "code.expandDir", Args: []string{"path"}, Required: []string{"path"}},
	"paths_get":            {Method: "settings.getPaths"},
	"ak_verify":            {Method: "ak.verify", Args: []string{"mode", "access_key", "secret_key", "fake_ip", "include_token", "body_limit"}, Required: []string{"mode", "access_key", "secret_key"}},
	"sessionkey_users":     {Method: "sessionkey.users"},
	"sessionkey_scan_disk": {Method: "sessionkey.scan", Args: []string{"dir"}, Required: []string{"dir"}},
	"extract_inventory":    {Method: "extract.inventory", Args: []string{"dir", "user_dir"}},
	"hook_list":            {Method: "hook.list"},
	// 只接受裸 *.js 文件名：脚本内容必须已在 hook_scripts/ 里，没有内联脚本文本
	// 入参，所以 agent 只能在操作者预先放好的脚本里选。
	"hook_inject": {Method: "hook.inject", Args: []string{"filename"}, Required: []string{"filename"}},
	// 一次调用拿到反编译工程的顶层目录树与根路径。
	"code_tree": {Method: "code.project", Args: []string{"appid"}, Required: []string{"appid"}},
	// 官方端点利用台：清单只读；调用按次携带凭据、不落盘。
	"wxopen_endpoints": {Method: "wxopen.endpoints"},
	"wxopen_call":      {Method: "wxopen.call", Args: []string{"mode", "access_key", "secret_key", "endpoint", "params"}, Required: []string{"mode", "access_key", "secret_key", "endpoint"}},
}

// ScanResult is the scan_dir tool result (category → count).
type ScanResult struct {
	FilesScanned int             `json:"files_scanned"`
	TotalSize    int64           `json:"total_size"`
	Summary      map[string]int  `json:"summary"`
	Report       json.RawMessage `json:"report,omitempty"`
}

// ScanFunc runs the sensitive-info analyzer over a directory.
type ScanFunc func(ctx context.Context, dir string) (ScanResult, error)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`

	// ctx is the cancellation source for this request: the HTTP transports
	// attach the inbound request context (a disconnecting client cancels
	// server-side long polls), stdio leaves it nil (= background).
	ctx context.Context
}

type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema map[string]any  `json:"inputSchema"`
	Annotations toolAnnotations `json:"annotations"`
}

// toolAnnotations are the MCP tool hints agents use to plan calls: whether a
// tool only reads, whether it can destroy data, whether repeats are safe, and
// whether it reaches beyond WxTap. All four are always emitted — the spec's
// defaults (destructive when unannotated) are the wrong guess for either side
// of a hint that is actually known.
type toolAnnotations struct {
	ReadOnly    bool `json:"readOnlyHint"`
	Destructive bool `json:"destructiveHint"`
	Idempotent  bool `json:"idempotentHint"`
	OpenWorld   bool `json:"openWorldHint"`
}

// readOnlyTools answer queries only: they never change what is captured, what
// runs in the miniapp, or anything on disk.
var readOnlyTools = map[string]bool{
	"traffic_list": true, "traffic_get_body": true, "traffic_stats": true, "traffic_appids": true,
	"engine_status": true, "hook_drain": true, "hook_stats": true,
	"wechat_status": true, "node_status": true,
	"miniapp_list": true, "targets_list": true,
	"navigator_page_stack": true, "navigator_auto_visit_state": true, "navigator_blocked_redirects": true,
	"code_projects": true, "code_list_dir": true, "paths_get": true,
	"sessionkey_users": true, "sessionkey_scan_traffic": true, "sessionkey_scan_decompiled": true,
	"sessionkey_scan_disk": true, "extract_inventory": true, "hook_list": true,
	"scan_dir":           true,
	"miniapp_screenshot": true, "miniapp_get_routes": true, "miniapp_get_current_route": true,
	"miniapp_list_contexts": true, "miniapp_get_info": true, "miniapp_get_storage": true,
	"miniapp_get_storage_key": true, "miniapp_cloud_scan": true, "miniapp_scan_sensitive": true,
	"miniapp_read_file": true, "miniapp_search_code": true, "miniapp_list_packages": true,
	"miniapp_get_skills": true, "miniapp_get_source": true,
	"debugger_state": true, "debugger_call_stack": true, "debugger_list_scripts": true,
	"debugger_get_scopes": true,
	"hook_wait":           true,
	"traffic_records":     true,
	"session_status":      true, "miniapp_page_stack": true, "sessionkey_scan": true, "wxopen_endpoints": true, "code_tree": true,
	// 审计面：查询型只读；asset_scan 会替换内存清单，不算只读也不算破坏。
	"asset_list": true, "asset_export": true, "traffic_curl": true,
}

// destructiveTools end or discard in-progress capture: records still waiting to
// be polled are dropped (already-stored records stay readable).
var destructiveTools = map[string]bool{
	"hook_stop": true, "engine_stop": true, "navigator_stop_auto_visit": true,
	"miniapp_clear_storage": true,
	// all:true 是与 miniapp_clear_storage 同义的全量清空，注解必须同判。
	"miniapp_remove_storage": true,
}

// idempotentTools can be repeated with the same arguments without changing the
// outcome beyond the first call.
var idempotentTools = map[string]bool{
	"session_start": true,
	"hook_start":    true, "engine_start": true, "miniapp_switch": true,
	"navigator_guard": true, "miniapp_navigate": true,
	"debugger_enable": true, "debugger_pause_on_exceptions": true,
	"miniapp_decompile": true,
}

// openWorldTools reach beyond WxTap and the debugged miniapp: they talk to
// whatever endpoints or services their arguments name.
var openWorldTools = map[string]bool{
	"miniapp_http_request": true, "ak_verify": true, "miniapp_call_cloud": true,
	"wxapi_replay": true, "wxopen_call": true,
	// 单条重放按捕获原文对真实后端再发一次，与 ak_verify 的外发同一警示级别。
	"traffic_replay": true,
}

func annotationsFor(name string) toolAnnotations {
	return toolAnnotations{
		ReadOnly:    readOnlyTools[name],
		Destructive: destructiveTools[name],
		Idempotent:  idempotentTools[name],
		OpenWorld:   openWorldTools[name],
	}
}

// Server is the MCP server instance.
type Server struct {
	deps Deps

	// breakpointMu / breakpoints back debugger_breakpoint {action:"list"}:
	// CDP has no breakpoint enumeration, so the server tracks what it set.
	breakpointMu sync.Mutex
	breakpoints  map[string]breakpointEntry

	// contextMu guards the cached execution-context ids. Context ids belong to
	// the page realm: reloading the miniapp, or replacing a page webview,
	// hands out new ones, so a cached id is re-validated before it is used.
	// pageCtxRoute is the route pageCtx was resolved for — the same id can be
	// a different page after a navigation.
	contextMu     sync.Mutex
	appServiceCtx int
	pageCtx       int
	pageCtxRoute  string
}

// New wires an MCP server over the given providers.
func New(deps Deps) *Server {
	return &Server{deps: deps}
}

// Serve reads newline-delimited requests until EOF, writing one response line
// per request with an id. Malformed lines are skipped.
func (s *Server) Serve(r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	writer := bufio.NewWriter(w)
	defer func() { _ = writer.Flush() }()

	for scanner.Scan() {
		var req request
		if json.Unmarshal(scanner.Bytes(), &req) != nil || req.Method == "" {
			continue
		}
		if strings.HasPrefix(req.Method, "notifications/") {
			continue
		}
		response := s.dispatchSafely(&req)
		if response == nil {
			continue // notification: nothing to answer
		}
		data, err := json.Marshal(response)
		if err != nil {
			continue
		}
		if _, err := writer.Write(data); err != nil {
			return err
		}
		if err := writer.WriteByte('\n'); err != nil {
			return err
		}
		if err := writer.Flush(); err != nil {
			return err
		}
	}
	return scanner.Err()
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// dispatchSafely runs dispatch behind a panic barrier: a bug in one tool must
// fail that single request instead of tearing down the MCP session.
func (s *Server) dispatchSafely(req *request) (resp *response) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("mcp: %s panicked: %v\n%s", req.Method, recovered, debug.Stack())
			resp = fail(req, &rpcError{Code: -32603, Message: fmt.Sprintf("internal error: %v", recovered)})
		}
	}()
	return s.dispatch(req)
}

// supportedProtocolVersions are the MCP protocol revisions this server can
// speak, newest first. initialize echoes the client's version when known, so
// both 2024-11-05 and current clients get a shape they expect.
var supportedProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// serverInstructions is returned from initialize. An agent that reads it can
// bootstrap a working session without discovering the ordering by trial and
// error — the ordering is the whole trick (capture starts only after
// engine_start + hook_start), so it is spelled out here and not just in the
// per-tool descriptions.
const serverInstructions = `WxTap debugs WeChat mini programs (authorized environments): it attaches to the WeChat desktop host, exposes the miniapp's JS runtime over CDP, captures wx API / cloud-call traffic, and reads decompiled package code.

Fast path: session_start bootstraps everything (prerequisite checks, engine_start, target selection, capture start) in one idempotent call and reports which step failed if anything blocks. session_status returns the equivalent one-call snapshot (engine, target, pages, capture health, store size).

Manual order for step-by-step execution — nothing is captured before the last step succeeds:
1. node_status, then wechat_status: Node 22+ and a supported WeChat build are the prerequisites (a missing address table is the usual engine_start failure).
2. engine_start: attach Frida and start the CDP proxy (idempotent).
3. miniapp_list, then miniapp_switch if several mini programs are open.
4. hook_start {name:"wxapi"} (and/or "cloud"): capture begins at that moment; hook_drain stays empty before it.

Then: hook_drain (records + settled updates, cursor-paged; waitMs>0 waits for new records after a flow is triggered) and traffic_records / traffic_get_body for the stored side. Read capture health (session_status or hook_stats) before concluding anything about missing data — pageDropped* counters mean permanently lost records. UI: miniapp_screenshot / click / type / navigate, miniapp_evaluate, miniapp_console_log, miniapp_get_storage. JS debugging: debugger_list_scripts, debugger_breakpoint {action:set}, debugger_control {action:pause|resume|step_*}, debugger_inspect (frame scopes / expressions) while paused; miniapp_get_source for script text. Offline audit: extract_inventory, miniapp_decompile, miniapp_search_code, miniapp_scan_sensitive, ak_verify. Resources (skill://, wxtap://reference/*) and prompts (wx_* workflows) carry the full methodology; unadvertised compat tool names still answer tools/call.`

func (s *Server) dispatch(req *request) *response {
	switch req.Method {
	case "initialize":
		return ok(req, s.initialize(req))
	case "ping":
		return ok(req, map[string]any{})
	case "tools/list":
		return ok(req, map[string]any{"tools": s.tools()})
	case "tools/call":
		return s.callTool(req)
	case "resources/list":
		return ok(req, map[string]any{"resources": s.listResources()})
	case "resources/templates/list":
		return ok(req, map[string]any{"resourceTemplates": []any{}})
	case "resources/read":
		return s.readResource(req)
	case "prompts/list":
		return ok(req, map[string]any{"prompts": listPrompts()})
	case "prompts/get":
		return s.getPrompt(req)
	default:
		return fail(req, &rpcError{Code: -32601, Message: "method not found: " + req.Method})
	}
}

// initialize answers with the client's protocol version when this server
// supports it, else its own latest — the negotiation both the 2024-11-05 and
// current revisions describe.
func (s *Server) initialize(req *request) map[string]any {
	var params struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(req.Params, &params)
	version := supportedProtocolVersions[0]
	for _, candidate := range supportedProtocolVersions {
		if params.ProtocolVersion == candidate {
			version = candidate
			break
		}
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			"tools":     map[string]any{},
			"resources": map[string]any{"subscribe": false, "listChanged": false},
			"prompts":   map[string]any{"listChanged": false},
		},
		"serverInfo":   map[string]any{"name": "wxtap-desktop", "version": s.serverVersion()},
		"instructions": serverInstructions,
	}
}

// serverVersion is what the handshake announces. A missing injection reads as
// "unknown" rather than as a plausible release number: an empty or invented
// version is exactly what a client would mistake for a real build.
func (s *Server) serverVersion() string {
	if s.deps.Version == "" {
		return "unknown"
	}
	// The shell spells it "v1.0.0"; the handshake convention is bare.
	return strings.TrimPrefix(s.deps.Version, "v")
}

func (s *Server) tools() []toolDef {
	schema := func(props map[string]any, required ...string) map[string]any {
		s := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	}
	str := map[string]any{"type": "string"}
	num := map[string]any{"type": "number"}
	integer := map[string]any{"type": "integer"}
	boolean := map[string]any{"type": "boolean"}
	object := map[string]any{"type": "object"}

	defs := []toolDef{
		// 会话组合工具：一次调用完成引导 / 一次调用拿到全量状态。引导顺序
		// 是 agent 最容易出错的地方，固化在服务端、失败带步名归因。
		{Name: "session_start", Description: "Bootstrap a full debug session in one call: node/wechat checks, engine_start (attach Frida), target selection (the only connected miniapp auto-locks; pass target:<id> when several are open), capture start (hooks defaults to [wxapi]; add \"cloud\" or pass [] for none). Stops at the first failure and names the step with its reason (engine codes: no_host / no_ancestor / ambiguous_host / no_version). Idempotent",
			InputSchema: schema(map[string]any{
				"target":  integer,
				"hooks":   map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []string{"wxapi", "cloud"}}},
				"cdpPort": integer,
			})},
		{Name: "session_status", Description: "One-call session snapshot: node/wechat prerequisites, engine state, locked target, page stack, capture health (per-hook dropped counters — check before concluding data is missing) and traffic-store size. Read-only; a failed read is reported inline instead of failing the call",
			InputSchema: schema(map[string]any{})},
		{Name: "traffic_list", Description: "List captured mini program traffic records with optional filtering and page-number pagination (newest first; the response carries the total the filter matches)",
			InputSchema: schema(map[string]any{
				"page": integer, "limit": num, "query": str,
				"apiType": str, "status": str, "appId": str,
			})},
		{Name: "traffic_get_body", Description: "Get the request or response body of one traffic record. Bodies over 256KB come back truncated (truncated:true, totalBytes) — narrow the query or use the GUI for the full text",
			InputSchema: schema(map[string]any{
				"id":   str,
				"part": map[string]any{"type": "string", "enum": []string{"request", "response"}},
			}, "id")},
		{Name: "traffic_records", Description: "One aggregated history page (newest first): summaries with each record's request/response bodies inlined (truncated to bodyBytes; binary bodies reported as binary). bodyBytes:0 gives a body-free listing. For one full body use traffic_get_body",
			InputSchema: schema(map[string]any{
				"page": integer, "query": str, "apiType": str, "status": str, "appId": str,
				"limit": num, "bodyBytes": num,
			})},
		{Name: "traffic_appids", Description: "List the mini programs that have stored traffic: one row per appid with its record count, last capture time and a display name when one is known (learned from connected sessions or the decompiled output). This is how you pick the appId for traffic_list / traffic_records and the appid for asset_scan — do not guess appids",
			InputSchema: schema(map[string]any{})},
		{Name: "traffic_stats", Description: "Get the capture store's size, time range and overload counters (records dropped by the capture path)",
			InputSchema: schema(map[string]any{})},
		{Name: "engine_status", Description: "Get the Frida engine and debug/CDP connection status",
			InputSchema: schema(map[string]any{})},
		{Name: "hook_start", Description: "Start capturing wx API / cloud audit traffic. Recording begins at this moment: connecting a miniapp does not record anything on its own, so hook_drain returns nothing until a capture has been started here (or in the desktop app). Idempotent.",
			InputSchema: schema(map[string]any{
				"name": map[string]any{"type": "string", "enum": []string{"wxapi", "cloud"}},
			}, "name")},
		{Name: "hook_stop", Description: "Stop capturing wx API / cloud audit traffic: nothing is recorded from this moment on, and records still waiting to be polled are dropped (they are already stored and stay readable through traffic_records)",
			InputSchema: schema(map[string]any{
				"name": map[string]any{"type": "string", "enum": []string{"wxapi", "cloud"}},
			}, "name")},
		{Name: "hook_drain", Description: "Read captured wxapi/cloud records after afterSeq (cursor-paged; nextSeq feeds back; default page 50 records, each with full bodies — keep paging the cursor instead of raising the limit) plus settled-record updates for async calls whose outcome landed after their record was drained — feed nextUpdateSeq back, otherwise the oldest batch is re-read on every call. waitMs>0 blocks server-side until a new record lands (or timeout) — use it after triggering a flow instead of polling. Empty until hook_start",
			InputSchema: schema(map[string]any{
				"name":     map[string]any{"type": "string", "enum": []string{"wxapi", "cloud"}},
				"afterSeq": num, "limit": num,
				"afterUpdateSeq": num, "updateLimit": num, "waitMs": num,
			}, "name")},
		{Name: "hook_wait", Description: "Long-poll the wxapi/cloud record stream: block until at least one new record after afterSeq lands (or timeoutMs), then return that page — the mechanism for waiting out a triggered flow instead of polling hook_drain. Read-only on the update stream; cursor semantics match hook_drain (nextSeq feeds the next afterSeq)",
			InputSchema: schema(map[string]any{
				"name":     map[string]any{"type": "string", "enum": []string{"wxapi", "cloud"}},
				"afterSeq": num, "limit": num, "timeoutMs": num,
			}, "name")},
		{Name: "scan_dir", Description: "Scan an unpacked wxapkg directory for sensitive information (credentials, IDs, URLs, OSS buckets). Findings are deduped by rule+value+file with the snippet windowed around the match and capped (total_findings/truncated report the rest) — the raw scanner report on a minified bundle is far too large to return",
			InputSchema: schema(map[string]any{"dir": str}, "dir")},
		{Name: "miniapp_screenshot", Description: "Take a screenshot of the current miniapp page",
			InputSchema: schema(map[string]any{"format": map[string]any{"type": "string", "enum": []string{"png", "jpeg"}}, "quality": integer})},
		{Name: "miniapp_click", Description: "Click an element by CSS selector or coordinates. Coordinates are the page's CSS pixels (typically a 414x780 viewport) — NOT screenshot pixels: screenshots are scaled up by the device pixel ratio (~1.5x), so divide screenshot coords by (screenshot_width/viewport_width). A click outside the viewport is rejected with the viewport size instead of silently clicking nothing; a successful click returns the viewport it used. Selector clicks resolve in the current page webview and read a miss as an error",
			InputSchema: schema(map[string]any{"selector": str, "x": num, "y": num})},
		{Name: "miniapp_type", Description: "Type text into the focused or selected input",
			InputSchema: schema(map[string]any{"text": str, "selector": str, "clear": boolean}, "text")},
		{Name: "miniapp_navigate", Description: "Navigate to a miniapp route. Tab-bar pages are auto-switched (the dispatch becomes switchTab semantics and the stack depth stays 1) — verify the landing with miniapp_page_stack, since ok reflects the dispatch, not the page's own load",
			InputSchema: schema(map[string]any{"route": str, "method": map[string]any{"type": "string", "enum": []string{"navigateTo", "redirectTo", "reLaunch", "switchTab"}}}, "route")},
		{Name: "miniapp_get_routes", Description: "Get all registered miniapp routes", InputSchema: schema(map[string]any{})},
		{Name: "miniapp_get_current_route", Description: "Get the current active route", InputSchema: schema(map[string]any{})},
		{Name: "miniapp_scroll", Description: "Scroll the page or a selected element", InputSchema: schema(map[string]any{"x": num, "y": num, "selector": str})},
		{Name: "miniapp_list_contexts", Description: "List JavaScript execution contexts", InputSchema: schema(map[string]any{})},
		{Name: "miniapp_evaluate", Description: "Execute JavaScript in the miniapp. Results over 48KB come back truncated (truncated:true, total_bytes) — project the fields you need in the expression instead of fetching whole datasets",
			InputSchema: schema(map[string]any{"expression": str, "await_promise": boolean, "context_id": integer}, "expression")},
		{Name: "miniapp_console_log", Description: "Get recent miniapp console output", InputSchema: schema(map[string]any{"clear": boolean})},
		{Name: "miniapp_get_info", Description: "Get miniapp metadata and routes", InputSchema: schema(map[string]any{})},
		{Name: "miniapp_get_storage", Description: "Read miniapp local storage: all keys+values+size without arguments, or one key's value with key", InputSchema: schema(map[string]any{"key": str})},
		{Name: "miniapp_get_storage_key", Description: "Get one miniapp storage value", InputSchema: schema(map[string]any{"key": str}, "key")},
		{Name: "miniapp_set_storage", Description: "Write one storage key (wx.setStorageSync) — the tamper primitive: overwrite a cached token/role/flag, re-trigger the flow and observe the result. value is any JSON value (string token, number, bool, object) and keeps its shape; page-side failures surface as errors",
			InputSchema: schema(map[string]any{"key": str, "value": map[string]any{}}, "key", "value")},
		{Name: "miniapp_remove_storage", Description: "Delete one storage key (key) or wipe all storage (all:true — destructive, unrecoverable). Pair with miniapp_get_storage before/after to prove the unauthenticated fallback",
			InputSchema: schema(map[string]any{"key": str, "all": boolean})},
		{Name: "miniapp_clear_storage", Description: "Wipe all miniapp local storage (wx.clearStorageSync) — destructive and unrecoverable. The legacy name for miniapp_remove_storage {all:true}, the form the default catalogue advertises; kept callable for clients that hardcode it", InputSchema: schema(map[string]any{})},
		{Name: "miniapp_call_cloud", Description: "Call a WeChat cloud function. Results over 48KB come back truncated (truncated:true, total_bytes) — project the fields you need instead of fetching the whole payload", InputSchema: schema(map[string]any{"name": str, "data": object}, "name")},
		{Name: "miniapp_cloud_captures", Description: "Get cloud calls captured by the dynamic hook (empty until a capture is started with hook_start). Snapshot read capped at 200 records (truncated:true points to hook_drain cursor paging for the rest)", InputSchema: schema(map[string]any{"clear": boolean})},
		{Name: "miniapp_cloud_scan", Description: "Statically scan loaded code for cloud references", InputSchema: schema(map[string]any{})},
		{Name: "miniapp_http_request", Description: "Send an HTTP request for backend testing",
			InputSchema: schema(map[string]any{"method": str, "url": str, "headers": object, "body": str, "timeout": num}, "method", "url")},
		{Name: "miniapp_decompile", Description: "Decompile wxapkg packages for an appid. Refuses an app whose page templates come from WeChat's newer template runtime (extract_inventory reports those as unsupported:true) — the restoration cannot succeed, so no output would be produced", InputSchema: schema(map[string]any{"appid": str, "packages_dir": str}, "appid")},
		{Name: "miniapp_scan_sensitive", Description: "Scan the decompiled output directory of an appid for sensitive information. Findings come back deduped by rule+value+file with the snippet windowed around the match and capped (total_findings/truncated report the rest) — run miniapp_decompile first, and expect minified bundles to inflate total_findings",
			InputSchema: schema(map[string]any{"appid": str}, "appid")},
		{Name: "miniapp_read_file", Description: "Read a decompiled source file", InputSchema: schema(map[string]any{"path": str, "max_length": integer}, "path")},
		{Name: "miniapp_search_code", Description: "Search decompiled source using text or regular expression. Each hit carries column (1-based rune offset of the match) and an excerpt windowed around the match — on minified one-line bundles the excerpt is the only part of the line worth reading",
			InputSchema: schema(map[string]any{"root": str, "query": str, "regex": boolean, "max_results": integer}, "root", "query")},
		{Name: "miniapp_list_packages", Description: "List available wxapkg packages", InputSchema: schema(map[string]any{"packages_dir": str})},
		{Name: "miniapp_get_skills", Description: "Read the bundled skill documents (agent methodology for WxTap debugging and audits)", InputSchema: schema(map[string]any{})},
		{Name: "miniapp_set_breakpoint", Description: "Set a breakpoint by URL and line", InputSchema: schema(map[string]any{"url": str, "line": integer, "condition": str}, "url", "line")},
		{Name: "miniapp_remove_breakpoint", Description: "Remove a breakpoint", InputSchema: schema(map[string]any{"breakpoint_id": str}, "breakpoint_id")},
		{Name: "miniapp_get_source", Description: "Get script source by script id or URL pattern", InputSchema: schema(map[string]any{"url_pattern": str, "max_length": integer}, "url_pattern")},
		{Name: "navigator_page_stack", Description: "Get the connected miniapp's runtime page stack, bottom page first, with each page's query params. This is what is actually on the navigation stack (unlike miniapp_get_routes, the configured page list), so it is the authoritative answer for the current page and how far back navigateBack can go",
			InputSchema: schema(map[string]any{})},
		{Name: "miniapp_page_stack", Description: "One read covering every page-level view: the live navigation stack (bottom first), the configured page list, tab bar pages and the current route. The consolidated form of the separate page reads",
			InputSchema: schema(map[string]any{})},
		{Name: "sessionkey_scan_traffic", Description: "Extract session_key / iv / encryptedData material from the packets already captured (the traffic store), including URL-encoded bodies. Read-only",
			InputSchema: schema(map[string]any{"limit": integer})},
		{Name: "sessionkey_scan_decompiled", Description: "Extract session_key / iv / encryptedData material from the restored (decompiled) mini-program sources. Read-only",
			InputSchema: schema(map[string]any{})},

		// 建立会话。
		{Name: "wechat_status", Description: "Check whether the WeChat desktop client is running and whether a Frida address table exists for its version. Never errors: a non-empty error field means the check itself failed, and an empty running:true result is not a success. A missing address table is the usual reason engine_start answers no_version, so this is the fastest way to tell 'WeChat is not running' from 'this WeChat version is unsupported'",
			InputSchema: schema(map[string]any{})},
		{Name: "node_status", Description: "Report which Node.js runtime WxTap resolved to launch the Core with, its version, and how it was found (env override / saved config / PATH / auto-detected). Node 22+ is the prerequisite for everything else: without it the Core never starts, and every traffic and hook tool answers with an error. Read-only and it never errors",
			InputSchema: schema(map[string]any{})},
		{Name: "engine_start", Description: "Attach Frida to the WeChat process and start the CDP proxy. This is the step that makes any capture possible — nothing is recorded until it has succeeded and a capture has been started with hook_start. Idempotent. Failures are explicit: no_host / no_ancestor / ambiguous_host (the WMPF host process could not be located) or a missing address table for this WeChat version (check wechat_status)",
			InputSchema: schema(map[string]any{"cdpPort": integer})},
		{Name: "engine_stop", Description: "Detach Frida, stop the CDP proxy and end the audit capture: wxapi/cloud recording stops and records still waiting to be polled are dropped (already-stored records stay readable through traffic_records)",
			InputSchema: schema(map[string]any{})},
		{Name: "miniapp_list", Description: "List the mini programs currently connected to the debug bridge, with the id to pass to miniapp_switch. An empty list with a non-empty `error` field means the engine itself is down (session_start will respawn it) — only an error-free empty list means no mini program is open",
			InputSchema: schema(map[string]any{})},
		{Name: "miniapp_switch", Description: "Pin the debug target to one connected mini program, by the id from miniapp_list. Switching resets the page generation, which reinstalls the hooks and re-injects the user scripts marked global. An unknown id answers {ok:false} rather than an error",
			InputSchema: schema(map[string]any{"id": integer}, "id")},
		{Name: "targets_list", Description: "List the CDP targets the Core can see. The authoritative view of what is attachable, unlike miniapp_list_contexts which probes heuristically. A non-empty error field alongside an empty list means the query failed, not that there is nothing there",
			InputSchema: schema(map[string]any{})},

		// 采集健康 / 驱动 / 验证。
		{Name: "hook_stats", Description: "Read a capture hook's health: pending and dropped counters plus the feeder cursors. pageDroppedRecords / pageDroppedUpdates count records the in-page buffer evicted before anything read them — those are permanently lost, so a conclusion like 'the traffic holds no session_key' is only sound once they are zero. dropped is different: those records reached the UI and the store and remain readable through traffic_records",
			InputSchema: schema(map[string]any{
				"name": map[string]any{"type": "string", "enum": []string{"wxapi", "cloud"}},
			}, "name")},
		{Name: "wxapi_replay", Description: "Re-invoke any wx function with caller-supplied parameters — {api_name:'request', options:{url,method,data,header}}. It runs inside the miniapp's realm, but app-level auth headers are NOT attached automatically: for an authenticated replay, copy the header (e.g. token) from the captured traffic you are replaying. ok is normalized at this boundary: a replayed call that fails answers ok:false, so ok alone decides whether the replay succeeded. status (success / fail / complete) and error / reason stay in the payload. The replay bypasses the hook, so it never appears in traffic; callbacks in options are ignored. Requires hook_start",
			InputSchema: schema(map[string]any{"api_name": str, "options": object}, "api_name")},
		{Name: "navigator_auto_visit", Description: "Start walking every route the mini program reported, one page roughly every 2s, reLaunching into each. Takes no options — the delay is fixed. started:false means a walk is already running. Progress is published only as a navigate_progress event, which an MCP client cannot receive, so poll navigator_auto_visit_state to learn when it finished and use navigator_stop_auto_visit to end it early",
			InputSchema: schema(map[string]any{})},
		{Name: "navigator_auto_visit_state", Description: "Report whether a route walk is running plus its progress {done,total,failed,route} (zeroed before the first walk). The completion signal an MCP client can observe, since progress itself is published as an event",
			InputSchema: schema(map[string]any{})},
		{Name: "navigator_stop_auto_visit", Description: "Cancel a running route walk. Best effort: the visiting flag clears once the walk goroutine observes the cancellation, so confirm with navigator_auto_visit_state",
			InputSchema: schema(map[string]any{})},
		{Name: "navigator_guard", Description: "Block and record the miniapp's own redirect calls (redirectTo/reLaunch/navigateTo). The audit pattern: guard enable → navigator_visit start → blocked_redirects — a page that attempted a redirect enforces auth, one that never attempted does not",
			InputSchema: schema(map[string]any{
				"action": map[string]any{"type": "string", "enum": []string{"enable", "disable", "state"}},
			}, "action")},
		{Name: "navigator_blocked_redirects", Description: "List the redirects the guard has blocked (type, url, time). Read-only, and it only accumulates while the guard is enabled",
			InputSchema: schema(map[string]any{})},
		{Name: "navigator_visit", Description: "Route walk: action start (walk every configured route, ~2s per page, reLaunching into each — the walk survives the tool call returning, and state carries progress {done,total,failed,route}), state (running? + progress — the completion signal an MCP client gets), stop (cancel; confirm with state). Pages whose route needs params may all fail; judge by the failed count, not by absence of errors",
			InputSchema: schema(map[string]any{
				"action": map[string]any{"type": "string", "enum": []string{"start", "state", "stop"}},
			}, "action")},

		// 代码 / 凭据闭环。
		{Name: "code_projects", Description: "List the decompiled mini program projects on disk, each with the absolute path it lives at — the root to pass to miniapp_search_code, code_list_dir and miniapp_read_file. name is a display label (the output's own metadata, else the name learned while that app was connected), possibly empty",
			InputSchema: schema(map[string]any{})},
		{Name: "code_list_dir", Description: "List the immediate children of a directory. Read-only — despite the family name it does not open a file manager. A request without path is rejected rather than answered with an empty listing",
			InputSchema: schema(map[string]any{"path": str}, "path")},
		{Name: "paths_get", Description: "Report the absolute paths WxTap is using: the decompiled output root, the traffic database, the hook_scripts directory, the log directory, and the skill documents directory. Read-only",
			InputSchema: schema(map[string]any{})},
		{Name: "ak_verify", Description: "Check whether an AppID/secret pair is live by asking WeChat for an access token — the step static scans cannot take. mode: oa / mini / work. Invalid credentials are a normal result (valid:false + errcode), not an error; credentials are used once and not stored",
			InputSchema: schema(map[string]any{
				"mode":          map[string]any{"type": "string", "enum": []string{"oa", "mini", "work"}},
				"access_key":    str,
				"secret_key":    str,
				"fake_ip":       str,
				"include_token": boolean,
				"body_limit":    integer,
			}, "mode", "access_key", "secret_key")},
		{Name: "sessionkey_users", Description: "List the WeChat account directories found on disk, each with the appids seen for it and the users directory to pass to sessionkey_scan_disk. This is the offline recovery path: it reads WeChat's own storage, so it works even when no capture was ever running",
			InputSchema: schema(map[string]any{})},
		{Name: "sessionkey_scan_disk", Description: "Extract session_key / iv / encryptedData material from WeChat's own on-disk stores (MMKV, logs) for one account directory. dir must lie inside the WeChat users root — take it from sessionkey_users. Complements sessionkey_scan_traffic, which reads captured packets instead",
			InputSchema: schema(map[string]any{"dir": str}, "dir")},
		{Name: "sessionkey_scan", Description: "Extract session_key / iv / encryptedData material in one call. source: traffic (captured packets; limit), decompiled (restored sources), users (list on-disk WeChat account dirs — the discovery step), disk (WeChat's own stores; dir from users; works with no capture ever run). Read-only",
			InputSchema: schema(map[string]any{
				"source": map[string]any{"type": "string", "enum": []string{"users", "traffic", "disk", "decompiled"}},
				"dir":    str, "limit": integer,
			}, "source")},
		{Name: "extract_inventory", Description: "List the mini programs known from three merged sources (WeChat's account index, the wxapkg cache, and existing decompiled output), each with a status of decompiled / ready / indexed_only plus its package and output paths. It establishes which appids are auditable on this machine before miniapp_decompile is used. An app whose page templates come from WeChat's newer template runtime carries unsupported:true — it is outside the decompilable set and miniapp_decompile refuses it",
			InputSchema: schema(map[string]any{"dir": str, "user_dir": str})},
		{Name: "code_tree", Description: "One call returns a decompiled project's top-level file tree plus its absolute root — the shape of the whole project before code_list_dir / miniapp_read_file are used for detail. appid must already be decompiled (extract_inventory reports which appids are decompiled)",
			InputSchema: schema(map[string]any{"appid": str}, "appid")},
		{Name: "wxopen_endpoints", Description: "List the official WeChat endpoints WxTap can probe (grouped mini / oa / work), each with its method, path, parameter list (id/kind/required/hint) and what it returns. Read-only; pair with wxopen_call to test whether credentials found in the code are live",
			InputSchema: schema(map[string]any{})},
		{Name: "wxopen_call", Description: "Call one official WeChat endpoint (from wxopen_endpoints) with per-request credentials — mode mini / oa / work, access_key, secret_key, endpoint id, params (string values). Credentials travel with this one request and are never stored; invalid credentials are a normal result (errcode/errmsg), not an error",
			InputSchema: schema(map[string]any{
				"mode":       map[string]any{"type": "string", "enum": []string{"mini", "oa", "work"}},
				"access_key": str, "secret_key": str, "endpoint": str,
				"params": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
			}, "mode", "access_key", "secret_key", "endpoint")},
		{Name: "hook_list", Description: "List the hook scripts available in hook_scripts/: filename, whether it is marked global (auto-injected into every rebuilt page realm), whether it has been injected into the current page realm, its file mtime, and lastRun — the outcome of the latest injection attempt with the settled value, failure text and duration. lastRun survives a realm rebuild while `injected` does not, and `stale` is true when the file was edited after that run, i.e. the edits are not in the page yet. An empty list means no scripts are installed yet: drop *.js files into the hook_scripts directory (paths_get reports where it is) and re-run",
			InputSchema: schema(map[string]any{})},
		{Name: "hook_inject", Description: "Run one hook script in the current page realm, once, immediately, and return its settled value (summary field) plus durationMs; a failure comes back as an error. The script runs inside a scope whose console is prefixed with [filename], so its console output stays identifiable in console_list/Runtime events, and its top-level var/function declarations stay in that scope instead of becoming globals — assign to window explicitly for anything that must outlive it. filename must be a bare *.js name that already exists in hook_scripts/ (paths_get reports the directory; there is no way to pass script text, so only scripts the operator placed there can be run). Re-injecting after editing the file is the normal loop. Not persistent: a page realm rebuild clears it. Use hook_list to see what is available",
			InputSchema: schema(map[string]any{"filename": str}, "filename")},

		// 调试器：暂停 / 单步 / 栈与作用域检查。这些工具把 DevTools 的断点会话
		// 交给 agent：事件（paused / scriptParsed）由 Core 在 CDP 层捕获，工具
		// 再从快照读取——事件不会作为 MCP 通知推送，所以 pause / step 返回前会
		// 短暂轮询快照等待暂停生效。
		{Name: "debugger_enable", Description: "Enable the CDP Debugger domain on the active miniapp target so pause/breakpoint/script events start flowing. Idempotent and prerequisite for the debugger_* family: debugger_list_scripts reads the script list Debugger.enable makes the target report, and debugger_pause waits for a paused event that only arrives once it is on. miniapp_set_breakpoint also enables it on demand",
			InputSchema: schema(map[string]any{})},
		{Name: "debugger_state", Description: "Read the debug session without changing anything: Debugger domain on?, paused and why, the paused call frames (top first, 1-based line/column, scope types), script count. Poll to confirm a pause is still held",
			InputSchema: schema(map[string]any{})},
		{Name: "debugger_pause", Description: "Pause the miniapp's JavaScript at the next statement. Enables the Debugger domain if needed, sends Debugger.pause, then waits (up to ~5s) for the paused event and returns debugger_state's snapshot. Note the whole miniapp stops while paused — every other page-side tool (evaluate, navigation, hooks reading) will hang until debugger_resume",
			InputSchema: schema(map[string]any{})},
		{Name: "debugger_resume", Description: "Resume execution of a paused miniapp. Not an error when not paused: the CDP command is harmless and the answer reports the (unchanged) state",
			InputSchema: schema(map[string]any{})},
		{Name: "debugger_step_over", Description: "Step over the next call on the top paused frame: run it to completion and pause again on the same frame. Requires being paused (check debugger_state); returns the new paused frames",
			InputSchema: schema(map[string]any{})},
		{Name: "debugger_step_into", Description: "Step into the next call on the top paused frame: pause at its first statement. Requires being paused; returns the new paused frames",
			InputSchema: schema(map[string]any{})},
		{Name: "debugger_step_out", Description: "Run until the top paused frame returns, then pause in its caller. Requires being paused; returns the new paused frames",
			InputSchema: schema(map[string]any{})},
		{Name: "debugger_call_stack", Description: "Read the paused call stack: each frame's index, callFrameId, function name, url and 1-based line/column. An error when not paused — there is no stack to read",
			InputSchema: schema(map[string]any{})},
		{Name: "debugger_list_scripts", Description: "List scripts the target has parsed (scriptId + url) — the authoritative source for what to break on or fetch with miniapp_get_source. Filter by url substring; ring holds the most recent 500",
			InputSchema: schema(map[string]any{"url_filter": str, "limit": integer})},
		{Name: "debugger_get_scopes", Description: "Read the variables of one paused call frame's scopes (local → closure → global), each variable's name and value. Requires being paused; scope object ids die on resume, so fetch before stepping. Use debugger_evaluate_on_call_frame for expressions instead of reading raw scopes",
			InputSchema: schema(map[string]any{"call_frame_index": integer})},
		{Name: "debugger_evaluate_on_call_frame", Description: "Evaluate an expression in the context of a paused call frame — local and closure variables are in scope, making state visible at the paused position available for inspection. Requires being paused. Read status, not absence of error: evaluation exceptions come back as an exception field",
			InputSchema: schema(map[string]any{"expression": str, "call_frame_id": str}, "expression")},
		{Name: "debugger_pause_on_exceptions", Description: "Configure whether the miniapp pauses when an exception is thrown: state none (default), uncaught, or all (caught too). Combine with a hook script or evaluate to plant a probe, then debugger_call_stack to see who threw. Idempotent",
			InputSchema: schema(map[string]any{
				"state": map[string]any{"type": "string", "enum": []string{"none", "uncaught", "all"}},
			}, "state")},
		{Name: "debugger_control", Description: "Drive the pause session: action pause (stop at the next statement; waits for the paused event), resume, step_over / step_into / step_out (require a held pause; return the new frames). The whole miniapp freezes while paused — other page-side tools hang until resume",
			InputSchema: schema(map[string]any{
				"action": map[string]any{"type": "string", "enum": []string{"pause", "resume", "step_into", "step_out", "step_over"}},
			}, "action")},
		{Name: "debugger_breakpoint", Description: "Manage breakpoints: action set (url + 1-based line, optional condition; enables the Debugger domain), remove (breakpoint_id), list (this server's registry — CDP has no enumeration; a page realm rebuild clears target-side breakpoints, re-set them)",
			InputSchema: schema(map[string]any{
				"action": map[string]any{"type": "string", "enum": []string{"list", "remove", "set"}},
				"url":    str, "line": integer, "condition": str, "breakpoint_id": str,
			}, "action")},
		{Name: "debugger_inspect", Description: "Inspect a paused frame: with expression, evaluate it in that frame's context (locals and closures in scope); without, dump the frame's scope variables. frame_index picks the frame (0 = top). Requires a held pause — debugger_state confirms",
			InputSchema: schema(map[string]any{"expression": str, "frame_index": integer})},
		{Name: "cdp_command", Description: "Raw CDP escape hatch for capabilities without a dedicated tool (DOMSnapshot, Performance.getMetrics, DOM queries, Network.getResponseBody). Domain allowlist: Console, DOM, DOMSnapshot, Debugger, Log, Network, Overlay, Page, Performance, Runtime; Browser/Target/Emulation/Input/HeapProfiler/Tracing refused. timeoutMs caps at 60s; responses over 2MB truncated to a preview. Prefer dedicated tools",
			InputSchema: schema(map[string]any{"method": str, "params": object, "timeoutMs": num}, "method")},
	}

	// 审计工具面定义在 audit_tools.go（与分派同文件，防漂移），这里只追加。
	defs = append(defs, auditToolDefs()...)
	for index := range defs {
		defs[index].Annotations = annotationsFor(defs[index].Name)
	}
	return s.catalog(defs)
}

// catalog applies the advertisement profile. Lean (default) hides the tools a
// merged form covers: the merged tool stays the one agents see, the old name
// keeps answering tools/call for clients that hard-coded it. This is what
// keeps the surface small without breaking the "keep serving" contract.
func (s *Server) catalog(defs []toolDef) []toolDef {
	if s.deps.ToolProfile == "all" {
		return defs
	}
	lean := make([]toolDef, 0, len(leanCatalog))
	for _, def := range defs {
		if leanCatalog[def.Name] {
			lean = append(lean, def)
		}
	}
	return lean
}

// leanCatalog is the consolidated advertisement set. Names absent from it are
// still dispatchable — a merge never removes an entry point, it only stops
// advertising the redundant shape of it.
var leanCatalog = map[string]bool{
	// 会话：两把钥匙。
	"session_start": true, "session_status": true,
	// 采集与流量。
	"hook_start": true, "hook_stop": true, "hook_drain": true,
	"traffic_records": true, "traffic_get_body": true, "scan_dir": true,
	// UI 与运行时。
	"miniapp_screenshot": true, "miniapp_click": true, "miniapp_type": true,
	"miniapp_navigate": true, "miniapp_evaluate": true, "miniapp_console_log": true,
	"miniapp_page_stack": true, "miniapp_get_source": true,
	// 存储。
	"miniapp_get_storage": true, "miniapp_set_storage": true, "miniapp_remove_storage": true,
	// 云与主动验证。
	"miniapp_cloud_captures": true, "miniapp_cloud_scan": true,
	"wxapi_replay": true, "miniapp_http_request": true,
	// 导航鉴权。
	"navigator_guard": true, "navigator_visit": true, "navigator_blocked_redirects": true,
	// 审计面：资产清单与流量单条全部上榜。traffic_appids 是选 appId/appid
	// 的入口（traffic_list 的过滤、asset_scan 的目标都靠它），不能缺席。
	"asset_scan": true, "asset_list": true, "asset_export": true,
	"traffic_appids": true,
	"traffic_curl":   true, "traffic_replay": true,
	// 离线审计。
	"extract_inventory": true, "miniapp_decompile": true,
	"code_projects": true, "code_list_dir": true, "miniapp_read_file": true,
	"miniapp_search_code": true, "miniapp_scan_sensitive": true,
	"ak_verify": true, "sessionkey_scan": true,
	"code_tree": true, "wxopen_endpoints": true, "wxopen_call": true,
	// 调试器。
	"debugger_state": true, "debugger_control": true, "debugger_breakpoint": true,
	"debugger_list_scripts": true, "debugger_inspect": true, "cdp_command": true,
	// 用户脚本。
	"hook_list": true, "hook_inject": true,
}

func (s *Server) callTool(req *request) *response {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(req.Params, &params) != nil {
		return ok(req, toolError("invalid tools/call params"))
	}

	ctx := req.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	var result any
	var err error
	switch params.Name {
	case "traffic_list":
		if s.deps.Traffic == nil {
			return ok(req, toolError("traffic store unavailable"))
		}
		var args ListParams
		_ = json.Unmarshal(params.Arguments, &args)
		page, err := s.deps.Traffic.List(ctx, args)
		if err != nil {
			return ok(req, toolError(err.Error()))
		}
		result = page
	case "traffic_get_body":
		if s.deps.Traffic == nil {
			return ok(req, toolError("traffic store unavailable"))
		}
		var args struct {
			ID   string `json:"id"`
			Part string `json:"part"`
		}
		_ = json.Unmarshal(params.Arguments, &args)
		if args.ID == "" {
			return ok(req, toolError("missing required argument \"id\""))
		}
		// 缺省取 response（最常用的取证面）；非法值当场拒绝而不是静默返回空体。
		if args.Part == "" {
			args.Part = "response"
		}
		if args.Part != "request" && args.Part != "response" {
			return ok(req, toolError("part must be one of: request, response"))
		}
		body, err := s.deps.Traffic.GetBody(ctx, args.ID, args.Part)
		if err != nil {
			return ok(req, toolError(err.Error()))
		}
		// 大文体整体灌进 agent 上下文既烧 token 也可能超出客户端载荷上限；
		// 截断并如实标注，完整内容让 GUI 或更窄的查询去取。
		const bodyLimit = 256 << 10
		if len(body) > bodyLimit {
			text := string(body[:bodyLimit])
			for i := 0; i < 4 && !utf8.ValidString(text); i++ {
				text = text[:len(text)-1]
			}
			result = map[string]any{"part": args.Part, "body": text, "truncated": true, "totalBytes": len(body)}
			break
		}
		result = map[string]any{"part": args.Part, "body": string(body), "truncated": false, "totalBytes": len(body)}
	case "engine_status":
		if s.deps.Core == nil {
			return ok(req, toolError("core engine unavailable"))
		}
		result, err = s.deps.Core.Status(ctx)
		if err != nil {
			return ok(req, toolError(err.Error()))
		}
	case "hook_start", "hook_stop":
		var args struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(params.Arguments, &args)
		// 采集是使用者的显式动作：这里只放行两个审计钩子，navigator 是控制类、
		// 没有记录流，console 常开（连接即采），都不该由 agent 启停。
		if args.Name != "wxapi" && args.Name != "cloud" {
			return ok(req, toolError("name must be wxapi or cloud"))
		}
		verb := ".start"
		if params.Name == "hook_stop" {
			verb = ".stop"
		}
		result, err = s.appCall(ctx, args.Name+verb, map[string]any{})
		if err != nil {
			return ok(req, toolError(err.Error()))
		}
	case "hook_drain":
		if s.deps.Core == nil {
			return ok(req, toolError("core engine unavailable"))
		}
		var args struct {
			Name           string `json:"name"`
			AfterSeq       int64  `json:"afterSeq"`
			Limit          int    `json:"limit"`
			AfterUpdateSeq int64  `json:"afterUpdateSeq"`
			UpdateLimit    int    `json:"updateLimit"`
			WaitMs         int    `json:"waitMs"`
		}
		_ = json.Unmarshal(params.Arguments, &args)
		// 与 hook_start/hook_wait 同一校验：schema 只是给客户端的提示，服务端
		// 的拒绝才是边界。未知名字在 Core 侧的报错不可预期。
		if args.Name != "wxapi" && args.Name != "cloud" {
			return ok(req, toolError("name must be one of: cloud, wxapi"))
		}
		// 每条记录都带完整 body：默认页宽按「一页读得完」取 50，要扫全量就
		// 沿游标翻页——这是上下文成本与覆盖面的平衡点。上限与 hook_wait 同值：
		// 放行任意大的 limit 等于给「不翻页」留后门，游标语义不受影响。
		if args.Limit <= 0 {
			args.Limit = 50
		}
		if args.Limit > hookWaitMaxLimit {
			args.Limit = hookWaitMaxLimit
		}
		// 0 表示「不取更新」（与 Core 侧同一语义），缺省取一小窗；要接着上次的窗口读，
		// 把上一页的 nextUpdateSeq 填回 afterUpdateSeq。
		if args.UpdateLimit < 0 {
			args.UpdateLimit = 0
		}
		if args.UpdateLimit > hookWaitMaxLimit {
			args.UpdateLimit = hookWaitMaxLimit
		}
		if args.UpdateLimit == 0 && args.AfterUpdateSeq == 0 {
			args.UpdateLimit = defaultUpdateLimit
		}
		page, err := s.deps.Core.HookDrain(ctx, args.Name, args.AfterSeq, args.Limit, args.AfterUpdateSeq, args.UpdateLimit)
		if err != nil {
			return ok(req, toolError(err.Error()))
		}
		// waitMs>0 且这页没有新记录：服务端轮询到记录落地或超时——
		// 「触发动作后等流量」不用再让 agent 紧密循环。
		if args.WaitMs > 0 && len(page.Records) == 0 {
			page, err = s.awaitHookRecords(ctx, args.Name, args.AfterSeq, args.Limit, args.AfterUpdateSeq, args.UpdateLimit, args.WaitMs)
			if err != nil {
				return ok(req, toolError(err.Error()))
			}
		}
		// 空流序列化为 [] 而不是 null：schema 严格的客户端会拿 null 去迭代。
		if page.Records == nil {
			page.Records = []DrainedRecord{}
		}
		if page.Updates == nil {
			page.Updates = []DrainedUpdate{}
		}
		result = page
	case "scan_dir":
		if s.deps.Scan == nil {
			return ok(req, toolError("scanner unavailable"))
		}
		var args struct {
			Dir string `json:"dir"`
		}
		_ = json.Unmarshal(params.Arguments, &args)
		if args.Dir == "" {
			return ok(req, toolError("dir is required"))
		}
		scan, err := s.deps.Scan(ctx, args.Dir)
		if err != nil {
			return ok(req, toolError(err.Error()))
		}
		// 与 miniapp_scan_sensitive 同一份扫描器、同一个输出问题：压缩单行
		// bundle 的原始报告能把一次调用的回复撑到几百 KB。结果在这里整形后
		// 再交给调用方（去重、snippet 窗口化、封顶，总量如实上报）。
		report := map[string]any{}
		if len(scan.Report) > 0 {
			_ = json.Unmarshal(scan.Report, &report)
		}
		findings, total, truncated := shapeScanFindings(report["findings"])
		result = map[string]any{
			"files_scanned": scan.FilesScanned, "total_size": scan.TotalSize,
			"summary": scan.Summary, "categories_found": mapKeys(scan.Summary),
			"findings": findings, "total_findings": total, "truncated": truncated,
		}
	case "hook_wait":
		result, err = s.hookWait(ctx, params.Arguments)
		if err != nil {
			return ok(req, toolError(err.Error()))
		}
	case "traffic_records":
		result, err = s.trafficRecords(ctx, params.Arguments)
		if err != nil {
			return ok(req, toolError(err.Error()))
		}
	case "cdp_command":
		result, err = s.callCDPCommand(ctx, params.Arguments)
		if err != nil {
			return ok(req, toolError(err.Error()))
		}
	case "asset_scan", "asset_list", "asset_export", "traffic_curl", "traffic_replay":
		result, err = s.callAuditTool(ctx, params.Name, params.Arguments)
		if err != nil {
			return ok(req, toolError(err.Error()))
		}
	default:
		if sessionTools[params.Name] {
			result, err = s.callSessionTool(ctx, params.Name, params.Arguments)
			if err != nil {
				return ok(req, toolError(err.Error()))
			}
			if result == nil {
				return ok(req, toolError("unknown tool: "+params.Name))
			}
			break
		}
		if spec, known := PassThroughTools[params.Name]; known {
			result, err = s.callPassThrough(ctx, params.Name, spec, params.Arguments)
			if err != nil {
				return ok(req, toolError(err.Error()))
			}
			break
		}
		if spec, known := SelectorTools[params.Name]; known {
			result, err = s.callSelectorTool(ctx, spec, params.Arguments)
			if err != nil {
				return ok(req, toolError(err.Error()))
			}
			break
		}
		if debuggerToolNames[params.Name] {
			result, err = s.callDebuggerTool(ctx, params.Name, params.Arguments)
			if err != nil {
				return ok(req, toolError(err.Error()))
			}
			if result == nil {
				return ok(req, toolError("unknown tool: "+params.Name))
			}
			break
		}
		result, err = s.callMiniappTool(ctx, params.Name, params.Arguments)
		if err != nil {
			return ok(req, toolError(err.Error()))
		}
		if result == nil {
			return ok(req, toolError("unknown tool: "+params.Name))
		}
	}

	if image, isImage := result.(imageResult); isImage {
		return ok(req, map[string]any{
			"content": []map[string]any{{"type": "image", "data": image.Data, "mimeType": image.MimeType}},
		})
	}
	data, err := json.Marshal(result)
	if err != nil {
		return ok(req, toolError(err.Error()))
	}
	return ok(req, map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(data)}},
	})
}

// selectorTool is one tool that fronts several IPC methods, picking between
// them by a single selector argument.
type selectorTool struct {
	// Selector is the argument key whose value chooses the method.
	Selector string
	// Methods maps each accepted selector value onto an IPC method name.
	Methods map[string]string
	// Args is the allow-list of argument keys forwarded verbatim (selector
	// tools used to drop every argument; sessionkey_scan needs dir/limit).
	Args []string
}

// SelectorTools is the data half of the multi-method tools. It lives here, and
// not as a switch inside the dispatcher, so the contract test can resolve every
// target against the real router — a switch buried in the dispatcher would let a
// renamed method reach an agent as "unsupported backend method" with nothing to
// catch it.
var SelectorTools = map[string]selectorTool{
	// 只有 wxapi 与 cloud 有记录流，也就只有它们有「页内缓冲在无人读之前淘汰」
	// 的丢弃计数。
	"hook_stats": {
		Selector: "name",
		Methods:  map[string]string{"wxapi": "wxapi.stats", "cloud": "cloud.stats"},
	},
	"navigator_guard": {
		Selector: "action",
		Methods: map[string]string{
			"enable":  "navigator.enableRedirectGuard",
			"disable": "navigator.disableRedirectGuard",
			"state":   "navigator.guardState",
		},
	},
	// 遍历的启动/查询/停止三步合一。
	"navigator_visit": {
		Selector: "action",
		Methods: map[string]string{
			"start": "navigator.autoVisit",
			"state": "navigator.autoVisitState",
			"stop":  "navigator.stopAutoVisit",
		},
	},
	// 会话密钥物料的四条提取路径合一。
	"sessionkey_scan": {
		Selector: "source",
		Args:     []string{"dir", "limit"},
		Methods: map[string]string{
			"users":      "sessionkey.users",
			"traffic":    "sessionkey.scanTraffic",
			"decompiled": "sessionkey.scanDecompiled",
			"disk":       "sessionkey.scan",
		},
	},
}

// callSelectorTool resolves the selector to one method and forwards. An
// unrecognised value is refused here rather than guessed at: sending it anyway
// would turn a typo into a call against whichever method the caller seemed to
// mean.
func (s *Server) callSelectorTool(ctx context.Context, spec selectorTool, raw json.RawMessage) (any, error) {
	provided := map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		_ = json.Unmarshal(raw, &provided)
	}
	selected, _ := provided[spec.Selector].(string)
	method, known := spec.Methods[selected]
	if !known {
		allowed := make([]string, 0, len(spec.Methods))
		for value := range spec.Methods {
			allowed = append(allowed, value)
		}
		sort.Strings(allowed)
		return nil, fmt.Errorf("%s must be one of: %s", spec.Selector, strings.Join(allowed, ", "))
	}
	callArgs := map[string]any{}
	for _, key := range spec.Args {
		if value, ok := provided[key]; ok {
			callArgs[key] = value
		}
	}
	return s.appCall(ctx, method, callArgs)
}

// callPassThrough forwards one tool call to its IPC method, passing only the
// argument keys the table declares. An undeclared key is dropped rather than
// forwarded — the router would accept it happily, so this allow-list is what
// keeps a tool from turning into a general "call any method with any parameters"
// hole. A missing Required key fails here instead of at the IPC, because several
// methods answer a missing argument with a plausible-looking empty success.
func (s *Server) callPassThrough(ctx context.Context, name string, spec passThrough, raw json.RawMessage) (any, error) {
	provided := map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		_ = json.Unmarshal(raw, &provided)
	}
	callArgs := map[string]any{}
	for _, key := range spec.Args {
		if value, ok := provided[key]; ok {
			callArgs[key] = value
		}
	}
	for _, key := range spec.Required {
		if _, ok := callArgs[key]; !ok {
			return nil, fmt.Errorf("missing required argument %q", key)
		}
	}
	result, err := s.appCall(ctx, spec.Method, callArgs)
	if err != nil {
		return nil, err
	}
	return normalizePassThrough(name, result), nil
}

// normalizePassThrough rewrites the one field a pass-through payload reports
// misleadingly. wxapi.replay answers {ok:true, status:"fail"} when the
// expression evaluated but the replayed call failed — there ok describes "the
// evaluation ran", while an agent reads it as "the replay succeeded". Since a
// replay is a test, getting that backwards flips the conclusion (a rejected
// unauthenticated request would read as accepted), so at the MCP boundary ok
// means "the replay succeeded". status / error / reason stay in the payload, so
// nothing is lost.
func normalizePassThrough(tool string, result any) any {
	if tool != "wxapi_replay" {
		return result
	}
	payload, ok := result.(map[string]any)
	if !ok {
		return result
	}
	status, _ := payload["status"].(string)
	if status != "fail" {
		return result
	}
	out := make(map[string]any, len(payload))
	for key, value := range payload {
		out[key] = value
	}
	out["ok"] = false
	return out
}

func toolError(message string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": message}},
		"isError": true,
	}
}

func ok(req *request, result any) *response {
	return &response{JSONRPC: "2.0", ID: req.ID, Result: result}
}

func fail(req *request, err *rpcError) *response {
	return &response{JSONRPC: "2.0", ID: req.ID, Error: err}
}
