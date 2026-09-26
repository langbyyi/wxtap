# 授权微信 E2E 门禁清单

> 本清单只能在**已获授权的测试环境**(自有微信账号 + 自有小程序或已书面授权的小程序)中执行。
> 任何离线测试、mock 或单元测试不得作为本清单项目的替代证据。
> 每个失败项必须记录:微信完整版本号、`resources/frida/config/win/addresses.<build>.json` 是否覆盖该 build、
> Core stderr 日志、复现步骤截图。
> 运行日志落盘于数据目录的 `logs/wxtap-<YYYYMMDD>.log`（Windows 即 `WxTap.exe` 所在目录；macOS 是 `~/Library/Application Support/WxTap`，不是 `.app` 旁边），
> 单文件达到 1 MB 时轮转为 `wxtap-<YYYYMMDD>.<n>.log`（同一天轮满 99 个之后改用 `<unixnano>` 后缀）；`settings.getPaths` 的 `logDir` 指向该目录。
> 日志含目标小程序运行数据，作为证据提交前请审阅并脱敏。

## 环境前置

### Windows 10/11 x64

- [ ] Windows 10/11 x64,已安装 WebView2 Runtime,且有 **Node.js 22+** 可用(缺省在 PATH 上找,也可由 `WXTAP_CORE_CMD` 或设置页保存的路径指定;WxTap 不自带运行时,Core 由用户环境里的 Node 启动;缺失或过旧时 `engine.start` 会报明确错误而非静默失败)
- [ ] 微信 PC 版已登录,记录完整版本号(帮助→关于),确认 `resources/frida/config/win/addresses.<build>.json` 存在对应 build 号;不存在时 Core 会自动检测偏移(WMPF 自动适配,进程内超时 5 分钟),记录 `自动检测完成` 日志中的结果 —— **但自动检测只认旧结构布局**:新结构布局的构建(分界见 `docs/PLATFORM.md`)缺表时会直接报「需补充 …json」,必须补表(照抄上游 `evi0s/WMPFDebugger` 同构建的表;新布局的表带 `CastToJsonHookOffset` + `MiniAppConfigStructOffsets`)
- [ ] 已准备至少一个小程序作为测试目标(记录 appid)
- [ ] `desktop/build/release/` 发布布局运行 WxTap.exe(或开发布局 `wails dev`),Core 成功拉起(「状态」页可见引擎与 Core 状态)
- [ ] **权限一致**:附加失败时报错原文会被 Core 保留下来,便于记录。Windows 上常见原因是**完整性级别不匹配**——微信以管理员身份运行时,本程序也要以管理员身份启动(Frida 无法从低完整性级别附加高完整性进程);其次确认安全软件没有拦截注入(随包的 `frida_binding.node` 是约 118 MB 原生模块,且本程序会向微信进程注入代码,是杀软常见误报对象)。上游 `evi0s/WMPFDebugger` 的 FAQ 记录的也是同一原因与同一处理(提升本进程权限)

### macOS(Intel / Apple Silicon)

- [ ] 已安装 Xcode command line tools;PATH 上有 **Node.js 22+**(构建期与运行期都需要:发布包不自带 `runtime/node`,Core 由用户环境里的 Node 启动)
- [ ] 微信 macOS 版已登录,记录芯片架构(x64 / arm64)与**构建号**——构建号取 `/Applications/WeChat.app/Contents/MacOS/WeChatAppEx.app/Contents/Info.plist` 的 `CFBundleVersion`(Core 读的就是它)。**把该值的原文一并记下**:它是纯整数就直接当构建号,点分(如 `4.269136.0`)则取其中最大的一段——这个格式正是本项目未核实的点之一。确认 `resources/frida/config/mac/addresses.<build>.json` 存在且含对应 `Arch` 段;缺表时 mac 侧**不会**自动检测,会直接报「需补充 …/addresses.<build>.json」
- [ ] `./scripts/build-wails.sh` 产出 `desktop/build/release/WxTap.app`,资源位于 `Contents/Resources/`;启动后 Core 成功拉起
- [ ] mac 宿主进程识别正确(优先按主进程所在的 bundle 路径定位 `WeChatAppEx`、其次回落到 `WeChatAppEx Helper` 的父进程;版本按 `Info.plist` 取,见 `core/src/engine/darwin-target.ts` 的 UNVERIFIED 注释),`engine.start` 能完成 attach
- [ ] **attach 权限**:SIP / 代码签名不允许调试时,Frida 报 `Unable to access process with pid … from the current user account`(Core 会把这条原始报错原文保留在错误里,便于记录)。上游 `evi0s/WMPFDebugger` 的 FAQ 给出两条路并**推荐前者**:(1) 对 WeChatAppEx 做 Ad-Hoc 重签名 —— `sudo codesign --force --sign - --preserve-metadata=identifier,entitlements,requirements "/Applications/WeChat.app/Contents/MacOS/WeChatAppEx.app/Contents/MacOS/WeChatAppEx"`;(2) 关闭 SIP(恢复模式 `csrutil disable` 后重启),上游明确标注**不推荐**。第三方 mac 端口(`linguo2625469/WMPFDebugger-mac`)则把「必须关 SIP」写成前置要求。**本项目尚未实机核实任何一条** —— 本项就是核实:先记录报错原文,再确认重签名(或关 SIP)后 attach 成功(排错见 `docs/RELEASE.md` §七)

