# WxTap

WxTap 是面向**授权安全测试**的微信小程序调试工具：一个 Wails 桌面 GUI，外加一套可供智能体接入的 MCP 工具面。

小程序在微信桌面端内运行于 WMPF 宿主进程（Windows 为 `WeChatAppEx.exe`；macOS 上使用该进程名的是其 worker `WeChatAppEx Helper`，宿主为这些 worker 的父进程），其运行时对象从外部不可见。WxTap 通过 Frida 注入宿主并接入 WMPF 自带的 9421 调试通道，从而可展开小程序运行时的以下层面：

- **发出的请求** —— `wx.*` 与云函数调用的参数、返回值、耗时、错误实时可见，并可按原参数重放；
- **当前页面路由** —— 当前路由实时回读，可跳转到任意配置页面或按列表自动遍历；页面栈与各层查询参数经 `navigator.pageStack` / MCP `navigator_page_stack` 可读；
- **源码** —— 从微信小程序包还原工程，标出敏感信息，并支持按目录与全文检索；
- **凭据与官方接口** —— 从报文或源码提取 `session_key` 用于开放数据解密，用 AppID / AppSecret 验活，或按凭据类型调用官方接口。

仓库 `github.com/langbyyi/wxtap`，作者主页 <https://github.com/langbyyi>。生产主线为 **Vue 前端 → Wails Go 桌面壳 → Node/TypeScript Core → Frida**。

> **使用边界**：WxTap 仅应用于自有小程序，或已获书面授权的目标。它刻意在本地**按原文**呈现请求参数、返回值、源码与凭据（掩码会使「签名算错」与「值看错」相互混淆），因此日志与导出结果对外分享前必须自行审阅与脱敏。

## 能力总览

界面按 7 个分组组织，共 17 个页面。

| 分组 | 页面 | 作用 |
| --- | --- | --- |
| **连接** | 状态 | 配置端口、启停引擎；查看微信宿主、Frida、小程序与 DevTools 的对接状态 |
| **调试** | 页面路由 · Console 日志 · DevTools · vConsole · 注入脚本 | 当前路由回读与跳转、console 与未捕获错误、独立 DevTools 暂停调试、vConsole 面板、自定义脚本注入 |
| **流量** | WxAPI · 云函数 · 历史记录 | 实时捕获与重放 `wx.*` 及云函数调用；已落库记录分页回看 |
| **代码** | 反编译 · 代码浏览 | 从 wxapkg 还原源码并标出敏感信息；按目录与全文检索浏览。页面模板由微信新版编译模板运行时（`__wxCodeSpace__`）生成的小程序无法还原——此类 app 在枚举阶段即被识别并排除出可反编译列表；否则整次还原会中止且不产出任何文件（该失败路径是原子的，不存在「部分成功」） |
| **利用** | SessionKey · 微信 AK | 提取 `session_key` 做 AES 加解密；填 AppID / AppSecret 向官方接口验活 |
| **系统** | 设置 · MCP 服务 | 运行目录与外部程序路径、版本检查；启动 MCP 服务供外部智能体接入 |
| **帮助** | 使用帮助 · 交流反馈 | 上手流程与报错处理；反馈渠道与更新说明 |

以下几条贯穿全局的设计由契约测试锁定：

- **采集不是连接的副作用** —— 连接小程序后未启用「开启捕获」时不会记录任何调用；启用后仅记录启用之后的调用。否则首次启用会把连接以来的积压误当作新流量输出。
- **一条记录只渲染一行** —— 记录身份 `rid` 与流量库主键同源，事件流与轮询两条投递路径对同一 `rid` 幂等，drain 之后的补投递也不会产生第二行。
- **正文按需读取** —— 列表始终不携带请求/响应正文，历史记录按页码逐页查看，不一次性加载全部记录。
- **丢弃计数如实上报** —— 页内缓冲或投递队列溢出时报 `dropped`，避免「数据不全」与「本来就没有」在呈现上无法区分。
- **长任务可见** —— 反编译、扫描、导出等异步任务必须显示阶段、进度与失败原因。

