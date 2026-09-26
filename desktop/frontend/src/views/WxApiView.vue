<script lang="ts">
// 无 rid 记录的稳定列表键：这类记录不会被原地更新（upsert / patch 只按 rid 匹配，落定与更新
// 都是换一个新对象），对象身份从进列表到被裁剪保持不变，WeakMap 以对象为键是安全的。达到
// 2000 上限裁最旧记录时，用 index 作键会让其余行的键全体移位、触发大面积行重建；localKeyOf
// 给每个无 rid 对象分配一个不再变化的序号（带 local- 前缀避免与 rid 撞），存活行的 DOM 节点
// 得以保留。
let localRecordSeq = 0;
const localRecordKeys = new WeakMap<object, number>();

function localKeyOf(item: object): string {
  let seq = localRecordKeys.get(item);
  if (seq === undefined) {
    seq = localRecordSeq += 1;
    localRecordKeys.set(item, seq);
  }
  return `local-${seq}`;
}
</script>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, shallowRef, watch } from 'vue';
import { backend } from '../api/bridge';
import ConfirmDialog from '../components/ConfirmDialog.vue';
import EmptyState from '../components/EmptyState.vue';
import ErrorState from '../components/ErrorState.vue';
import DensityToggle from '../components/DensityToggle.vue';
import PageHeader from '../components/PageHeader.vue';
import SplitPane from '../components/SplitPane.vue';
import StateToggle from '../components/StateToggle.vue';
import { createBatchQueue } from '../utils/batch';
import { useDensity } from '../utils/density';
import { boundedPrettyJson, messageOf, prettyJson } from '../utils/format';
import { copyText, notify } from '../utils/notify';
import { useEngineStore } from '../stores/engine';

type WxApiItem = {
  rid?: string;
  type?: string;
  name?: string;
  data?: object;
  status?: string;
  timestamp?: string;
  result?: unknown;
  error?: string;
  durationMs?: number;
};

// 同一条记录落定（success / fail）后由 wxapi_update 补发的帧；用来把 pending 行
// 原地刷成终态，而不是再插一行。
type WxApiUpdate = {
  rid?: string;
  status?: string;
  result?: unknown;
  error?: string;
  durationMs?: number;
};

type WxApiStats = {
  running?: boolean;
  pending?: number;
  dropped?: number;
  ack?: number;
  updateAck?: number;
  // 页面侧两条缓冲各自累计丢弃（R12）。与 dropped 互补不重叠：dropped 是 shell 的 pending
  // 溢出（那些记录页面还留着，下一轮还能取回），这两个是页面缓冲溢出、谁也补不回来。
  pageDroppedRecords?: number;
  pageDroppedUpdates?: number;
  // 存储初始化失败时后端明确报 false：实时列表照常收记录，但不会写入历史库。缺省
  // （旧后端没有这个字段）一律按可用处理，前端只把明确的 false 当不可用。
  storageAvailable?: boolean;
};

// 两个读数含义不同，不能合成一个「已丢弃」（见 WxApiStats 的注释）：
//   永久丢失 —— 页面缓冲溢出：记录在被任何人读到之前就被淘汰，数据库里也没有。
//   未进列表 —— shell 待投递缓冲溢出：记录已作为 capture 事件投给过界面、也已入库，
//              只是 poll 路径再也取不回（ack 已越过它们），历史记录里查得到。
function droppedLost(stats: WxApiStats | undefined): number {
  const count = (value: unknown) => (typeof value === 'number' && Number.isFinite(value) ? value : 0);
  return count(stats?.pageDroppedRecords) + count(stats?.pageDroppedUpdates);
}

function droppedUnlisted(stats: WxApiStats | undefined): number {
  return typeof stats?.dropped === 'number' && Number.isFinite(stats.dropped) ? stats.dropped : 0;
}

type DetailTab = 'request' | 'response' | 'replay';

// 记录投递与更新投递共用一条队列：只有同队 FIFO 才能保证「先见记录、后见落定」，
// 否则更新会赶在记录进入列表之前到达而被丢弃。
type CaptureEntry = { item: WxApiItem } | { update: WxApiUpdate };

const CATEGORIES = [
  { key: 'native', label: '原生桥接' },
  { key: 'request', label: 'wx.request' },
  { key: 'cache', label: '缓存' },
] as const;

const DETAIL_TABS: Array<{ key: DetailTab; label: string }> = [
  { key: 'request', label: '请求参数' },
  { key: 'response', label: '返回结果' },
  { key: 'replay', label: '重放结果' },
];

function categoryOf(type?: string): 'native' | 'request' | 'cache' {
  if (type === 'wx.request' || type === 'request') return 'request';
  // 页面钩子把缓存类 API 记成 wx.storage；旧记录里出现过 cache，两者都归「缓存」。
  if (type === 'wx.storage' || type === 'cache') return 'cache';
  return 'native';
}

