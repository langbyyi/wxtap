package mcp

// prompts 面：内置工作流配方。每条都是一份照着 WxTap 真实工具的先后约束写的
// 剧本——初始化顺序（engine_start 之后 hook_start 才有数据）、游标语义、丢弃
// 计数这些坑都来自工具实现本身，不是泛泛的渗透测试清单。agent 用 prompts/get
// 一次取走一份可直接执行的指引。

import (
	"encoding/json"
	"fmt"
	"sort"
)

type promptDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type promptMessage struct {
	Role    string         `json:"role"`
	Content map[string]any `json:"content"`
}

// promptBodies maps each prompt onto its user-message template. Bodies are
// data (not per-name switches) so prompts/get and prompts/list cannot drift.
var promptBodies = map[string]string{
	"wx_debug_session": `建立 WxTap 在线调试会话，并验证其已生效。
快速路径：session_start（幂等）单次完成 前置检查 → engine_start → 选目标 → 开采集；返回 ok:false 时按 step 字段归因（engine_start 失败码：no_host / no_ancestor / ambiguous_host / no_version），并按返回值如实报告。
需要分步执行时按以下顺序：
1. node_status — Node 22+ 是所有前置条件的前提。
2. wechat_status — 微信处于运行状态且地址表存在；地址表缺失是 no_version 的常见原因。
3. engine_start — 附加 Frida 并启动 CDP 代理（幂等）。
4. miniapp_list / miniapp_switch — 多开时选定目标。
5. hook_start {name:"wxapi"}（云函数审计追加 "cloud"）— 采集自此刻开始，此前 hook_drain 恒为空。
验证与会话快照：session_status 单次返回引擎状态、目标、页面栈、采集健康（丢弃计数）与库大小。需向用户说明：engine_stop 会丢弃尚未被取走的记录。`,

	"wx_traffic_audit": `审计一个小程序的网络与云函数行为。先按 wx_debug_session 的流程建立会话并开启采集，随后：
1. 在小程序内实际操作（或以 navigator_visit {action:"start"} 遍历路由，以 navigator_visit {action:"state"} 轮询完成）；触发动作后以 hook_drain {name, afterSeq, waitMs} 在服务端等待流量落地，不使用紧密轮询。
2. hook_drain {name:"wxapi", afterSeq:<游标>} 增量读取记录：records 为请求发起时的记录，updates 为异步调用落定后的结局（status / 返回值 / durationMs）。必须将上一页的 nextUpdateSeq 回填到 afterUpdateSeq 才能继续读取。记录字段 schema 见资源 wxtap://reference/hook-records。
3. hook_stats {name:"wxapi"}（或 session_status）先读取健康度：pageDroppedRecords / pageDroppedUpdates 不为 0 表示存在记录在无人读取前被永久淘汰，任何「流量中不含 X」的结论都必须先说明丢失数量。
4. traffic_records 按 appId / apiType / status 过滤，单页返回摘要与截断后的正反文体；需要某条完整文体时改用 traffic_get_body。检索明文凭据、越权参数与未加密的敏感字段。
5. wxapi_replay 可以小程序自身凭据重放某个 wx 调用以验证假设；ok 已在接口边界归一化 —— 被调调用失败时 ok 为 false，status（success / fail / complete）与 error 一并保留。
6. 输出：按接口归纳数据流，标注可疑点（明文敏感信息、无鉴权参数、可重放调用）。`,

	"wx_code_audit": `离线代码审计：反编译 → 检索 → 敏感信息 → 凭据验活。
1. extract_inventory {dir?, user_dir?} — 三源合并的 appid 清单（status ∈ decompiled | ready | indexed_only；unsupported: true 表示页面模板是微信新版编译模板运行时生成的、WxTap 还原不了，不在可反编译范围内，别对它调 miniapp_decompile），先确定审计目标。
2. miniapp_decompile {appid} — 该工具不检查反编译状态，重复调用会重新反编译并替换产物目录。
3. code_projects / code_tree / code_list_dir / paths_get — 取得产物目录的绝对路径。读取反编译产物优先直接访问源码目录；MCP 读取工具用于客户端无文件系统通道的场景。
4. miniapp_search_code {root, query, regex} 分主题检索：网络（https://、request）、云函数（callFunction）、加密（encrypt/AES/RSA）、凭据（secret/key/token）、鉴权（login/checkSession）。
5. miniapp_read_file 读取命中文件以确认上下文；scan_dir 对整个目录执行敏感信息分析。
6. miniapp_scan_sensitive {appid} — 结构化 findings（severity/confidence/file/line/masked）。该项仅提供静态线索，不验证凭据有效性。
7. 对疑似有效的 AppID 与 Secret 组合以 ak_verify {mode, access_key, secret_key} 验活：凭据无效（valid:false 与 errcode）属正常结果，仅传输失败才是错误。
8. 输出：发现清单（位置、证据、建议），静态推测与已验证结论分列标注。`,

	"wx_pause_debug": `以断点调试一个运行中的小程序 —— 将 DevTools 的暂停会话交由 agent 执行的流程：
1. 建立会话并选定目标（见 wx_debug_session）。
2. debugger_list_scripts {url_filter} — 定位目标脚本（scriptId/url；Debugger 域会按需自动启用）。
3. debugger_breakpoint {action:"set", url, line}（line 从 1 起）或 debugger_control {action:"pause"} 在下一条语句暂停。注意：暂停会冻结整个小程序，其它页面侧工具均无响应，直到 resume。
4. 暂停后：debugger_state 查看调用栈（基于 1 的行列号、每帧 scope 类型）→ debugger_inspect {frame_index} 读取作用域变量，或 debugger_inspect {expression, frame_index} 在该帧上下文中求值。
5. debugger_control {action:"step_over"/"step_into"/"step_out"} 单步执行（均要求处于暂停态）；以 debugger_state 随时确认是否仍处于暂停态。
6. debugger_control {action:"resume"} 恢复执行。断点不再需要时以 debugger_breakpoint {action:"remove", breakpoint_id} 删除，{action:"list"} 查看当前断点。
7. 输出：触发路径、关键变量的值、结论。`,
}

