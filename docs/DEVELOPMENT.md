# 开发与验证（生产：Wails）

## 前提

- Windows 10/11 x64 与 macOS Apple Silicon 提供发布包；微信版本与挂钩限制见 [PLATFORM.md](PLATFORM.md)。Linux 用于离线测试与交叉编译，不是桌面产品目标。
- Go 1.25+、Node.js 22+、Wails v2 CLI（`wails`）。
  两个 Wails 版本号含义不同：发布构建固定用 CLI `v2.16.0`（`build-wails.ps1` / `build-wails.sh` 里的 `@v2.16.0`），而 `desktop/go.mod` 依赖的模块是 `v2.11.0` —— 修改其中一个不会带动另一个。
  Node 22 的下限不在 `core/package.json`（它没有 `engines` 字段），而是 `desktop/node_runtime.go` 的 `minNodeMajor = 22` 在启动时实体校验；`desktop/frontend/package.json` 的 `engines` 只是声明。
- **NSIS 不需要手动安装**：Wails 自身只在 PATH 上查找 `makensis`，找不到时只输出一行警告、不产出安装包。`build-wails.ps1` 因此在调用 Wails 前先解析它——PATH → 常见安装位置 → 最后自动下载便携版到 `desktop/build/tools/nsis`（无需安装、无需管理员），并把它的目录前置到 PATH。解析失败即直接报错，不会静默产出一个没有安装包的「成功」构建。
- **发布包不再内置 Node**：`build-wails.ps1` / `build-wails.sh` 只构建产物，运行时使用目标机器上的 Node，因此用户机器同样需要 Node.js 22+。
  解析与校验集中在 `desktop/node_runtime.go` 的 `resolveNodeRuntime`，顺序是 `WXTAP_CORE_CMD` → 设置页保存的 `config.json:node_path` → PATH → 常见安装位置（自动检测）；每个候选都先跑 `node -p process.versions.node` 校验门槛。前两级是显式指定，不合格即报错，不会改用其他 Node（静默替换用户指定的运行时会使该覆盖失去意义）；后两级是「查找可用项」，会跳过不合格的候选继续查找，原因记入 `Skipped`，全部不可用时才给出可执行的错误信息。设置页「Node 运行时」面板可填路径、自动检测（`node.status` / `node.detect`），保存前先校验再落盘。
- 实机调试需：微信桌面端（授权小程序）、Frida 可附加目标进程；Windows 另需 WebView2 Runtime，独立 DevTools 窗口另需可用的 Electron。

## 安装与构建

在仓库根目录执行 Windows 一键构建：

```powershell
.\scripts\build-wails.ps1
```

脚本安装依赖、构建 Core 与前端、调用 Wails，并整理 `desktop/build/release/`（含 `WxTap.exe`、配套 `core/`、`resources/`、`migrations/` 等），额外产出 `desktop/build/release-dist/WxTap-setup.exe`。单独运行 `wails build` 不会完成配套资源整理，不能替代发布脚本。两个平台的发布脚本均启用 `-trimpath`，避免产物嵌入本机编译路径。

发版流程、产物清单、清单契约与排错都在 **[RELEASE.md](RELEASE.md)**，这里不重复，避免两处描述漂移。

macOS 构建需在目标架构的 macOS 上执行（脚本按当前架构选择 `darwin/arm64` 或 `darwin/amd64`）：`./scripts/build-wails.sh` → `desktop/build/release/WxTap.app`，`core/`、`resources/`、`migrations/` 全部位于 bundle 的 `Contents/Resources/`（Go 侧 `resourceProbeRootsFor` / `resolveCoreScript` 按此探测）。Node 不由 bundle 提供，运行时取用户 PATH。

开发期也可在 `desktop/` 使用 `wails dev`：探测会向上找到仓库根的 `resources/`（frida 配置、mcp skills、Electron 启动器都可直接使用，与 Core 默认的 `../../resources` 规则一致）。

## 自动化测试（每次改动至少运行相关套件）

依赖需先通过各 npm 项目的 `npm ci` 安装。以下命令从仓库根目录开始，按顺序执行：

