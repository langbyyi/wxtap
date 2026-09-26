# 模块映射（生产：Wails / Go / Core）

## 入口与编排（生产）

| 模块 | 职责 |
| --- | --- |
| `desktop/main.go` / `desktop/app.go` | Wails 入口、`App.Call`、生命周期、Core 监督、事件回推 |
| `desktop/ipc_bridge.go` / `desktop/ipc_exploit.go` | **全部 GUI IPC 方法的注册点**（`App.Call` → Router；engine / traffic / wxapi / cloud / console / navigator / extract / code / hook / sessionkey / ak / config / settings / log / node / mcp / shell / update；`wxopen.*` 在 `ipc_exploit.go`），handler 转发给 `internal/*` 各域包 |
| `desktop/internal/api` + `internal/api/ipc` | Router 与 IPC 信封（`{result}`/`{error}`）、绑定层类型，以及 extract / code / hook feeder / sessionkey / ak / cloud 导出 / task 的共享实现（方法本身不在这里注册）。`ipc/hookscript.go` 生成两类页内 JS：vConsole 开关表达式与**用户脚本包装层**（`[文件名]` console 前缀 + sourceURL + 直接 eval 保住完成值），`hookscript_node_test.go` 用真实 node 运行这些语义 |
| `desktop/mcp_bridge.go` + `internal/mcp` | `WxTap.exe -mcp` stdio 与 GUI 内 HTTP（`/mcp` Streamable + `/sse` legacy）；工具清单见 `internal/mcp/server.go` 的 `tools()`，接入指南见 docs/MCP.md |
| `scripts/build-wails.ps1` | Windows 生产发布：core + desktop/frontend + wails build → `desktop/build/release/`（载荷）+ `release-dist/WxTap-setup.exe`（NSIS 安装包，自行获取便携 NSIS，并把载荷插进安装段重打） |
| `scripts/package-release.ps1` | 两平台共用：把载荷按平台切成 tar.gz + sha256 → 清单片段 → 合并 `latest.json` → `desktop/build/release-dist/`（客户端只认这份清单） |
| `scripts/merge-manifest.mjs` | 清单合并的唯一实现：一个平台写一条 `platforms[<goos>-<goarch>]`，两个平台依次运行即得到双平台文档 |
| `scripts/build-wails.sh` | macOS 生产发布：core + desktop/frontend + wails build → `WxTap.app`（资源入 `Contents/Resources/`）+ `release-payload/`（扁平更新载荷）+ `release-dist/WxTap.dmg` |
| `.github/workflows/release.yml` | 打 `v*` tag 即自动构建两平台产物、合并清单、创建 Release、把 `latest.json` 提交到默认分支 |
| `docs/RELEASE.md` | 发版操作说明（产物、清单契约、验收、排错）。**改发布链路必须同步改它** |
| `core/src/cli.ts` → `core/dist/cli.js` | Node Core 进程入口（stdio JSON-RPC） |

GUI 方法仍按域命名，已注册的前缀：`engine.*`、`miniapp.*`、`navigator.*`、`traffic.*`、`cloud.*`、`cloudapi.*`、`electron.*`、`wxapi.*`、`console.*`、`extract.*`、`code.*`、`hook.*`、`targets.*`、`sessionkey.*`、`wxopen.*`、`ak.verify`、`config.*`、`settings.*`、`mcp.*`、`update.*`、`shell.*`、`node.*`、`log.*`、`wechat.*`、`fetch.md`、`test.*`（只在测试里注册，不是产品面）。新增方法先在 Go Router 注册稳定名，再补前端与契约测试，并同步 `desktop/frontend/src/api/bridge.ts` 的 `supportedMethods` 白名单 —— 前端导出的 `backend.call` 对白名单外的名字直接抛 `Unsupported backend method`，而 `TestFrontendMethodAllowListMatchesRouter` 要求「Router 注册了就必须被前端调用，或列进该测试的 `backendOnly`」。（这是**前端那一层**的名单；Go 侧 MCP 直通用的 `AppBridge`（`internal/mcp/server.go` 的接口）是另一条通路，它的边界由 `PassThroughTools` 的 `Args` 白名单约束。）

