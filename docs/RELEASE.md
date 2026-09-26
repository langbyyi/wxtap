# 发布流程

这份文档是发版的唯一操作说明。改动发布链路（清单形状、分片切法、产物种类）时**必须同步改这里**，
否则下次发版只能靠推测。

## 一、交付物：两种形态，各管一段

| 用途 | 产物 | 谁下载 |
| --- | --- | --- |
| **新用户安装** | Windows `WxTap-setup.exe`<br>macOS `WxTap.dmg` | 用户手动下载**一个文件** |
| **已有用户更新** | `latest.json` + 平台分片 `*.tar.gz` | 客户端自己下载，用户不需要手动获取 |

**为什么不能只发一个 `WxTap.exe`**：它需要从同级目录读取 `core/`、`resources/`、`migrations/`。
单独一个 exe 无法运行（引擎缺 `core/dist/cli.js`，数据库缺 `migrations/`）。因此「一个文件」由
安装包 / 磁盘镜像实现，而非单文件 exe。

体积参考（v1.0.0 实际发布值；仓库内无法测量——`desktop/build/` 已被 gitignore，此处仅作量级参考）：

| 产物 | 大小 |
| --- | --- |
| `WxTap-setup.exe`（含全部载荷，NSIS Solid LZMA） | 41.4 MiB |
| `WxTap.dmg`（含 `.app`，UDZO/zlib） | 55.3 MiB |
| `WxTap-<v>-a.tar.gz` + `-b.tar.gz`（Windows 分片，gzip） | 9.9 + 44.1 MB |
| `WxTap-<v>-mac.tar.gz`（macOS 分片，gzip） | 44.2 MB |

Windows 安装包比它自己的两个分片合计还小（41.4 MiB 对 53.9 MB），因为 NSIS 用 Solid LZMA
（约 26%）而分片用 gzip（约 40%）。macOS 侧相反：`.dmg` 用 UDZO/zlib，压缩率不如 LZMA，
所以 dmg 比 Windows 安装包大，尽管两者的载荷量级相同。两处单位不同源于量具不同 ——
安装包按 NSIS 报表记 MiB，分片按文件大小记 MB，不应直接相减比较。

## 二、版本号：唯一真源是 wails.json

版本号只写一处：`desktop/wails.json` 的 `info.productVersion`（不带 `v`，如 `1.0.0`）。

- 前端在**构建期**读它注入顶栏（`desktop/frontend/vite.config.ts`）；
- 外壳在**运行期**读它 —— `desktop/version.go` 用 `//go:embed wails.json` 解析出 `appVersion`（带 `v`，如 `"v1.0.0"`）；`update.checkVersion`、设置页顶栏与 MCP 握手上报的都是它；
- MCP 握手不再自己写版本：外壳把 `appVersion` 注入 `mcp.Deps.Version`，由 `TestMCPServerAnnouncesTheAppVersion` 锁定。

**发版只改 `desktop/wails.json`。** 它解析不出来时启动会记一条日志并把版本上报为 `unknown`（而不是编一个像发布号的数字），`TestVersionComesFromTheEmbeddedWailsJSON` 会在 CI 里先挡住。
`package-release.ps1` 传的 `-Version` 与它不一致时只**告警**，不阻断构建。

流程：先改 `wails.json` → **提交并 push** → 打 tag `v<同号>` → push tag。

**tag 必须指向已经 push 的提交**：CI 检出的是该提交，而非本地工作区。
改动未提交即打 tag，构建出的是上一次的代码 —— 且不会有任何报错提示。

## 三、推荐路径：CI 自动构建

`.github/workflows/release.yml`，触发方式是**打 tag**：

```bash
# 1) 改版本号
#    desktop/wails.json -> info.productVersion = "1.1.0"
git commit -am "release: 1.1.0"
git push

# 2) 打 annotated tag 即触发（tag 与版本号一致，附注即发布说明）
git tag -a v1.1.0 -m "本次更新的说明，用户在设置页看到的就是它"
git push origin v1.1.0
```