// listPrompts is the prompts/list payload, stable order for client UIs.
func listPrompts() []promptDef {
	names := make([]string, 0, len(promptBodies))
	for name := range promptBodies {
		names = append(names, name)
	}
	sort.Strings(names)
	prompts := make([]promptDef, 0, len(names))
	for _, name := range names {
		prompts = append(prompts, promptDef{Name: name, Description: promptDescription(name)})
	}
	return prompts
}

func promptDescription(name string) string {
	switch name {
	case "wx_debug_session":
		return "Bootstrap a WxTap debug session step by step (prerequisites, engine_start, target selection, capture start) and verify each step took effect"
	case "wx_traffic_audit":
		return "Capture and audit mini-program network / cloud traffic: cursored hook_drain reads, dropped-record health checks, body inspection and replay"
	case "wx_code_audit":
		return "Offline code audit: inventory, decompile, search, sensitive-info scan and credential liveness verification"
	case "wx_pause_debug":
		return "Debug the running miniapp with breakpoints and the pause session: scripts, stack, scopes, frame evaluation, stepping"
	default:
		return ""
	}
}

// getPrompt answers prompts/get. An unknown name is a JSON-RPC invalid-params
// error, not an empty prompt — a typo must not read as an empty workflow.
func (s *Server) getPrompt(req *request) *response {
	var params struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(req.Params, &params) != nil || params.Name == "" {
		return fail(req, &rpcError{Code: -32602, Message: "prompts/get requires a name"})
	}
	body, known := promptBodies[params.Name]
	if !known {
		return fail(req, &rpcError{Code: -32602, Message: fmt.Sprintf("unknown prompt: %s (prompts/list names the valid ones)", params.Name)})
	}
	return ok(req, map[string]any{
		"description": promptDescription(params.Name),
		"messages": []promptMessage{{
			Role: "user",
			Content: map[string]any{
				"type": "text",
				"text": body,
			},
		}},
	})
}
