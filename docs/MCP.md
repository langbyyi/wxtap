# WxTap MCP 接口面（agent 接入指南）

WxTap 将自身的调试能力作为 MCP（Model Context Protocol）服务器暴露给任意 agent：附加微信宿主、采集 wx API 与云函数流量、驱动小程序 UI、在 JS 运行时下断并暂停调试、离线反编译审计。本文为接入方式与能力索引；工具实现以 `desktop/internal/mcp/server.go` 为唯一事实来源。

## 工具分档

`tools/list` 缺省广播 **lean 目录**：合并形态的高杠杆工具。被合并吸收的旧名称仍可继续调用（`tools/call` 不返回 unknown tool），仅不再广播——硬编码旧名称的客户端不受影响，agent 的上下文中也不再出现冗余形态。需要完整清单时：stdio 加 `-tools=all`，HTTP 模式使用 `mcp.start {tools:"all"}`。本文不列出两道清单的条目数：唯一事实来源是 `desktop/internal/mcp/server.go` 中的 `tools()` 与 `leanCatalog`，`tools/list` 的返回同样可直接读取。

## 传输

| 传输 | 端点 | 适用场景 |
| --- | --- | --- |
| **Streamable HTTP**（推荐） | `POST http://127.0.0.1:<port>/mcp` | Claude / Cursor / Codex 等现代客户端 |
| HTTP+SSE（legacy） | `GET /sse` + `POST /messages?sessionId=…` | 旧客户端 |
| stdio | `WxTap.exe -mcp` | 客户端以子进程方式拉起时 |

三种传输使用同一份工具实现。HTTP 模式由 GUI 的「MCP 服务」页启动（缺省端口 9527，同一端口同时服务 `/mcp` 与 `/sse`），仅绑定 `127.0.0.1`；stdio 模式独立启动，不依赖 GUI。`initialize` 支持协议版本协商（`2025-06-18` / `2025-03-26` / `2024-11-05`），并在结果中返回 `instructions`（会话引导顺序）与 tools / resources / prompts 三项能力声明。

### 客户端配置

Streamable HTTP（GUI 启动 MCP 服务后）：

```json
{ "mcpServers": { "wxtap": { "url": "http://127.0.0.1:9527/mcp" } } }
```

Claude Code 亦可直接执行：`claude mcp add --transport http wxtap http://127.0.0.1:9527/mcp`。

stdio（无需打开 GUI，由客户端拉起进程）：

```json
{ "mcpServers": { "wxtap": { "command": "WxTap.exe", "args": ["-mcp"] } } }
```

## initialize 与引导

`initialize` 返回的 `instructions` 把引导写成两条路径：**快路径**是 `session_start` 一次调用走完（前置检查 → `engine_start` → 选目标 → 开采集，中途受阻时报出受阻于哪一步）——它在引擎与采集两段上幂等（已在运行则提前返回），**指定 `target` 时并非「什么都不做」**：会重跑 `miniapp.switch`，重置页面 generation 与两个 feeder 的 ack（切换后的小程序拥有自己的页内序号空间）。重复调用仍是幂等的：重读的帧被 shell 的去重 FIFO 抑制，面板又按 `rid` 原地覆写，同一批记录不会出现两次。`session_status` 给出同一份状态快照，但不带 `session_start` 的 `available` 字段（哪些目标可选），是近似等价而非同一份返回。**手动路径**是逐步执行 `node_status` → `wechat_status` → `engine_start` → `miniapp_list` → `miniapp_switch` → `hook_start`，最后一步成功之前无法采集到任何记录。agent 无需额外文档即可据此完成引导，各步骤的失败归因与验证见 `resources/skills/session.md`。

## 能力面

### tools