以上两步可由 `scripts/new-release.ps1` 一并完成：改版本号（含 core 与前端
package.json 的元数据版本）→ 跑与 ci.yml 同一套本地门禁 → 提交 → 打 annotated
tag → 推送。`pwsh -File scripts/new-release.ps1 -Version 1.1.0 -Notes "说明"`。
脚本拒绝已存在的 tag；重发一版先删掉它（`gh release delete`、`git push
origin :refs/tags/<v>`、本地 `git tag -d`），publish 腿对已存在的 Release
会就地刷新资产，清单提交走同一个固定地址。

workflow 会：

1. **windows 腿**：`build-wails.ps1`（出载荷 + 安装包）→ `package-release.ps1`（出分片 + 本平台清单片段）
2. **macos 腿**：`build-wails.sh`（出 `.app` + 扁平载荷 + `.dmg`）→ 同一份 `package-release.ps1`（在 CI 上有 pwsh）
3. **publish 腿**：把各腿的清单片段合并成一份 `latest.json` → 创建 Release，附上分片 + 安装包
   （macOS 腿跑了才附 dmg）→ 把 `latest.json` 提交到**默认分支**（走 `gh api`，不 clone）
4. **同一腿还会发布 `feedback.md`**：把 `docs/FEEDBACK.md` 原样推到默认分支，供应用内「交流反馈」页拉取。
   这一步有 `test -f docs/FEEDBACK.md` 前置检查——**删除或改名这个文件会让整次发布失败**，而非静默跳过。

CI 里 `latest.json` 的 `notes` 来自 **annotated tag 的附注消息**（verify 腿解析
一次，三条腿共用），轻量 tag 或无附注时回退为版本号本身。所以要写真正的更新
说明，就用 `git tag -a v1.1.0 -m "说明"` 打 tag；附注里的双引号会被替换成单引号，
其余内容原样到达用户的应用内设置页。

也可以手动跑（`workflow_dispatch`），填 `version` 即可。

### 仓库与权限

客户端唯一的清单入口是编译进去的常量（`desktop/internal/update/manifest.go`）：

```
ReleaseRepo = "langbyyi/wxtap"
→ https://raw.githubusercontent.com/langbyyi/wxtap/main/latest.json
```

**这个仓库必须匿名可下载**：私有仓库的 raw 与 Release 附件都要 token，而放进客户端的 token
每个用户都能读出来。产物因此住在**源码仓库本身**（公开），代码、Issue、Releases 与 `latest.json`
共用一个地址 —— 早先「私有源码仓 + 独立公开产物仓」的拆分与它带来的跨仓库 PAT（`RELEASE_TOKEN`）
都已取消。

仓库名写在两处，必须一致：上面的 `ReleaseRepo`，和 `.github/workflows/release.yml` 顶部的
`env.RELEASE_REPO` —— 由 `desktop/update_contract_test.go` 的
`TestReleaseWorkflowPublishesWhereTheClientLooks` 校验，改一处不改另一处会导致测试失败。

权限无需额外配置：Release 创建与清单提交都发生在 workflow 自己所在的仓库，
`permissions: contents: write` 下的默认 `GITHUB_TOKEN` 就够。

换仓库＝换客户端地址，所以改名要连客户端一起重发（老客户端只认旧地址）。

### macos 腿：默认开启，发布 `darwin-arm64`

**macOS 随每次发布产出 `WxTap.dmg` 与 `darwin-arm64` 分片，并写入清单。默认就是开的，
不需要设置任何东西。**

唯一的开关是**反向**的：把仓库变量 `SKIP_MACOS` 设为 `true`（Settings → Secrets and
variables → Actions → Variables）可把 macOS 从某一次发布里摘掉。之所以写成"开启需要变量"
的反面，是因为那样会让「某个平台发不发」变成**代码之外的隐形状态** —— 换个 fork、重置设置、
或者没人读文档，就会静默产出一个没有 macOS 的版本。默认开、显式关，代码里说的和实际发的一致。

摘掉 macOS 时，publish 腿会删除本次构建不再产出的旧资产（`.dmg` 与 `-mac.tar.gz`），
所以 release 页与清单不会一边有 dmg、一边没有 `darwin-*` 条目。

发布的依据是这两件事已经分开、且第一件已经做完：

