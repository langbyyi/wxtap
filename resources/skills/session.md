# 建立在线调试会话：前置检查 → engine_start → 选目标 → 开采集

一切在线能力的前提。该流程首先是一条顺序约束：连接小程序不等于开启采集。顺序错误不产生报错，只会得到恒空的 `hook_drain` 与空列表。

开始前必须由用户登录微信并先打开自有或已授权小程序，使 WMPF 宿主进程存在；否则 `engine_start` 提示未找到宿主（内部类型为 `no_host`）。仅 Frida 附加成功也不表示小程序通道已经连接。

## 快速路径：单次调用完成引导

`session_start` 按固定顺序执行：前置检查 → `engine_start` → 选目标（唯一连接自动锁定；多开时传 `target:<id>`）→ 开采集（`hooks` 缺省为 `["wxapi"]`，可追加 `"cloud"`，传 `[]` 表示本轮不采集）→ 汇总快照。整个调用是状态感知的幂等：引擎已附加时不再重建；**`target` 与当前锁定一致时不再重跑 switch**（重复 switch 会重置页面 generation 与 feeder ack，等于把正在工作的会话拆了重来——只有真的换目标才切，并强制重装钩子）。钩子按三态判定：没在跑 → 启动；在跑但断线（引擎重启、切页重建让页内钩子消失）→ 停止再启动；健康 → 跳过（回包标 `already:true`）。新启动会清一次页内记录缓冲：`hook_drain` 的 `afterSeq=0` 从本次启动起算，旧 cursor 不会再回放跨天历史。

任一步失败即停止，返回 `ok:false`、`step`（失败的步骤）与 `error`（原因），并带回已取得的诊断信息（node/wechat 状态、`available` 目标清单）。按返回内容如实转述，不补充推测。

`session_status` 为该流程的只读形式：一次返回引擎状态、锁定目标、页面栈、采集健康（丢弃计数）与库大小，不产生副作用。另外：由 MCP 侧启动过采集之后，若采集在会话中途断线（feeder 停了、或 realm 重建后页内钩子没了），`miniapp_*` 会话类工具与 `hook_drain` 的回包会带上 `capture_health` 字段——看到它就重跑 `session_start`，健康时该字段不出现。

## 分步路径

仅在需要逐步归因时使用，顺序与快速路径一致。本节第 1-4 步（`node_status` / `wechat_status` / `engine_start` / `engine_status`）与 `miniapp_list`、`miniapp_switch`、`targets_list`、`miniapp_list_contexts`、`targets_probe` 都在 lean 目录里；`engine_stop` 不在（收尾走 `session_status` 观察引擎状态，或用 `-tools=all` 档案）。**只有 `tools/list` 广播的名字保证可被客户端调用**——客户端按目录预过滤，目录外的名字（合并吸收的旧形态等）只有硬编码直呼的旧客户端才用得上。

1. `node_status` — Node 22+ 是所有前置条件的前提。Core 由该运行时启动；不满足时引擎与采集类工具均会报错，归因应从该项开始。
2. `wechat_status` — 该工具不产生错误：`error` 非空表示检测本身失败；`running:false` 表示微信未运行；`addressTable:false` 表示静态地址表未覆盖当前构建或架构；仅 Windows 旧布局缺表时可自动检测，其他缺表情况需补表，缺少架构不能正常挂钩。区分「微信未运行」与「微信版本不支持」以该项最快。
3. `engine_start` — 附加 Frida 到微信宿主进程并启动 CDP 代理。幂等，可重复调用。失败时保留中文原始错误，stdio 可能包装为通用错误码 `1000`，不要假定文案包含 `no_host` 等内部类型；类型含义、处置与完整诊断顺序见资源 `wxtap://reference/engine-errors`。
4. `engine_status` — 确认 `frida:true` 后再检查 `miniapp:true`；`devtools:true` 表示外部 DevTools 客户端已连接，MCP 调试不要求该状态为 true。
5. `miniapp_list` — 列出已接入调试桥的小程序。空列表可能表示引擎未启动、小程序未打开或尚未接入调试通道；先用 `engine_status` 区分附加与连接状态。
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