## 核心运行时（生产）

| 模块 | 职责 | 主要依赖 |
| --- | --- | --- |
| `core/src/engine/*` | Frida 附加、状态、页面 generation；`wmpf-frida-runtime.ts` 双平台共用生命周期，`win32-target.ts` / `darwin-target.ts` 各管宿主进程定位与地址表目录 | `frida`（**external**，勿打包进 bundle） |
| `core/src/bridge/*` | 9421 / 31415（CDP，可配）两个 WebSocket 服务端、多连接/锁定 | `ws` |
| `core/src/protocol/*` | WMPF 私有协议编解码 | protobufjs |
| `core/src/hooks/*` + `core/hooks/*.js` | drain 协议与页内 Hook 源（wxapi / cloud / console / nav）；**wxapi 与 cloud 各带落定更新流**（独立游标与 ack；console 只有单条记录缓冲，nav 是控制类、无记录流），记录身份 `rid` 与 `traffic_records.id` 同公式 | — |
| `core/src/rpc/*` | stdio JSON-RPC | — |
| `desktop/internal/engine` | Go 侧 Core 客户端、日志、拉起 | — |
| `desktop/internal/navigator` / `cloud` / `extract` / `traffic` | 导航、云、解包扫描、SQLite 流量库 | — |
| `desktop/internal/devtools` / `cloudapi` | DevTools Electron 启动器定位、云 API HTTP | — |
| `desktop/internal/update` | 两条链路：资源增量同步（WMPF Frida 地址表 / MCP skills，从仓库 contents API 匿名读取，按 git blob 名比对内容，无对象存储）；应用自更新（清单 → 分片下载 + sha256 校验 → 解到数据目录 `.update/` → 写待生效标记 → 下次启动换入 + 失败回滚，见 `apply.go`） | 仅标准库 |
| `desktop/internal/ak` | 公众号 / 小程序 / 企业微信凭据官方接口验活（请求与响应原样返回，不脱敏） | — |
| `desktop/internal/wxopen` | 官方接口调用台：只读/生成类接口，token 进程内缓存 | `internal/ak` |

## 离线分析（生产）

| 模块 | 职责 |
| --- | --- |
| `desktop/internal/extract` | wxapkg 解密/解包、结构化敏感信息发现（内存态，页面展示） |
| `desktop/internal/api/ipc`（extract/code handlers） | 与前端约定的路径参数、进度事件、代码浏览器 |

## 前端（生产）

| 模块 | 职责 |
| --- | --- |
| `desktop/frontend/src` | Vue 3 源码（Pinia / Router） |
| `desktop/frontend/src/api/bridge.ts` | IPC：Wails 绑定（`window.go.main.App.Call`）> 无壳浏览器调试时的本地 mock（**不是**生产路径）；调用前按 `supportedMethods` 白名单过滤，白名单外的名字直接抛错 |
| `desktop/frontend/` | 唯一前端；`npm run build` → `dist/` 供 `go:embed` |

## MCP 工具面（生产）

工具清单以 `internal/mcp/server.go` 的 `tools()` 为唯一事实来源（文档不再写死数量：每新增一个工具都要同步三处数字，必然会产生漂移）。`tools/list` 默认广播 lean 合并目录；被合并吸收的旧名字仍可调用（dispatch 与广播分离），`-tools=all` / `mcp.start {tools:"all"}` 恢复全量广播。工具分四类：