- **包本身：已由 CI 在真 Mac 上逐次验证**。`ci.yml` 的 `macos-package` 腿每次 push / PR 都跑
  （打包路径就是发布路径）：`build-wails.sh` 跑得完，`WxTap.app` 与扁平更新载荷的必需项齐全、
  没有打包进 Node 运行时，`.dmg` 能挂载且含 `WxTap.app` 与 `/Applications` 软链，app 能启动并
  写出数据目录与启动记录，`frida_binding.node` 能在本架构上真正 `dlopen` 并导出符号。
  在此之前这条脚本**没有任何执行点** —— 唯一的入口就是这条腿，离线门禁只解析它、只做交叉编译。
- **挂钩能力：仍未实证，且不随发布改变**。对着已登录的微信桌面端能否 attach（SIP / 代码签名
  那道墙）、以及地址表对给定微信构建号的覆盖 —— runner 上既没有登录的微信，也没有目标小程序。

所以发布的定位必须说清楚：**这是一个能安装、能启动、能与官方接口交互的 macOS 构建；挂钩能力
取决于你的微信构建号是否在表内。** 静态地址表只有 1 个构建（见 [PLATFORM.md](PLATFORM.md)），
且只有 `arm64`：

- **Intel Mac 与不在表内的构建号：引擎可启动，但不会挂钩**（`hook.js` 对表里没有的 arch 保持
  完全不 patch，状态页会说明原因）。这是刻意选择的行为，不是遗漏。
- 表内构建上仍可能撞上 attach 权限墙：对 `WeChatAppEx` 做 Ad-Hoc 重签名是上游推荐做法，关 SIP
  是备选且不推荐。两条**本项目都尚未实机核实**。

其余约定不变：

- **macos 腿执行失败会阻止整次发布**（宁可晚发，也不发半个版本）
- 清单按平台键组织，publish 腿按实际存在的片段合并
- 想把 macOS 摘出某次发布，把变量设为 `false` 即可 —— **不需要改代码**；缺 `darwin-*` 不报错
- 另有一个不发布任何东西的旁路：`macos-package` 腿会把 `WxTap.dmg` 作为 CI artifact 上传
  （`macos-dmg`，保留 7 天），用来在不动发布的情况下试包
- `.dmg` **未公证**：打开需右键 → 打开，去掉该提示需要付费的 Apple Developer ID
- Intel（`darwin-amd64`）**没有 x64 地址表**，因此不发布；`build-wails.sh` 会在 Intel Mac 上
  构建出 `darwin-amd64` 包，但它挂不上钩，不在交付范围内

### 为什么清单要提交到默认分支

`desktop/internal/update/manifest.go` 里 `ManifestURLs` 指向的是**固定地址**：

```
https://raw.githubusercontent.com/langbyyi/wxtap/main/latest.json
```

这是编译进客户端的常量。换地址等于换客户端，所以清单必须在该分支上，不能只当 Release 附件。
它就是源码仓库的默认分支（见 §三），publish 腿用 `GITHUB_TOKEN` 提交。

## 四、本地发布（Windows，需要手动上传时）

```powershell
# 1) 出载荷 + 安装包
.\scripts\build-wails.ps1
#    -> desktop/build/release/            载荷（core/ resources/ migrations/ WxTap.exe）
#    -> desktop/build/release-dist/WxTap-setup.exe

# 2) 出分片 + 清单（-Repo 就是本仓库）
.\scripts\package-release.ps1 -Version 1.1.0 -Repo "langbyyi/wxtap" -Notes "更新说明"
#    -> desktop/build/release-dist/WxTap-1.1.0-a.tar.gz / -b.tar.gz
#    -> desktop/build/release-dist/latest.json
#    -> desktop/build/release-dist/assets-windows-amd64.json（清单片段，CI 用）
```

然后手动做 CI 自动做的三步：

```bash
gh release create v1.1.0 desktop/build/release-dist/WxTap-1.1.0-a.tar.gz desktop/build/release-dist/WxTap-1.1.0-b.tar.gz desktop/build/release-dist/WxTap-setup.exe
# latest.json 提交到 main（客户端读的就是它）
gh api -X PUT repos/langbyyi/wxtap/contents/latest.json -f message="release: latest.json for v1.1.0" -f content="$(base64 -w0 desktop/build/release-dist/latest.json)"
```

macOS 侧本地发布：`./scripts/build-wails.sh` 出 `.app`、`desktop/build/release-payload/`（扁平载荷）
和 `desktop/build/release-dist/WxTap.dmg`；再在该机器上用 pwsh 调 `package-release.ps1 -Source desktop/build/release-payload -Platform darwin-arm64`。

