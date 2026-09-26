# 流量与云函数审计：游标读法、丢弃计数、凭据验活

前提：会话已建立且 `hook_start` 已开（见 session.md）。本文覆盖采集侧的读取方法与判读要点。

## 1. 两条数据通路

- **hook 流**（`hook_drain`）：请求发起时即可见，含请求体；异步调用的结局后到。
- **入库流**（`traffic_records` / `traffic_get_body`）：已落库的历史记录，支持条件过滤与完整正反文体。库不会自动裁剪：只有用户手动删除或「清空」才会减少，所以历史长度取决于用户操作，核对时间范围时不要假定有保留窗口。

审计时两条通路并用：hook 流反映「刚刚发生的行为」，入库流回答「该时间段内的全部行为」。

## 2. hook_drain 的游标语义（读取方式错误将导致始终读取最旧一批）

```
hook_drain {name, afterSeq, limit, afterUpdateSeq, updateLimit, waitMs}
```

- **记录流** — 以 `afterSeq` 游标分页，将上一页的 `nextSeq` 回填以读取下一页。
- **落定更新流** — 异步调用的 `status`、返回值与 `durationMs` 在其记录被取走后才落地，作为独立帧投递。必须将上一页的 `nextUpdateSeq` 回填到 `afterUpdateSeq`；不回填（保持 0）将始终重读最旧的一批更新。
- **`updateLimit`** — 缺省取一个小窗口（200）。传 `0` 表示本页不读取更新且游标不动，但该语义仅在 `afterUpdateSeq` 非 0 时成立：`afterUpdateSeq:0` 与 `updateLimit:0` 同时出现时按缺省处理，取 200 条。需要读取更新时必须填写 `afterUpdateSeq`。
- 仅读记录流而不读更新流的调用会将异步调用判定为「永久 pending」。下结论前应补齐更新流。
- **触发动作后等待流量落地使用 `hook_drain {name, afterSeq, waitMs}`** — 服务端长轮询，至少一条新记录或超时（上限 25s）后返回，可替代紧密轮询。

记录与更新的完整字段 schema 属稳定知识，见资源 `wxtap://reference/hook-records`，无需从样例反推。

## 3. 结论可信度：先读取丢弃计数

三组读数名称相近、含义不同，不可相加：

| 出处 | 字段 | 含义 |
| --- | --- | --- |
| `session_status` 的 `capture`，或 `hook_stats {name}` | `pageDroppedRecords` / `pageDroppedUpdates` | 页侧缓冲的累计淘汰：在无人读取之前即被丢弃，永久丢失，库中亦不存在 |
| 同上 | `dropped` | 含义不同：这些记录已进入 UI 与数据库，仅 poll 路径无法取回 |
| `session_status` 的 `store`，或 `traffic_stats` | `droppedRecords` / `droppedUpdates` | 两个钩子对应计数的汇总口径 |

`hook_drain` 的返回不含丢弃计数，其字段仅为 `records` / `updates` / `nextSeq` / `nextUpdateSeq` / `hasMore`。

`pageDropped*` 不为 0 时，「流量中不含 session_key」一类结论不成立，必须先说明丢失数量及其时间范围。字段语义的完整定义见 `wxtap://reference/hook-records`。

## 4. 入库流检索

- `traffic_records {query?, apiType?, status?, appId?, page?, limit?, bodyBytes?}` — 单页聚合视图：摘要与每条记录截断后的正反文体（二进制内容标注为 binary，不内联），可省去逐条调用 `traffic_get_body` 的往返。`page` 从 1 起（缺省 1），`limit` 缺省 20（上限 50），`bodyBytes` 缺省 2048（上限 16384）；`bodyBytes:0` 返回不含文体的列表。返回里的 `total` 是同一套筛选条件下的总条数，按它算页数；列表**最新在前**，第 1 页就是最近的一批。注意 offset 窗口在捕获进行中会随新记录移动（后面的页整体后移），要**不重不漏**地跟一条记录流请用 `hook_drain` 的 `afterSeq`，不要靠翻页。
- `traffic_get_body {id, part:"request"|"response"}` — 获取完整文体（`part` 缺省为 `response`）。超过 256KB 时截断，并标注 `truncated:true` 与 `totalBytes`。用于检索明文凭据、token、身份证号与内部接口参数。
- `traffic_stats` — 库大小（`bytes`）、时间范围（`oldestCapturedAt` / `newestCapturedAt`）与过载丢弃计数。引用历史流量前应先核对其时间范围是否覆盖目标区间。

## 5. 主动验证

- `wxapi_replay {api_name, options}` — 见 debugging.md 的 JS 求值节：以小程序自身凭据重放任意 wx 调用；`ok` 在本接口边界已归一化：被调调用失败时 `ok` 为 `false`，可据 `ok` 直接判定重放是否成功；`status`（`success` / `fail` / `complete`）与 `error` / `reason` 一并保留。重放本身不进入流量记录。
- `miniapp_http_request {method, url, headers?, body?, timeout?}` — 向已发现的接口发起请求并可自定义 header 与 body：从流量中提取的 URL 与 token 需经该项验证越权与重放。`wxapi_replay` 不适用于该场景，它仅能重放 wx.* 调用。判读时注意两点：`status` 为 HTTP 状态码（与 `wxapi_replay` 的 `success`/`fail` 同名不同义）；`body` 静默截断至 50000 字符且不带 truncated 标记，大响应的截断部分不可作为结论依据。TLS 校验缺省关闭，自签证书与内网地址可直接访问。
- `miniapp_cloud_scan` — 静态扫描已加载代码中的云函数、集合与存储引用，产出攻击面清单。

## 6. 直呼云函数

以 `miniapp_evaluate {expression:"wx.cloud.callFunction({name, data})", await_promise:true}` 调用，走页面自身凭据，等价于页面发起；配合 `miniapp_cloud_captures` 查看捕获结果。兼容名 `miniapp_call_cloud {name, data}` 同样可调用，但不在默认广播的工具目录中。

## 7. 会话密钥物料

`sessionkey_scan {source}` 合并四条提取路径，`source` 为必填项：

| source | 提取内容 |
| --- | --- |
| `traffic` | 从已捕获的数据包中提取 session_key / iv / encryptedData 物料（含 URL-encoded 体）。只读 |
| `users` | 列出磁盘上的微信账号目录，每项含该账号出现过的 appid 与需传给 `disk` 的目录。离线路径的发现步骤 |
| `disk` | 从微信自身存储（MMKV、日志）中恢复，`dir` 必须取自 `users` 的返回结果 |
| `decompiled` | 从反编译产物中提取加解密物料 |

离线两路（`users` → `disk`）在无采集运行时同样可用。

## 8. 输出约定

- 按接口归纳：URL、方法、鉴权方式（header 或参数中的 token 形态）、敏感字段及其是否明文。
- 引用记录时附 id（可回查 body）；区分「已捕获」与「推断存在」。
- 丢弃计数非零时，明确写出丢失规模及其对结论的影响。
