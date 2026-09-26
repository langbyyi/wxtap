# 架构（生产：Wails）

## 运行模型

```text
Vue 前端 (desktop/frontend → 嵌入 WxTap.exe)
          │ Wails IPC: window.go.main.App.Call(method, paramsJSON)
          ▼
desktop/ App (Go) ──► internal/api/ipc Router ──► 各域 handler
    │                      │
    │                      ├── extract / traffic / navigator / cloud / mcp / …
    │                      └── engine.Client (stdio JSON-RPC)
    │                                ▼
    │                         core/dist/cli.js (Node)
    │                                │
    │                                ├── Frida 注入微信进程
    │                                ├── 9421：微信小程序调试 WebSocket
    │                                └── 31415：CDP 代理 WebSocket
    │
    ├── CloudApiServer（可选，127.0.0.1:27182）
    └── MCP：`WxTap.exe -mcp`（stdio）或 GUI 内嵌 HTTP（loopback，Streamable HTTP + legacy SSE）
```

入口：`desktop/` 下的 Wails 应用（发布物 `WxTap.exe`）。前端经 `bridge.ts` 走 Wails 绑定；无壳浏览器调试时回退到本地 mock，**不是**生产路径。

## 主要链路

1. 前端调用 `App.Call(method, paramsJSON)`，信封为 IPC `{result}|{error}`。
2. Go Router 分派到 `internal/api/ipc` 及各 `internal/*` 包；需注入/CDP 的请求经 `engine.Client` 打到 Core。
3. Core（`core/dist/cli.js`）加载 `resources/frida/hook.js` 与地址表，维护 9421/31415 与页面 generation。
4. 页内 Hook（`core/hooks/wxapi.js` / `cloud.js` / `console.js` / `nav.js`）经 `Runtime.evaluate` / drain RPC 回到 Go：审计类入库并推事件，`console.js` 只进有界环形缓冲供 Console 页与 MCP 读取，`nav.js` 是控制类（页面栈、导航、防跳转），没有记录流。
   - `wxapi.js` 与 `cloud.js` 都有**落定更新流**：记录在调用时先以 `pending` 入队（进行中可见），回调落定时把 `status`/返回值/`durationMs` 作为独立一帧投递（与记录流共用一次 `hook.drain` 往返、各自独立游标与 ack）。Go 侧先入库存记录，再按 `id` 原地覆写落定字段（`traffic.Repository.ApplyUpdates`）并发 `wxapi_update` / `cloud_update` 事件；页面按 `rid` 更新同一行。
   - 记录身份 `rid = <apiType>-<appId>-<ts>-<seq>` **即** `traffic_records.id`（主键），页面钩子直接携带该值：因此「事件流 + `*.poll`」两条投递路径对页面而言是幂等的（同一 `rid` 只渲染一行），drain 后的补投递也不会产生第二行。
   - 容量关系是跨层不变量：**两个记录钩子**（`wxapi.js` / `cloud.js`）各带 5000 条记录缓冲与 5000 条更新缓冲；`console.js` 只有 1000 条记录缓冲、没有更新流，不在此等式内。shell 的重投递去重 FIFO `deliveredCapacity = 16384` 必须覆盖**单个钩子**的记录缓冲与更新缓冲之和（5000 + 5000 = 10000），两侧测试各自从一个钩子的源码解析这两个常量再断言（`hookfeeder_test.go` 读 `wxapi.js`，`cloud_contract_test.go` 读 `cloud.js`）——**并非**把两个钩子相加，`16384` 也不是按两个钩子之和取值的。
   - 事件按批投递（每 tick 每流最多一次、载荷为数组），`*.poll` 支持可选 `limit`（缺省 500、钳制 1..2000、响应恒为数组）；`*.stats` 暴露 `pending`/`dropped`（shell pending 溢出）与 `pageDroppedRecords`/`pageDroppedUpdates`（页面缓冲溢出，无法挽回），`traffic.stats` 把两者求和后给历史记录页。
   - **采集不是连接的副作用**：`wxapi` / `cloud` 钩子由 `*.start` 安装（不在连接时预装），因此连接后的调用既不进页内缓冲也不入库；`*.stop` 顺序为 stop → 清页面缓冲 → 卸载钩子 → 清 shell pending，下次 `*.start` 只记录启用之后的调用。例外是**正在捕获**时 realm 重建：钩子会被重装并把 ack 归 0（新 realm 的 seq 从 1 重来）。`console` 相反，连接即采、常开；契约由 `desktop/capture_lifecycle_contract_test.go` 锁定。
5. 后端事件经 Wails `EventsEmit` → 前端 `window.__onBackendEvent(...)`（bridge 透传）；Console 页按序号增量拉取 `console.list`，不叠加事件流，避免重复行；WxAPI / 云函数页因为记录带 `rid`，事件与 poll 双路并存也不会重复渲染。
6. 页面 realm 重建（generation 变化）时 Go 会重装 Hook、丢弃注入登记，并重新注入标记为全局的用户脚本；`window.nav` 的注入缓存同时失效并重建。

## 数据与资源位置