## 架构

```text
Vue (desktop/frontend) → Wails Go (desktop/) → Node Core (core/) → Frida / CDP
```

- **Core 是独立进程**，Go 负责拉起、监督与日志转发；Core 加载 `resources/frida/hook.js` 与地址表，维护 9421（WMPF 调试通道）与 31415（CDP 代理）。
- **两个回环 HTTP 面**：27182（可选云函数转发）与 9527（GUI 内嵌 MCP 服务，同一端口同时提供 Streamable HTTP 的 `POST /mcp` 与旧式 SSE 的 `GET /sse`），仅绑定 `127.0.0.1`，且刻意不发送 `Access-Control-Allow-Origin`。
- **MCP 同一份实现跑三种传输**：Streamable HTTP、HTTP+SSE（legacy）、stdio（`WxTap.exe -mcp`）。

进程模型、端口、数据位置与来源校验的细节见 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)。

## 平台支持

| 平台 | 状态 |
| --- | --- |
| **Windows x64** | 当前交付主线（Wails `windows/amd64`，Core `createWindowsFridaRuntime`），地址表覆盖 54 个微信构建 |
| **macOS（Apple Silicon）** | 已发布 `WxTap.dmg`（`darwin-arm64`，未公证，首次打开需右键 → 打开）。构建、bundle、`.dmg` 挂载与启动由 CI 在真 Mac 上逐次验证；但**可用边界很窄**，用之前请读 [docs/PLATFORM.md](docs/PLATFORM.md)：静态地址表**只覆盖 1 个微信构建**，且只有 `arm64` —— 不在表内的构建号与本机是 Intel 时，引擎能启动但**不会挂钩**；Frida attach 还可能要先处理 SIP / 代码签名 |
| **macOS（Intel）** | 代码与打包脚本支持（`build-wails.sh` 按当前架构构建），但**没有发布产物**，也**没有 x64 地址表** |
| Linux 桌面微信 | **不在产品范围**（云端 Linux 仅作开发 / 单测 / 交叉编译） |

macOS 侧的**实机 E2E（对已登录微信的 attach、地址表准确性）尚未做过**，这也是 `docs/WECHAT_E2E_CHECKLIST.md` 里 mac 前置项仍待办的原因；已发布的是「能装、能启动、能与官方接口交互」的部分，挂钩能力取决于你的微信构建号是否在表内。详见 [docs/PLATFORM.md](docs/PLATFORM.md) 与 [docs/RELEASE.md](docs/RELEASE.md) §三。

## 安装（发布包）

Windows 10/11 x64 用户只需下载**一个文件** `WxTap-setup.exe`。它以 per-user scope 安装到 `%LOCALAPPDATA%\Programs\WxTap`（普通用户即可写入，无需管理员权限）。

macOS（Apple Silicon）用户下载 `WxTap.dmg`，打开后把 `WxTap.app` 拖进 `Applications`。`.dmg` 未公证，首次打开需在 Finder 里右键 → 打开（或 `xattr -dr com.apple.quarantine /Applications/WxTap.app`）；去掉这一步需要付费的 Apple Developer ID。

单独一个 `WxTap.exe` 无法运行 —— 它需要从同级目录读取 `core/`、`resources/`、`migrations/`，因此「一个文件」由安装包实现，而非单文件 exe。安装后的原位更新也在同一目录替换文件。macOS 上同样的内容位于 bundle 的 `Contents/Resources/`，所以「一个文件」由 `.dmg` 实现。

运行还需要：

