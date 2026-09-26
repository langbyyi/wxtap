# 建立在线调试会话：前置检查 → engine_start → 选目标 → 开采集

一切在线能力的前提。该流程首先是一条顺序约束：连接小程序不等于开启采集。顺序错误不产生报错，只会得到恒空的 `hook_drain` 与空列表。

## 快速路径：单次调用完成引导

`session_start` 按固定顺序执行：前置检查 → `engine_start` → 选目标（唯一连接自动锁定；多开时传 `target:<id>`）→ 开采集（`hooks` 缺省为 `["wxapi"]`，可追加 `"cloud"`，传 `[]` 表示本轮不采集）→ 汇总快照。引擎与采集两段是幂等的（已在跑即早返回），但**传 `target:<id>` 时并非「什么都不做」**：换目标会重置页面 generation 与两个 feeder 的 ack —— 新目标有自己的页内序号空间。重复调用仍不会产生重复读数：重读的帧由 shell 的去重 FIFO 抑制，面板按 `rid` 原地覆写。

任一步失败即停止，返回 `ok:false`、`step`（失败的步骤）与 `error`（原因），并带回已取得的诊断信息（node/wechat 状态、`available` 目标清单）。按返回内容如实转述，不补充推测。

`session_status` 为该流程的只读形式：一次返回引擎状态、锁定目标、页面栈、采集健康（丢弃计数）与库大小，不产生副作用。

## 分步路径

仅在需要逐步归因时使用，顺序与快速路径一致。缺省广播的是 lean 目录（只有合并形态与高杠杆工具），本节的 `node_status`、`wechat_status`、`engine_status`、`miniapp_list`、`miniapp_switch`、`engine_stop` 不在其中；第 7 步的 `hook_start` / `hook_drain` 在。不在目录里的照名调用仍然有效。

1. `node_status` — Node 22+ 是所有前置条件的前提。Core 由该运行时启动；不满足时引擎与采集类工具均会报错，归因应从该项开始。
2. `wechat_status` — 该工具不产生错误：`error` 非空表示检测本身失败；`running:false` 表示微信未运行；`addressTable:false` 是 `engine_start` 返回 `no_version` 的常见原因。区分「微信未运行」与「微信版本不支持」以该项最快。
3. `engine_start` — 附加 Frida 到微信宿主进程并启动 CDP 代理。幂等，可重复调用。失败时错误中带可读失败码（`no_host` / `no_ancestor` / `ambiguous_host` / `no_version`）；失败码的含义、处置与完整诊断顺序见资源 `wxtap://reference/engine-errors`。
4. `engine_status` — 确认 `frida:true`。`miniapp` 与 `devtools` 需等到小程序连接后才为 true。
5. `miniapp_list` — 列出已接入调试桥的小程序。空列表有两种成因（未打开小程序 / 引擎未启动），以 `engine_status` 排除后者。
6. `miniapp_switch {id}` — 多开时将调试目标固定到其中一个小程序。未知 id 返回 `{ok:false}` 而非报错。切换会重置页面代次：钩子重新安装，标记为 global 的用户脚本重新注入。
7. `hook_start {name:"wxapi"}` — 云函数审计追加 `hook_start {name:"cloud"}`。

采集是显式动作：连接小程序本身不记录任何数据。在 `hook_start` 之前，`hook_drain` 恒为空、`miniapp_cloud_captures` 恒为空。该状态不是故障，仅表示顺序尚未完成。console 为例外：连接即采集，无需启动。

## 验证生效

- `hook_drain {name:"wxapi"}` 读取一页，确认记录数在增长（需要在小程序内实际操作以产生流量）。
- `session_status` 检查 `capture` 中的丢弃计数是否为 0。
- `engine_status` 的 `appInfo` 中应已包含 appid 与名称。
- `miniapp_page_stack` 核对当前页面栈是否为目标页面。

## 收尾

`engine_stop` 与 `hook_stop {name}` 均会丢弃尚未被取走的记录。收尾前应先读取需要保留的证据；已入库的记录之后仍可通过 `traffic_records` 读取。

## 排错速查

| 症状 | 首要检查项 |
| --- | --- |
| `hook_drain` 恒空 | 是否调用过 `hook_start`；`hook_stats {name:"wxapi"}` 的丢弃计数是否为 0 |
| `miniapp_list` 为空 | `wechat_status` → `engine_status` → 是否有小程序在前台打开 |
| 全部工具报引擎错误 | `node_status`（Core 未启动）或 Core 进程已退出 |
| 全部工具无响应 | 调试器可能处于暂停态（`debugger_state`）；`debugger_control {action:"resume"}` 恢复 |