## A. 注入与通道

- [ ] **Frida attach**:engine.start 后 Core 日志显示附加到微信进程,无 "unable to find process"
- [ ] **WMPF 版本识别**:地址表加载或自动检测成功,`LoadStartHookOffset` / `CDPFilterHookOffset(Offsets)` 偏移生效(日志里不出现偏移相关的报错)
- [ ] **9421 调试通道**:9421 端口监听;小程序侧(无 Origin 的原生客户端)WebSocket 握手成功。被拒时 Core 日志会打印 `已拒绝 <端口> 端口上来自 <origin> 的 WebSocket 握手：来源不是本机`(同一 Origin 只打印一次)。放行名单是**编译期常量**(`core/src/bridge/websocket-servers.ts` 的 `LOOPBACK_ORIGIN_HOSTS` + `devtools://`),没有运行时白名单 —— 要放行新来源须改代码重发
  - 自动化实测：微信 4.x 自绘 UI（UIA 窗口仅 52 节点、无具名控件），URL scheme 无 ticket 不生效，故「打开一个小程序」这一步只能人工完成
  -**顺序（实测）**：引擎 attach 的是 WMPF 宿主 `WeChatAppEx.exe`，所以必须**先在微信里打开一个小程序**，再 `engine.start`；否则返回 `core error 1000: 未找到微信 WMPF 宿主进程（WeChatAppEx.exe）：请先登录微信，并打开任意小程序后再启动引擎`（`1000` 是 stdio 层的通用码，类型化失败码是 `no_host`，只出现在引擎内部与 MCP 描述里）。9421 由 Core 监听、小程序拨入，因此目标出现的前提是「先开小程序 → 再启动引擎」
- [ ] **31415 CDP 通道**:默认 31415 端口监听（可配置）;`shell.openDevtoolsWindow`（Electron `devtools://` 独立窗口）握手成功并看到小程序源码面板。纯 HTTP `GET /json` 不属于 WMPF CDP 面,返回 426 Upgrade Required 属预期

## B. 多开与 Hook 捕获