function ridOf(item: WxApiItem | undefined): string {
  return typeof item?.rid === 'string' ? item.rid : '';
}

function statusTone(status?: string): string {
  if (status === 'success') return 'success';
  if (status === 'fail' || status === 'error') return 'fail';
  return 'pending';
}

function statusLabel(status?: string): string {
  if (status === 'success') return '成功';
  if (status === 'fail' || status === 'error') return '失败';
  if (status === 'complete') return '完成';
  return '等待中';
}

// 终态：除 pending（含未给状态）之外都是已经落定的行。
function isSettled(status?: string): boolean {
  return !!status && status !== 'pending';
}

function durationLabel(item: WxApiItem): string {
  const duration = item.durationMs;
  // 0 表示「没测到耗时」，不是「耗时为零」：有值才显示。
  if (typeof duration !== 'number' || !Number.isFinite(duration) || duration <= 0) return '';
  return duration >= 1000 ? `${(duration / 1000).toFixed(1)}s` : `${Math.round(duration)}ms`;
}

const store = useEngineStore();
const items = shallowRef<WxApiItem[]>([]);
const ITEM_LIMIT = 2000;
// 单帧最多应用的条数：一次事件送来的超大批次按帧摊开，任何单帧的 DOM 工作量都有界。
const FRAME_APPLY_LIMIT = 400;
// 批量队列的 pending 上限。推导：单 tick 最多两条投递路径叠加 —— 事件批 2000
// （Go 侧已有 maxRecordRounds = 10 × feedDrainLimit 200）+ poll 最多 10 轮 × 500 = 5000，
// 合计 7000 < 8192。取 ITEM_LIMIT（2000）会在入队阶段就丢掉最旧的一批，而那些记录已被
// Go 从 pending 取走，丢了补不回来；ITEM_LIMIT 的裁剪是应用阶段的事，两者不是一回事。
const QUEUE_PENDING_LIMIT = 8192;
// 每轮 poll 取多少条；取满说明后端还有积压，同一个 tick 内继续取（最多 10 轮）。
const POLL_PAGE_LIMIT = 500;
const POLL_MAX_ROUNDS = 10;
// 孤儿更新帧的容量上限，超出丢最旧。
const ORPHAN_UPDATE_LIMIT = 500;
const selected = shallowRef<WxApiItem>();
const search = ref('');
const listTrimmed = ref(false);
const categories = ref<string[]>([]);
const followTail = ref(false);
const { compact } = useDensity('wxapi');
const replay = shallowRef<{ owner: WxApiItem; rid: string; raw: unknown }>();
const replaying = ref(false);
const detailTab = ref<DetailTab>('request');
const confirmingClear = ref(false);
// 动作类错误（停止、清空、重放）就地提示；启动捕获失败要用户改条件，用 ErrorState。
const error = ref('');
// 轮询失败单独一个 ref：否则每 2s 一次的成功轮询会把动作类错误抹掉。
const pollError = ref('');
const captureError = ref('');
const statsDropped = ref(0);
const statsUnlisted = ref(0);
// 存储不可用警示：只认 stats 里明确的 false，缺省（旧后端）按可用处理。
const storageAvailable = ref(true);
const listEl = ref<HTMLUListElement | null>(null);
const tabListEl = ref<HTMLElement | null>(null);
const running = computed({
  get: () => store.wxapiCapturing,
  set: (value: boolean) => { store.wxapiCapturing = value; },
});
const starting = ref(false);
let disposed = false;
let pollInFlight = false;
// 清空代次：每次 poll 在发起时记下当时的代次，响应落地时对不上就说明这一页属于「清空之前
// 的世界」，整批丢弃。否则用户点清空时恰有一个 poll 在飞，它的响应会把已清的记录加回来。
let pollEpoch = 0;
// 轮次标记：stopPolling 会同时作废上一轮（在飞的响应落地时不能把新一轮的在飞标记抹掉，
// 否则同一个 tick 里会出现两条并行的轮询）。清空正是「停一轮、再起一轮」的场景。
let pollRun = 0;
let pollTimer: ReturnType<typeof setInterval> | undefined;
let stopCaptureEvents = () => {};
let stopUpdateEvents = () => {};

// rid -> 行下标。事件流与 poll 是同一批记录的两条投递路径，按 rid upsert 才不会
// 把同一次调用渲染成两行。
const ridIndex = new Map<string, number>();

