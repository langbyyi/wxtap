package mcp

// 内置参考文档（wxtap://reference/*）：agent 不用从工具输出反推的稳定知识。
// hook 记录形状对齐 core/hooks/wxapi.js 与 cloud.js 的实现，错误码对齐
// core/src/engine/win32-target.ts / darwin-target.ts 的 wmpfTargetError。
// 注意：raw string 里不能出现反引号，markdown 一律不用行内代码标记。

// referenceBodies maps the reference name onto its markdown body.
var referenceBodies = map[string]string{
	"hook-records": `# Hook 记录格式（wxapi / cloud）

hook_drain / hook_wait 返回的每条记录是 {seq, record}，落定更新是 {seq, update}。两条流游标独立。

## record（请求发起时投递，status 为 pending）

| 字段 | 说明 |
| --- | --- |
| rid | 稳定标识："<type>-<appId>-<ts>-<seq>"，与 traffic_records 的 id 同源 |
| type | 记录族：wxapi 钩子为 wx.request / wx.auth / wx.storage / wx.system 等；cloud 钩子为 function / storage / container / database 等 |
| name | API 名（wx.request、callFunction、collection.add…） |
| appId | 发起调用的小程序 appid（取自 __wxConfig，可能为空） |
| data | 调用参数的深拷贝（请求体、云函数入参、存储 key…） |
| status | pending（发起时）/ success / fail |
| result / error | 两者互斥；异步调用的结局不走这里，走 update 流 |
| ts | 调用发起时刻（epoch 毫秒）；durationMs 以它为基准 |
| timestamp | 页面本地时间字符串（展示用） |

## update（落定后投递，与 record 游标分列）

| 字段 | 说明 |
| --- | --- |
| rid | 对应 record 的 rid |
| status | success / fail |
| durationMs | settledAt - ts，异步调用耗时 |
| settledAt | 落定时刻（epoch 毫秒） |
| result / error | 互斥；记录异步调用的返回值或失败原因 |

仅消费 record 流而不读取 update 流时，异步调用会呈现为永久 pending。下结论前应补读 update 流，方法是将上一页的 nextUpdateSeq 回填到 afterUpdateSeq。

## 游标与丢失

- records 按 seq 单调递增，drain 返回 nextSeq（下一页 afterSeq）与 hasMore。
- 页内缓冲有界：读取不及时而被淘汰的记录永久丢失，数据库中亦不存在。该累计淘汰数只在 hook_stats 的 pageDroppedRecords / pageDroppedUpdates 中（session_status 的 capture 字段给出同一读数）。hook_drain 的返回只含 records / updates / nextSeq / nextUpdateSeq / hasMore，不含任何丢弃计数。hook_stats 的另一字段 dropped 含义不同：该批记录已进入 UI 与数据库，仅 poll 路径无法取回。两者互补、不可相加；结论必须说明丢失规模。
`,

	"engine-errors": `# 引擎失败码与前置诊断

engine_start 失败时错误中带可读失败码，按下表归因并如实转述：

| 码 | 含义 | 处置 |
| --- | --- | --- |
| no_host | 未找到微信 WMPF 宿主进程（WeChatAppEx.exe） | 登录微信并打开任意小程序后重试 |
| no_ancestor | 未能定位微信主进程 | 确认微信已登录、未被安全软件隔离 |
| ambiguous_host | 宿主与已登录微信不匹配 / 多个微信实例 | 确认同时仅登录一个微信 |
| no_version | 读不到该微信版本的 WMPF 版本信息或地址表缺失 | wechat_status 确认版本支持情况 |

## 诊断顺序

1. node_status —— Node 22+ 缺失时 Core 无法启动，引擎与采集类工具均会报错，归因应从该项开始。
2. wechat_status —— 该工具不产生错误：error 字段非空表示检测本身失败；running:false 表示微信未运行；地址表缺失（addressTable:false）是 no_version 的常见原因。用于区分「微信未运行」与「版本不支持」。
3. engine_start —— 幂等；失败按上表归因。
4. engine_status —— frida:true 表示附加成功；miniapp 与 devtools 需等到小程序连接后才为 true。

## 判读要点

- wechat_status 的 running:true 不表示一切就绪：地址表缺失时 engine_start 仍会失败。
- miniapp_list 为空有两种成因（未打开小程序 / 引擎未启动），以 engine_status 区分。
- engine_stop 会丢弃尚未被取走的记录；已入库的记录仍可通过 traffic_records 读取。
`,
}