- [ ] **miniapp.list**:打开 ≥2 个小程序后列表返回全部,appid/名称正确
- [ ] **miniapp.setLock / getLock**:锁定某 appid 后其余实例被拒绝
- [ ] **miniapp.switch**:切换目标后 status/app_info 事件反映新目标
- [ ] **wxapi.start**:进入小程序触发请求,wxapi_capture 事件**按 tick 批量**到达(每 tick 一个数组载荷,最多 2000 条 = 10 轮 × 200 条;剩余等下一 tick),wxapi.poll 返回且消费后不清空尚未确认的记录
- [ ] **wxapi.stop / wxapi.clear**:停止后不再有新事件,且 `wxapi.stats.pending` 归零、`wxapi.poll` 为空(停止会清掉未投递的待展示部分);清空后列表为空
- [ ] **重载续捕**:捕获中让小程序页面重载(realm 重建),捕获必须自动恢复(`wxapi.stats.running` 仍为 true、重载后的新调用照常到达);未捕获时的重载不得把钩子装上
- [ ] **wxapi.replay**:对某 GET 接口重放,返回与原请求语义一致的结果
- [ ] **WxAPI 页无重复行**:捕获期间 `wxapi_capture` 事件与 `wxapi.poll` 同时投递同一条记录,列表里该 `rid` 只出现一行(两路投递必须幂等)
- [ ] **慢接口落定**:触发一个 >1s 的 wx.request,列表行由 `pending` 变为 `success` 并显示返回结果与耗时(`durationMs`);`历史记录` 里同一条不再是 pending 且能读到响应正文
- [ ] **启动失败可见**:小程序页面上下文不可用(未连接 / 页面无 wx 对象)时点「开启捕获」必须报错并停留在「未捕获」,不得显示「捕获中」
- [ ] **连接不采集**:连接小程序后不点「开启捕获」,在页面里触发若干 wx.* 调用再打开「WxAPI」页,面板必须恒为「未捕获」且不出现任何记录;此时点「开启捕获」,列表里只出现**点击之后**的调用,不得补投连接以来的积压
- [ ] **点击即装钩子**:连接小程序后**立即**点「开启捕获」,必须进入「捕获中」并确实收到新记录;不得报「页面未找到可注入的 wx 环境」(该 realm 里找不到 `WeixinJSBridge` 时曾必现)
- [ ] **停止不复活**:捕获中触发若干调用 → 点「停止捕获」→ 同一个按钮此时写着「开启捕获」,再点它,面板里不得出现停止之前的记录;这批记录仍能在「历史记录」里查到(停止只丢未展示的待投递部分)
- [ ] **启停是一个开关**:wxapi 捕获 / 云函数捕获 / 云函数 API 服务 / 引擎 / MCP 服务 / vConsole / 页面遍历 **七处**(七个 `StateToggle`),每处都只有**一个**按钮,标签按各页实际用词:开启捕获·停止捕获 / 动态捕获·停止捕获 / 启动服务·停止服务 / 启动引擎·停止引擎 / 启动服务·停止服务 / 开启调试·关闭调试 / 开始遍历·停止访问;不存在第二个同时可见的启停按钮;在飞时该按钮写「…中…」并禁用;开着时点它是停止、关着时点它是开启
- [ ] **引擎停止即结束**:捕获中点「停止引擎」(与「启动引擎」是同一个按钮),面板回到「未捕获」;重新启动引擎并连接小程序后**不得**自动开始采集(`wxapi.stats.running` 必须为 false)
- [ ] **MCP 采集开关**:`hook_start(name=wxapi)` 之后 `hook_drain` 能取到记录,`hook_stop` 之后取不到;`hook_start(name=navigator)` 必须被拒绝
- [ ] **落定突发不丢**:让 ≥1000 个慢请求在记录已被 drain 之后集中返回,面板里这些行必须都落定为 success/fail 并显示耗时(不出现永久「等待中」),`历史记录` 同步不再是 pending
- [ ] **高流量下列表流畅**:在小程序里以千次/秒量级连续触发 wx.* 调用,列表在 2000 行上限下滚动、选择、切换分类仍然流畅(不掉帧、无长时间白屏);补齐大量积压时记录渐进出现,而非一次性阻塞
- [ ] **wxapi.stats**:`pending` 溢出 Shell 缓冲上限时才出现丢弃提示,数值与实际丢弃条数一致
- [ ] **云函数页无重复行**:`cloud_capture`(按批数组)与 `cloud.poll` 同时投递同一 `rid` 时只渲染一行
- [ ] **云函数耗时真实**:`wx.cloud.callFunction` 与 `wx.cloud.database()` 终端操作(db.get/add/update/remove/count…)在列表里显示**非 0** 的真实耗时与状态;若恒为 0 说明调用时刻未被正确记录
- [ ] **清空不复活**:任一捕获页点「清空」后,**在飞的 drain tick / poll 响应不得把已清记录加回面板**(清空与下一 tick 交错的窗口)
- [ ] **页面侧过载提示**:让页面缓冲溢出(短时间 >5000 条),「已丢弃」数值必须随之增长 —— 只加 shell 侧 `dropped` 而不含页面侧 `pageDroppedRecords` 属于口径错误
- [ ] **cloud.start**:触发云函数调用,cloud_capture 事件与 cloud.poll 正常,记录落入 SQLite(重启后仍在)
- [ ] **页面刷新/小程序切换后 seq 重置**:重装 hook 后新记录不会被旧 ack 值丢弃(验证 ResetAck 链路)

## C. 导航与调试面

