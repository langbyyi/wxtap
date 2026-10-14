# 在线调试：观测、UI 驱动、JS 求值、断点暂停

前提：会话已建立（见 session.md）。本文覆盖两层能力：不打断运行时的观测与 UI 操作，以及暂停整个运行时的断点调试。

## 1. 观测（不暂停）

- `miniapp_page_stack` — 单次读取全部页面信息：实际导航栈（底页在前，含各页 query）、配置页面列表、tabBar 页与当前路由。栈上 query 值默认打码（键名保留），`redact:false` 出原值。判断「当前所在页面」与「`navigateBack` 可回退的距离」用它，不使用配置页面列表推断。
- `navigator_page_stack` — 仅返回实际导航栈的窄形态；它不在缺省广播的工具目录中（硬编码的旧客户端仍可调用）。
- `miniapp_console_log` — 最近 100 条控制台输出（含 CDP 层捕获的未捕获异常）。记录默认打码，`redact:false` 出原值。`clear:true` 先清空再复现：结论应建立在复现后新产生的输出上。
- `miniapp_get_storage` — 不带参数返回全量（键、值、大小，值默认打码，`redact:false` 出原值）；`mode:"keys"` 只回键名与体长（先看有什么再决定看不看内容）；传 `key` 返回单键值。定位缓存的 token、用户资料与开关位从该项开始。
- `miniapp_screenshot {format, quality}` — 截图存证。图像之外的第二个 content 块带这一张的路由 / CSS 视口 / 像素比——截图走浏览器级端点，与页面栈可能错位，先核对元数据再下结论。
- `miniapp_a11y_tree` — 当前页 webview 的无障碍树：每个节点带稳定路径（`0/2/1`）、标签、文本与矩形。给 `miniapp_click {node_path}` 用；上限 300 节点，超出以 `truncated` 标注。web-view 盖住的内容不属于页 DOM，不在此树中。

## 2. UI 驱动

- `miniapp_click` — 三种寻址：`node_path`（a11y 树的路径；组件桥可达时派发 exparser tap，回包的 `via` 注明走的是 exparser 还是 DOM click）、`selector`（当前页 webview 内执行，未命中报错）、`x,y`（底层鼠标事件，落点越界当场拒绝）。坐标点击可能落到 web-view 下面的页面上——有 web-view 时优先用 `node_path` 或 `selector`。
- `miniapp_type {text, selector?, clear?}` — selector 命中组件输入（wx-input 一类）时走组件的 `input` 事件（键盘事件改不了组件值），回包带 `value_len`，空值按 `ok:false` 处理；非组件元素退回逐字符按键。
- `miniapp_navigate {route?, method, delta?}` — 一律经页面侧导航钩子：由它解析 `wxFrame` 并区分 tabBar 页。**回包以调用之后的真实页面栈为准**：回调说 ok 而栈没变的「假 ok」会如实回 `ok:false` 并带 `route_observed`；`navigateBack` 用 `delta`（缺省 1）且 route 可省。插件页在前台时宿主侧回退可能被拒，此时自动改在该页 webview 里调。
- `miniapp_scroll {x, y, selector?}` — 在当前页派发滚轮事件（页面由合成器滚动，改 `scrollTop` 之类不会生效）。返回的是落点与位移，**不含滚动位置**：页内的 `scrollY` / `scrollTop` 恒为 0，回读会误导。是否真的滚了用 `miniapp_screenshot` 前后对比确认。

页面侧的读写都要先定位到正确的执行上下文：存储类工具落在逻辑层（`wx` 所在），`miniapp_click` / `miniapp_scroll` 的 `selector` 形态落在当前页 webview（DOM 所在）。定位不到时报错，不退回默认上下文——静默退回只会得到「元素找不到」或 `wx is not defined` 这类看不出原因的结果。
- `navigator_visit {action:"start"}` — 逐页 reLaunch 遍历小程序上报的全部路由，每页约 2s。启动后轮询 `navigator_visit {action:"state"}` 直至结束（进度仅经事件发布，agent 侧该轮询是唯一完成判据）；提前结束用 `navigator_visit {action:"stop"}`。

## 3. JS 求值

