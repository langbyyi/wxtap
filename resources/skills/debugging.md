# 在线调试：观测、UI 驱动、JS 求值、断点暂停

前提：会话已建立（见 session.md）。本文覆盖两层能力：不打断运行时的观测与 UI 操作，以及暂停整个运行时的断点调试。

## 1. 观测（不暂停）

- `miniapp_page_stack` — 单次读取全部页面信息：实际导航栈（底页在前，含各页 query）、配置页面列表、tabBar 页与当前路由。判断「当前所在页面」与「`navigateBack` 可回退的距离」用它，不使用配置页面列表推断。
- `navigator_page_stack` — 仅返回实际导航栈的窄形态；它不在缺省广播的工具目录中（照名调用仍然有效）。
- `miniapp_console_log` — 最近 100 条控制台输出（含 CDP 层捕获的未捕获异常）。`clear:true` 先清空再复现：结论应建立在复现后新产生的输出上。
- `miniapp_get_storage` — 不带参数返回全量（键、值、大小）；传 `key` 返回单键值。定位缓存的 token、用户资料与开关位从该项开始。
- `miniapp_screenshot {format, quality}` — 截图存证。返回内容为图像，应直接读取图像，不以调用成功与否作为结论。

## 2. UI 驱动

- `miniapp_click` — 优先传 `selector`（在当前页 webview 内执行 `querySelector` 与 `click()`，返回元素中心坐标）；传 `x,y` 则派发底层鼠标事件。
- `miniapp_type {text, selector?, clear?}` — 先聚焦，再逐字符派发按键。
- `miniapp_navigate {route, method}` — 一律经页面侧导航钩子：由它解析 `wxFrame` 并区分 tabBar 页。`switchTab` 因此只对 tab 页成立，目标不符时如实报错（不会退化成 `navigateTo`）。以 `navigator_page_stack` 验证已到达目标页。
- `miniapp_scroll {x, y, selector?}` — 在当前页派发滚轮事件（页面由合成器滚动，改 `scrollTop` 之类不会生效）。返回的是落点与位移，**不含滚动位置**：页内的 `scrollY` / `scrollTop` 恒为 0，回读会误导。是否真的滚了用 `miniapp_screenshot` 前后对比确认。

页面侧的读写都要先定位到正确的执行上下文：存储类工具落在逻辑层（`wx` 所在），`miniapp_click` / `miniapp_scroll` 的 `selector` 形态落在当前页 webview（DOM 所在）。定位不到时报错，不退回默认上下文——静默退回只会得到「元素找不到」或 `wx is not defined` 这类看不出原因的结果。
- `navigator_visit {action:"start"}` — 逐页 reLaunch 遍历小程序上报的全部路由，每页约 2s。启动后轮询 `navigator_visit {action:"state"}` 直至结束（进度仅经事件发布，agent 侧该轮询是唯一完成判据）；提前结束用 `navigator_visit {action:"stop"}`。

## 3. JS 求值

- `miniapp_evaluate {expression, await_promise?, context_id?}` — 在页面内执行任意 JS，`returnByValue`。异常经 `exception` 字段返回；`error:"Evaluation error"` 不表示表达式为假。
- `miniapp_list_contexts` — 列出可用执行上下文（`appservice` / `webview`），其 id 用于指定 `context_id`。业务逻辑主要位于 appservice 上下文。
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

专用工具未覆盖的 DevTools 能力通过 `cdp_command {method, params?, timeoutMs?}` 调用，例如 `DOMSnapshot.captureSnapshot`（整页 DOM 文本）、`Performance.getMetrics`、`DOM.querySelector`、`Network.getResponseBody`。仅放行 Console / DOM / DOMSnapshot / Debugger / Log / Network / Overlay / Page / Performance / Runtime 十个域；Browser（可关闭宿主）、Target（会话不受桥管理）、HeapProfiler / Tracing（载荷过大）等一律拒绝。响应超过 2MB 时截断为预览，此时应缩小查询范围后重试，不应原样重试。

## 6. 调试循环模板

```
观测（console / storage / 页面栈） → 形成假设
→ debugger_list_scripts 定位代码
→ debugger_breakpoint {action:"set"} 或 debugger_control {action:"pause"}
→ debugger_state（调用栈） → debugger_inspect（作用域 / 帧上求值）
→ 需要时 debugger_control {action:"step_*"} 单步推进
→ debugger_control {action:"resume"} → 在正常执行中验证假设（wxapi_replay / 再次观测）
```
