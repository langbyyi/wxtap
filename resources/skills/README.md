# WxTap 技能文档目录

本目录存放供 AI agent 读取的方法论文档。WxTap 通过两条路径将其中的 `.md` 暴露给 agent：

- **工具** — `miniapp_get_skills` 将全部技能拼接为单份文档返回（每份前缀 `## [文件名]`，以 `---` 分隔）；
- **资源** — `resources/list` 列出 `skill://<文件名>`，`resources/read` 按名返回原文。

本目录中的 `.md` 无需注册，连接后即可被发现；`README.md` 除外（它说明目录本身，不作为技能暴露）。

## 目录内文件

| 文件 | 分支：agent 的适用场景 | 内容要点 |
| --- | --- | --- |
| `session.md` | 一切在线能力的前提 | 前置检查与失败归因、engine_start 失败码（指向参考资源）、多开选目标、采集是显式动作、收尾的丢弃语义、排错速查 |
| `debugging.md` | 在线观测、UI 驱动、JS 求值、断点暂停 | 两个 page_stack 的区别、`wxapi_replay` 的 status 判读、`debugger_*` 工具族全流程、`cdp_command` 域白名单 |
| `traffic-audit.md` | 流量与云函数审计 | `hook_drain` 双游标与 `updateLimit` 语义、三组丢弃计数（五个字段名）、正反文体检索与截断、`miniapp_http_request` 与 `wxapi_replay` 的分工、`sessionkey_scan` 四路 |
| `code-audit.md` | 离线代码审计 | 三源清点、反编译的两种失败形态、优先直接读源码目录、检索主题表、`ak_verify` 与 `wxopen_*` 验活链路 |

## 写作约定

- **首行即 `resources/list` 的 description**（取首个非空行，截断 100 字符）。格式为「分支名：执行内容」，使 agent 仅凭列表即可选中该文档，或判定其不适用。
- **字段名写在调用指令同处，语义定义只保留一处。** 字段名构成 agent 的调用词汇表，必须与「调用该工具」的指令同屏出现；定义、判读规则与失败归因各有唯一真源，不重复书写。
- 稳定知识的真源为 `wxtap://reference/*`（记录字段 schema、引擎失败码），技能文档指向该处。两者不在同一份载荷中：`miniapp_get_skills` 仅拼接技能，参考资源需 agent 另行发起 `resources/read`。因此仅将大块 schema 交由参考资源承载，阻塞下一步执行的一两句语义保留在技能文档内。
- 工具名、参数与返回字段一律以 `tools/list` 的实际返回为准；工具实现变更时同步本目录与 `wxtap://reference/*`。
- 内容须可落实为动作：前置条件、顺序约束、游标与计数器语义、失败归因、结果判读。工具描述已覆盖的内容不重复。
- 跨文档引用不带编号（写「见 session.md」而非「见 session.md 的第 1–5 步」）：条目编号会随重排失效。
- 语体为技术陈述式，使用第三人称与条件-结果结构；不使用口语化表述、第二人称与修辞性设问。
- `prompts/list` 中的执行剧本（`wx_debug_session` 等）是本目录的浓缩形式，与本目录共用同一套术语。