```bash
cd core
npm run lint                         # ESLint
npm run typecheck                    # tsc 类型检查
npm run build                        # 先生成 dist，供 npm test 检查
npm test                             # vitest + check-dist（frida 必须 external）
npm run coverage                     # 覆盖率门禁
cd ../desktop/frontend
npm run typecheck
npm test
npm run build                        # 生成 Go embed 所需的前端产物
cd ..
go vet ./...                         # 静态检查
golangci-lint run ./...               # 0 issues 为准
go test ./...                        # 契约 / 路由 / 流量 / MCP / 引擎客户端
go test -race ./...                   # 竞态检测；CI 固定在 Linux 执行
```


> `desktop/main.go` 用 `//go:embed all:frontend/dist` 嵌入前端产物。全新克隆后直接 `go build` / `go test` 会因该目录缺失而报 `pattern all:frontend/dist: no matching files found`：请先执行上面的前端构建，或临时放一个占位 `frontend/dist/index.html`（CI 即用此方式，Go 侧测试不依赖真实前端）。

涉及 IPC 形状时优先补 `desktop/extract_contract_test.go` / `desktop/app_test.go` 或对应 `internal/*/…_test.go`，不要仅依赖手工点选。

**外部面冻结契约**：`desktop/testdata/compat-surface.json` 冻结 v0.1.0 的对外面（IPC 方法 + MCP 工具名单）；`TestCompatIPCMethodsStayRegistered` 与 `TestCompatMcpToolsStayExposed` 保证这些名字始终已注册、可调用。「可调用」不等于「广播」：lean 目录收敛后，清单外的旧名字仍须应答 `tools/call`，只是不再出现在 `tools/list`。删除或改名其中一项属于功能回归，而非清理；新增能力请添加新名字，不要复用旧名。

## 改动指南

| 目标 | 优先修改位置 | 同步检查 |
| --- | --- | --- |
| 调试协议 / WebSocket / Frida | `core/src/engine/*`、`bridge/*`、`protocol/*` | Core 测试、地址表、`check-dist` |
| 页内 Hook | `core/hooks/*.js` + Go feeder | drain/ack、generation、契约测试 |
| GUI 后端方法 | `desktop/internal/api/ipc` + Router | 前端调用名、事件名、契约测试 |
| MCP | `desktop/internal/mcp`、`resources/skills/` | tools/list、技能说明 |
| wxapkg / 扫描 | `desktop/internal/extract` | 样本、路径参数、进度事件 |
| 前端 UI | `desktop/frontend/src` | `npm test`、bridge 信封 |

## 实机验证

授权环境下的 Frida / 9421 / 31415 / 多开 / Hook 实捕 **不能**以离线测试替代。完整门禁见
[WECHAT_E2E_CHECKLIST.md](WECHAT_E2E_CHECKLIST.md)。

其中有三个 **opt-in** 用例把部分验收做成了可重复执行的测试。默认**全部跳过**（向使用者已登录的微信附加调试器不应由测试套件自行决定），必须显式开启：

| 用例 | 闸门 | 覆盖 |
| --- | --- | --- |
| `TestLiveEngineStartStopDrivesThePorts` | `WXTAP_LIVE_WECHAT=1` | 「状态」页契约：attach 后 9421 与 CDP 端口确实已监听，stop 后释放 |
| `TestRealAttachAndDeepLink` | `WXTAP_REAL_ATTACH=1` | 真实宿主上的附加与跳转链路 |
| `TestRealAttachCaptureLifecycle` | `WXTAP_REAL_ATTACH=1` | 捕获生命周期在真实 WMPF 宿主上成立（离线用例只证明 shell 发给 Core 的调用序列） |

从 `desktop/` 目录执行：

```powershell
$env:WXTAP_LIVE_WECHAT = 1
go test ./ -run TestLiveEngineStartStopDrivesThePorts -v
```

`WXTAP_REAL_WAIT=<秒>` 可延长「等待小程序连上 9421 / 等待目标出现」的时间——默认窗口仅数秒，
刚打开小程序即重跑时会不足。这些用例把所有可写路径指向临时目录（`WXTAP_DATA_DIR` / `WXTAP_TRAFFIC_DB`），
真实数据目录与配置不受影响。