## 五、清单契约（改它 = 改客户端）

`latest.json` 的字段是**编译进客户端**的，发布之后改结构会让已发出去的用户更新不了。
当前形状：

```json
{
  "version": "v1.1.0",
  "notes": "更新说明（设置页展示）",
  "platforms": {
    "windows-amd64": { "assets": [ { "name": "...", "size": 0, "sha256": "...", "urls": ["https://github.com/langbyyi/wxtap/releases/download/v1.1.0/WxTap-1.1.0-a.tar.gz"] } ] },
    "darwin-arm64":  { "assets": [ ... ] }
  }
}
```

规则（前几条是**客户端会直接拒收**的硬约束，手写清单最易违反）：

- 平台键是 **`GOOS-GOARCH`**（`windows-amd64` / `darwin-arm64` / `darwin-amd64`），客户端用它查自己那一份
- `platforms` 与顶层 `assets` **只能出现一个**；顶层 `assets` 是单平台旧格式，今天仍被接受（只发 Windows 时可以用）
- 每个分片必须有 `size` 与 `sha256`（**64 位十六进制**，大小写都收，客户端归一成小写）；**校验通过才落位**，不匹配就整片重下
- 分片 `name` 必须是纯文件名（不含 `/`、`\`、`:`）且在载荷内唯一；`size` 必须 `> 0` 且 `≤ 200 MiB`；
  `urls` 去掉空白项后不能为空——违反任意一条，整个清单被拒
- 清单的 `version` 必须能当目录名用（要被拿去拼更新目录）
- 下载地址必须是 `https`（回环 `http` 例外：测试与本地镜像使用，远端攻击者无法触达）
- **所有平台条目都会被校验**，不只当前平台：一份文档由一个脚本产出，某平台写错说明文档本身有误，
  宁可报错也不要发布
- `urls` 是多源列表，按序回退。**换镜像只改这里，不用重发客户端**
- 合并由 `scripts/merge-manifest.mjs` 完成（两个平台各写一条，脚本保证形状一致）

## 六、发版前检查清单

- [ ] `desktop/wails.json` 的 `productVersion` 已改，且与 tag 一致（外壳、前端与 MCP 握手都从它派生，
  由 `TestVersionComesFromTheEmbeddedWailsJSON` 与 `TestMCPServerAnnouncesTheAppVersion` 锁定）
- [ ] `cd core && npm run build && npm test && npm run coverage`（`dist/` 是载荷的一部分；覆盖率门禁与 CI 同一条）
- [ ] `cd desktop/frontend && npm test && npm run typecheck`
- [ ] `cd desktop && go vet ./... && golangci-lint run ./... && go test ./...`
- [ ] 载荷里**没有** `runtime/`（发布包不自带 Node）
- [ ] `latest.json` 里 `platforms` 的每个键都是 `<goos>-<goarch>` 形式
- [ ] 安装包 / dmg 安装一次再卸载一次，确认清理干净（见下）

### 手工验收（安装包 / dmg）

```powershell
# 静默装到临时目录，核对载荷齐全，再静默卸载
Start-Process .\desktop\build\release-dist\WxTap-setup.exe -ArgumentList '/S','/D=C:\wttest' -Wait
#   C:\wttest 应含 WxTap.exe core resources migrations uninstall.exe
#   且 core/node_modules/frida/build/frida_binding.node、resources/frida/config/win 都在
Start-Process C:\wttest\uninstall.exe -ArgumentList '/S' -Wait
#   目录、注册表卸载项、开始菜单与桌面快捷方式都应消失
```

## 七、终端用户需要什么

- **Windows 10/11 x64**：安装 `WxTap-setup.exe`（per-user 安装到 `%LOCALAPPDATA%\Programs\WxTap`，**无需管理员**）
- **macOS（Apple Silicon）**：**已发布** `WxTap.dmg`（见 §三）。下载后拖进 `Applications` 即可；未公证，首次打开需右键 → 打开。**挂钩能力取决于你的微信构建号是否在地址表内**（表只有 1 个构建、只有 `arm64`），Intel Mac 与表外构建号会「引擎启动但不挂钩」，也可能撞上 SIP / 代码签名的 attach 权限墙 —— 见 [PLATFORM.md](PLATFORM.md)
- **Node.js 22+**：**唯一的外部运行时要求**。发布包不自带 Node，Core 由用户环境里的 Node 启动；
  自动识别顺序是 `WXTAP_CORE_CMD` → 设置页保存的路径 → PATH → 常见安装位置，
  都找不到时启动报错会给出下载地址。设置页「Node 运行时」可手动指定并自动检测
- **WebView2 Runtime**：Win11 自带；Win10 通常随 Edge 装上，安装包在缺失时会尝试下载
- **微信桌面端**：附加目标。地址表覆盖哪些构建、两代结构布局怎么分，见 [PLATFORM.md](PLATFORM.md)（唯一真源，这里不再复述数字）；
  未知构建时 Windows 走自动偏移检测（**仅支持旧布局**：新结构布局的构建缺表时直接报「需补充 …/addresses.<build>.json」
  而不是执行一次无实际作用的扫描；布局分界见 PLATFORM.md），macOS **不做**自动检测（偏移自动检测是 PE 扫描器，没有 Mach-O 对应物），
  同样报「需补充 …/addresses.<build>.json」。macOS 的构建号取自宿主 bundle 的
  `Info.plist:CFBundleVersion`，加表时文件名必须与它一致；表里没有的 arch 在 `hook.js` 里完全不 patch
- **macOS 的 attach 权限**：这是 macOS 侧比 Windows 重得多的前置条件。SIP / 代码签名不允许调试时，Frida 报
  `Unable to access process with pid … from the current user account`（Core 把这条原文保留在错误里，便于记录）。
  上游 `evi0s/WMPFDebugger` 的 FAQ 给出两条路并**推荐前者**：对 WeChatAppEx 做 Ad-Hoc 重签名
  （`sudo codesign --force --sign - --preserve-metadata=identifier,entitlements,requirements "/Applications/WeChat.app/Contents/MacOS/WeChatAppEx.app/Contents/MacOS/WeChatAppEx"`），
  而关闭 SIP（恢复模式 `csrutil disable` 后重启）被上游明确标注为**不推荐**。第三方 mac 端口
  （`linguo2625469/WMPFDebugger-mac`）则把「必须关 SIP」写成前置要求。**两条路我们都尚未实机核实** ——
  列入 mac E2E 门禁的核实项（见 `docs/WECHAT_E2E_CHECKLIST.md` 的 macOS 前置项）。

## 八、常见陷阱与处理

| 现象 | 原因 / 处理 |
| --- | --- |
| `npm ci` 报 EPERM（unlink `frida_binding.node`） | 有 WxTap 实例或其 Core 正在运行，占用着原生模块。请先退出应用再构建。`build-wails.ps1` 会在开始前探测并直接报错 |
| `npm ci` 破坏依赖树 | 失败的 `npm ci` 会留下残缺的 `core/node_modules`（缺 typescript/esbuild/vitest）。用 `npm install` 修复（它不会 unlink 被锁定的 frida） |
| 构建成功但没有安装包 | Wails 只在 PATH 上查找 `makensis`，找不到时只输出一行 warning。`build-wails.ps1` 会自行解析（PATH → 常见位置 → **复用 `desktop/build/tools/nsis` 里的缓存** → 下载便携版到同一处），解析失败即直接报错 |
| `Join-Path` / PowerShell 数组拼接报参数错误 | PowerShell 里逗号优先级**高于** `+`：`@("a=" + $x, "b=" + $y)` 会并成一个字符串。一行一个元素，不要使用逗号 |
| 5.1 下脚本解析失败（中文报错消失/缺 `}`） | `.ps1` 没有 BOM 时 5.1 按 ANSI/GBK 解码，脚本里的非 ASCII 字符（中文、破折号）会破坏语法。`build-wails.ps1` 与 `package-release.ps1` 都带 UTF-8 BOM，不要让编辑器将其移除；CI 的解析检查使用 `pwsh`（默认按 UTF-8 识别），**无法捕获这类问题** |
| 本地连续运行两个平台的 `package-release.ps1` | 它每次打包都会清除输出目录中已有的 `*.tar.gz`，而 `latest.json` 是合并语义、仍保留另一个平台的条目，于是清单会指向已被删除的分片。两个平台各用一个 `-Out` 目录，或最后重新运行一次合并 |
| NSIS 下载下来不是 zip | SourceForge 会给 `Invoke-WebRequest` 返回 HTML 中间页；脚本用系统 `curl.exe` 并校验 zip 魔数 |
| 安装后无法运行 | Wails 的 NSIS 工程只打包 `build/bin/<exe>`。脚本会把载荷插入安装段并重新打包一次安装包（补丁写进 `wxtap-payload.nsi` 副本，不改动 Wails 生成的 `project.nsi`） |
| 更新一直失败、日志说「更新包不完整」 | 载荷缺必需项，或安装的是 old layout。Windows 必需 `WxTap.exe` + `core/dist/cli.js`；macOS 必需 `Contents/MacOS/WxTap` + `Contents/Resources/core/dist/cli.js` |
| 老客户端更新不上 | 清单字段是编译进去的：**改过清单结构的新版本无法通过自动更新到达旧版本用户**，只能让用户重装一次 |
| `package-release.ps1` 报 `Cannot bind argument to parameter 'Repo' because it is an empty string` | 该步骤在 pwsh 下写了 `-Repo "$RELEASE_REPO"`。PowerShell 不把环境变量当 `$NAME` 读，必须写 `$env:RELEASE_REPO`，否则取到空串。同一处在 macos 腿是 bash（`$RELEASE_REPO` 可用），所以只有 Windows 腿会这样失败 |
| golangci-lint 在 CI 上 panic：`file requires newer Go version goX.Y (application built with goA.B)` | golangci-lint 由某个 Go 版本构建，却要类型检查 runner 上那套标准库；runner 的 Go 比它新就崩。workflow 用 `go-version-file: desktop/go.mod` 钉住，别改回 `stable` |
| 改完测试后只在本地跑绿，CI 的 macos 腿仍红 | `runtime.GOOS` 分派会把测试绑到跑它的那台机器上。平台差异在本仓库一律表达成**数据**（`layoutFor(goos)`），测试应显式传入要验的布局（`applyPendingFor` / `stageVersionFor` / `cleanupFor` 就是为此提供的接缝），使两种布局在任一平台上都被覆盖 |

## 九、发布链路相关文件

| 文件 | 职责 |
| --- | --- |
| `scripts/build-wails.ps1` | Windows：构建 + 出载荷 + 出 NSIS 安装包 |
| `scripts/build-wails.sh` | macOS：构建 + 出 `.app` + 出扁平载荷 + 出 dmg |
| `scripts/package-release.ps1` | 两个平台共用：切分片、算 sha256、写清单片段、合并 `latest.json` |
| `scripts/merge-manifest.mjs` | 清单合并的唯一实现（唯一被两个平台共用的那一段） |
| `desktop/internal/update/manifest.go` | 清单契约与平台解析（客户端侧） |
| `desktop/internal/update/apply.go` | 换入布局（平台差异在这里，是**数据**不是分支） |
| `docs/FEEDBACK.md` | 应用内「交流反馈」页的源文档；publish 腿把它原样发布为默认分支的 `feedback.md`（文件缺失会让整次发布失败） |
| `.github/workflows/release.yml` | 打 tag 自动构建 + 发布 |
| `.github/workflows/ci.yml` | CI：`scripts`（ubuntu，`bash -n` 检查 shell 脚本）、`core` / `frontend`（ubuntu）、`desktop`（**ubuntu / windows / macos 三条腿都跑，`fail-fast: false`**；跑 gofmt / golangci-lint / `go vet` / `go test`，`-race` 固定只在 Linux 腿，另有 darwin 交叉编译，Windows 腿用 `pwsh` 另做一次 `.ps1` 解析）、`macos-package`（macos-14 真机跑 `build-wails.sh`，断言 bundle 与更新载荷，挂载 `.dmg`，启动 app 冒烟，把 `.dmg` 作为 artifact 上传；**不发布任何东西**）。Go 版本由 `go-version-file: desktop/go.mod` 决定，不追 `stable`：CI 的 golangci-lint 由某个 Go 版本构建，而它要类型检查该版本的标准库，两者错开时它会直接 panic。`release.yml` 的 `verify` 腿会在构建前拒绝 tag 与 `wails.json` 版本号不一致的发布 |