| 位置 | 用途 | 发布是否需要 |
| --- | --- | --- |
| `desktop/` | Go 壳、IPC 路由、MCP、解包、流量库 | 是（编译进 exe + migrations） |
| `desktop/frontend/` | Vue 源码；构建产物嵌入 exe | 是（构建期） |
| `core/` | Node TS WMPF Core（Frida/CDP/RPC） | 是（`dist/` + `hooks/` + `node_modules/`） |
| `resources/frida/` | Frida 注入脚本及平台地址表 | 是 |
| `resources/skills/` | MCP 技能说明 | 是 |
| `resources/devtools_electron.js` / `resources/icons/` | DevTools 独立窗口的 Electron 启动器（前端文件来自 Electron 自带的 `devtools://` 副本）与图标 | 是 |
| `configs/gui_config.example.json` | GUI 配置示例 | 否（示例入库；本地覆盖 gitignore） |
| `scripts/build-wails.ps1` | Windows 一键构建（出安装包） | 否（构建期） |
| `scripts/build-wails.sh` | macOS 一键构建（bundle 布局） | 否（构建期） |

可写数据默认与运行中的 `WxTap.exe` 位于同一目录（macOS 落在 `~/Library/Application Support/WxTap`）：配置、日志、反编译输出与流量数据库均位于该目录；Windows WebView2 状态放在 `webview/`。常用覆盖：`WXTAP_DATA_DIR`、`WXTAP_TRAFFIC_DB`、`WXTAP_MIGRATIONS_DIR`、`WXTAP_PACKAGES_DIR`、`WXTAP_USERS_DIR`；另有三个由**桌面壳**读取、用于决定如何拉起 Core 的变量 `WXTAP_CORE_CMD` / `WXTAP_CORE_SCRIPT` / `WXTAP_CORE_DIR`（Core 进程自身只读 `WXTAP_RESOURCE_ROOT`）。不要将用户配置或抓取结果提交到 Git。

`config.json` 除界面偏好外还存两项「本机外部程序」选择：`electron_path`（DevTools 启动器，见 `desktop/electron_runtime.go`）与 `node_path`（运行 Core 的 Node，见 `desktop/node_runtime.go`）。两者都允许留空，留空时按设置 → PATH → 常见安装位置解析。

运行日志写入 `<userBaseDir>/logs/wxtap-YYYYMMDD.log`（超 1 MB 轮转），Core stderr 与 shell 事件同文件，便于实机排障与 E2E 取证。
日志可能包含目标小程序/云函数的运行数据，对外分享前请自行审阅与脱敏。

## 端口与并发

| 端口 | 方向 | 作用 |
| --- | --- | --- |
| 9421 | 微信小程序 → Core | 固定调试服务端端口 |
| 31415 | DevTools / Core | CDP 代理默认端口（可配） |
| 27182 | 调用方 → WxTap | 可选云函数转发 HTTP |
| 9527 | MCP 客户端 → WxTap | GUI 内嵌 MCP 服务（默认；同一端口服务 `/mcp` 与 `/sse`） |

端口**缺省值**的真源是 `desktop/ports.go`（31415 / 27182 / 9527）与 `core/src/engine/wmpf-frida-runtime.ts` 的 `DEBUG_PORT`（9421）；`internal/cloudapi`、`internal/mcp` 与 `core/src/bridge/websocket-servers.ts` 都只接收端口参数，不在自身文件中写死。但端口号在别处仍有独立写法，改端口时均需同步修改：`desktop/frontend/src/ports.ts`（9421 / 31415，这两个前端常量由 `debug_menu_contract_test.go` 防止漂移）、`resources/frida/hook.js` 里的 `ws://localhost:9421`（文件内注明必须与 `DEBUG_PORT` 一致）、`CloudView.vue` 的 `27182`、`McpView.vue` 的 `9527`，以及 `configs/gui_config.example.json` 的 `cdp_port`。除 `ports.ts` 外均无测试保障。

两个回环 HTTP 面（27182 / 9527）**刻意不发送 `Access-Control-Allow-Origin`**：它们面向本机原生调用方（curl、MCP 客户端），通配的 allow-origin 会让任意网页得以驱动调试接口。不要为浏览器调用而将其加回。（27182 的每个 JSON 响应确实带 `Access-Control-Allow-Headers: Content-Type`，但缺少 allow-origin，浏览器仍然读不到响应——不应将这一条视为「已经开启 CORS」。）

仅靠“不发送 CORS 头”无法阻止网页：`text/plain` POST 属于不触发预检的 simple request，浏览器仍可发出。因此 27182 还校验 Origin（无 Origin 的原生调用、`devtools://`、回环页面放行，其余 403，见 `desktop/internal/cloudapi/cloudapi.go`）；9527 的消息端点以随机 16 字节 sessionId 作为凭据，跨站页面无法读取 SSE 流，因此拿不到该 id。9527 的三个端点（`/sse`、`/messages`、`/mcp`）另有一层 `Host` 必须是回环地址的校验，挡 DNS rebinding（`/mcp` 的校验在 `desktop/internal/mcp/http.go`，`/sse` 与 `/messages` 在 `sse.go`，三处共用 `http.go` 的 `isLoopbackHost`）；27182 没有这一层，它靠上面那条 Origin 校验。

9421 / 31415 两个 WebSocket 面做同样的来源校验（CORS 不约束 WebSocket）：只放行无 Origin 的原生客户端、`devtools://`（Electron 独立 DevTools 窗口）以及回环页面，公网页面与 `file://` 的 `null` 来源一律以 403 拒绝，不完成升级。名单在 `core/src/bridge/websocket-servers.ts`；新增客户端类型前先确认其 Origin 不是任意网页可伪造的值。

Core 是独立进程；Go 负责拉起、监督与日志转发。