- [ ] **navigator.pages**:返回小程序页面清单,与实际页面一致
- [ ] **navigator.pageStack**:运行时页面栈与 `getCurrentPages()` 一致(含每层 `route` 与查询参数);`navigator.navigate` 用 `navigateBack` + `delta` 后栈长度相应减少(该读数在 MCP 的 `navigator_page_stack` 与 `miniapp_page_stack`、以及 IPC 上都有,界面不再展示页面栈)
- [ ] **navigator.navigate / autoVisit / stopAutoVisit**:自动遍历推进,navigate_progress 事件的 progress/current/done/total/failed 数值合理;**单页 reLaunch 失败时 failed 必须计数**(不是 100% 成功);页面列表为空时 autoVisit 应直接报错而非静默走完
- [ ] **navigator.guardState / enableRedirectGuard / disableRedirectGuard**:劫持跳转被拦截并记录;开启后重新加载小程序,`guardState.enabled` 必须变回 false(拦截器随 realm 消失)
- [ ] **注入脚本 hook.list / inject / setGlobal**:`hook_scripts/` 里放一个 `.js` 即可见;注入后行里能看到落定值/错误与耗时(`lastRun`);改完文件再点「重新注入」必须按新版本立刻执行(按钮**不得**因已注入而禁用),此时 `stale` 为真、行里出现「文件已更新」;标记全局的脚本在小程序重新加载后自动注入(`injected` 随 realm 重建复位,`lastRun` 保留)
- [ ] **脚本输出可辨认**:注入脚本里的 `console.log` 在 Console 页带 `[文件名]` 前缀;脚本抛错时页内堆栈指向 `wxtap-user-script/文件名`;脚本最后一条表达式是值(或 Promise)时 `lastRun.summary` 给出落定值,而不是恒定的「无返回值」
- [ ] **console.list / clear**:小程序里 console.log/error、未捕获异常与未处理 Promise 拒绝都能进 `Console 日志` 页;暂停恢复后不丢行不重复;清空后新日志继续进入
- [ ] **engine.vconsole**:开启/关闭的裁决只认页面回执 —— `setEnableDebug` 的 success/fail 决定 `vconsole_result.ok`,页面没有 `wxFrame` 或直接拒绝都必须报失败(不得因为 RPC 没报错就算「已生效」);面板出现在小程序窗口里,本程序无法读取其内容,要看日志走 Console 页(`Runtime.consoleAPICalled`),不看 `vconsole_result`
- [ ] **targets.list / targets.attach**:列出页面 target 并可附加
- [ ] **code.readFile / expandDir / search**:反编译产物目录浏览与全文搜索返回正确内容;产物里的图片（png/jpg/gif/webp 等）点开就显示图片本身,字体、压缩包等二进制文件给出一句说明,而不是输出大量乱码

## D. 云函数调用与导出

- [ ] **cloud.call / cloud.call_container**:从前端发起云函数调用,返回结构与旧版一致
- [ ] **cloud.export**:导出文件生成,表头为 `AppID / 类型 / 名称 / 参数 / 状态 / 时间 / 结果`(七列);超大 payload 截断逻辑生效
- [ ] **export_progress 事件**:分批推进,done 到位

## E. MCP

- [ ] **mcp.start / mcp.status / mcp.stop**:端口监听、状态查询、停止释放端口（status 汇报 `mode:"http+sse"` 与两种端点 URL）
- [ ] **Streamable HTTP 端点**:MCP 客户端对 `POST /mcp` 完成 initialize（协议版本回声 + instructions）与 tools/call;GET /mcp 回 405
- [ ] **SSE 端点（legacy）**:MCP 客户端能完成 initialize 与工具列举
- [ ] **resources / prompts**:`resources/list` 列出 skills 技能并 `resources/read` 取回;`prompts/get {name:wx_pause_debug}` 返回剧本
- [ ] **调试器工具链**:`debugger_state`(确认 Debugger 域状态) → `debugger_list_scripts` 命中真实脚本 → `debugger_breakpoint{action:"set"}` → 触发后 `debugger_state`(帧表,1-based 行列) → `debugger_inspect{frame_index:0}` 读到作用域变量 → `debugger_inspect{expression}` 帧内求值 → `debugger_control{action:"resume"}` 恢复;暂停期间确认其它页面侧工具挂起、恢复后正常
- [ ] **skills**:技能目录在 settings.getPaths 指向的位置可被发现
- [ ] **stdio 工具清单**:`WxTap.exe -mcp` 下 `tools/list` 返回的工具数与 `tools()` 在该 profile 下的一致(缺省 `-tools=lean` 只广播合并目录,`-tools=all` 才是全量)、无重名（`TestEveryAdvertisedToolIsImplemented` 只保证「广告即实现」，数量需人工核对一次）
- [ ] **会话建立链（stdio，无 GUI）**:`wechat_status` → `engine_start` → `session_start`（它内部依次调 `miniapp_list` 选目标、`hook_start` 开采集 —— 这两个工具仍然各自可调，收敛的是调用次数而非工具面；`wxapi.start` 只是 IPC 方法名，没有同名 MCP 工具）→ 驱动页面 → `hook_drain` 拿到记录。这条链是本次新增的全部意义所在：此前 agent 能够拉起 Core 进程，却始终拿不到流量
- [ ] **engine_start 的可读失败**:微信未运行时调用，确认给出 `未找到微信 WMPF 宿主进程（WeChatAppEx.exe）…` / `缺少微信版本 X 的地址表：…` 这类可读错误(内部失败码 `no_host` / `no_version` 不出现在返回文案里)，而不是挂起或超时
- [ ] **丢弃计数**:持续采集至页内缓冲淘汰后，`session_status` 报的 `pageDroppedRecords` / `pageDroppedUpdates` 必须 > 0，且与 GUI 面板「已丢弃」一致
- [ ] **wxapi_replay**:重放一个 `request`，`status:"success"` 且响应体正确；**确认它不进 wxapi 流水**（绕过包装器是预期行为，也是审计时要说明的事实）
- [ ] **未授权访问遍历法**:`navigator_guard{action:enable}` → `navigator_visit{action:start}` → 轮询 `navigator_visit{action:state}` 至结束 → `navigator_blocked_redirects` 列出被拦页面；与手工截图对比法结论一致
- [ ] **sessionkey 的磁盘路径**:`sessionkey_scan{source:users}` 取到的 `dir` 传给 `sessionkey_scan{source:disk}` 能取到 key —— 在**完全没有启用过采集**的会话上也应成立
- [ ] **ak_verify**:用一对有效凭据得 `valid:true`，用伪造的得 `valid:false` + errcode；确认凭据未落盘
- [ ] **白名单边界**:用未声明的参数键调用一个直通工具，确认键没到达 IPC（`TestPassThroughDropsUndeclaredArguments` 是离线版，实机再抽查一次 `config.save` 这类危险方法无法被触达）

