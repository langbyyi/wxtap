# GitHub 相关项目调研与开发建议

调研日期：2026-09-30。当前仓库基线：`6b048bb1bdb8749aafd351cbe8adca066a05dea7`。

本文保留上述调研基线及后续分批实施记录。各批次中的“待实现”和测试数量仅描述当时状态，不代表当前版本；现有能力与限制以 [FEATURE_MATRIX.md](FEATURE_MATRIX.md) 和 [RELEASE.md](RELEASE.md) 为准。

v1.0.4 已实现暂停策略、H5 目标发现、隔离的临时验证与常驻 DevTools 会话，并同步导航后的页面信息。H5 Network 可在独立 DevTools 查看，但尚未接入 WxTap 流量库；独立 XWeb、复杂反调试保护处理与微信业务 H5 实机验收仍未完成。以下外部项目资料保留原调研日期，本次文档核对未重新验证上游状态。

## 1. 当前项目与可复用能力

生产链路为 **Vue 3 / Pinia → Wails Go → Node / TypeScript Core → Frida / WMPF / CDP**。Core 是独立进程，通过 stdio JSON-RPC 与桌面壳通信。GUI 与 MCP 尽量共享 Go 域服务，新增功能应沿这条链路落地。

| 范围 | 已有实现与源码入口 | 后续开发应复用的基础 |
| --- | --- | --- |
| 引擎与版本适配 | `core/src/engine/wmpf-frida-runtime.ts`、`address-table.ts`、`windows-auto-detect.ts`；`resources/frida/` | Windows 两代地址布局；macOS 按架构判断覆盖；Frida 生命周期与 CDP 多目标管理 |
| 流量与历史 | `core/hooks/wxapi.js`、`cloud.js`；`desktop/internal/traffic/` | 显式启停、`rid` 幂等、落定更新、丢弃计数、SQLite 分页与正文延迟读取 |
| 请求互通 | `desktop/internal/replay/`；`desktop/internal/api/ipc/audit.go` | 已有 cURL、HTTP 重放、上游代理配置与 HAR 1.2 导出，不应重复新建这些模块 |
| 反编译与扫描 | `desktop/internal/extract/`；`desktop/internal/api/ipc/extractsvc.go` | wxapkg 处理、源码还原、规则匹配、文件/行列定位、严重性与置信度区分 |
| 资产清单 | `desktop/internal/assets/`；`desktop/ipc_audit.go`；`AssetsView.vue` | 已按 AppID 汇总代码、流量和云函数资产，支持归档、筛选、分页与 JSON/TXT/CSV/nuclei/httpx 导出 |
| 智能体集成 | `desktop/internal/mcp/`、`desktop/mcp_bridge.go`、`resources/skills/` | 已有 stdio、Streamable HTTP、legacy SSE；精简工具目录、兼容旧工具名、resources/prompts、共享 IPC 的参数白名单 |
| 构建与发布 | `.github/workflows/ci.yml`、`release.yml`、`scripts/` | 三平台离线测试、Core 构建产物校验、发布载荷、自更新校验和回滚 |

能力总览原先仍写 17 页且漏列资产页；源码实际为 7 组、18 页。本轮已同步根 README 和功能矩阵，具体入口以 [MODULE_MAP.md](MODULE_MAP.md) 与 [FEATURE_MATRIX.md](FEATURE_MATRIX.md) 为准。

## 2. GitHub 参考实现

参考项目与固定版本源码链接见下表。引用意味着确认存在相应实现，不意味着 WxTap 已完成集成或实机验证。