| 工具族 | 代表工具（lean） | 说明 |
| --- | --- | --- |
| 会话 | `session_start` `session_status` | 单次调用完成引导（前置检查 → engine_start → 选目标 → 开采集，失败按步骤归因）；单次调用取得全量状态快照 |
| 采集 | `hook_start/stop` `hook_drain`（`waitMs` 长轮询） `traffic_records` `traffic_get_body` `scan_dir` | 采集需显式启动；记录流与落定更新流使用独立游标；单页聚合摘要与截断正文；丢弃计数是结论可信度的前提 |
| UI / 运行时 | `miniapp_screenshot/click/type/navigate` `miniapp_evaluate` `miniapp_console_log` `miniapp_page_stack` `miniapp_get_source` | 不打断运行时的观测与驱动；`miniapp_page_stack` 单次返回导航栈、配置页面列表与当前路由 |
| 存储 | `miniapp_get_storage`（`key` 可选） `miniapp_set_storage` `miniapp_remove_storage`（`all:true` 全量清空） | 授权测试的篡改入口 |
| 调试器 | `debugger_state` `debugger_control`（pause/resume/step_*） `debugger_breakpoint`（set/remove/list） `debugger_list_scripts` `debugger_inspect`（作用域 / 帧上求值） `cdp_command` | DevTools 暂停会话；`cdp_command` 为受域白名单管控的原始 CDP 通道（响应超 2MB 截断）。暂停期间整个小程序被冻结 |
| 导航审计 | `navigator_guard` `navigator_visit`（start/state/stop） `navigator_blocked_redirects` | 守卫法检测未授权页面 |
| 代码 / 凭据 | `extract_inventory` `miniapp_decompile` `code_projects` `code_list_dir` `miniapp_search_code` `miniapp_read_file` `miniapp_scan_sensitive` `ak_verify` `sessionkey_scan`（四路合一） `hook_list` `hook_inject` | 离线审计闭环；`ak_verify` 验证凭据活性（凭据无效属正常结果，而非错误） |
| 云 / 重放 | `miniapp_cloud_captures` `miniapp_cloud_scan` `wxapi_replay` `miniapp_http_request` | 主动验证假设；`wxapi_replay` 的 `ok` 已在接口边界归一化（调用失败即 `ok:false`），`status` 取值为 `success` / `fail` / `complete`；云函数直呼使用 `miniapp_evaluate` 调用 `wx.cloud.callFunction`（`await_promise:true`） |
| 工程树 / 官方端点 | `code_tree` `wxopen_endpoints` `wxopen_call` | `code_tree` 单次返回反编译工程的顶层结构与根路径；wxopen 两项列出官方端点清单并按次携带凭据探测（配合 `ak_verify` 完成验活链路，凭据不落盘） |
| 资产清单 | `asset_scan` `asset_list` `asset_export` | 从反编译代码、最近 500 条流量与云函数名三类来源构建接口资产清单（`asset_scan` 异步受理，`asset_list` 分页读取）；`asset_export` 按 nuclei/httpx/json/txt/csv 直接回传 content（超 64KB 截断并标注 `totalBytes`），不弹保存对话框 |
| 流量单条 | `traffic_curl` `traffic_replay` | `traffic_curl` 把最近 500 条里的一条捕获记录渲染成 bash/cmd 两种 curl 命令行（保持捕获时的原始头大小写，体走 `--data-raw`），给 Repeater/脚本重放做起点；`traffic_replay` 按捕获原文对真实后端重放一次（openWorld，走 replayUpstreamProxy，响应体 ≤4KB 截断），与 `wxapi_replay` 的分工：那个在页内以小程序环境重放 wx 调用，这个在进程内验证「服务端认不认这份头」。记录不在窗口内回 `记录不存在`。`traffic.exportHar` 走原生保存对话框，不接入 MCP |

每个工具带 **annotations**（`readOnlyHint` / `destructiveHint` / `idempotentHint` / `openWorldHint`），agent 据此规划调用并判断安全边界。

刻意排除在暴露面之外的 IPC 方法（删除类、自由配置、消费语义的 poll 等）不在工具表中，见 `docs/MODULE_MAP.md` 的排除清单。

### resources

`resources/list` 列出两类资源，`resources/read` 按名返回原文（`text/markdown`）：

- `wxtap://reference/hook-records` — wxapi / cloud 钩子记录与落定更新的完整字段 schema（对齐 core/hooks 实现）；
- `wxtap://reference/engine-errors` — 引擎失败码（no_host / no_ancestor / ambiguous_host / no_version）与前置诊断顺序；
- `skill://<文件名>` — `resources/skills/` 下的技能文档：会话建立、在线调试、流量审计、代码审计、未授权检测。

### prompts

`prompts/list` 与 `prompts/get` 提供内置工作流剧本，与技能文档同源：`wx_debug_session`（会话引导）、`wx_traffic_audit`（流量审计）、`wx_code_audit`（代码审计）、`wx_pause_debug`（断点调试）。

## 实现索引

| 层 | 位置 |
| --- | --- |
| MCP 服务器（stdio + HTTP） | `desktop/internal/mcp/`（`server.go` 工具表、`debug_tools.go` 调试器、`traffic_tools.go` 长轮询与聚合、`audit_tools.go` 资产清单与流量单条助手、`cdp_command.go` 逃生舱、`http.go` Streamable HTTP、`sse.go` legacy SSE、`resources.go` 与 `reference.go`、`prompts.go`） |
| GUI 内启动与状态 | `desktop/ipc_bridge.go` 的 `mcp.start` / `mcp.status` / `mcp.stop`；前端 `desktop/frontend/src/views/McpView.vue` |
| Core 侧调试事件捕获 | `core/src/bridge/cdp-bridge.ts`（`Debugger.paused` / `scriptParsed` 环形缓冲）经 `cdp.debug` RPC 供 Go 侧读取 |
| GUI 外 stdio 启动 | `desktop/mcp_bridge.go`（`WxTap.exe -mcp`） |
| 技能文档 | `resources/skills/` |

## 事件模型

MCP 客户端无法接收 WxTap 的内部事件（例如自动遍历进度、调试器暂停通知）。凡异步完成的能力均以**状态轮询**收口：`navigator_visit {action:"state"}`（遍历完成）、`debugger_state`（是否仍处于暂停态）、`hook_drain {waitMs}`（等待服务端长轮询出新记录）、`session_status`（采集健康度）、`asset_scan` → `asset_list`（清单构建完成）。各工具描述中已写明对应的轮询义务。
