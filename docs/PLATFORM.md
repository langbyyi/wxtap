# 产品平台定位

## 结论

**WxTap** 的目标平台是 **macOS / Windows**。

| 平台 | 产品态度 | 当前仓库状态（2026-09-23） |
| --- | --- | --- |
| Windows x64 | 正式目标；第一阶段交付 | Core `createWindowsFridaRuntime`；`resources/frida/config/win` 地址表 54 个 build（最新 25715，文件名与表内版本一致、**两代结构布局**由测试锁定），缺表时走 `frida/autodetect/win.js` 自动检测偏移（仅支持旧布局：≥25710 属新结构布局，缺表时直接要求补表）；Wails `windows/amd64` 构建已通过，`scripts/build-wails.ps1` 在 CI 做语法门禁 |
| macOS Apple Silicon | **已发布**（`darwin-arm64` 的 `WxTap.dmg`）；挂钩能力受限 | Core 已接入 mac 运行时（`WmpfFridaRuntime` 双平台策略）：宿主**优先**按主进程所在的 bundle 路径匹配 `WeChatAppEx`，找不到才**回落**到 `WeChatAppEx Helper` 的父进程（后者是上游 `evi0s/WMPFDebugger` 的 darwin 做法）；构建号从宿主 bundle 的 `Info.plist:CFBundleVersion` 取（纯整数直接用，点分如 `4.269136.0` 取其中最大的一段）；`config/mac` **只有 1 个 build（269136，仅 arm64）**，结构（`LoadStartHookOffset` / `CDPFilterHookOffset` / 6 段 `SceneOffsets`）由 `core/src/engine/frida-tables.test.ts` 锁定，hook 路径与 Windows 共用 —— `hook.js` 用 Frida 的 `args[]` 屏蔽 ABI 差异；`scripts/build-wails.sh` 按当前架构产出 `WxTap.app` bundle 与 `.dmg`，staging 由 `TestMacOSBuildScriptStagesBundleLayout` 锁定，脚本本身由 CI 的 `macos-package` 腿在真 Mac 上执行；darwin arm64/amd64 交叉编译另有门禁。**仍未**做的是需要微信桌面端的部分：Frida attach 能否越过 SIP / 代码签名、以及地址表对给定构建号的覆盖 |
| macOS Intel（x64） | **不发布** | 没有 `x64` 地址表：`config/mac/addresses.269136.json` 只分 `arm64` 一段，而 `hook.js` 对表里没有的 arch 保持**完全不 patch**，所以 Intel Mac 上是「引擎可启动但不挂钩」。`build-wails.sh` 能在 Intel Mac 上构建出 `darwin-amd64` 包，但不在交付范围内 |
| Linux 桌面微信 | **不在产品范围** | 云端 Linux 仅用于开发、单测、交叉编译；Linux 微信客户端 ≠ Windows/mac WMPF 路径 |

## 说明