| 项目 | 已核验内容与适用方向 | 许可来源与边界 |
| --- | --- | --- |
| [evi0s/WMPFDebugger](https://github.com/evi0s/WMPFDebugger) | [配置与运行入口](https://github.com/evi0s/WMPFDebugger/blob/832b2bb0399d81eda8bad2cea776b68444505287/src/index.ts)、[适配说明](https://github.com/evi0s/WMPFDebugger/blob/832b2bb0399d81eda8bad2cea776b68444505287/ADAPTATION.md)：按平台/build 加载地址表，区分探测与加载错误；适合补配置来源及阶段诊断 | [GPL-2.0 文本](https://github.com/evi0s/WMPFDebugger/blob/832b2bb0399d81eda8bad2cea776b68444505287/LICENSE)；仓库另含腾讯来源代码，应逐文件核对。WxTap 地址表已来自它，不是新的依赖 |
| [linguo2625469/WMPFDebugger-mac](https://github.com/linguo2625469/WMPFDebugger-mac) | [入口](https://github.com/linguo2625469/WMPFDebugger-mac/blob/3c321171f5ac69a67dc29dfafa047a2574e38b6a/src/index.js)、[Hook](https://github.com/linguo2625469/WMPFDebugger-mac/blob/3c321171f5ac69a67dc29dfafa047a2574e38b6a/frida/hook.js)：多 PID 资源释放与 arm64/x64 配置可作研究参照；挂钩点/schema 与 WxTap 不同 | [GPL-2.0 文本](https://github.com/linguo2625469/WMPFDebugger-mac/blob/3c321171f5ac69a67dc29dfafa047a2574e38b6a/LICENSE)。版本探测失败回退固定值的做法不建议复制；上游 SIP 说明不代表本项目已验证 |
| [cqg21/wxappUnpacker](https://github.com/cqg21/wxappUnpacker) | 原项目的搬运镜像；[wuJs.js](https://github.com/cqg21/wxappUnpacker/blob/f5f0a5d9aaf082654ba46d05ae099e7725cf8727/wuJs.js) 用 sandbox 的 `define` 拆模块，适合建立旧编译格式 fixture；未核验到现代 `__wxCodeSpace__` 支持 | [GPL-3.0 文本](https://github.com/cqg21/wxappUnpacker/blob/f5f0a5d9aaf082654ba46d05ae099e7725cf8727/LICENSE)。仅借鉴格式与样例设计，不直接移植执行包内代码的恢复策略；本项目仍保留原子失败约束 |
| [wux1an/wxapkg](https://github.com/wux1an/wxapkg) | [固定 README](https://github.com/wux1an/wxapkg/blob/bae9b0f4c405ae9dfa0474daa54bb97e2074c571/README.md) 确认 Wails、缓存扫描、手选包与解包流程，适合 UX 对照；未核验内部恢复算法 | 固定快照根目录未核验到 LICENSE，不能推定 MIT 或直接搬代码。WxTap 已有主要对应能力，优先级低 |
| [gitleaks/gitleaks](https://github.com/gitleaks/gitleaks) | [文件源](https://github.com/gitleaks/gitleaks/blob/b58d3f102cf3a2c84cb7f923d05c25c9b1aed84b/sources/files.go) 分类记录部分遍历错误/文件跳过，并支持取消；[检测器](https://github.com/gitleaks/gitleaks/blob/b58d3f102cf3a2c84cb7f923d05c25c9b1aed84b/detect/detect.go) 有 entropy、allowlist、关键词和 baseline，适合完善扫描反馈与结果基线 | [MIT](https://github.com/gitleaks/gitleaks/blob/b58d3f102cf3a2c84cb7f923d05c25c9b1aed84b/LICENSE)。上游也有未完整暴露的打开文件错误，不能当作完整覆盖率范本；核心扫描器与独立 Action 的许可需区分 |
| [modelcontextprotocol/inspector](https://github.com/modelcontextprotocol/inspector) | [固定 README](https://github.com/modelcontextprotocol/inspector/blob/1e31c78fbf81a989e8eb47021c6281d7876ad7fd/README.md)、[CLI 冒烟说明](https://github.com/modelcontextprotocol/inspector/blob/1e31c78fbf81a989e8eb47021c6281d7876ad7fd/docs/cli-smoke-testing.md)：外部客户端验证入口，适合给现有协议与工具建立互操作证据 | [Apache-2.0 / 历史 MIT 过渡条款](https://github.com/modelcontextprotocol/inspector/blob/1e31c78fbf81a989e8eb47021c6281d7876ad7fd/LICENSE)。快照为 v2，Node 要求 `>=22.19.0`；实际使用须固定发布版本，不直接套用 `latest` |
| [modelcontextprotocol/go-sdk](https://github.com/modelcontextprotocol/go-sdk) | [HTTP transport](https://github.com/modelcontextprotocol/go-sdk/blob/e2ad683969e6d88216636725e467fe4cdfbabc55/mcp/streamable.go) 包含 body 上限、Host 防护、协议头校验、session 生命周期；适合核对 WxTap 手写协议行为 | [Apache-2.0 / 历史 MIT 过渡条款](https://github.com/modelcontextprotocol/go-sdk/blob/e2ad683969e6d88216636725e467fe4cdfbabc55/LICENSE)。接入前需核对 Go 与协议版本需求；本轮不替换 transport |
| [mitmproxy/mitmproxy](https://github.com/mitmproxy/mitmproxy) | [HAR 导出](https://github.com/mitmproxy/mitmproxy/blob/d0d5a70be9bfc4f99e6d90e4aa10acc142b85d21/mitmproxy/addons/savehar.py) 统计非 HTTP 跳过、表达网络错误和二进制正文；[重放](https://github.com/mitmproxy/mitmproxy/blob/d0d5a70be9bfc4f99e6d90e4aa10acc142b85d21/mitmproxy/addons/clientplayback.py) 检查资格并支持队列停止，适合完善当前导出和重放反馈 | [MIT](https://github.com/mitmproxy/mitmproxy/blob/d0d5a70be9bfc4f99e6d90e4aa10acc142b85d21/LICENSE)。用格式与行为对照即可，不需要嵌入 Python 代理 |

补充集成参照：[官方 MCP conformance 接入说明](https://github.com/modelcontextprotocol/conformance/blob/main/SDK_INTEGRATION.md)区分客户端与服务器测试，可用于设计 CI 门禁；[nuclei](https://github.com/projectdiscovery/nuclei) 与 [httpx](https://github.com/projectdiscovery/httpx) 的官方 README 分别确认 JSONL 输出能力，适合已有目标导出后的结果回流。补充链接为调研日读取的可变分支，实施前须锁定版本及对应样例。

上表源码链接固定至调研时 `git ls-remote` 取得的 HEAD，以便复查；内容经 GitHub 页面/raw 源码核验。未核验项目的归档状态或最近发布时间，也未运行上游程序。许可栏只记录源码文件声明，不替代对具体复用文件及分发方式的判断。

## 3. 源码确认的完善点

### 3.1 历史记录可见，但审计操作取不到

证据：`desktop/internal/traffic/repository.go` 的 `MaxAuditRecords = 500`；`desktop/internal/api/ipc/audit.go` 的 `auditRecord` 与 `TrafficExportHar` 只读取这个最近记录窗口。窗口按 `seq DESC, id DESC` 排序，与历史页的时间排序口径并不完全相同。

因此，历史页仍能显示的旧记录，生成 cURL 或 HTTP 重放可能返回“记录不存在”；HAR 的显式 ID 列表中，窗口外记录会被跳过，当前保存结果不报告跳过数量。这个上限是现有设计边界，不应直接删除上限或改成全库加载。

建议：单条操作按 ID 精确读取摘要及正文；显式选中导出按受限批次读取 ID；没有选中项时仍保留有界窗口，并显示实际范围。导出响应补充 `exported`、`skipped` 和原因，区分不存在、非 HTTP 记录和不支持的载荷。

落点：`internal/traffic/repository.go`、`internal/api/ipc/audit.go`、`TrafficView.vue`。验收：插入超过 500 条记录后，最旧可见记录仍能生成 cURL；显式选择含无效 ID 时不会静默成功；现有分页和正文按需读取约束保持成立。

### 3.2 扫描未命中与扫描不完整难以区分

证据：`desktop/internal/extract/scan.go` 的 `ScanFilesWithPatternsProgress` 忽略目录遍历错误，读取失败则继续；`maxFileRead` 与 `analyzer.go` 的 `maxAnalyzeLen` 均为 2,000,000 字节。`ScanResult` 有文件列表、总磁盘大小、结果与 findings，但没有读取失败及截断明细。

建议：先补覆盖情况，报告成功/跳过/失败文件数、错误与截断文件；明确标识“仅分析前 2 MB”，不要将总磁盘大小解释为已分析字节数。保持现有内存限制；是否改为分块扫描，应在跨块匹配和行列定位测试后另行决定。

落点：`internal/extract/scan.go`、`internal/api/ipc/extractsvc.go`、`components/extract/ExtractFindingsPanel.vue`、MCP 扫描返回结构。验收：空目录、路径不存在、不可读文件、超大 JS 和合法无命中文件分别得到可区分结果；旧客户端字段保持兼容。

### 3.3 MCP 有内部契约测试，仍需外部客户端验证

证据：`desktop/internal/mcp/server.go` 的 `supportedProtocolVersions` 当前声明 `2025-06-18`、`2025-03-26`、`2024-11-05`；`http_test.go`、`sse_test.go`、`surface_test.go` 已覆盖多项传输与工具行为。CI 未接入外部 Inspector / conformance 验证。

建议：先用官方客户端验证 initialize、tools/list、resources、prompts、工具错误与连接关闭；记录协商协议版本，再决定是否扩展。将 Inspector 冒烟检查与协议 conformance 检查分开，后者需选择匹配版本的场景，不能把新版测试对旧协议的失败直接认作回归。先定位协议差异，再评估是否采用 Go SDK，避免整体重写已有工具实现。

落点：`internal/mcp/`、`mcp_bridge.go`、`.github/workflows/ci.yml`、`docs/MCP.md`。验收：stdio、Streamable HTTP、legacy SSE 各有外部客户端证据；lean/all 广播差异与旧工具名仍可调用；本机来源校验与参数白名单不回退。

### 3.4 资产清单已能输出目标，缺少外部结果回流

证据：`desktop/internal/assets/export.go` 已输出 nuclei/httpx 目标列表；当前资产模型有来源、命中数和 `trafficSeen`，没有外部扫描结果导入服务。URL 查询值已归一化为键，代码中推断的方法可为 `*`，这些目标不等于完整可重放请求。

建议：新增功能优先做**用户选择的本地 JSONL 结果导入**，将 httpx 的状态/标题、nuclei 的规则与命中证据关联回指定 AppID 的资产。保留外部结果的原始 URL，并对归一化冲突、未匹配和过期结果明确计数。运行扫描器属于另一项需求，不与结果导入捆绑。

落点：`internal/assets/`、`internal/api/ipc/audit.go`、`AssetsView.vue`。验收：同域名不同方法/路径不会误合并；跨 AppID 不串档；非法或超大 JSONL 有行号错误及大小限制；重复导入有确定的幂等规则。

### 3.5 结构化 findings 可用于标准报告和版本差异

证据：`desktop/internal/extract/findings.go` 已有 `rule_id`、文件、行列、证据、严重性、置信度和 note；当前 ID 包含行号及原始值，直接用该 ID 做跨版本基线容易受代码移动影响。

建议：先增加 findings JSON / SARIF 导出，再考虑基线差异与误报标注。为跨版本比较定义单独指纹，例如规则、归一化路径、证据摘要；不要改变当前 UI 行 ID。外部分享可提供明确的脱敏导出选项，同时保留本地原文展示惯例。

落点：`internal/extract/findings.go`、扫描结果服务与反编译结果面板。验收：中文路径、中文列号、重复命中和无命中报告均有效；源码移动不会造成大量假新增；严重性不被误当成已验证漏洞。

### 3.6 平台适配和新版模板需要单独的兼容性证据

Windows 新布局缺表时，旧自动探测器不能推导新结构。macOS 当前只有一张 arm64 表；其他 mac 项目的偏移字段、挂钩点及架构分段可能不同，不能仅改字段名后合并。边界详见 [PLATFORM.md](PLATFORM.md) 与根 [NOTICE](../NOTICE)。

建议：先建立“平台 / 架构 / 微信构建 / 表 schema / 来源提交 / 实机验证状态”的兼容矩阵，候选表通过既有 `frida-tables.test.ts` 结构校验后仍必须做实机确认。新版 `__wxCodeSpace__` 模板还原应作为独立探索项，使用自有最小样例，不以旧解包器的 README 作为已支持证据。

落点：`core/src/engine/`、`resources/frida/`、`internal/extract/restore_wxml.go`、`docs/PLATFORM.md`。验收：不兼容 schema 和缺架构明确拒绝；已支持样例不回归；不支持模板仍按现有原子失败语义退出。

## 4. 建议开发顺序

| 顺序 | 工作 | 价值与工作量判断 | 完成证据 |
| --- | --- | --- | --- |
| P0-1 | 修正审计记录读取与导出范围反馈 | 直接完善现有功能；范围集中，预计小到中等 | 超过 500 条后的单条操作、跳过反馈、GUI 契约测试 |
| P0-2 | 扫描覆盖、读取错误与截断提示 | 消除“没发现”与“没扫到”的歧义；预计中等 | 失败/截断样例、扫描返回契约、结果页验证 |
| P1-1 | MCP 外部客户端兼容性门禁 | 提高整体集成可验证性；预计中等 | 固定客户端版本与协议版本、三传输报告 |
| P1-2 | findings 标准导出 | 复用现有结构化结果，便于交付；预计中等 | JSON/SARIF 样例、消费者解析、脱敏选项验证 |
| P1-3 | 外部扫描结果导入与资产关联 | 新增闭环能力；预计中等到较大 | 本地 JSONL 导入、AppID 隔离、冲突反馈 |
| P2 | 微信新构建/macOS 覆盖、新模板还原 | 价值高但取决于真实样例与设备；不提供无依据工期 | 已授权实机测试与兼容矩阵 |

以上工作量是依据当前源码的工程估计，未作实现承诺。推荐首个开发切片为 P0-1：先让用户选中的旧记录可执行单条操作，再完善 HAR 的范围与跳过反馈。它不依赖外部软件或微信版本适配，验收最直接。

新增 IPC 方法需要同步 Router、`desktop/frontend/src/api/bridge.ts` 白名单与契约测试；MCP 只开放有明确参数边界的能力，保留 [MODULE_MAP.md](MODULE_MAP.md) 已说明的 GUI 专属操作边界。修改数据返回形状时，同步 `contracts/` 及对应 Go/TS 类型。

## 5. 本轮验证与限制

- `core/`：`npm run typecheck` 通过；`npm test` 通过，24 个测试文件、318 项测试，dist RPC 校验通过。
- `desktop/frontend/`：`npm run typecheck` 通过；`npm test` 通过，44 个测试文件、524 项测试。
- `desktop/`：`go test ./...` 退出码 0；部分包使用 Go 测试缓存。
- 前端测试有 `connected` / `active` 传入 `undefined` 的 Vue prop 警告；未引起失败，本轮未修改测试夹具或产品代码。
- 本轮未重新构建安装包，也未运行覆盖率、完整 lint / vet 门禁。离线检查不代表已完成微信 attach、真实采集或 macOS E2E；这些仍以 [WECHAT_E2E_CHECKLIST.md](WECHAT_E2E_CHECKLIST.md) 为准。
- GitHub 匿名 API 受出口速率限制，调研改用 GitHub 页面、raw 源码及 `git ls-remote`。未使用 star 数或未经核实的更新时间评价项目质量；固定源码版本与许可证引用见第 2 节。

## 6. 补充：反调试干扰处理与 H5 调试

本节将“反调试能力提升”理解为：在授权调试中，减少目标的反调试逻辑对排查的干扰。以下是调研时的边界与候选方案；首批实现进展见第 7 节，尚未实机验证。

### 6.1 当前边界

- `resources/frida/hook.js` 已处理 WMPF 调试开关与 CDP 过滤；这是打开宿主调试通道的基础，不代表已解除页面 JS 的反调试检测。
- `desktop/internal/mcp/debug_tools.go` 已有暂停、恢复、单步、断点和异常暂停控制；`cdp_command` 允许 Debugger/Page/Runtime/Network 等域，可用于手动探测支持情况，但没有专门的反调试策略及重连恢复管理。
- `DevtoolsView.vue` 的 `loadTargets` 只保留 `describeTarget` 判定为小程序页面且有 AppID 的目标；`target-role.ts` 以 `servicewechat.com/.../page-frame.html` 识别这类目标。普通 H5 页面当前不会进入选择器。
- `core/src/bridge/cdp-bridge.ts` 的 `sendCommand` 依赖小程序连接，发送对象包含 `id/method/params`，未提供通用的 H5 `sessionId` 路由。已有身份探测中的临时 attach 不等于已实现 H5 会话管理。

### 6.2 值得优先实现的切片

| 方向 | 最小可用实现 | 限制与验收 |
| --- | --- | --- |
| 反复 `debugger` 暂停处理 | 增加按目标启停的“跳过全部暂停”；再提供按脚本 URL 的忽略规则。记录策略及应用结果，重连后重新探测/应用 | `Debugger.setSkipAllPauses` 连正常断点和异常暂停一起跳过，UI 必须明示，退出模式后恢复调试。blackbox 的支持与效果需实机验证；不宣称能解除计时、完整性或 CDP 检测 |
| 文档创建前注入 | 对支持 Page 域的 H5，登记预加载脚本及其 identifier，按目标管理卸载和重连；处理 frame/context 生命周期 | `Page.addScriptToEvaluateOnNewDocument` 针对创建时机，现有文档另行处理；不能假定 WMPF 逻辑层支持 Page 域。自有页面 fixture 验证首个业务脚本之前执行以及 iframe 行为 |
| WMPF 已暴露 H5 目标 | 展示目标 URL/title/type，区分小程序页面、H5、worker；增加目标 attach/detach 和 session 路由 | 先用真实 `Target.getTargets` 证据确认版本能暴露 H5；多页面命令与事件不串线，目标关闭后会话释放；不能只删除前端过滤就声称 H5 已完成 |
| 独立 XWeb H5 调试 | 探测本机 inspect/CDP 入口，读取目标列表，连接对应 `webSocketDebuggerUrl`；复用 DevTools 启动器 | 微信内置浏览器可能走独立 XWeb 进程，不能假定与 WMPF 同通道或固定使用 9222。显示“进程参数已启用 / 端点可达 / 有目标 / 已连接”四个不同状态 |
| H5 Network 与页面检查 | 按目标订阅 Network 事件、读取响应正文，补充 DOM/iframe 检查和 WebSocket 帧；带来源标记接入流量模型 | H5 的 fetch/XHR 不经过 `wx.request`，不能只复用 wxapi hook；沿用正文按需读取、容量限制与丢弃计数，和小程序采集区分来源 |

CDP 行为依据官方 [Debugger 协议定义](https://github.com/ChromeDevTools/devtools-protocol/blob/master/json/js_protocol.json)及 [Page 协议定义](https://github.com/ChromeDevTools/devtools-protocol/blob/master/json/browser_protocol.json)：跳过全部暂停、忽略脚本与新文档预加载是不同能力。协议定义来自可变 master，微信内嵌内核是否支持必须单独探测。

### 6.3 新查到的 GitHub 参照

- [Bai-Ye-Yi/WMPFDebugger-kiss](https://github.com/Bai-Ye-Yi/WMPFDebugger-kiss)：[README](https://github.com/Bai-Ye-Yi/WMPFDebugger-kiss/blob/main/README.md)声明可从目标列表调试微信内置网页，但要求先开启小程序，并保持小程序及调试窗口不关闭。可借鉴目标选择和依赖提示；本次未验证其 H5 实机兼容性，也未审查底层完整实现。
- [Zhong-fangshuo/zhong-wechat-wmpf-debugger 的 H5 探测模块](https://github.com/Zhong-fangshuo/zhong-wechat-wmpf-debugger/blob/main/h5-webview-debugger/README.md)：明确区分 WMPF 与 XWeb，探测 inspect 参数及 DevTools HTTP 端点；即使参数存在，端点也可能不开放。适合借鉴独立通道诊断，不能视作通用可用的 H5 开启方案。
- [wechatjs/mprdev](https://github.com/wechatjs/mprdev)：[README](https://github.com/wechatjs/mprdev/blob/master/README.md)使用页面 SDK 与调试服务连接。适合可修改源码的自有 H5 或测试环境，不能替代无法改源码时的原生 CDP 接入。
- [javascript-obfuscator/javascript-obfuscator](https://github.com/javascript-obfuscator/javascript-obfuscator)：[README](https://github.com/javascript-obfuscator/javascript-obfuscator/blob/master/README.md)描述 debugger、console 与 inspector 检测等保护选项，适合在自有测试页生成不同干扰样例；它是保护方案与测试参照，不是通用绕过库。

这些补充项目仅作文档级参照，尚未固定 SHA 或复用代码；实施前需再核查具体源码、许可证与微信构建。按本轮用户关注调整优先级：**先做反复暂停的可逆控制和 H5 目标发现验证，再补会话路由与 Network 接入；复杂 JS/原生反调试按实际样例另行诊断。**

## 7. 首批实现：按锁定目标控制暂停

- DevTools 新增「暂停控制」，GUI `debugger.pausePolicy` 经 Go 转发到 Core `cdp.pausePolicy`，查询与设置共用明确参数校验。浏览器预览不模拟真实目标设置成功。
- 开启依次确认 `Debugger.enable`、`Debugger.setSkipAllPauses`；若目标已经暂停，还须确认 `Debugger.resume`。正常断点和异常暂停也会被跳过，界面明确说明。
- 策略按连接保存，只允许写当前锁定目标；重载作废确认并重新应用，后台重载等切回再确认，新连接不继承策略。迟到回执不能覆盖切换后的状态。
- 超时、不支持命令与外部调试命令修改均显示未确认及错误；未确认状态下先提供显式恢复正常暂停操作。状态只是本工具最近一次确认，不承诺能读取所有外部客户端的修改。
- 补充 CDP 状态机、前端交互、IPC 参数与真实 Core bundle 接线回归。真实微信验证步骤已加入 [WECHAT_E2E_CHECKLIST.md](WECHAT_E2E_CHECKLIST.md)，离线通过不能替代实机证据。
- 本批未实现脚本忽略规则、H5 会话路由或 Network 接入，未宣称能解除完整性、计时或调试器检测。
- 本批验证：Core lint、类型检查、构建、330 项测试与 bundle RPC 校验通过；前端类型检查、生产构建校验与 527 项测试通过；Go vet、全包测试与构建通过。未重新生成安装包，未执行真实微信 E2E。

## 8. 第二批实现：H5 目标发现与诊断

- 复用现有 `targets.list`，DevTools 新增完整目标清单与 H5 候选计数，保留小程序打开入口。普通 HTTP(S) page/iframe 仅标为候选，暂不提供未经验证的 H5 attach 操作。
- 目标分类改为解析 URL 的真实主机和路径，避免普通网页的查询参数或相似域名被误判为小程序。
- 后端区分 CDP 错误、无效回执与成功空清单，保留目标查询错误原文；前端失败时清除旧结果，断线或卸载后丢弃迟到回执。
- `Target.getTargets` 的目标清单形状依据 [CDP 官方 Target 协议](https://chromedevtools.github.io/devtools-protocol/tot/Target/)。当前仅覆盖已连接的 WMPF 通道，不能据此推断独立 XWeb 是否可调试。
- 实机发现检查已加入 E2E 清单。H5 会话隔离、重连及 Network 接入仍待实现和真实目标验证。
- 本批验证：前端类型检查、生产构建校验与 533 项测试通过；Go vet、全包测试与构建通过。复核发现的“小程序断线但 Frida 仍在线时旧清单未失效”已修复，25 项分类与视图定向测试通过。未执行真实微信 H5 验证或重新生成安装包。

## 9. 第三批实现：隔离的 H5 临时会话验证

- 新增 GUI `targets.probe` → Go `ProbeTarget` → Core `cdp.probeTarget`，携带锁定连接 ID 与目标 ID；Core 重新确认目标是 HTTP(S) page/iframe，再以 `flatten:true` 临时附加，通过 `sessionId` 读取页面信息并释放会话。
- 实际执行上下文也必须是 HTTP(S) 网页，避免目标清单已出现 URL、运行时仍为 about:blank 的假成功。成功表示单次读取与释放均已确认，不表示常驻调试窗口已连接。
- 隔离早到 attach 事件、超时晚回执和近期已关闭会话事件；普通 Console 和 DevTools 不接收临时会话消息。内部验证保留 CDP ID `1000000000` 起的区间，外部客户端使用该区间会收到明确错误；外部普通命令 ID 应小于该值。
- 已知会话释放失败时保留状态，下次验证先重试释放；未确认 attach 不允许叠加新会话，晚到成功回执会触发释放尝试。目标切换后仍允许原连接上的清理回执落定，连接断开会拒绝待执行验证。
- 本机 Chrome 自有 HTTP 页面真实验证通过：`Target.getTargets` → `Target.attachToTarget` → `Runtime.evaluate` → `Target.detachFromTarget`，读到预期 URL、标题和 `complete`，没有临时会话消息转发。此验证使用内存编译的真实 Core 与 WMPF 编解码、浏览器 CDP 转接；不代表微信 WMPF 版本兼容性已验证。
- Core 347 项与前端 536 项测试通过，Core lint/类型检查/构建与 bundle RPC 校验、前端类型检查与生产构建校验、Go vet/全包测试/构建通过。真实微信步骤已加入 E2E 清单；常驻 H5 调试窗口及 Network 接入仍未实现。