- `miniapp_evaluate {expression, await_promise?, context_id?}` — 在页面内执行任意 JS，`returnByValue`。异常经 `exception` 字段返回；`error:"Evaluation error"` 不表示表达式为假。宿主按「表达式位置」编译表达式：语句级输入（`var` / 函数声明、分号）会抛 SyntaxError，工具会自动用 IIFE 重试一次——取值需显式 `return`。超时的命令其实已下发（副作用可能已发生），失败信息里会注明。`context_id` 不存在时是明确报错，不是空结果。
- `miniapp_call {expression, await_promise?}` — 在 appservice 里调用小程序自己的代码（页面方法、`store.dispatch`、业务辅助函数）：这才是带登录态与请求拦截器的通路，裸调 `wx.request` 不带令牌。回包只有结果的形状（类型 / 长度 / 键名，深度 4、每对象前 64 键；敏感键整体折叠）——令牌不出小程序。
- `miniapp_list_contexts` — 列出可用执行上下文：每项带 `id`、`kind`（`appservice` / `webview`）与 webview 的路由（`body[is]`）。宿主上报上下文生命周期时走事件注册表（`source:"events"`），否则回退有界探测（`source:"probe"`）。业务逻辑主要位于 appservice 上下文。
- `miniapp_get_source {url_pattern, max_length?}` — 按 url_pattern 获取脚本正文（缺省截断于 5 万字符），也可直接传 `scriptId`。读取运行中页面的代码时，先以 `debugger_list_scripts` 取得 url，再取全文。**匹配不到即报错**（并点出 pattern），不会回「成功但正文为空」；若错误说清单还是空的，那是 Debugger 域刚启用、`scriptParsed` 尚未到达，稍后重试即可。
- `wxapi_replay {api_name, options}` — 以小程序自身凭据重放任意 wx 调用（例如 `{api_name:"request", options:{url,method,data,header}}`）。`ok` 在本接口边界已归一化：被调调用失败时 `ok` 为 `false`，可据 `ok` 直接判定重放是否成功；`status`（`success` / `fail` / `complete`）与 `error` / `reason` 一并保留，用于区分失败原因。重放走钩子捕获前的原函数，不会出现在流量记录中。

## 4. 断点调试（暂停整个运行时）

暂停期间整个小程序被冻结，其它页面侧工具（evaluate、导航、采集读取）均无响应，直到调用 `debugger_control {action:"resume"}`。执行顺序应为：先观测，后暂停；暂停期间只做检查；完成后立即恢复。

1. `debugger_list_scripts {url_filter?, limit?}` — 列出已解析脚本（`scriptId` 与 `url`）。该项是脚本清单的权威来源；Debugger 域会按需自动启用。
2. 两种暂停方式：
   - `debugger_breakpoint {action:"set", url, line, condition?}` — 按 url 与行号下断点（`line` 从 1 起）；`{action:"list"}` 查看当前断点，`{action:"remove", breakpoint_id}` 撤销。
   - `debugger_control {action:"pause"}` — 在下一条语句暂停，内部等待 `Debugger.paused` 事件落地（上限约 5s）后返回快照。
3. 暂停后读取现场（均要求处于暂停态，以 `debugger_state` 确认）：
   - `debugger_state` — 暂停原因与调用栈（每帧含 `index`、函数名、url、基于 1 的行列号、scope 类型）。
   - `debugger_inspect {frame_index?}` — 读取指定帧各作用域（local→closure→global）的变量名与值。objectId 在恢复后失效，应在恢复前读取。
   - `debugger_inspect {expression, frame_index?}` — 在该帧上下文中求值，局部变量与闭包均在作用域内。
4. `debugger_control {action:"step_over"|"step_into"|"step_out"}` — 每步等待新的暂停落地并返回新的帧。
5. `debugger_control {action:"resume"}` 恢复执行。

## 5. CDP 逃生舱

专用工具未覆盖的 DevTools 能力通过 `cdp_command {method, params?, timeoutMs?}` 调用，例如 `DOMSnapshot.captureSnapshot`（整页 DOM 文本）、`Performance.getMetrics`、`DOM.querySelector`、`Network.getResponseBody`。仅放行 Console / DOM / DOMSnapshot / Debugger / Log / Network / Overlay / Page / Performance / Runtime 十个域；Browser（可关闭宿主）、Target（会话不受桥管理）、HeapProfiler / Tracing（载荷过大）等一律拒绝。宿主回错误信封（非法 contextId 等）时该命令如实报错，不再回「成功但 result 为 null」。响应超过 2MB 时截断为预览，此时应缩小查询范围后重试，不应原样重试。

H5 / web-view 目标的一次性验证用 `targets_probe {clientId, targetId}`：创建失败的原因（上下文未就绪、不是可调试网页等）只有这里可见——`miniapp_*` 工具的回包不带 H5 通道的失败文案。

## 6. 调试循环模板

```
观测（console / storage / 页面栈） → 形成假设
→ debugger_list_scripts 定位代码
→ debugger_breakpoint {action:"set"} 或 debugger_control {action:"pause"}
→ debugger_state（调用栈） → debugger_inspect（作用域 / 帧上求值）
→ 需要时 debugger_control {action:"step_*"} 单步推进
→ debugger_control {action:"resume"} → 在正常执行中验证假设（wxapi_replay / 再次观测）
```