- **Node.js 22+** —— 发布包**不自带** Node 运行时，Core 由用户环境里的 Node 启动。解析顺序是 `WXTAP_CORE_CMD` → 设置页保存的路径 → PATH → 常见安装位置，每个候选都先跑 `node -p process.versions.node` 校验。前两级是**显式指定**，不合格即当场报错，不会静默改用其他 Node；PATH 与自动检测这两级会跳过不合格的候选继续查找，跳过原因记录在设置页的解析结果中；全部不可用时给出可执行的错误信息。
- **WebView2 Runtime** —— 仅 Windows：Win11 自带，Win10 通常随 Edge 装上，精简版可能缺失。
- **微信桌面端** —— 已登录，作为附加目标。

## 从源码构建

```powershell
.\scripts\build-wails.ps1
```

脚本会构建 `core/` 与 `desktop/frontend/`、调用 Wails，并将可运行目录整理到 `desktop/build/release/`（含 `WxTap.exe`、旁路 `core/`、`resources/`、`migrations/`），同时产出安装包 `desktop/build/release-dist/WxTap-setup.exe`。

macOS（需在对应架构的 macOS 上执行，脚本按当前架构构建）：

```bash
./scripts/build-wails.sh
```

产出 `desktop/build/release/WxTap.app`，`core/`、`resources/`、`migrations/` 全部位于 bundle 的 `Contents/Resources/`。

手动步骤等价于：

```powershell
cd core;                npm ci; npm run build
cd ..\desktop\frontend; npm ci; npm run build
cd ..;                  go run github.com/wailsapp/wails/v2/cmd/wails@v2.16.0 build -clean -platform windows/amd64
```

**环境要求**：Go 1.25+、Node.js 22+、Wails v2 CLI（发布构建时）。发布目录携带 `core/`（含 `node_modules`）、`resources/` 与 `migrations/`，但**不含 Node 运行时**。

## 开发与测试

```bash
cd core && npm run lint && npm test        # ESLint + vitest + dist 检查（frida 必须 external）
cd core && npm run coverage                # 覆盖率门禁
cd ../desktop/frontend && npm ci && npm test && npm run build
cd ../desktop && go vet ./... && go test ./...   # 契约 / 路由 / 流量 / MCP / 引擎客户端
cd ../desktop && golangci-lint run ./...   # 0 issues 为准
```

> `desktop/main.go` 用 `//go:embed all:frontend/dist` 嵌入前端产物。全新克隆后直接 `go build` / `go test` 会因该目录缺失而报 `pattern all:frontend/dist: no matching files found`：请先执行前端构建，或临时放一个占位 `frontend/dist/index.html`（CI 即用此方式）。

实机验证（Frida / 9421 / 31415 / 多开 / Hook 实捕）**不能**以离线测试替代，必须按 [docs/WECHAT_E2E_CHECKLIST.md](docs/WECHAT_E2E_CHECKLIST.md) 在授权微信桌面端上完成端到端确认。更多命令与改动指南见 [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md)。

## 发布新版本

一条命令，其余全自动（跑本地门禁 → 改版本号 → 提交 → 打 tag → 推送；CI 接管构建、发布 Release、把含更新说明的 `latest.json` 提交回 main，已装用户的应用会在启动时自动收到更新）：

```powershell
pwsh -File scripts/new-release.ps1 -Version 1.2.0 -Notes "用户可读的更新说明，会显示在应用内设置页"
```

细节、重发一版、手动流程与验收清单见 [docs/RELEASE.md](docs/RELEASE.md)。

## 数据与日志

可写数据默认与运行中的 `WxTap.exe` 位于同一目录（macOS 不同：为 `~/Library/Application Support/WxTap`）：配置、日志、反编译输出与流量数据库均位于该目录，Windows WebView2 状态放在 `webview/`。运行日志写入 `<数据目录>/logs/wxtap-YYYYMMDD.log`，单文件超 1 MB 时轮转。

