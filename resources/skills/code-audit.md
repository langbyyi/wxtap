# 离线代码审计：清点 → 反编译 → 检索 → 敏感信息 → 凭据验活

不依赖在线会话：磁盘上存在 wxapkg 缓存即可执行。顺序固定：先清点，再反编译，最后检索，不应直接对随机目录扫描。

## 1. 清点可审对象

`extract_inventory {dir?, user_dir?}` — 三源合并（微信账号索引 ∩ wxapkg 缓存 ∩ 已有反编译产物）的 appid 清单，每项包含：

- `status ∈ decompiled | ready | indexed_only` — 已反编译 / 包在缓存中可直接反编译 / 仅索引可见；
- `unsupported: true` — 页面模板由微信新版编译模板运行时（`__wxCodeSpace__.batchAddCompiledTemplate`）生成，WxTap 还原不了这类模板，因此它不在可反编译范围内：不进「N 个小程序」的计数，`miniapp_decompile` 会直接拒绝。列表里看不到它属正常。
- 包路径与产物路径。

`dir` 为 wxapkg 包目录，`user_dir` 为微信账号索引根；两者可分别覆盖，均缺省时自动探测。该项用于确定本机可审计的小程序范围。

## 2. 反编译

`miniapp_decompile {appid, packages_dir?}` — `packages_dir` 缺省时自动探测默认包目录。该工具不检查反编译状态：对已反编译的 appid 再次调用会重新反编译并替换产物目录。`extract_inventory` 的 `decompiled` 状态只用于规划，不构成跳过。

两种失败形态需区分处理：

- **未探测到包目录** — 工具报错 `Cannot find wxapkg packages directory. Provide packages_dir parameter.`，此时不返回 appid 清单。应改用 `extract_inventory`（缺省广播）自行定位目录，或用 `miniapp_list_packages`。
- **目录中不含该 appid 的 wxapkg** — 不报错，返回 `{error:"No wxapkg found for <appid>", available_appids:[…]}`（最多 20 项，字典序）。此为正常结果，按 `available_appids` 更换目标。

产物位置：`code_projects` 直接列出已有工程及其绝对路径（缺省广播），或用 `paths_get` 的 `outputDir`。

## 3. 检索与阅读

**读取反编译产物优先直接访问源码目录。** 先用 MCP 取得绝对根路径：`code_projects` 返回各工程的绝对路径，`code_tree {appid}` 返回单个工程的顶层结构与根路径，`paths_get` 的 `outputDir` 返回产物根目录（三者中只有 `paths_get` 不在缺省广播目录中，照名调用有效）。随后用文件系统工具直接读取与检索。该方式无截断、无结果条数上限，检索能力也强于工具内置检索。

MCP 侧的读取工具在两种情况下使用：客户端无文件系统通道（如远程 HTTP 接入），或仅需读取少量确定内容。其行为边界如下：

- `code_tree {appid}` — 返回工程顶层目录树与绝对根路径。`appid` 必须已反编译。
- `code_list_dir {path}` — 返回目录的直接子项。path 为必填：缺参会报错，不返回空目录。
- `miniapp_search_code {root, query, regex?, max_results?}` — 全文或正则检索，`root` 取 `code_projects` 返回的绝对路径（越界会被拒绝）。结果可能截断（`truncated:true`），需要更全结果时应缩窄 query 或按主题分批检索；`max_results` 缺省 200。
- `miniapp_read_file {path, max_length?}` — 读取文件内容（`max_length` 缺省 10 万字符，截断时标注 `truncated:true`）。

根目录限制只作用于 `miniapp_read_file` 与 `miniapp_search_code`（越界被拒），`code_tree` 由产物根路径构造；`code_list_dir` 无此限制，可列出磁盘上任意目录。`scan_dir {dir}` 对整个目录执行敏感信息分析（凭据、证件号、URL、OSS bucket 等），返回分类计数与报告。

推荐检索主题（逐个执行）：

| 主题 | 关键词 |
| --- | --- |
| 网络面 | `https://`、`request`、`baseUrl`、`api.` |
| 云函数 | `callFunction`、`cloud.init`、`database(` |
| 加密 | `encrypt`、`decrypt`、`AES`、`RSA`、`md5`、`CryptoJS` |
| 凭据 | `secret`、`appsecret`、`access_key`、`token`、`Bearer` |
| 鉴权 | `login`、`checkSession`、`authorize`、`getToken` |

## 4. 敏感信息扫描

`miniapp_scan_sensitive {appid}` — 对该 appid 的反编译产物执行结构化扫描，findings 含 `severity` / `confidence` / `file` / `line` / `masked`（脱敏后的展示形态）/ `privilege`。

前提：产物必须已存在（先执行第 2 步）；目录缺失时该项报错，不返回 0 结果。

静态扫描仅提供线索，不验证凭据有效性。硬编码的 AppSecret 可能已被轮换，有效性需由第 5 步确认。

## 5. 凭据验活（静态扫描无法完成的一步）

`ak_verify {mode, access_key, secret_key, fake_ip?, include_token?, body_limit?}` — 向微信请求 access token，以验证凭据是否可用：

- `mode`：`oa`（公众号）/ `mini`（小程序）/ `work`（企业微信）；
- 凭据无效属正常结果：返回 `valid:false` 与 `errcode`/`errmsg`，不是工具错误；仅传输失败才是 error；
- 凭据仅用于该次请求，不落盘；
- `include_token:true` 时结果中才包含 token（缺省不回传）。

验活之后的步骤是**端点级探测**：`wxopen_endpoints` 列出可探测的官方端点（按 mini / oa / work 分组，含各端点的参数表），`wxopen_call {mode, access_key, secret_key, endpoint, params}` 按次携带凭据调用（凭据不落盘，`errcode`/`errmsg` 同样属正常结果）。由此将「凭据有效」落实为「可调通哪些接口」的证据。

报告中应将「静态发现」「验活通过」「验活失败」三种状态分列。

## 6. 输出约定

- 每项发现包含：文件与行号、证据摘录（引用敏感值时保留脱敏形态）、影响说明、建议。
- 区分事实（扫描与检索命中）与推断（例如「疑似生产密钥」）。
- 对验活通过的凭据，标注风险等级并建议立即轮换。
