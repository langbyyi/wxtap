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

下表是 Core 内部的宿主定位失败类型。engine_start 通常返回中文原始错误，stdio 可能包装为通用错误码 1000；不要要求返回文案包含这些内部码，按实际错误归因并如实转述：

| 码 | 含义 | 处置 |
| --- | --- | --- |
| no_host | 未找到微信 WMPF 宿主进程（WeChatAppEx.exe） | 登录微信并打开任意小程序后重试 |
| no_main_process | 宿主父进程信息缺失 | 检查微信宿主进程与原始错误 |
| no_ancestor | 未能定位微信主进程 | 确认微信已登录、未被安全软件隔离 |
| ambiguous_host | 宿主与已登录微信不匹配 / 多个微信实例 | 确认同时仅登录一个微信 |
| no_version | 无法读取微信宿主的 WMPF 构建号 | wechat_status 查看构建号及原始错误 |

## 诊断顺序

1. node_status —— Node 22+ 缺失时 Core 无法启动，引擎与采集类工具均会报错，归因应从该项开始。
2. wechat_status —— 该工具不产生错误：error 字段非空表示检测本身失败；running:false 表示微信未运行；addressTable:false 表示静态地址表未覆盖当前构建或架构，是否可自动检测取决于平台与布局。用于区分「微信未运行」与「版本不支持」。
3. engine_start —— 幂等；失败按上表归因。
4. engine_status —— frida:true 表示附加成功，miniapp:true 表示小程序通道已连接；devtools:true 表示外部 DevTools 客户端已连接，MCP 调试不要求后者。

## 判读要点

- wechat_status 的 running:true 不表示一切就绪：缺表时仅 Windows 旧布局可自动检测，新布局与 macOS 需补表，缺少架构也不能正常挂钩。
- miniapp_list 为空可能是引擎未启动、小程序未打开或尚未接入调试通道，以 engine_status 区分附加与连接状态。
- engine_stop 会丢弃尚未被取走的记录；已入库的记录仍可通过 traffic_records 读取。
`,
}