// rid -> 更新帧。更新可能赶在承载它的记录之前到达（Go 取走 pending 后无法再补丁
// 在途载荷），此时行还不存在：先暂存，等记录进列表时立刻补打，否则该行会永久停
// 在「等待中」。按插入顺序丢最旧，容量有界。
const orphanUpdates = new Map<string, WxApiUpdate>();

const textMatched = computed(() => {
  const needle = search.value.trim().toLowerCase();
  if (!needle) return items.value;
  return items.value.filter((item) => `${item.type ?? ''} ${item.name ?? ''}`.toLowerCase().includes(needle));
});

const categoryCounts = computed(() => {
  const counts: Record<string, number> = { native: 0, request: 0, cache: 0 };
  for (const item of textMatched.value) counts[categoryOf(item.type)] += 1;
  return counts;
});

const filteredItems = computed(() => (
  categories.value.length
    ? textMatched.value.filter((item) => categories.value.includes(categoryOf(item.type)))
    : textMatched.value
));

// 重放结果只在它所属的那条记录被选中时展示：换一条记录不能看到上一条的结果。
const visibleReplay = computed(() => {
  const result = replay.value;
  const current = selected.value;
  if (!result || !current) return undefined;
  // 记录落定会原地替换行对象，所以有 rid 的按身份比，只有无 rid 的才退回对象引用。
  return result.rid ? (ridOf(current) === result.rid ? result : undefined) : (result.owner === current ? result : undefined);
});

// 详情正文必须是 computed 而不是模板内联调用：捕获中列表定期刷新，组件每次重渲染都会对大
// 对象重新 stringify（几 MB 的返回体会卡顿数秒）；computed 只在换选中 / 行落定时重算一次。
// 展示超过 DETAIL_TEXT_LIMIT 只留前缀并明说截断（与历史页同口径），复制仍走全量 prettyJson。
const requestView = computed(() => boundedPrettyJson(selected.value?.data));
const responseView = computed(() => boundedPrettyJson(selected.value?.result ?? selected.value?.error));
// 重放结果存原始返回值，展示时才有界 stringify：同一套截断口径。
const replayView = computed(() => {
  const result = visibleReplay.value;
  return result ? boundedPrettyJson(result.raw) : undefined;
});

function reindex(list: WxApiItem[]) {
  ridIndex.clear();
  for (let index = 0; index < list.length; index += 1) {
    const rid = ridOf(list[index]);
    if (rid) ridIndex.set(rid, index);
  }
}

function upsert(list: WxApiItem[], record: WxApiItem) {
  const rid = ridOf(record);
  const at = rid ? ridIndex.get(rid) : undefined;
  if (at !== undefined) {
    const previous = list[at];
    // 已落定的行不能被在途 poll 里的旧 pending 版本覆盖：记录被 poll 取走后才落定，
    // 那一帧的落定不会再发第二次，覆盖后这一行会永久停在「等待中」。
    if (!(record.status === 'pending' && isSettled(previous.status))) {
      list[at] = record;
      if (selected.value === previous) selected.value = record;
    }
  } else {
    list.push(record);
    if (rid) ridIndex.set(rid, list.length - 1);
  }
  if (!rid) return;
  // 这一行的落定帧可能早就到了（当时还没有行）：行一落地就补打，别让它停在等待中。
  const orphan = orphanUpdates.get(rid);
  if (!orphan) return;
  orphanUpdates.delete(rid);
  patch(list, orphan);
}

function rememberOrphan(rid: string, update: WxApiUpdate) {
  // 同一个 rid 只留最新一帧（后端每 tick 每记录只发一次落定帧）。
  orphanUpdates.set(rid, update);
  while (orphanUpdates.size > ORPHAN_UPDATE_LIMIT) {
    const oldest = orphanUpdates.keys().next().value;
    if (oldest === undefined) break;
    orphanUpdates.delete(oldest);
  }
}

function patch(list: WxApiItem[], update: WxApiUpdate) {
  const rid = typeof update.rid === 'string' ? update.rid : '';
  if (!rid) return;
  const at = ridIndex.get(rid);
  if (at === undefined) {
    // 记录还没到（或已被清空）：暂存这一帧，等记录落地时补打。
    rememberOrphan(rid, update);
    return;
  }
  const current = list[at];
  if (!current) return;
  const next: WxApiItem = { ...current, status: update.status ?? current.status };
  if (update.result !== undefined) next.result = update.result;
  if (update.error !== undefined) next.error = update.error;
  if (typeof update.durationMs === 'number') next.durationMs = update.durationMs;
  list[at] = next;
  if (selected.value === current) selected.value = next;
}