## F. 利用：官方接口

- [ ] **wxopen.endpoints**：「微信 AK」页按凭据类型分组列出接口；列表中**不含**任何发送/发布类接口（没有消息发送、模板发布、客服消息、webhook 投递——收录原则是「只列读取、生成、查询类」）。注意 `订阅消息模板列表` 是 GET 读取，属允许面
- [ ] **官方接口调用**：填真实 AppID/AppSecret 调用「内容安全检测」，errcode 为 0；结果区与复制内容里是原样的 `access_token`（本页不脱敏，凭据本来就是调用者自己的）
- [ ] **生成小程序码**：调用「生成小程序码（不限量）」，`page` 指向一个后台页面，页内渲染出图片；**用微信扫码确认能直达该路径**，页面 onLoad 能读到 `scene`
- [ ] **凭据不落盘**：调用前后检查配置目录与 `traffic.db`，确认无凭据/token 写入；重启 WxTap 后凭据页为空（凭据只存在于会话内存）

## G. 流量历史与数据生命周期

- [ ] **分页不跳行不重复**:**停止捕获后**从第 1 页翻到末页,记录总数与 `traffic.stats.records` 一致,无重复行、无跳过的行(同毫秒多条时顺序仍稳定);捕获进行中翻页允许窗口移动(新记录落在第 1 页,后面的页会整体后移),但第 1 页必须始终显示最新的记录
- [ ] **正文按需读取**:不点开记录时不得发起 `traffic.getBody`;点开后每记录每侧只读一次;切换记录后上一条的正文与重放结果不再显示
- [ ] **实时刷新不打断**:捕获进行中,历史记录页按 ≥2s 节流刷新;用户已翻到第 N 页时不把列表弹回页首、也不重取该页(只更新统计);**正在查看的详情不被清掉**(选中项仍在时不重置)
- [ ] **删除选中**:勾选若干行 →「删除选中 (N)」→ 确认框写明将删除 N 条 → 确认后这些行消失,`traffic.stats.records` 与列表总数同步下降,勾选清空,页面停在删除前那一页(该页被删空时回到末页)
- [ ] **删除单条**:详情面板「删除这条」删掉正在看的那条并关闭详情;后台自动刷新把已删记录从勾选里摘掉,但**不会**取消其它已勾选的行
- [ ] **清空**:点「清空」打开确认框,写明将删除全部 N 条且不可恢复;确认前一条都不删;确认后列表清空、`traffic.stats.records` 归零、勾选与选中项一并重置;清空不带任何参数,也不再有保留条数/天数可填
- [ ] **appId 过滤下推**:按 AppID 筛选时请求参数里带 `appId`(而非仅本地过滤已加载页),结果与后端一致;清空输入后恢复全量
- [ ] **回收失败如实提示**:若空间回收(WAL checkpoint / VACUUM)失败,界面同时给出「已清空 N 条」与「空间回收失败」两条,不得只报成功、也不得把已删除报成清空失败

## H. 收尾

- [ ] 全部通过 → 在本清单标注该轮实机验收完成（记录日期、微信完整版本号、执行人与所用构建号）
- [ ] 任一失败 → 按头部要求记录证据,在报告中列为阻塞项;不得以"离线测试已过"替代