1. **`miniapp_*` 直连工具**：走 Core 的 CDP 直连，命名与实现保持不变；这份冻结面的名字记在 `desktop/testdata/compat-surface.json`，由 `TestCompatMcpToolsStayExposed` 锁定。
2. **直通工具（`PassThroughTools`）**：把 agent 的调用转成一条已注册的 GUI IPC 方法，agent 与 GUI 看到同一份实现。表里每条声明 `Method`，**需要入参的条目再声明入参白名单 `Args`**（外加 `Required`；无参的读工具不声明）。白名单是必需的：`AppBridge.Call` 能到达每一个已注册方法，一个无界转发就等于把整个 router 交给 agent（含 `extract.delete`、`config.save`）。**不要新增「调用任意 IPC 方法」的通用工具。**
3. **一工具落多方法（`SelectorTools`）**：`hook_stats`、`navigator_guard`、`navigator_visit`、`sessionkey_scan` 四个——按选择子（`name` / `action` / `source`）在**数据表**里映射到具体方法，非法选择子当场拒绝、不触达 IPC。表刻意放在 `server.go` 的 `SelectorTools` 而不是 dispatcher 里的 `switch`：契约测试要拿真 router 逐条解析每个目标，埋在 switch 里的映射会让改名后的方法以「unsupported backend method」到达 agent 而无人能够拦截。
4. **`debugger_*` 工具族（`debug_tools.go`）**：DevTools 暂停会话。CDP 事件（`Debugger.paused` / `scriptParsed`）由 Core 在 CDP 层捕获（`core/src/bridge/cdp-bridge.ts`），经 `cdp.debug` RPC 供工具读快照；pause / step 返回前轮询快照等事件落地。

采集开关 `hook_start` / `hook_stop` 只是把 `wxapi.*` / `cloud.*` 暴露给 agent（记录流仍由 `hook_drain` 读）—— 连接小程序本身不采集，所以 agent 必须先显式开启采集，才有内容可供 drain。

**agent 面刻意排除的能力**（而非遗漏）：`extract.delete` / `extract.clearOutput`（无 dryRun、无回收站）、`config.save`（自由 patch、无键校验、无备份）、`traffic.clear` / `traffic.delete`（删多少、删哪几条都要人在环里确认）、`hook.setGlobal`（粘性配置）、`wxapi.poll` / `cloud.poll`（**消费语义**会抢 GUI 的记录，`hook_drain` 已是游标式）、`miniapp.getLock`（任何失败都回 `{enabled:true}`，乐观真值会产生误导）、`targets.attach`（Go 侧丢弃 sessionId、无后续管道，疑为空操作，待实机确认），以及需要 Wails GUI ctx 弹原生对话框的 `extract.browse` / `cloud.export`（stdio 模式结构性不可用；审计面的落盘导出同理——`assets.export {save:true}`、`traffic.exportHar` 不进 MCP，agent 侧取清单用 `asset_export` 直接回传 content；`traffic.replay` 同样只留给 GUI 主动触发，不设工具入口，带外发请求的只有既有 openWorld 工具）。同理刻意不暴露的还有：`update.*`（agent 自更新运行中的工具是事故）、`shell.openDevtoolsWindow` / `shell.openFolder` / `shell.openUrl` / `code.openDir` / `extract.openDir` / `engine.vconsole`（均为「替用户开窗口」，agent 有 CDP 与 http_request，无需代为开窗；`shell.openDevtools` 浏览器路径已于 2026-09 整体删除）、`cloudapi.*`（给小程序用的本地云 API mock 服务，由 GUI 按需启停）、`config.load`（`settings.getPaths` 已覆盖 agent 需要的读面；`config.save` 自由 patch 无校验）、`code.formatFile` / `code.formatAll`（agent 原生会格式化）、`log.list` / `log.clear`（桌面壳自身日志，诊断走 `node_status` / `wechat_status` / engine 失败码）、`wxapi.clear` / `cloud.clear` / `hook.clear`（游标式读取下清缓冲没有必要，`hook_stop` 已丢弃未取记录）、`fetch.md`（`miniapp_http_request` 覆盖）、`node.detect` / `sessionkey.detect`（分别被 `node_status` / `sessionkey_scan {source:users}` 覆盖）、`extract.builtinPatterns` / `extract.candidateDirs` / `extract.scan` / `extract.decompileAll`（被 `scan_dir` / `extract_inventory` / `miniapp_decompile` 循环覆盖）、`test.*`（自测钩子）。`miniapp.getLock` / `miniapp.setLock`、`wxapi.poll` / `cloud.poll`、`targets.attach`、`hook.setGlobal`、`extract.delete` / `extract.clearOutput`、`traffic.clear` / `traffic.delete` 见上文理由。

更改工具名须同步 `resources/skills/`。