const itemQueue = createBatchQueue<CaptureEntry>((entries) => {
  const next = items.value.slice();
  for (const entry of entries) {
    if ('update' in entry) patch(next, entry.update);
    else upsert(next, entry.item);
  }
  if (next.length > ITEM_LIMIT) {
    next.splice(0, next.length - ITEM_LIMIT);
    // 静默丢弃会让用户误以为记录完整，这里明确提示
    listTrimmed.value = true;
  }
  reindex(next);
  items.value = next;
}, {
  pendingLimit: QUEUE_PENDING_LIMIT,
  applyLimit: FRAME_APPLY_LIMIT,
  // 单波超过队列上限被丢掉时也要提示（与 ITEM_LIMIT 的提示共用一条）。
  onDrop: () => { listTrimmed.value = true; },
});

function addCapture(payload: unknown) {
  const records = Array.isArray(payload) ? payload : [payload];
  itemQueue.pushMany(
    records
      .filter((record) => record && typeof record === 'object')
      .map((record) => ({ item: record as WxApiItem })),
  );
}

function applyUpdate(payload: unknown) {
  // 后端按批投递更新帧（一次 tick 一个数组），单个对象的旧形状继续兼容。
  const frames = Array.isArray(payload) ? payload : [payload];
  for (const frame of frames) {
    if (frame && typeof frame === 'object') itemQueue.push({ update: frame as WxApiUpdate });
  }
}

// 按 rid 找行：裁剪之后下标会指向别的记录，只有 rid 才是稳定的身份。
function rowForRid(rid: string): HTMLElement | null {
  const rows = listEl.value?.querySelectorAll<HTMLElement>('[data-rid]');
  for (const row of rows ?? []) {
    if (row.dataset.rid === rid) return row;
  }
  return null;
}

function selectByOffset(offset: number) {
  const list = filteredItems.value;
  if (!list.length) return;
  const current = selected.value ? list.indexOf(selected.value) : -1;
  const index = current === -1
    ? (offset > 0 ? 0 : list.length - 1)
    : Math.min(Math.max(current + offset, 0), list.length - 1);
  const target = list[index];
  selected.value = target;
  const rid = ridOf(target);
  void nextTick(() => {
    const row = rid
      ? rowForRid(rid)
      // 没有 rid 的记录只有下标可依据（这类记录不会被更新帧原地替换）。
      : document.querySelector<HTMLElement>(`[data-testid="wxapi-record-${index}"]`);
    row?.scrollIntoView?.({ block: 'nearest' });
  });
}

function moveTab(step: number) {
  const at = DETAIL_TABS.findIndex((tab) => tab.key === detailTab.value);
  const next = DETAIL_TABS[(at + step + DETAIL_TABS.length) % DETAIL_TABS.length];
  if (!next || next.key === detailTab.value) return;
  detailTab.value = next.key;
  void nextTick(() => {
    tabListEl.value?.querySelector<HTMLElement>(`[data-testid="detail-tab-${next.key}"]`)?.focus();
  });
}

function toggleCategory(key: string) {
  categories.value = categories.value.includes(key)
    ? categories.value.filter((item) => item !== key)
    : categories.value.concat(key);
}

function stopPolling() {
  if (pollTimer) clearInterval(pollTimer);
  pollTimer = undefined;
  pollRun += 1;
  pollInFlight = false;
}

async function refreshStats() {
  try {
    const stats = await backend.call<WxApiStats>('wxapi.stats');
    if (disposed) return;
    statsDropped.value = droppedLost(stats);
    statsUnlisted.value = droppedUnlisted(stats);
    storageAvailable.value = stats?.storageAvailable !== false;
    // 后端已不在捕获态（引擎掉线或被外部停掉）而界面还停在「捕获中」：复位并停表，
    // 否则用户以为在抓，实际一条也不会来。
    if (stats?.running === false && running.value) running.value = false;
  } catch {
    // 丢弃计数只是展示用，读不到不该打断捕获链路（轮询自身的失败另有提示）。
  }
}

async function pollOnce() {
  if (disposed || pollInFlight) return;
  pollInFlight = true;
  const run = pollRun;
  try {
    // 一页取满说明后端还有积压：同一个 tick 内继续取，把「补齐」摊成多次小渲染，
    // 而不是一次巨渲染（上限 10 轮，避免长时间占用主线程）。
    for (let round = 0; round < POLL_MAX_ROUNDS; round += 1) {
      // 发起这一轮时的代次：响应落地时若已经变了（用户在这一页在飞时点了清空），整页都是
      // 清空之前的记录，整批丢弃 —— 既不 addCapture，也不 refreshStats。
      const epoch = pollEpoch;
      const page = await backend.call<WxApiItem[]>('wxapi.poll', { limit: POLL_PAGE_LIMIT });
      if (disposed || epoch !== pollEpoch) return;
      const records = Array.isArray(page) ? page : [];
      if (records.length) addCapture(records);
      if (records.length < POLL_PAGE_LIMIT) break;
    }
    await refreshStats();
    if (!disposed) pollError.value = '';
  } catch (reason) {
    if (disposed) return;
    // 只有轮询自己的错误：动作类错误（停止 / 清空 / 重放失败）不能被轮询抹掉。
    pollError.value = messageOf(reason);
    // 引擎已经掉线时，继续每 2s 重刷同一条错误没有意义：捕获状态归零即停表。
    if (!store.status.frida) running.value = false;
  } finally {
    // 只有本轮的响应才能释放这一轮的在飞标记：清空期间 stopPolling 之后起的那些轮次
    // 不能被上一轮迟到的响应解锁。
    if (run === pollRun) pollInFlight = false;
  }
}