- Windows 离线测试与授权微信 E2E 仍是发布前实机验证的主路径。
- Windows 地址表有**两代结构**：≤25560 用 6 段 `SceneOffsets`，≥25710 用 `CastToJsonHookOffset` + `MiniAppConfigStructOffsets`（新布局还必须把 `ws://localhost:9421` 写进 launch config，否则小程序不会建立连接）。`hook.js` 按表里携带哪个键选择路径，因此补表时照抄上游同构建的表即可；25710 / 25715 两张即取自上游 `evi0s/WMPFDebugger`（**GPL-2.0，仅作数据参考**，与 macOS 那张同源），并人工抽查比对过若干张共用表逐字段一致（**一次性人工记录，不随每次补表更新**，不应视为持续保证）。`frida/autodetect/win.js` **仅识别旧布局**（不做反汇编，无法推出新结构偏移），`MODERN_LAYOUT_BUILD` 是这条分界线。
- macOS 对等能力是产品完整度目标。运行时已就位（`createMacOSFridaRuntime`），剩下的是**两项实证**：授权 mac 机器上的实机 E2E（attach、地址表准确性、宿主进程名与 `Info.plist` 版本读取），以及可测的 mac 地址表覆盖（覆盖多少个构建见上表）。在这两项补齐之前不应宣称「双平台能力完整」。**另外需注意，这条路径没有已确认可用的参考实现**：上游 `evi0s/WMPFDebugger` 引入 darwin 支持的提交自称 *"macos support clean up (not working yet)"*，其 `hook.js` 至今留着 `// TODO: this was not tested on darwin`；只有第三方 mac 端口声称实机可用。
- macOS 的离线可验证面（bundle 布局探测、地址表结构、`hook.js` 的 ABI 无关参数读取、darwin 交叉编译）已有测试与 CI 门禁；发布的包还由 CI 在真 Mac 上验过构建、`.dmg` 挂载、启动与原生 binding 加载。实机 attach、地址表准确性、宿主进程名与 `Info.plist` 版本读取仍只能由授权 mac 环境的 E2E 清单证明。
- **别家的 darwin 表不能直接并进来**：macOS 地址表存在两套 schema —— 本项目与 `evi0s/WMPFDebugger` 用 `LoadStartHookOffset` + `CDPFilterHookOffset` + 6 段 `SceneOffsets`；`linguo2625469/WMPFDebugger-mac` 与 `Spade-sec/First` 用 `LoadStartHookOffset2` + `ResourceCachePolicyHookOffset` + **单值** `StructOffset` 取代 `SceneOffsets`，并另带 `x64` 段。
- 两派 hook 的是**不同的点**：后一派的表里没有 `SceneOffsets`，而我们的 `hook.js` 读的正是它。把它的表并进来，会让状态页报「有表」而 hook 静默失效 —— 比报「缺表」更糟，所以不做。同理，Windows 上那类「偏移生成器」（分析的是 `flue.dll`）对 Mach-O 无效。要扩展 mac 覆盖，只能自己写 darwin 的偏移解析，并以已知正确的 `269136` 作为锚点做离线校验。
- `config/mac/addresses.269136.json` 的偏移取自上游 `evi0s/WMPFDebugger` 的 darwin 表（**GPL-2.0，仅作数据参考**，出处与许可范围见仓库根的 [NOTICE](../NOTICE)），逐值比对一致。但**表的形状由本项目自行定义**：上游那张表把偏移平铺在顶层、全仓库没有任何架构判断，因此它在 Intel mac 上会拿 arm64 偏移去打；我们的表按 `Arch` 分段，表里没有的 arch 在 `hook.js` 里保持**完全不 patch**（不会拿另一套偏移去 hook），状态页也会说明是这个原因。所以 Intel mac 目前是「引擎可启动但无法挂钩」，而非钩错地址 —— 在这一条上本项目的处理比上游更安全，而非跟随上游。

## 参考

- <https://github.com/evi0s/WMPFDebugger> —— 地址表数据的来源：Windows 的 25710 / 25715 与 macOS 的 269136 都取自这里（**GPL-2.0，仅作数据参考**）。它的 `FAQ.zh.md` 同时是 macOS attach 失败**真实报错文本**的出处：`Unable to access process with pid … from the current user account`，并**推荐**对 WeChatAppEx 做 Ad-Hoc 重签名、把关闭 SIP 标注为**不推荐**。注意其 darwin 支持在引入该支持的提交中被标注为 *"not working yet"*。
- <https://github.com/linguo2625469/WMPFDebugger-mac> —— 另一个方向的来源：它声称实机支持 Intel 与 Apple Silicon，把「必须关 SIP」写成前置要求（与上游 FAQ 的推荐顺序相反），并从点分的 `CFBundleVersion` 里取 `split('.')[1]` 当构建号 —— 我们的版本解析接受这种点分格式正是因为它。这几条**本项目尚未实机核实**（核实方式见 [WECHAT_E2E_CHECKLIST.md](WECHAT_E2E_CHECKLIST.md) 的 macOS 前置项）。
