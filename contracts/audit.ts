// Contracts published to the frontend: the stable audit surface (资产清单与
// 单条流量的 curl/重放/HAR 导出) the Vue app depends on. Matches
// desktop/internal/api/ipc/audit.go and the design spec.
//
// IPC methods (desktop/ipc_audit.go):
//   assets.scan / assets.list / assets.export
//   traffic.curl / traffic.replay / traffic.exportHar
// Wails events:
//   "assets_progress" — {status, current, total, message?},
//     status 取 working | done | error（照 export_progress 模板）。

// ---- 资产清单 ----

// 资产的观测来源：code（ref = 相对文件路径或 "cloud"）、traffic（ref = 记录 ID）
// 或 more（ref = "+N"，被来源上限收敛掉的数量）。
export type AssetSource = {
  type: 'code' | 'traffic' | 'more' | string;
  ref: string;
};

// 一个去重后的端点。同一端点跨次构建 ID 不变（fnv-1a64 over kind|method|scheme|host|path）。
export type AssetItem = {
  id: string;
  kind: 'api' | 'static' | 'ws' | 'cloud' | string;
  url: string; // 展示形：去 fragment、host 小写、query 只留键名
  host: string;
  path: string;
  method: string; // "*" = 仅来自代码/云函数，任意方法皆可
  sources: AssetSource[];
  hits: number;
  firstSeen: string; // RFC3339
  lastSeen: string; // RFC3339
  tags?: string[];
  trafficSeen: boolean; // 抓包流量里出现过（hits 算代码出现次数，判断不了这个）
};

// host 汇总的一行：多少个资产落在同一台主机上。
export type AssetHostStat = {
  host: string;
  count: number;
};

// assets.list 的一页：hosts 汇总随筛选联动（按 count 降序），未构建过时
// total=0、items/hosts 为空——不是错误。appid/builtAt 标识清单归属的小程序
// 与构建时间（启动时从 <dataDir>/assets/<appid>.json 恢复的存档保留原时间）。
export type AssetsPage = {
  ok: boolean;
  total: number;
  hosts: AssetHostStat[];
  items: AssetItem[];
  appid?: string; // 清单目标小程序；「未指定」构建过则为空
  builtAt?: string; // RFC3339；从未构建则为空
};

// assets.scan 的受理：ok:false + error:"没有可用来源" 表示三类来源全空。
// 请求参数：dir（反编译目录）、appid（流量收窄到单个小程序，建议传）、
// includeTraffic（默认 true）、cloudFns（云函数名列表）。
export type AssetsScanAccepted = {
  ok: boolean;
  async?: boolean;
  taskId?: string;
  error?: string;
};

// ---- 单条流量的 curl / 重放 / HAR ----

// traffic.curl：bash 是 *nix 形式（单引号转义），cmd 是 Windows cmd 形式
// （双引号转义）；-k 与重放客户端「不校验 TLS」的立场一致。
export type CurlResult = {
  ok: boolean;
  bash?: string;
  cmd?: string;
  error?: string; // ok:false 时的原因（如「记录不存在」）
};

// traffic.replay：重放本身跑起来 ok 就是 true——被测后端的拒绝是测试结果，
// 不是故障；传输层失败进 error 字段。body 截到 4KB，truncated 如实标注。
export type ReplayResult = {
  ok: boolean;
  status?: number; // HTTP 状态码；0 = 没拿到响应
  elapsedMs?: number;
  body?: string; // ≤4KB，已按 UTF-8 清洗
  truncated?: boolean;
  error?: string; // 传输层失败原因（超时、连接拒绝…）
};

// assets.export(save) / traffic.exportHar 的对话框落盘结果：
// 用户取消是 answer 不是 error（ok:false + reason）。
export type AuditExportResult = {
  ok: boolean;
  path?: string;
  reason?: string; // 「用户取消」
  error?: string;
};

// assets.export 不落盘时的回传内容（format = nuclei|httpx|json|txt|csv）。
export type AssetsExportContent = {
  ok: boolean;
  content?: string;
  error?: string;
};