function startPolling() {
  stopPolling();
  if (disposed) return;
  pollTimer = setInterval(() => { void pollOnce(); }, 2000);
}

async function start() {
  if (running.value || starting.value) return;
  starting.value = true;
  error.value = '';
  pollError.value = '';
  captureError.value = '';
  try {
    await backend.call('wxapi.start');
    if (disposed) return;
    const stats = await backend.call<WxApiStats>('wxapi.stats');
    if (disposed) return;
    if (stats?.running === false) {
      // 后端没进入捕获态：界面不能停在「捕获中」，否则用户以为在抓、其实一条也不会来
      throw new Error('后端未进入捕获状态，请确认小程序已连接后重试');
    }
    if (typeof stats?.dropped === 'number' || stats?.pageDroppedRecords !== undefined) {
      statsDropped.value = droppedLost(stats);
      statsUnlisted.value = droppedUnlisted(stats);
    }
    // 存储可用性在这条成功路径上也要刷一次：用户开启捕获时就能看到写库是否可用。
    storageAvailable.value = stats?.storageAvailable !== false;
    running.value = true;
    notify('WxAPI 捕获已开启', 'success');
  } catch (reason) {
    if (disposed) return;
    running.value = false;
    captureError.value = messageOf(reason);
  } finally {
    if (!disposed) starting.value = false;
  }
}

async function stop() {
  error.value = '';
  pollError.value = '';
  captureError.value = '';
  try {
    await backend.call('wxapi.stop');
    running.value = false;
    notify('WxAPI 捕获已停止');
  } catch (reason) {
    error.value = messageOf(reason);
  }
}

function clear() {
  if (items.value.length) {
    confirmingClear.value = true;
    return;
  }
  void performClear();
}

async function performClear() {
  confirmingClear.value = false;
  error.value = '';
  // 清空期间不起新的 poll：那一轮会带着旧代次发起、却取到清空之后写入的记录，响应落地时
  // 被 epoch 守卫整批丢掉，而那批记录已被 shell 从 pending 取走 —— 永久消失。停在飞的那
  // 一页由 epoch 作废就够了，清空回来后照旧起表。
  const resuming = pollTimer !== undefined;
  if (resuming) stopPolling();
  try {
    await backend.call('wxapi.clear');
    // 提升代次：清空之前发起的 poll 拿回的是旧世界的记录，落地时会被 pollOnce 里那道守卫
    // 整批丢掉，不会把已清的行加回面板。放在 itemQueue.clear() 之前，两者之间没有缝隙。
    pollEpoch += 1;
    itemQueue.clear();
    items.value = [];
    ridIndex.clear();
    orphanUpdates.clear();
    selected.value = undefined;
    replay.value = undefined;
    notify('WxAPI 记录已清空');
    listTrimmed.value = false;
    // 后端 dropped 随 clear 归零，本地的丢弃计数也要跟着归零。
    statsDropped.value = 0;
    statsUnlisted.value = 0;
  } catch (reason) {
    error.value = messageOf(reason);
  } finally {
    if (resuming && running.value) startPolling();
  }
}

async function copyDetail() {
  if (!selected.value) return;
  await copyText(prettyJson(selected.value), '调用详情');
}

async function replaySelected() {
  const target = selected.value;
  if (!target || replaying.value) return;
  error.value = '';
  replay.value = undefined;
  replaying.value = true;
  const apiName = target.type === 'wx.request' || target.type === 'request'
    ? 'request'
    : target.name ?? '';
  try {
    // 存原始返回值，详情展示时才 boundedPrettyJson（复制与重放本身都不进 DOM，无界成本只在
    // 用户主动触发时付一次）。
    const raw = await backend.call('wxapi.replay', { api_name: apiName, options: target.data ?? {} });
    if (disposed) return;
    replay.value = { owner: target, rid: ridOf(target), raw };
    detailTab.value = 'replay';
  } catch (reason) {
    if (!disposed) error.value = messageOf(reason);
  } finally {
    if (!disposed) replaying.value = false;
  }
}

