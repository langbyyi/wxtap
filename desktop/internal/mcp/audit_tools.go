package mcp

// 审计工具面：资产清单与流量单条助手。五个工具全部经 appCall 直通 GUI 的
// 同一份 IPC 实现（internal/api/ipc/audit.go），stdio 与 SSE 两种模式因此
// 天然同口径；这里只做三件事——参数白名单、边界钳制与诚实标注（截断、
// 上限），语义都归 IPC 服务。鉴权检测面（authz_*）随页面功能移除已下线。

import (
	"context"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// auditResultCap 是 asset_export 直接回传 content 的上限：一个完整清单的
// JSON/TXT 导出足以撑爆一次工具回复，超限截断并如实标注总字节数。
const auditResultCap = 64 << 10

// auditToolNames 是审计工具面的成员表：tools/list 的定义与测试都认这一份，
// 避免定义与断言各写一遍名单然后漂移。
var auditToolNames = []string{
	"asset_scan", "asset_list", "asset_export", "traffic_curl", "traffic_replay",
}

// auditToolDefs is the tools/list half of the audit surface, kept beside the
// dispatch so the two can not drift.
func auditToolDefs() []toolDef {
	schema := func(props map[string]any, required ...string) map[string]any {
		s := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	}
	str := map[string]any{"type": "string"}
	integer := map[string]any{"type": "integer"}
	boolean := map[string]any{"type": "boolean"}
	cloudFns := map[string]any{"type": "array", "items": str}
	return []toolDef{
		{Name: "asset_scan", Description: "构建小程序接口资产清单：从反编译代码目录（dir）、最近 500 条流量记录（includeTraffic 默认 true）与云函数名列表（cloudFns）三类来源提取 http(s)/ws 端点并去重成稳定资产（同一端点跨次运行同 ID）。appid 把流量收窄到单个小程序（清单是单个小程序的档案，建议传；缺省读全部程序的最近记录）。dir 缺省时按 appid 自动取该程序的产物目录 output/<appid>（未反编译则视为无代码来源）。三类来源全空回 {ok:false,error:\"没有可用来源\"}；代码目录不可读是错误，即使流量记录存在。异步受理：回答 taskId 即返回，完成判据是 asset_list 看到 total 变化（进度事件 MCP 收不到）。只读本地代码与流量库，不对外发请求；dir 可从 code_projects 取",
			InputSchema: schema(map[string]any{
				"dir": str, "appid": str, "includeTraffic": boolean, "cloudFns": cloudFns,
			})},
		{Name: "asset_list", Description: "分页读取资产清单：kind（api/static/ws/cloud）与 host 精确匹配、query 按 URL 子串筛选；hosts 汇总随筛选联动（host + count，按命中数降序）。尚未构建过时回答空页（total:0、空 items/hosts）而不是报错——先用 asset_scan 构建。响应带 appid 与 builtAt（这份清单属于哪个小程序、何时构建），每条资产含 url/host/path/method、来源列表（code 文件或 traffic 记录 ID）、命中次数、首末观测时间与 trafficSeen",
			InputSchema: schema(map[string]any{
				"kind": str, "host": str, "query": str,
				"offset": integer, "limit": integer,
			})},
		{Name: "asset_export", Description: "导出资产清单文本：format 取 nuclei（api/ws 的 URL 行，给 `nuclei -l`）/httpx（scheme://host 行，给 `httpx -l`）/json/txt/csv，kind/host 可先筛再导。直接回传 {ok,content}，内容超过 64KB 截断并如实标注 truncated:true 与 totalBytes；不弹保存对话框，落盘导出走 GUI 的 assets.export",
			InputSchema: schema(map[string]any{
				"format": map[string]any{"type": "string", "enum": []string{"nuclei", "httpx", "json", "txt", "csv"}},
				"kind":   str, "host": str,
			}, "format")},
		{Name: "traffic_curl", Description: "把一条已捕获流量记录渲染成 curl 命令行：bash（单引号转义，*nix）与 cmd（双引号转义，Windows）两个等价形式，-k 与重放客户端的「不校验 TLS」立场一致；头保持捕获时的原始大小写，体走 --data-raw。在最近 500 条记录里按 id 查找（更早的记录取不到），找不到回 {ok:false,error:\"记录不存在\"}",
			InputSchema: schema(map[string]any{"id": str}, "id")},
		{Name: "traffic_replay", Description: "重放一条已捕获的流量记录：以捕获时的原始方法、URL 与头对真实后端再发一次（走 GUI 设置里的 replayUpstreamProxy，不校验 TLS），响应体 ≤4KB 直接返回、超出截断并带 truncated:true——需要完整响应体时用 traffic_get_body 读捕获原文。与 wxapi_replay 的分工：那个在页内以小程序自身环境重放 wx 调用，这个在 WxTap 进程内按捕获原文重放 HTTP，适合验证「服务端认不认这份头」。在最近 500 条记录里按 id 查找，找不到回 {ok:false,error:\"记录不存在\"}。会向真实服务端发请求——只对被授权测试的环境使用",
			InputSchema: schema(map[string]any{"id": str}, "id")},
	}
}

// callAuditTool dispatches the audit tool names. Every call forwards to the
// GUI IPC method; argument handling is a whitelist (an undeclared key is
// dropped, never forwarded), matching the pass-through stance.
func (s *Server) callAuditTool(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	provided := map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		_ = json.Unmarshal(raw, &provided)
	}
	switch name {
	case "asset_scan":
		return s.appCall(ctx, "assets.scan", whitelist(provided, "dir", "appid", "includeTraffic", "cloudFns"))
	case "asset_list":
		return s.appCall(ctx, "assets.list", whitelist(provided, "kind", "host", "query", "offset", "limit"))
	case "asset_export":
		args := whitelist(provided, "format", "kind", "host")
		if _, ok := args["format"]; !ok {
			return nil, fmt.Errorf("missing required argument \"format\"")
		}
		result, err := s.appCall(ctx, "assets.export", args)
		if err != nil {
			return nil, err
		}
		capAuditContent(result)
		return result, nil
	case "traffic_curl":
		id, _ := provided["id"].(string)
		if id == "" {
			return nil, fmt.Errorf("missing required argument \"id\"")
		}
		return s.appCall(ctx, "traffic.curl", map[string]any{"id": id})
	case "traffic_replay":
		id, _ := provided["id"].(string)
		if id == "" {
			return nil, fmt.Errorf("missing required argument \"id\"")
		}
		return s.appCall(ctx, "traffic.replay", map[string]any{"id": id})
	}
	return nil, fmt.Errorf("unknown tool: %s", name)
}

// whitelist copies only the declared keys; the copy itself is the boundary —
// extra keys die here instead of reaching the IPC.
func whitelist(provided map[string]any, keys ...string) map[string]any {
	args := map[string]any{}
	for _, key := range keys {
		if value, ok := provided[key]; ok {
			args[key] = value
		}
	}
	return args
}

// capAuditContent truncates an oversized inline export at 64KB with the honest
// total. The cut lands on a byte edge, so a broken UTF-8 tail is trimmed
// before the text goes out.
func capAuditContent(result any) {
	payload, ok := result.(map[string]any)
	if !ok {
		return
	}
	content, _ := payload["content"].(string)
	if len(content) <= auditResultCap {
		return
	}
	text := content[:auditResultCap]
	for i := 0; i < 4 && !utf8.ValidString(text); i++ {
		text = text[:len(text)-1]
	}
	payload["content"] = text
	payload["truncated"] = true
	payload["totalBytes"] = len(content)
}
