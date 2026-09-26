// Contracts published to the frontend: the stable traffic model the Vue app
// depends on. Matches desktop/internal/traffic/model.go and the design spec.
export type TrafficStatus = 'pending' | 'success' | 'fail';

export type TrafficSummary = {
  id: string;
  seq: number;
  capturedAt: string; // RFC3339Nano
  apiType: string;
  appId: string; // 产出该记录的小程序；捕获里没有 appid 时是空串
  name: string;
  method?: string;
  url?: string;
  status: TrafficStatus;
  requestBytes: number;
  responseBytes: number;
  durationMs: number;
};

// 一页摘要，外加它来自哪个窗口。page / pageSize 回显后端钳制后的**生效值**，total 是同一套
// 筛选条件下的总条数 —— 面板的「共 N 条」与「第 X / Y 页」都出自这里，调用方不必猜自己在不在
// 最后一页。列表按 (captured_at, seq, id) 倒序，第 1 页就是最新的一页。
export type TrafficPage = {
  items: TrafficSummary[];
  total: number;
  page: number;
  pageSize: number;
};

export type TrafficListParams = {
  page?: number; // 1 基；小于 1 按第 1 页处理
  pageSize?: number; // default 100, clamped to 1..1000
  query?: string;
  apiType?: string;
  status?: TrafficStatus;
  appId?: string; // 精确匹配（DB 的 appid 列）
};

export type TrafficDeleteParams = {
  ids: string[]; // 重复 id 只算一条；库里已经没有的 id 不是错误
};

export type TrafficDelete = {
  deleted: number;
  // 行**已经删掉**，只是后续的空间回收（WAL checkpoint / VACUUM）没跑成。删除本身成功了，
  // 所以不能报成「删除失败」，但空间也没还回去 —— 两件事都得让用户看见。字段缺失表示没失败。
  reclamationFailed?: boolean;
};

// traffic.stats 的冻结形状：条数必给，其余字段表示「未知」时省略 —— 读不到不能报错。
export type TrafficStats = {
  records: number;
  oldestCapturedAt?: string; // RFC3339Nano；库空时省略
  newestCapturedAt?: string; // RFC3339Nano；库空时省略
  bytes?: number; // page_count × page_size，含索引与空闲页
  droppedRecords?: number; // R12：捕获链路过载丢弃的记录数（shell 待投递缓冲溢出 + 两个行钩子页面侧缓冲溢出；后端已求和）
  droppedUpdates?: number; // R12：页面侧更新缓冲溢出的落定帧数（shell 不为更新帧单独缓冲）
};

// traffic.clear 的返回：清空一次删掉全部记录，所以没有参数可传（上限已随保留策略一起删除），
// 载荷与 traffic.delete 同一个 Go 类型（traffic.DeleteResult）。
export type TrafficClear = {
  deleted: number;
  // The rows were deleted, but the follow-up space reclamation (WAL checkpoint /
  // VACUUM) did not complete. Omitted on success, so a caller that never sees it
  // knows the clear finished in full.
  reclamationFailed?: boolean;
};

export type TrafficGetBodyParams = {
  id: string;
  part: 'request' | 'response';
};

// Wails binding surface (bound from desktop/app.go):
//   TrafficList(params: TrafficListParams): Promise<TrafficPage>
//   TrafficGetBody(params: TrafficGetBodyParams): Promise<number[]>
// Wails events:
//   "traffic:available" — the history changed; refresh the current page. Sent as
//                         {inserted: n} when new records were ingested and as
//                         {updated: n} when settled updates rewrote stored rows
//                         (a slow call's status/body/duration lands after its
//                         record was stored), so a page that only listens for
//                         insertions would leave those rows looking pending.
//   "engine:status"     — {frida, miniapp, devtools} changed
//
// IPC methods (desktop/ipc_bridge.go):
//   traffic.stats (no params)           → TrafficStats
//   traffic.clear (no params)            → TrafficClear
//   traffic.delete (TrafficDeleteParams) → TrafficDelete
//
// traffic.clear 与 traffic.delete 都是删库：清空一次删掉全部记录，删除按勾选的 id 删。
// 两者都刻意不进 MCP，删多少由人在环里确认
// （见 docs/MODULE_MAP.md 的「agent 面刻意排除的能力」）。