// 捕获开关由 store 持有：它翻转时同步启停轮询，引擎掉线把 wxapiCapturing 置回
// false 也就自动停表，不会再每 2s 刷一条错误。
watch(() => store.wxapiCapturing, (capturing) => {
  if (capturing) startPolling();
  else stopPolling();
}, { immediate: true });

// 筛选条件变化后，选中项可能已经不在结果里：留着会让详情显示一条「看不见」的记录。
watch(filteredItems, (list) => {
  if (selected.value && !list.includes(selected.value)) selected.value = undefined;
});

// 跟随最新：新行到达后滚到底部（原地更新不改变长度，因此不会打断正在看的那一行）。
watch(() => items.value.length, async (length, previous) => {
  if (!followTail.value || length <= previous) return;
  await nextTick();
  const list = listEl.value;
  if (list) list.scrollTop = list.scrollHeight;
});

onMounted(() => {
  stopCaptureEvents = backend.on('wxapi_capture', addCapture);
  stopUpdateEvents = backend.on('wxapi_update', applyUpdate);
});

onBeforeUnmount(() => {
  disposed = true;
  itemQueue.dispose();
  stopPolling();
  stopCaptureEvents();
  stopUpdateEvents();
});
</script>

<template>
  <section class="wxapi-view page-workbench" aria-labelledby="wxapi-title">
    <!-- 捕获操作并入内容区：页头只留无障碍标题，不再占一整行工具条 -->
    <PageHeader title="WxAPI" title-id="wxapi-title" />

    <div class="panel">
      <header class="panel-header">
        <div class="status-cluster">
          <span data-testid="wxapi-capture-status" class="status-pill" :class="running ? 'ok' : 'off'">{{ running ? '捕获中' : '未捕获' }}</span>
          <span class="status-pill" :class="store.status.miniapp ? 'ok' : 'off'">{{ store.status.miniapp ? '小程序已连接' : '小程序未连接' }}</span>
          <span data-testid="wxapi-count" class="status-pill off subnav-count">{{ filteredItems.length }} / {{ items.length }}</span>
          <span v-if="statsDropped > 0" data-testid="wxapi-dropped" class="status-pill warn" title="页面缓冲溢出：记录在被读到之前就被淘汰，历史记录里也查不到">已丢弃 {{ statsDropped }} 条</span>
          <span v-if="statsUnlisted > 0" data-testid="wxapi-unlisted" class="status-pill warn" title="shell 待投递缓冲溢出：未进入实时列表，但仍可在历史记录里查到">未进列表 {{ statsUnlisted }} 条</span>
          <span v-if="!storageAvailable" data-testid="wxapi-storage-warning" class="status-pill warn" title="存储初始化失败：实时列表照常显示，但记录不会写入历史记录页">存储不可用</span>
        </div>
        <div class="toolbar panel-actions">
          <StateToggle
            test-id="wxapi-toggle"
            :active="running"
            :pending="starting ? 'start' : ''"
            start-label="开启捕获"
            stop-label="停止捕获"
            @start="start"
            @stop="stop"
          />
          <button data-testid="clear-wxapi" class="danger" type="button" @click="clear">清空</button>
        </div>
      </header>

      <header class="panel-header">
        <div class="chip-row" role="group" aria-label="按类型筛选">
          <button data-testid="category-all" class="chip" :class="{ active: !categories.length }" :aria-pressed="!categories.length" type="button" @click="categories = []">全部 <span class="chip-count">{{ textMatched.length }}</span></button>
          <button v-for="category in CATEGORIES" :key="category.key" :data-testid="`category-${category.key}`" class="chip" :class="{ active: categories.includes(category.key) }" :aria-pressed="categories.includes(category.key)" type="button" @click="toggleCategory(category.key)">
            {{ category.label }} <span class="chip-count">{{ categoryCounts[category.key] }}</span>
          </button>
        </div>
        <div class="toolbar filter-tools">
          <input v-model="search" data-testid="wxapi-search" class="record-filter" placeholder="搜索 API" aria-label="搜索 API">
          <button v-if="search" class="ghost small" type="button" @click="search = ''">清除</button>
          <label class="check-inline"><input v-model="followTail" data-testid="follow-tail" type="checkbox">跟随最新</label>
          <DensityToggle v-model="compact" test-id="wxapi-density" />
          <span v-if="filteredItems.length > 1" class="status-line key-hint">↑↓ 切换记录</span>
        </div>
      </header>

      <ErrorState v-if="captureError" class="panel-notice" :message="captureError" retryable @retry="start" />
      <p v-if="error" data-testid="wxapi-action-error" class="error error-line" role="alert">{{ error }}</p>
      <p v-if="pollError" data-testid="wxapi-poll-error" class="error error-line" role="alert">{{ pollError }}</p>
      <p v-if="listTrimmed" class="callout warning limit-callout panel-notice" role="status">已达上限 {{ items.length }} 条，最早的记录已被丢弃；请及时查看或复制需要的记录。</p>

      <SplitPane class="wxapi-split">
        <template #main>
          <ul ref="listEl" class="record-list" :class="{ 'is-compact': compact }" aria-label="WxAPI 捕获记录">
            <!-- 稳定键：达到上限后裁剪最旧记录时，只有被删的与新增的行需要动 DOM
                 （无 rid 记录用对象身份换来的稳定序号，见 localKeyOf 的注释） -->
            <li v-for="(item, index) in filteredItems" :key="item.rid ?? localKeyOf(item)">
              <button :data-testid="`wxapi-record-${index}`" :data-rid="item.rid || undefined" type="button" :class="{ selected: selected === item }" @click="selected = item" @keydown.down.prevent="selectByOffset(1)" @keydown.up.prevent="selectByOffset(-1)">
                <span class="record-head">
                  <span class="record-badge">{{ item.type || '未知' }}</span>
                  <span class="record-title">{{ item.name || item.type || '未知调用' }}</span>
                  <span class="status-badge" :class="statusTone(item.status)">{{ statusLabel(item.status) }}</span>
                </span>
                <span class="record-meta">
                  <span>{{ item.timestamp ?? '' }}</span>
                  <span v-if="durationLabel(item)" class="record-duration">{{ durationLabel(item) }}</span>
                </span>
              </button>
            </li>
            <li v-if="!filteredItems.length">
              <EmptyState
                :title="items.length ? '没有匹配的记录' : '暂无记录'"
                :description="items.length ? '清空搜索或调整分类后重试。' : '开启捕获后，wx.* 调用会实时出现在这里。'"
              />
            </li>
          </ul>
        </template>

        <template #side>
          <aside data-testid="wxapi-detail" class="detail-pane">
            <template v-if="selected">
              <h2>{{ selected.name || '调用详情' }}</h2>
              <div ref="tabListEl" class="detail-tabs" role="tablist" aria-label="调用详情视图">
                <button
                  v-for="tab in DETAIL_TABS"
                  :id="`detail-tab-${tab.key}`"
                  :key="tab.key"
                  :data-testid="`detail-tab-${tab.key}`"
                  type="button"
                  role="tab"
                  :aria-selected="detailTab === tab.key"
                  :aria-controls="`detail-panel-${tab.key}`"
                  :tabindex="detailTab === tab.key ? 0 : -1"
                  :class="{ active: detailTab === tab.key }"
                  @click="detailTab = tab.key"
                  @keydown.right.prevent="moveTab(1)"
                  @keydown.left.prevent="moveTab(-1)"
                >{{ tab.label }}</button>
              </div>
              <!-- 只有正文滚：标签页与下面的重放/复制按钮固定在面板里（base.css 的 .detail-body） -->
              <div class="detail-body">
                <template v-if="detailTab === 'request'">
                  <!-- 截断了就必须说：用户看到的不是全文（与历史页正文截断同一句式） -->
                  <p v-if="requestView.truncated" data-testid="wxapi-detail-truncated" class="status-line">内容过大（共 {{ requestView.truncated?.total }} 个字符），已截断显示前 {{ requestView.truncated?.shown }} 个字符</p>
                  <pre id="detail-panel-request" data-testid="wxapi-request-view" role="tabpanel" aria-labelledby="detail-tab-request" tabindex="0">{{ requestView.text }}</pre>
                </template>
                <template v-else-if="detailTab === 'response'">
                  <p v-if="responseView.truncated" data-testid="wxapi-detail-truncated" class="status-line">内容过大（共 {{ responseView.truncated?.total }} 个字符），已截断显示前 {{ responseView.truncated?.shown }} 个字符</p>
                  <pre id="detail-panel-response" data-testid="wxapi-response-view" role="tabpanel" aria-labelledby="detail-tab-response" tabindex="0">{{ responseView.text }}</pre>
                </template>
                <div v-else id="detail-panel-replay" role="tabpanel" aria-labelledby="detail-tab-replay" tabindex="0">
                  <template v-if="replayView">
                    <p v-if="replayView.truncated" data-testid="wxapi-detail-truncated" class="status-line">内容过大（共 {{ replayView.truncated?.total }} 个字符），已截断显示前 {{ replayView.truncated?.shown }} 个字符</p>
                    <pre data-testid="wxapi-replay-result">{{ replayView.text }}</pre>
                  </template>
                  <EmptyState v-else title="暂无重放结果" description="点击「一键重放」后在此处查看结果。" />
                </div>
              </div>
              <div class="detail-actions">
                <button data-testid="replay-wxapi" type="button" :disabled="replaying" @click="replaySelected">{{ replaying ? '重放中…' : '一键重放' }}</button>
                <button class="secondary" type="button" @click="copyDetail">复制详情</button>
              </div>
            </template>
            <EmptyState v-else title="未选择记录" description="选择一条记录查看请求参数与返回结果。" />
          </aside>
        </template>
      </SplitPane>
    </div>

    <ConfirmDialog
      :open="confirmingClear"
      title="清空捕获记录"
      message="确认清空实时捕获列表？已写入历史记录的记录不受影响。"
      confirm-text="清空"
      tone="danger"
      @confirm="performClear"
      @cancel="confirmingClear = false"
    />
  </section>
