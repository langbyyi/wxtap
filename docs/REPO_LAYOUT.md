# 仓库布局

```text
wxtap/
  README.md
  LICENSE                  # MIT（地址表数据例外，见 NOTICE）
  NOTICE                   # 第三方来源披露（frida 地址表来自 GPL-2.0 上游）
  .gitignore
  scripts/
    build-wails.ps1          # Windows 一键构建（出安装包，相对仓库根解析路径）
    build-wails.sh           # macOS 一键构建（WxTap.app bundle，资源入 Contents/Resources）
    package-release.ps1      # 两平台共用：切分片、算 sha256、写清单片段、合并 latest.json
    merge-manifest.mjs       # 清单合并的唯一实现（两个平台各写一条）
  desktop/                   # Wails v2 Go 应用（唯一前端在此）
    main.go, app.go, ...
    frontend/                # Vue3 + Vite 源码
      package.json, src/, vite.config.ts, ...
      dist/                  # 构建产物，供 //go:embed（gitignore）
    internal/, migrations/, wails.json, go.mod
  core/                      # Node/TS WMPF 引擎
  contracts/                 # 对外发布的契约：traffic.ts（流量模型，对应 desktop/internal/traffic/model.go）与 audit.ts（资产清单/单条流量载荷，对应 desktop/internal/api/ipc/audit.go）；Core 侧 RPC 类型在 core/src/rpc/protocol.ts
  resources/                 # 运行时资源
    frida/                   # Frida hook 与平台地址表
    skills/                  # 技能文档（agent 方法论）
    devtools_electron.js     # DevTools 独立窗口的 Electron 启动器
    icons/                   # 应用图标（png / ico）
  configs/
    gui_config.example.json  # 示例配置（本地覆盖 gitignore）
  docs/
    README.md                # 文档索引
    REPO_LAYOUT.md
    ARCHITECTURE.md
    DEVELOPMENT.md
    MODULE_MAP.md
    PLATFORM.md
    RELEASE.md               # 发版操作说明
    FEATURE_MATRIX.md
    MCP.md
    WECHAT_E2E_CHECKLIST.md
    FEEDBACK.md
```

## 约定

- **前端只有一处**：`desktop/frontend/`。构建产物写到 `desktop/frontend/dist`，由 `desktop/main.go` 的 `//go:embed all:frontend/dist` 嵌入。仓库根不再存在 `frontend/`。
- **运行资源统一在 `resources/`**。开发时相对仓库根；Windows 发布时 `scripts/build-wails.ps1` 把整个 `resources/` 拷到 `desktop/build/release/resources/`（与 `WxTap.exe`、`core/`、`migrations/` 并列）；macOS 发布时 `scripts/build-wails.sh` 把同样的内容放进 `WxTap.app/Contents/Resources/`。
- **路径解析**：Go `resolveSyncBaseDir` / `devtoolsBaseDirs` 优先探测 `resources/`；Core 默认 `WXTAP_RESOURCE_ROOT` 为 `../../resources`（相对 `core/dist`）。
- **本地配置**：复制 `configs/gui_config.example.json` → `configs/gui_config.json`（已 gitignore）按需修改。