常用覆盖变量：`WXTAP_DATA_DIR`、`WXTAP_TRAFFIC_DB`、`WXTAP_MIGRATIONS_DIR`、`WXTAP_PACKAGES_DIR`、`WXTAP_USERS_DIR`。另一组由**桌面壳**读取、用于决定如何拉起 Core：`WXTAP_CORE_CMD`、`WXTAP_CORE_SCRIPT`、`WXTAP_CORE_DIR`；其中只有 `WXTAP_RESOURCE_ROOT` 由 Core 进程自身读取。

**不要将用户配置或抓取结果提交到 Git** —— 日志与流量库含目标小程序的运行数据。

## 智能体接入（MCP）

WxTap 将调试能力以 MCP 服务器形式暴露给任意 agent：附加宿主、采集流量、驱动小程序 UI、在 JS 运行时下断暂停、离线反编译审计。`tools/list` 默认广播合并形态的精简目录（被合并吸收的旧工具名仍可调用，仅不再广播）；需要完整清单时，stdio 加 `-tools=all`，HTTP 模式使用 `mcp.start {tools:"all"}`。

```json
{ "mcpServers": { "wxtap": { "url": "http://127.0.0.1:9527/mcp" } } }
```

在 GUI 的「MCP 服务」页启动服务后即可使用。传输方式、工具分档、resources / prompts 与接入引导顺序见 [docs/MCP.md](docs/MCP.md)。

## 仓库地图

```text
wxtap/   # 本地目录名可自定；产品名 WxTap
  README.md
  .gitignore
  scripts/build-wails.ps1   # Windows 一键构建（出安装包）
  scripts/build-wails.sh    # macOS 一键构建（WxTap.app bundle）
  desktop/                  # Wails v2 Go 应用（前端仅在 desktop/frontend；migrations/ 为 SQLite 迁移，随发布分发）
  core/                     # Node/TS WMPF 引擎
  contracts/                # 对外发布的契约：traffic.ts（流量模型，对应 desktop/internal/traffic/model.go）与 audit.ts（资产清单/单条流量载荷，对应 desktop/internal/api/ipc/audit.go）；Core 侧 RPC 类型在 core/src/rpc/protocol.ts
  resources/                # 运行时资源（frida、skills、devtools_electron.js、icons）
  configs/                  # 示例配置
  docs/                     # 文档索引与专题
```

完整树与目录约定见 [docs/REPO_LAYOUT.md](docs/REPO_LAYOUT.md)。

## 文档

- [docs/README.md](docs/README.md) — 文档索引
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — 进程模型、端口、数据与资源位置
- [docs/MODULE_MAP.md](docs/MODULE_MAP.md) — 模块职责与 GUI / MCP 索引
- [docs/FEATURE_MATRIX.md](docs/FEATURE_MATRIX.md) — 功能矩阵（各工作区的前端入口、后端能力与数据策略）
- [docs/MCP.md](docs/MCP.md) — MCP 接入指南（传输、客户端配置、能力面）
- [docs/PLATFORM.md](docs/PLATFORM.md) — 平台定位与地址表覆盖
- [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) — 本地开发、构建与自动化测试
- [docs/RELEASE.md](docs/RELEASE.md) — 发版操作说明（tag 触发 CI、清单契约、验收清单）
- [docs/WECHAT_E2E_CHECKLIST.md](docs/WECHAT_E2E_CHECKLIST.md) — 授权微信实机验收门禁
- [docs/FEEDBACK.md](docs/FEEDBACK.md) — 交流反馈（应用内同名页面的源文档）

## 授权与反馈

代码以 [MIT License](LICENSE) 发布；`resources/frida/config/` 下的地址表数据来自第三方项目，例外条款见 [NOTICE](NOTICE)。

- 问题反馈：提交 [Issue](https://github.com/langbyyi/wxtap/issues)，应用内「交流反馈」页的按钮直达同一地址。
- 各版本更新内容见 [Releases](https://github.com/langbyyi/wxtap/releases)；应用内「设置」页的版本检查读取 `latest.json` 的 `notes` 字段。