</template>

<style scoped>
/* 第 1 行左侧的状态 pill 组 */
.status-cluster {
  align-items: center;
  display: flex;
  flex-wrap: wrap;
  gap: .45rem;
}

.panel-actions {
  margin-left: auto;
}

.filter-tools {
  margin-left: auto;
}

.record-filter {
  max-width: 14rem;
}

.chip-count {
  color: var(--muted);
  font-family: var(--mono);
  font-size: .72rem;
}

/* 面板内的原位提示（错误 / 上限 / 启动失败） */
.panel-notice,
.error-line {
  margin: .75rem 1.1rem 0;
}

.limit-callout {
  white-space: normal;
}

/* 列表行：类型徽标 + 名称 + 状态徽标，第二行是时间与耗时 */
/* 全局 .record-list button 的隐式轨道按内容收缩，整行会被挤到中间；这里让行铺满列表宽度，
   名称靠左、状态徽标贴右。 */
.record-list button {
  grid-template-columns: minmax(0, 1fr);
}

.record-head {
  align-items: center;
  display: flex;
  gap: .4rem;
}

.record-title {
  min-width: 0;
  overflow-wrap: anywhere;
}

.record-badge {
  background: var(--panel-2);
  border: 1px solid var(--border-strong);
  border-radius: 4px;
  color: var(--muted);
  flex: none;
  font-family: var(--mono);
  font-size: .68rem;
  padding: .02rem .32rem;
}

.status-badge {
  border-radius: 99px;
  flex: none;
  font-size: .68rem;
  font-weight: 650;
  margin-left: auto;
  padding: .04rem .4rem;
}

.status-badge.success {
  background: var(--success-soft);
  color: var(--success);
}

.status-badge.fail {
  background: var(--danger-soft);
  color: var(--danger);
}

.status-badge.pending {
  background: var(--panel-3);
  color: var(--muted);
}

.record-meta {
  align-items: center;
  display: flex;
  gap: .5rem;
}

.record-duration {
  color: var(--accent-strong);
}

/* 列表 / 详情分栏：面板本身是 flex 列，分栏要吃掉剩余高度 */
.wxapi-split {
  flex: 1 1 auto;
  gap: 0;
  min-height: 0;
}

.wxapi-split :deep(.split-pane-main),
.wxapi-split :deep(.split-pane-side) {
  display: flex;
  min-height: 0;
  min-width: 0;
}

.wxapi-split .record-list,
.wxapi-split .detail-pane {
  flex: 1 1 auto;
  max-height: none;
  min-height: 0;
}

.detail-tabs {
  border-bottom: 1px solid var(--border);
  display: flex;
  flex-wrap: wrap;
  gap: .2rem;
}

.detail-tabs button {
  background: transparent;
  border: 0;
  border-bottom: 2px solid transparent;
  border-radius: 0;
  color: var(--muted);
  font-size: .82rem;
  padding: .35rem .55rem;
}

.detail-tabs button:hover:not(:disabled) {
  background: var(--panel-2);
  color: var(--text);
}

.detail-tabs button.active {
  border-bottom-color: var(--accent);
  color: var(--accent-strong);
}

/* 小屏单列：操作与筛选各自占满一行，不会被挤到看不见；列表与详情各自的滚动交给面板，
   叠起来后不会有一半被裁在面板外。 */
@media (max-width: 900px) {
  .wxapi-view .panel {
    overflow-y: auto;
  }

  .wxapi-split {
    flex: 0 0 auto;
  }

  .wxapi-split .record-list {
    max-height: 22rem;
  }

  .panel-header {
    align-items: flex-start;
  }

  .panel-actions,
  .filter-tools {
    justify-content: flex-start;
    margin-left: 0;
    width: 100%;
  }

  .record-filter {
    max-width: none;
    width: 100%;
  }

  .panel-notice,
  .error-line {
    margin: .75rem .9rem 0;
  }
}
</style>
