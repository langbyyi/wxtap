<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, shallowRef, watch } from 'vue';
import { useRoute } from 'vue-router';
import { backend } from '../api/bridge';
import ConfirmDialog from '../components/ConfirmDialog.vue';
import DensityToggle from '../components/DensityToggle.vue';
import EmptyState from '../components/EmptyState.vue';
import ErrorState from '../components/ErrorState.vue';
import PageHeader from '../components/PageHeader.vue';
import SplitPane from '../components/SplitPane.vue';
import { useDensity } from '../utils/density';
import { messageOf, prettyJson } from '../utils/format';
import { notify } from '../utils/notify';

// 列表行只依赖 contracts/traffic.ts 的 TrafficSummary（永远不带正文）。
type TrafficSummary = {
  id: string;
  seq?: number;
  capturedAt?: string;
  apiType?: string;
  appId?: string;
  name?: string;
  method?: string;
  url?: string;
  status?: string;
  requestBytes?: number;
  responseBytes?: number;
  durationMs?: number;
};

// 一页摘要 + 它来自哪个窗口。total 是同一套筛选条件下的总条数：页码与「共 N 条」都由它算，
// page / pageSize 是后端钳制后的生效值（页面据此回显，不拿自己发出去的请求当真）。
type TrafficPage = { items?: TrafficSummary[]; total?: number; page?: number; pageSize?: number };
type BodyPart = 'request' | 'response';
// 正文展示态：text 为空 = 这一侧没有内容（IPC 的 null 与 0 字节正文对用户是同一件事）；
// truncated 只在正文超过展示上限时出现。capped 表示解码前就已经被限制（正文比解码护栏还大，
// 真实总长度无从得知），此时 total 只能当作下界来读。
type BodyText = { text: string; truncated?: { shown: number; total: number; capped?: boolean } };
// 一代筛选条件（全部下推到 traffic.list 的那几个）：游标只在这一代内有效。
type Conditions = { query: string; apiType: string; appId: string; status: string };

// traffic.stats 的冻结形状（roadmap 2.3）：条数与时间范围必给，其余字段都是可选的
// —— 体积与 R12 丢弃计数（2.4）读不到不能报错。
type TrafficStats = {
  records?: number;
  oldestCapturedAt?: string;
  newestCapturedAt?: string;
  bytes?: number;
  // R12 过载可观测（roadmap 2.4）：droppedRecords = shell 待投递缓冲溢出 + 两个钩子页面侧
  // 记录缓冲溢出；droppedUpdates = 两个页面侧更新缓冲溢出。两者互补不重叠（都是用户看不
  // 到的记录），来自后端一次求和的结果，前端不得再自行相加以免重复计数。
  droppedRecords?: number;
  droppedUpdates?: number;
};

// traffic.delete / traffic.clear 的返回：行删掉了、空间回收没跑成时才有 reclamationFailed
// （见 contracts/traffic.ts）。两件事都得让用户看见：删除本身成功了，不能报成失败，但空间
// 也没还回去。字段缺失（Go 侧 omitempty）或为假都表示没这个失败。
type DeleteResult = { deleted?: number; reclamationFailed?: boolean };

// traffic.exportHar 的返回：ok + path = 已落盘；ok:false + reason = 用户取消（是回答不是错误）。
type ExportHarResult = { ok: boolean; path?: string; reason?: string };

const PAGE_LIMIT = 100;
// 每页条数（后端把 pageSize 钳在 1..1000，见 contracts/traffic.ts）：一页最多几百行，
// 一次替换整列表、不再累积，所以不给更大的档位。
const PAGE_LIMITS = [50, 100, 200, 500] as const;
// traffic:available 的节流窗口：高频捕获下不能让每条记录都触发一次整页重取。
const AVAILABLE_THROTTLE_MS = 2000;
// 正文展示上限（字符数）：IPC 单侧最多 16MB（后端上限），整段 JSON.parse + pretty 再塞进 DOM
// 会把页面卡住好几秒，而且用户看不出发生了什么。超过上限只渲染前 N 个字符并明说被截断了。
const BODY_DISPLAY_LIMIT = 2 * 1024 * 1024;
// 解码护栏（base64 字符数）：真正的逐字符开销在解码本身（atob + Uint8Array 逐元素填 + TextDecoder），
// 展示上限拦不住它 —— 16MB 正文仍会全量解码一遍。解码前按 base64 长度预判，超了只取前缀（按 4 字符
// 对齐，避免切坏 base64 三元组）。它比展示上限大，所以正常正文的总长度仍然准确。
const BODY_DECODE_LIMIT = 4 * 1024 * 1024;
// 正文为空时的展示文案：不能再渲染成字面量 `""` —— 那看着像正文内容就是两个引号。
const EMPTY_BODY_LABEL = '（正文为空）';
const STATUS_OPTIONS = ['success', 'fail', 'pending'];
const BODY_TABS: Array<{ key: BodyPart; label: string; testId: string }> = [
  { key: 'request', label: '请求正文', testId: 'load-request-body' },
  { key: 'response', label: '响应正文', testId: 'load-response-body' },
];

const items = shallowRef<TrafficSummary[]>([]);
const page = ref(1);
// 当前筛选条件下的总条数（后端给）：页数、末页禁用与「共 N 条」都从它算。
const total = ref(0);
const pageSize = ref<number>(PAGE_LIMIT);

const { compact } = useDensity('traffic');
const route = useRoute();
const listEl = ref<HTMLElement | null>(null);
const selected = shallowRef<TrafficSummary>();
const keyword = ref('');
const apiTypeFilter = ref('');
const appIdFilter = ref('');
const statusFilter = ref('');
const loading = ref(false);
const listError = ref('');
const notice = ref('');

// 小程序下拉的选项：库里出现过的 appid + 记录数（traffic.appids）。后端会
// 把连接时记住的小程序名回填进 name（库里只有 appid）。加载失败只是少一个
// 快捷入口，AppID 手输框仍然可用。
type AppIDStat = { appid: string; name?: string; count: number; lastSeen?: string };
const appIdOptions = ref<AppIDStat[]>([]);

// 有名字显示「名字 · appid」，没调试过（拿不到名字）的退回裸 appid。
function appIdOptionLabel(stat: AppIDStat): string {
  const base = stat.name ? `${stat.name} · ${stat.appid}` : stat.appid;
  return `${base}（${stat.count}）`;
}

function pickAppId(event: Event) {
  appIdFilter.value = (event.target as HTMLSelectElement).value;
}

// 勾选集合：只覆盖**当前这一页**。换页 / 换筛选 / 刷新都会清空 —— 留着会让「删除选中」作用
// 于用户已经看不到的行，而确认框上只有一个数字，验不出删的是谁。
// ref 而不是 shallowRef：Set 的增删要触发重渲染，shallowRef 下 .add() 静默不更新。
const checkedIds = ref<Set<string>>(new Set());
// 待确认的删除：ids 在打开确认框的那一刻定下来（列表可能随后自动刷新，行位置会变，id 不会）。
const deleteRequest = shallowRef<{ ids: string[]; message: string }>();
const deleting = ref(false);
const deleteError = ref('');
// 跳到第几页的输入框（回车才走）。
const jumpInput = ref('');

const bodyPart = ref<BodyPart>('request');
// 当前选中记录的正文缓存，按 part 存：同一条记录的同一侧只读一次，切换记录即作废。
const bodies = ref<Partial<Record<BodyPart, BodyText>>>({});
// 每份缓存是在哪个记录状态下读到的（见 cachedBodyIsCurrent）：落定会让 pending 时期读到的
// 空正文失效 —— 那正是响应正文第一次入库的时刻。
const bodyReadStatus = ref<Partial<Record<BodyPart, string>>>({});
const bodyLoading = ref(false);
const bodyError = ref('');

function statusOf(item?: TrafficSummary): string {
  return item?.status ?? '';
}

function isSettled(item?: TrafficSummary): boolean {
  const status = statusOf(item);
  return status === 'success' || status === 'fail';
}

// 正文缓存与选中项同生死：换记录、按条件重来、选中项从结果里消失，都要一起清。
function clearBodies() {
  bodies.value = {};
  bodyReadStatus.value = {};
}

const stats = shallowRef<TrafficStats>();
// 「清空」确认框的开关与在飞状态；失败就地显示在列表上方（与「删除选中」的失败同一位置）。
const clearOpen = ref(false);
const clearing = ref(false);
const clearError = ref('');

const tabListEl = ref<HTMLElement | null>(null);
let disposed = false;
// traffic:available 的节流状态
let lastRefreshAt = 0;
let availableTimer: ReturnType<typeof setTimeout> | undefined;
let stopAvailableEvents = () => {};
// 在飞期间被挡下的标签页：等当前那次读完就补读（见 loadBody）
let pendingPart: BodyPart | undefined;

const currentBody = computed(() => bodies.value[bodyPart.value]);
// 页数至少 1：空结果也要显示「第 1 / 1 页」，不然页码区会出现 0 页。
const pageCount = computed(() => Math.max(1, Math.ceil(total.value / Math.max(1, pageSize.value))));
// 本页是否整页勾选（决定「全选本页」的勾选与半选状态）。空列表不算全选。
const allChecked = computed(() => items.value.length > 0 && items.value.every((item) => checkedIds.value.has(item.id)));
const checkedCount = computed(() => checkedIds.value.size);
// 选中集合的实例身份在切换时要换一个（Set 原地改也能触发，但换新实例让 computed 与模板的
// 依赖关系一眼可读，也避免任何“漏掉一次触发”的可能）。
function setChecked(next: Set<string>) {
  checkedIds.value = next;
}

// 筛选（含 appId）全部下推到 traffic.list：页面不再对已加载的页做本地过滤，深分页里符合
// 条件的记录同样会被后端取回。列表为空时要区分「被筛掉了」与「什么都没抓到」。
function conditionsOf(): Conditions {
  return {
    query: keyword.value.trim(),
    apiType: apiTypeFilter.value.trim(),
    appId: appIdFilter.value.trim(),
    status: statusFilter.value,
  };
}

function sameConditions(left: Conditions, right: Conditions): boolean {
  return left.query === right.query && left.apiType === right.apiType
    && left.appId === right.appId && left.status === right.status;
}

// 已应用的那一代条件 = 当前列表内容与游标所属的条件。
const appliedConditions = shallowRef<Conditions>(conditionsOf());

// 语义：**改筛选条件即视为新查询**（不必先点「应用筛选」）。游标是后端「上一页之后」的续页
// 指针，只在同一代条件内有效 —— 代次不一致时既不能带着旧游标按新条件翻页，也不能把新条件的
// 第一页追加到旧列表后面，那正是「旧条件前半截 + 新条件后半截」的混排。
function conditionsChanged(): boolean {
  return !sameConditions(conditionsOf(), appliedConditions.value);
}

// 措辞看的是**已应用**的那一代条件，不是输入框里正在打的字：只在关键词框里打字（还没回车、
// 还没点「应用筛选」）并没有改变列表，这时把空列表说成「没有匹配的记录」就是在骗人。
const hasActiveFilter = computed(() => {
  const applied = appliedConditions.value;
  return Boolean(applied.query || applied.apiType || applied.appId || applied.status);
});

// R12 丢弃总数（roadmap 2.4）：两组计数互补不重叠，直接相加。字段缺失按 0，不得因缺字段报错。
const droppedTotal = computed(() => {
  const current = stats.value;
  if (!current) return 0;
  return counter(current.droppedRecords) + counter(current.droppedUpdates);
});

const statsText = computed(() => {
  const current = stats.value;
  if (!current || typeof current.records !== 'number') return '';
  const range = current.oldestCapturedAt && current.newestCapturedAt
    ? `${formatTime(current.oldestCapturedAt)} ~ ${formatTime(current.newestCapturedAt)}`
    : '时间范围未知';
  const bytes = counter(current.bytes) > 0 ? ` · ${formatBytes(current.bytes)}` : '';
  return `库内 ${current.records.toLocaleString()} 条 · ${range}${bytes}`;
});

function counter(value: unknown): number {
  return typeof value === 'number' && Number.isFinite(value) && value > 0 ? value : 0;
}

// AppID 展示口径（roadmap 2.3）：后端给的 appId 优先（DB 的 appid 列）；捕获里没有 appid
// 的老记录退回 rid 的第二段 <apiType>-<appId>-<ts>-<seq>（roadmap 2.1）。两处都取不到
// （段数不足）就不显示 —— 不要把整条 rid 当成 AppID。
function appIdLabel(item: TrafficSummary): string {
  const explicit = typeof item.appId === 'string' ? item.appId.trim() : '';
  if (explicit) return explicit;
  const parts = (item.id ?? '').split('-');
  return parts.length >= 4 ? (parts[1] ?? '') : '';
}

// 这一条的 URL 有没有必要单独占一行：请求记录的 name 已经是「方法 + URL」，
// 云函数记录的函数名后面才是独立 url。已经出现在 name 里的就不重复打一遍。
function recordUrl(item: TrafficSummary): string {
  const url = (item.url ?? '').trim();
  if (!url) return '';
  return (item.name ?? '').includes(url) ? '' : url;
}

function formatTime(value?: string): string {
  if (!value) return '—';
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
}

function formatBytes(value?: number): string {
  return typeof value === 'number' && Number.isFinite(value) ? `${value.toLocaleString()} B` : '—';
}

function statusTone(status?: string): string {
  if (status === 'success') return 'success';
  if (status === 'fail') return 'fail';
  return 'pending';
}

function statusLabel(status?: string): string {
  if (status === 'success') return '成功';
  if (status === 'fail') return '失败';
  if (status === 'pending') return '等待中';
  return status || '未知';
}

// 0 表示「没测到耗时」，不是「耗时为零」：>0 才显示。
function durationLabel(item: TrafficSummary): string {
  const duration = item.durationMs;
  if (typeof duration !== 'number' || !Number.isFinite(duration) || duration <= 0) return '';
  return duration >= 1000 ? `${(duration / 1000).toFixed(1)}s` : `${Math.round(duration)}ms`;
}

// 只发后端确实支持的参数，条件由调用方给定（可能是已应用的那一代，也可能是刚改完的新一代）。
// appId 是**精确匹配**（DB 的 appid 列）：留空必须整条去掉，空串在 Go 侧是 omitempty 的零值，
// 但真发出去就成了「按空 appid 匹配」，结果会筛成空集。
function listParams(conditions: Conditions, targetPage: number): Record<string, unknown> {
  const params: Record<string, unknown> = {
    page: Math.max(1, Math.floor(targetPage)),
    pageSize: pageSize.value,
    query: conditions.query,
    apiType: conditions.apiType,
    status: conditions.status,
  };
  if (conditions.appId) params.appId = conditions.appId;
  return params;
}

// 整页替换：一页就是全部内容，没有追加这回事。同时把勾选里已经不在库里的 id 摘掉 ——
// 勾选只覆盖本页，所以「不在新页里」就等于「已经不在了」（另一个窗口删的）。
function setItems(next: TrafficSummary[]) {
  const ids = new Set(next.map((item) => item.id));
  items.value = next;
  if (!checkedIds.value.size) return;
  const kept = [...checkedIds.value].filter((id) => ids.has(id));
  setChecked(new Set(kept));
}

// 列表被整页换掉之后，选中项要么按新页里的同 id 对象重新挂上，要么清空。两种错法都真出现过：
// 留着旧对象会让详情停在旧事实（列表行说「成功 1.8s」、详情还说「等待中」）；而选中一条已经不
// 在结果里的记录，详情里就是一条「看不见」的记录。
// 返回是否清掉了选中，调用方据此决定要不要给提示。
function resyncSelection(page: TrafficSummary[]): boolean {
  const current = selected.value;
  if (!current) return false;
  const fresh = page.find((item) => item.id === current.id);
  if (fresh) {
    // 新页里的对象才是当前事实（落定状态、耗时、字节数都会变）。键是记录 id，重挂不会重建行。
    // 但「落定」是响应正文第一次入库的时刻：pending 时期读到的空正文不能当最终事实留着，否则
    // 详情会一边显示「成功 1.8s」一边显示「（正文为空）」。这时作废缓存，并把用户正看着的那
    // 一侧立即重取一次（落定是终态，最多多发一次请求）。
    const settledNow = !isSettled(current) && isSettled(fresh);
    const displayedPart = bodies.value[bodyPart.value] !== undefined ? bodyPart.value : undefined;
    selected.value = fresh;
    if (settledNow) {
      clearBodies();
      bodyError.value = '';
      if (displayedPart) void loadBody(displayedPart);
    }
    return false;
  }
  selected.value = undefined;
  clearBodies();
  bodyError.value = '';
  return true;
}

// 被互斥挡下的那次加载不能就这么丢掉（appId 输入框逐字符改值，自动刷新也可能撞上），
// 记下来等当前请求落地再补一次。
let pendingLoad: { page: number; replace: boolean } | undefined;

function flushPendingLoad() {
  const next = pendingLoad;
  pendingLoad = undefined;
  if (!next || disposed) return;
  // 用户在补取落地之前已经翻到了别的页：这时重取第一页会把他弹回页首（roadmap 2.3 明确
  // 不许），只补统计。
  if (next.replace && page.value !== 1) {
    void refreshStats();
    return;
  }
  void load(next.page, next.replace);
}

function reloadFirstPage() {
  if (loading.value) {
    pendingLoad = { page: 1, replace: true };
    return;
  }
  void load(1, true);
}

async function load(targetPage: number, replace = false) {
  if (loading.value) {
    pendingLoad = { page: targetPage, replace };
    return;
  }
  // 条件换代了（改了筛选框却没点「应用筛选」）：这次翻页就是一次新查询 —— 回第 1 页、
  // 按新条件取，选中项按新结果重挂或失效。
  const replacing = replace || conditionsChanged();
  const conditions = replacing ? conditionsOf() : appliedConditions.value;
  let wanted = replacing ? 1 : targetPage;
  loading.value = true;
  listError.value = '';
  try {
    // 最多两轮：这一轮可能落在「比现在页数还大」的页码上（页被删空就会这样），那就按后端
    // 刚给的总数算出末页再取一次。两轮都在 loading 里跑完，中间不出现「总数非 0 但这一页
    // 是空的」过渡帧，空状态不会闪一下再变。
    for (let attempt = 0; attempt < 2; attempt += 1) {
      const result = await backend.call<TrafficPage>('traffic.list', listParams(conditions, wanted));
      if (disposed) return;
      total.value = counter(result?.total);
      pageSize.value = counter(result?.pageSize) || pageSize.value;
      // 这一代条件就是刚取回的这份列表的归属（在请求发出时捕获，途中再改输入也不影响）。
      appliedConditions.value = conditions;
      const landed = counter(result?.page) || wanted;
      const last = Math.max(1, Math.ceil(total.value / Math.max(1, pageSize.value)));
      if (landed <= last || attempt === 1) {
        // 第二轮仍然越界（比如期间又被删了几页）就本地钳一下：宁可停在末页，也不把
        // 一份越界页当作当前页显示。
        page.value = Math.min(landed, last);
        const incoming = result?.items ?? [];
        setItems(incoming);
        // 翻页本身不动选中项：它多半在别的页上，不是「没了」，误判成失效会把正在看的详情关掉。
        // 只有整页替换（刷新 / 换筛选 / 删除后重载）才判「已不在本页」。
        if (replacing && resyncSelection(incoming)) notice.value = '选中的记录已不在本页，详情已关闭';
        break;
      }
      wanted = last;
    }
    jumpInput.value = '';
  } catch (reason) {
    if (!disposed) listError.value = messageOf(reason);
  } finally {
    if (!disposed) {
      loading.value = false;
      flushPendingLoad();
    }
  }
}

// 刷新 / 应用筛选 / 换每页条数：页码、列表、选中项与勾选一起归零。
function reset() {
  page.value = 1;
  total.value = 0;
  setChecked(new Set());
  selected.value = undefined;
  clearBodies();
  bodyPart.value = 'request';
  bodyError.value = '';
  notice.value = '';
  deleteError.value = '';
  items.value = [];
  jumpInput.value = '';
  // reset 就是「按当前这套条件重来」：先认下这一代，列表清空后空状态的措辞才和用户刚做的
  // 操作一致（否则会拿上一代的措辞去描述一个刚清空的列表）。
  appliedConditions.value = conditionsOf();
  reloadFirstPage();
  void refreshStats();
}

function applyFilters() {
  reset();
}

function selectStatus(event: Event) {
  statusFilter.value = (event.target as HTMLSelectElement).value;
  reset();
}

// 换每页条数等于换代：当前页的意义变了（第 3 页 / 每页 50 与第 3 页 / 每页 500 不是同一个
// 窗口），回第 1 页重取，与改条件同一条路径。
function changePageSize(event: Event) {
  const next = Number((event.target as HTMLSelectElement).value);
  if (!Number.isFinite(next) || next === pageSize.value) return;
  pageSize.value = next;
  reset();
}

// 翻页：勾选随页清空（见 checkedIds 的注释）。越界的目标钳到 [1, 页数]，末页按钮与跳转框
// 因此都不会把用户送到一个空页上。
function goToPage(target: number) {
  if (!Number.isFinite(target)) return;
  const next = Math.min(Math.max(1, Math.floor(target)), pageCount.value);
  if (next === page.value) return;
  setChecked(new Set());
  void load(next);
}

function submitJump() {
  // 空框不跳：输入框失焦也会走到这里，清空过数字的人不该被送回第 1 页。
  const raw = String(jumpInput.value).trim();
  if (!raw) return;
  goToPage(Number(raw));
}

function toggleChecked(id: string) {
  const next = new Set(checkedIds.value);
  if (next.has(id)) next.delete(id); else next.add(id);
  setChecked(next);
}

// 全选本页：已经全选就整页取消 —— 取消勾选不再单列一个按钮，用户看得见自己勾的是什么。
function toggleAllOnPage() {
  setChecked(allChecked.value ? new Set() : new Set(items.value.map((item) => item.id)));
}

// 删除走一个确认框、两条入口（选择条与详情面板），与 extract 页的删除同一形状。
function requestDelete(ids: string[]) {
  if (!ids.length || deleting.value) return;
  deleteRequest.value = {
    ids,
    message: `将删除 ${ids.length} 条历史记录，此操作不可恢复。`,
  };
}

async function runDelete() {
  const request = deleteRequest.value;
  deleteRequest.value = undefined;
  if (!request) return;
  deleting.value = true;
  deleteError.value = '';
  try {
    const result = await backend.call<DeleteResult>('traffic.delete', { ids: request.ids });
    if (disposed) return;
    notify(`已删除 ${counter(result?.deleted)} 条历史记录`, 'success');
    // 行删了、空间没回收：与「清空」同口径 —— 删了就是删了，报成失败是另一种不诚实。
    if (result?.reclamationFailed === true) notify('记录已删除，但空间回收失败', 'error');
    setChecked(new Set());
    // 删的正好是详情里那条：详情跟着关掉。删的不是它就不动 —— 用户可能在对照另一条记录。
    if (selected.value && request.ids.includes(selected.value.id)) {
      selected.value = undefined;
      clearBodies();
      bodyError.value = '';
    }
    // 停在当前页；这一页被删空时 load 里的两轮循环会把页码钳回末页。
    await load(page.value);
    await refreshStats();
  } catch (reason) {
    // 勾选与选中项都留着：重试就是再点一次。
    if (!disposed) deleteError.value = `删除失败：${messageOf(reason)}`;
  } finally {
    if (!disposed) deleting.value = false;
  }
}


// 键盘移动选中行：几万条的库里只靠鼠标点太慢。↑/↓ 逐行、PgUp/PgDn 十行、Home/End 到两端
//（WxAPI 与云函数两页只有 ↑/↓，这里补上大跨度移动；data-rid 是同一套行身份）。
function selectByOffset(offset: number) {
  const list = items.value;
  if (!list.length) return;
  const current = selected.value ? list.findIndex((item) => item.id === selected.value?.id) : -1;
  const index = current === -1
    ? (offset > 0 ? 0 : list.length - 1)
    : Math.min(Math.max(current + offset, 0), list.length - 1);
  select(list[index]);
  void nextTick(() => {
    listEl.value?.querySelector<HTMLElement>(`[data-rid="${list[index].id}"]`)
      ?.scrollIntoView?.({ block: 'nearest' });
  });
}

function selectEdge(which: 'first' | 'last') {
  const list = items.value;
  if (!list.length) return;
  select(which === 'first' ? list[0] : list[list.length - 1]);
  void nextTick(() => {
    listEl.value?.querySelector<HTMLElement>(`[data-rid="${list[which === 'first' ? 0 : list.length - 1].id}"]`)
      ?.scrollIntoView?.({ block: 'nearest' });
  });
}

function clearFilters() {
  keyword.value = '';
  apiTypeFilter.value = '';
  appIdFilter.value = '';
  statusFilter.value = '';
  reset();
}

function select(item: TrafficSummary) {
  if (selected.value?.id === item.id) return;
  // 切换记录必须清掉上一条的正文与加载态，不能让详情里短暂显示别人的正文。
  selected.value = item;
  clearBodies();
  bodyPart.value = 'request';
  bodyError.value = '';
  notice.value = '';
}

// IPC 上的「没有正文」是 null，与「正文内容就是空字符串」不是一回事 —— 旧代码把 null 走成
// JSON.stringify(value ?? '')，渲染出来是字面量 `""`，看着像正文内容就是两个引号。
// 返回 null 表示这一侧没有正文；返回 '' 表示正文是 0 字节。
// 返回正文文本与「解码前是否已被护栏截断」：后者让提示不必谎报总长度。
type DecodedBody = { text: string | null; capped: boolean };

function decodeBody(value: unknown): DecodedBody {
  if (value === null || value === undefined) return { text: null, capped: false };
  if (value instanceof Uint8Array) return { text: new TextDecoder().decode(value), capped: false };
  if (typeof value !== 'string') return { text: JSON.stringify(value), capped: false };
  // 按 base64 长度预判解码后的规模（4 字符 → 3 字节）；超护栏只解前一段，按 4 字符对齐以免切坏三元组。
  const estimated = Math.floor(value.length / 4) * 3;
  const capped = estimated > BODY_DECODE_LIMIT;
  const payload = capped ? value.slice(0, Math.floor(BODY_DECODE_LIMIT / 3) * 4) : value;
  try {
    return { text: new TextDecoder().decode(Uint8Array.from(atob(payload), (char) => char.charCodeAt(0))), capped };
  } catch {
    return { text: value, capped: false };
  }
}

// 正文展示态：超过上限只截前缀，而且**不再**做 JSON 美化（截断的 JSON 本来就解析不了，何况
// parse 一整段 16MB 正文正是要避开的开销）。解码已被护栏截断时，total 只是下界。
function buildBodyView(decoded: DecodedBody): BodyText {
  const text = decoded.text;
  if (!text) return { text: '' };
  if (text.length > BODY_DISPLAY_LIMIT || decoded.capped) {
    const shown = Math.min(text.length, BODY_DISPLAY_LIMIT);
    return { text: text.slice(0, shown), truncated: { shown, total: text.length, capped: decoded.capped } };
  }
  return { text: prettyOrRaw(text) };
}

// 缓存的有效性绑在「读取时的记录状态」上：落定会让 pending 时期读到的空正文作废。
function cachedBodyIsCurrent(part: BodyPart): boolean {
  return bodies.value[part] !== undefined && bodyReadStatus.value[part] === statusOf(selected.value);
}

async function loadBody(part: BodyPart) {
  const target = selected.value;
  if (!target) return;
  bodyPart.value = part;
  // 同一条记录、同一状态下只读一次：标签页来回切换不该重复打后端。
  if (cachedBodyIsCurrent(part)) return;
  if (bodyLoading.value) {
    // 另一侧正在读：记下这一侧，等它落地再读 —— 否则刚切过来的标签页会一直停在
    // 「正文未加载」，用户得再点一次。
    pendingPart = part;
    return;
  }
  bodyLoading.value = true;
  bodyError.value = '';
  let refetchPart: BodyPart | undefined;
  try {
    const decoded = decodeBody(await backend.call('traffic.getBody', { id: target.id, part }));
    if (disposed) return;
    // 响应回来时用户可能已经换了记录：这一份正文属于上一条，丢掉。
    if (selected.value?.id !== target.id) return;
    bodies.value = { ...bodies.value, [part]: buildBodyView(decoded) };
    bodyReadStatus.value = { ...bodyReadStatus.value, [part]: statusOf(target) };
    // 请求在飞时这一行落定了：拿回来的很可能是 pending 时期的空正文 —— 重取一次。落定是终态，
    // 所以这最多多发一次请求，不会自旋。
    if (!isSettled(target) && isSettled(selected.value)) refetchPart = part;
  } catch (reason) {
    if (!disposed) bodyError.value = messageOf(reason);
  } finally {
    if (!disposed) {
      bodyLoading.value = false;
      // 两种待读来源合一：落定后要重取的那一侧，或在飞期间被挡下的那一侧（先到先读）。
      const next = refetchPart ?? pendingPart;
      pendingPart = undefined;
      if (next !== undefined) void loadBody(next);
    }
  }
}

function prettyOrRaw(text: string): string {
  try {
    return prettyJson(JSON.parse(text));
  } catch {
    return text;
  }
}

function moveTab(step: number) {
  const at = BODY_TABS.findIndex((tab) => tab.key === bodyPart.value);
  const next = BODY_TABS[(at + step + BODY_TABS.length) % BODY_TABS.length];
  if (!next || next.key === bodyPart.value) return;
  void loadBody(next.key).then(() => nextTick(() => {
    tabListEl.value?.querySelector<HTMLElement>(`[data-testid="${next.testId}"]`)?.focus();
  }));
}

async function refreshStats() {
  try {
    const next = await backend.call<TrafficStats>('traffic.stats');
    if (disposed) return;
    stats.value = next && typeof next === 'object' ? next : undefined;
  } catch {
    // 统计只是展示用：读不到（后端未实现或临时失败）不该打断列表浏览。
  }
}

// traffic:available → 页面上有新记录时刷新。停在第 1 页才重取列表：第 1 页就是最新的一页，
// 新记录正落在那里。翻到第 N 页时只更新统计 —— 一页是 offset 窗口，重取会把用户正在看的
// 那几行换成别的记录，而且会把他弹回页首（roadmap 2.3 明确不许）。
async function refreshCurrentPage() {
  if (disposed) return;
  if (page.value !== 1) {
    await refreshStats();
    return;
  }
  // 有在飞请求：这次刷新不能被无声吞掉（一串事件落地后没人补取），记下来等它落地再补一次。
  if (loading.value) {
    pendingLoad = { page: 1, replace: true };
    return;
  }
  loading.value = true;
  try {
    // 自动刷新重取的是用户正在看的那一代条件，而不是输入框里可能还没提交的内容。
    const result = await backend.call<TrafficPage>('traffic.list', listParams(appliedConditions.value, 1));
    if (disposed) return;
    total.value = counter(result?.total);
    pageSize.value = counter(result?.pageSize) || pageSize.value;
    const incoming = result?.items ?? [];
    setItems(incoming);
    // 后台刷新不动勾选（setItems 只摘掉库里已经没有的 id）：用户勾好的行不该因为一次自动
    // 刷新被悄悄取消。选中项则按新对象重挂，不在新页里才失效并说明。
    if (resyncSelection(incoming)) notice.value = '选中的记录已不在本页，详情已关闭';
  } catch {
    // 后台刷新失败不该弹错误打断用户：用户并没有主动点任何东西。
  } finally {
    if (!disposed) {
      loading.value = false;
      flushPendingLoad();
    }
  }
  await refreshStats();
}

function onTrafficAvailable() {
  const now = Date.now();
  const elapsed = now - lastRefreshAt;
  if (elapsed >= AVAILABLE_THROTTLE_MS) {
    lastRefreshAt = now;
    void refreshCurrentPage();
    return;
  }
  // 窗口内只留一个尾随刷新：一串事件最多换来窗口末尾的一次重取。
  if (availableTimer) return;
  availableTimer = setTimeout(() => {
    availableTimer = undefined;
    lastRefreshAt = Date.now();
    void refreshCurrentPage();
  }, AVAILABLE_THROTTLE_MS - elapsed);
}


// 「导出 HAR」把最近 500 条 request 记录落成一个 HAR 1.2 文件（ids 空 = 最近窗口）。
// 取消与成功都要明说：取消是回答不是错误，失败才进 error 通道。
const exportingHar = ref(false);

async function exportHar() {
  if (exportingHar.value) return;
  exportingHar.value = true;
  try {
    const result = await backend.call<ExportHarResult>('traffic.exportHar', {});
    if (disposed) return;
    if (result?.ok && result.path) notify(`已导出 ${result.path}`, 'success');
    else if (!result?.ok && result?.reason) notify('已取消导出', 'info');
    else notify('导出失败：后端没有返回保存路径', 'error');
  } catch (reason) {
    if (!disposed) notify(`导出失败：${messageOf(reason)}`, 'error');
  } finally {
    if (!disposed) exportingHar.value = false;
  }
}

// 「清空」把整库记录一次删掉。它取代了原来的「清理」（填保留条数 / 保留天数、实时算「将删除
// N 条」）：不再有上限可填，也就不存在「框上的数字与实际会删掉的东西对不上」这类事 —— 用户
// 确认的就是「全部删除」这一件事。
const clearMessage = computed(() =>
  `将删除全部 ${counter(stats.value?.records)} 条历史记录，此操作不可恢复。`);

function openClear() {
  clearError.value = '';
  clearOpen.value = true;
}

function closeClear() {
  clearOpen.value = false;
  clearError.value = '';
}

async function runClear() {
  if (clearing.value) return;
  clearing.value = true;
  clearError.value = '';
  try {
    const result = await backend.call<DeleteResult>('traffic.clear');
    if (disposed) return;
    notify(`已清空 ${counter(result?.deleted)} 条历史记录`, 'success');
    // 行删掉了、空间没回收：与「删除选中」同一口径 —— 删了就是删了，报成失败是另一种不诚实。
    if (result?.reclamationFailed === true) notify('记录已清空，但空间回收失败', 'error');
    closeClear();
    // 清空改变了整个窗口：页码、列表与选中项一起重置，再拉一次统计。
    reset();
    await refreshStats();
  } catch (reason) {
    if (!disposed) clearError.value = `清空失败：${messageOf(reason)}`;
  } finally {
    if (!disposed) clearing.value = false;
  }
}

// appId 是后端的精确匹配条件：值一变，当前页就作废，按既有筛选的同一套 reset 语义回到第一页
// 重取，选中项（可能已经不在新结果里）一并清掉；清空则等于去掉该参数。
watch(appIdFilter, () => { reset(); });

onMounted(() => {
  // 资产清单「在流量记录中查看」跳转过来：?q= 作为关键词预填，再走首查。
  const preset = route.query.q;
  if (typeof preset === 'string' && preset.trim()) keyword.value = preset.trim();
  void load(1, true);
  void refreshStats();
  void backend
    .call<AppIDStat[]>('traffic.appids')
    .then((stats) => {
      if (disposed) return;
      appIdOptions.value = Array.isArray(stats) ? stats : [];
    })
    .catch(() => { /* 拿不到小程序列表就只保留手输 AppID */ });
  stopAvailableEvents = backend.on('traffic:available', onTrafficAvailable);
});

onBeforeUnmount(() => {
  disposed = true;
  if (availableTimer) clearTimeout(availableTimer);
  availableTimer = undefined;
  stopAvailableEvents();
});
</script>

<template>
  <section class="traffic-page page page-workbench" aria-labelledby="traffic-title">
    <!-- 操作并入内容区：页头只留无障碍标题，不再占一整行工具条 -->
    <PageHeader title="历史记录" title-id="traffic-title" />

    <div class="panel">
      <header class="panel-header">
        <div class="status-cluster">
          <!-- 当前筛选条件下的总条数（后端 total）：翻页时它不变，行的增减由「第 X / Y 页」体现 -->
          <span data-testid="traffic-count" class="status-pill off subnav-count">共 {{ total.toLocaleString() }} 条</span>
          <span v-if="statsText" data-testid="traffic-stats" class="status-line traffic-stats">{{ statsText }}</span>
          <span v-if="droppedTotal > 0" data-testid="traffic-dropped" class="status-pill warn" title="捕获过载：这些记录没有进入实时列表；其中页面缓冲溢出的部分在数据库里也没有（永久丢失），shell 缓冲溢出的部分仍可在下方历史里查到">未进实时列表 {{ droppedTotal }} 条</span>
        </div>
        <div class="toolbar panel-actions">
          <DensityToggle v-model="compact" test-id="traffic-density" />
          <label class="check-inline">每页
            <select :value="pageSize" data-testid="traffic-page-limit" aria-label="每页条数" @change="changePageSize">
              <option v-for="size in PAGE_LIMITS" :key="size" :value="size">{{ size }}</option>
            </select>
          </label>
          <button data-testid="traffic-refresh" type="button" :disabled="loading" @click="reset">{{ loading ? '加载中…' : '刷新' }}</button>
          <button data-testid="traffic-export-har" class="secondary" type="button" :disabled="exportingHar" title="把最近 500 条请求记录导出为 HAR 1.2 文件" @click="exportHar">{{ exportingHar ? '导出中…' : '导出 HAR' }}</button>
          <button data-testid="traffic-clear-open" class="secondary" type="button" @click="openClear">清空</button>
          <span v-if="items.length > 1" class="status-line key-hint">↑↓ 在本页内切换</span>
        </div>
      </header>

      <header class="panel-header traffic-filters">
        <span class="filter-label">筛选</span>
        <div class="toolbar filter-tools">
          <!-- 占位符只放字段名（控件宽度有限，长的说明会被切掉）；「如 wx.request」「精确匹配」
               这类说明挂 title，需要时悬停看。 -->
          <input v-model="keyword" data-testid="traffic-keyword" class="record-filter filter-keyword" type="search" placeholder="接口名 / URL" title="按接口名或 URL 模糊匹配" aria-label="关键词筛选" @keyup.enter="applyFilters">
          <input v-model="apiTypeFilter" data-testid="traffic-api-type" class="record-filter" placeholder="API 类型" title="如 wx.request" aria-label="API 类型筛选" @keyup.enter="applyFilters">
          <!-- 小程序下拉与 AppID 手输共用 appIdFilter：下拉选择即精确过滤，历史记录按小程序分类的快捷入口 -->
          <select v-if="appIdOptions.length" :value="appIdFilter" data-testid="traffic-appid-picker" aria-label="按小程序筛选" @change="pickAppId">
            <option value="">全部小程序</option>
            <option v-for="stat in appIdOptions" :key="stat.appid" :value="stat.appid" :title="`该小程序共 ${stat.count} 条记录`">{{ appIdOptionLabel(stat) }}</option>
          </select>
          <input v-model="appIdFilter" data-testid="traffic-app-id" class="record-filter" placeholder="AppID" title="精确匹配 AppID" aria-label="AppID 筛选">
          <!-- v-model 的 change 监听由指令注册，和这里的 @change 谁先跑不保证：直接读事件目标，
               免得筛选用的是上一个状态值。 -->
          <select :value="statusFilter" data-testid="traffic-status" aria-label="状态筛选" @change="selectStatus">
            <option value="">全部状态</option>
            <option v-for="option in STATUS_OPTIONS" :key="option" :value="option">{{ statusLabel(option) }}</option>
          </select>
          <!-- 两个按钮成组：窄屏换行时它们一起落到下一行，不会一个留上面、一个孤零零在下面 -->
          <div class="filter-actions">
            <button data-testid="traffic-apply-filters" class="secondary" type="button" @click="applyFilters">应用筛选</button>
            <!-- 清空筛选与筛选控件同域：它作用的是这几个输入，不是列表本身 -->
            <button data-testid="traffic-clear-filters" class="ghost" type="button" @click="clearFilters">清空筛选</button>
          </div>
        </div>
      </header>

      <ErrorState v-if="listError" class="panel-notice" :message="listError" retryable @retry="reset" />
      <p v-if="deleteError" data-testid="traffic-delete-error" class="error error-line" role="alert">{{ deleteError }}</p>
      <p v-if="clearError" data-testid="traffic-clear-error" class="error error-line" role="alert">{{ clearError }}</p>
      <p v-if="notice" data-testid="traffic-notice" class="status-line notice-line" role="status">{{ notice }}</p>

      <SplitPane class="traffic-split">
        <template #main>
          <!-- 三段：选择条 / 列表 / 分页条。只有列表滚（base.css 的 .record-list），
               勾选与翻页控件不能跟着滚走。 -->
          <div class="list-column">
            <!-- 勾选栏：始终占位（不按「有勾选才出现」隐藏），否则勾第一行会把整个列表往下推一格 -->
            <div v-if="items.length" class="selection-bar" data-testid="traffic-selection-bar">
              <label class="check-inline">
                <input
                  type="checkbox"
                  data-testid="traffic-select-page"
                  :checked="allChecked"
                  :indeterminate="checkedCount > 0 && !allChecked"
                  @change="toggleAllOnPage"
                >全选本页
              </label>
              <span class="status-line subnav-count" data-testid="traffic-selected-count">已选 {{ checkedCount }} 条</span>
              <button
                data-testid="traffic-delete-selected"
                class="danger small"
                type="button"
                :disabled="!checkedCount || deleting"
                @click="requestDelete([...checkedIds])"
              >删除选中{{ checkedCount ? ` (${checkedCount})` : '' }}</button>
            </div>

            <ul ref="listEl" class="record-list" :class="{ 'is-compact': compact }" aria-label="历史记录列表">
              <!-- 稳定键：记录 id。整页替换时只有 id 变了的行需要重建。 -->
              <li v-for="item in items" :key="item.id" class="record-item" :class="{ selected: selected?.id === item.id }">
                <!-- 复选框与行按钮是兄弟：input 放进 button 里是无效 HTML，点击也会被按钮吃掉。
                     勾选不改选中项（那是「看详情」），点行仍走 select。 -->
                <input
                  class="record-check"
                  type="checkbox"
                  :data-testid="`traffic-check-${item.id}`"
                  :aria-label="`选择 ${item.name || item.id}`"
                  :checked="checkedIds.has(item.id)"
                  @change="toggleChecked(item.id)"
                >
                <button
                  class="traffic-row"
                  :data-testid="`traffic-${item.id}`"
                  :data-rid="item.id"
                  type="button"
                  :class="{ selected: selected?.id === item.id }"
                  @click="select(item)"
                  @keydown.down.prevent="selectByOffset(1)"
                  @keydown.up.prevent="selectByOffset(-1)"
                  @keydown.page-down.prevent="selectByOffset(10)"
                  @keydown.page-up.prevent="selectByOffset(-10)"
                  @keydown.home.prevent="selectEdge('first')"
                  @keydown.end.prevent="selectEdge('last')"
                >
                  <span class="record-head">
                    <!-- 只留 API 类型一个徽标：方法在 name 里（wx.request 的 name 就是「POST https://…」，
                         钩子拼的），AppID 每行都一样、挪到下面那行灰色的里去。 -->
                    <span class="record-badge">{{ item.apiType || '未知类型' }}</span>
                    <span class="record-title" :title="item.name || ''">{{ item.name || item.apiType || '未命名调用' }}</span>
                    <span class="status-badge" :class="statusTone(item.status)">{{ statusLabel(item.status) }}</span>
                  </span>
                  <!-- 云函数记录的函数名之外还带独立 url；它若已经出现在 name 里就不再重复一行 -->
                  <span v-if="recordUrl(item)" class="record-url" :title="recordUrl(item)">{{ recordUrl(item) }}</span>
                  <span class="record-meta">
                    <span>{{ formatTime(item.capturedAt) }}</span>
                    <span v-if="durationLabel(item)" class="record-duration">{{ durationLabel(item) }}</span>
                    <span>{{ formatBytes(item.requestBytes) }} / {{ formatBytes(item.responseBytes) }}</span>
                    <span v-if="appIdLabel(item)" class="record-appid" :title="appIdLabel(item)">{{ appIdLabel(item) }}</span>
                  </span>
                </button>
              </li>
              <li v-if="!items.length">
                <EmptyState
                  :title="hasActiveFilter ? '没有匹配的记录' : '暂无历史记录'"
                  :description="hasActiveFilter ? '清空筛选条件或更换 AppID 后重试。' : '在 WxAPI 或云函数里开始捕获后，记录会写入这里。'"
                />
              </li>
            </ul>

            <nav v-if="total > 0" class="list-pager" data-testid="traffic-pager" aria-label="历史记录分页">
              <button class="ghost small" data-testid="traffic-page-first" type="button" :disabled="page <= 1 || loading" @click="goToPage(1)">首页</button>
              <button class="ghost small" data-testid="traffic-page-prev" type="button" :disabled="page <= 1 || loading" @click="goToPage(page - 1)">上一页</button>
              <span class="status-line subnav-count" data-testid="traffic-page-indicator">第 {{ page }} / {{ pageCount }} 页</span>
              <button class="ghost small" data-testid="traffic-page-next" type="button" :disabled="page >= pageCount || loading" @click="goToPage(page + 1)">下一页</button>
              <button class="ghost small" data-testid="traffic-page-last" type="button" :disabled="page >= pageCount || loading" @click="goToPage(pageCount)">末页</button>
              <!-- 跳转只在回车或失焦时走：逐字符跳页会把中间页码也请求一遍 -->
              <label class="check-inline">跳至
                <input v-model="jumpInput" data-testid="traffic-page-jump" class="page-jump" type="number" min="1" :max="pageCount" aria-label="跳至第几页" @keyup.enter="submitJump" @blur="submitJump">
                页
              </label>
            </nav>
          </div>
        </template>

        <template #side>
          <aside data-testid="traffic-detail" class="detail-pane traffic-detail-panel">
            <template v-if="selected">
              <h2>{{ selected.name || selected.apiType || '调用详情' }}</h2>
              <p class="traffic-detail-meta">
                <span class="record-badge">{{ selected.apiType || '未知类型' }}</span>
                <span v-if="appIdLabel(selected)" class="record-badge">{{ appIdLabel(selected) }}</span>
                <span v-if="selected.method" class="record-badge">{{ selected.method }}</span>
                <span class="status-badge" :class="statusTone(selected.status)">{{ statusLabel(selected.status) }}</span>
                <span>{{ formatTime(selected.capturedAt) }}</span>
                <span v-if="durationLabel(selected)" class="record-duration">{{ durationLabel(selected) }}</span>
                <span>{{ formatBytes(selected.requestBytes) }} / {{ formatBytes(selected.responseBytes) }}</span>
              </p>
              <p class="traffic-url">{{ selected.url || '未记录 URL' }}</p>

              <!-- 单条删除与批量删除走同一个确认框：直接子节点在滚动区之外，滚正文时按钮不跟着动 -->
              <div class="detail-actions">
                <button data-testid="traffic-delete-one" class="danger small" type="button" :disabled="deleting" @click="requestDelete([selected.id])">删除这条</button>
              </div>

              <div ref="tabListEl" class="detail-tabs" role="tablist" aria-label="正文视图">
                <button
                  v-for="tab in BODY_TABS"
                  :id="`traffic-tab-${tab.key}`"
                  :key="tab.key"
                  :data-testid="tab.testId"
                  type="button"
                  role="tab"
                  :aria-selected="bodyPart === tab.key"
                  aria-controls="traffic-body-panel"
                  :tabindex="bodyPart === tab.key ? 0 : -1"
                  :class="{ active: bodyPart === tab.key }"
                  @click="loadBody(tab.key)"
                  @keydown.right.prevent="moveTab(1)"
                  @keydown.left.prevent="moveTab(-1)"
                >{{ tab.label }}</button>
              </div>
              <!-- 正文按需读取：未选中记录、或还没点过标签页时，这里不发 traffic.getBody。
                   只有这块滚（base.css 的 .detail-body）：身份行与标签页固定在面板里。 -->
              <div id="traffic-body-panel" class="detail-body" role="tabpanel" :aria-labelledby="`traffic-tab-${bodyPart}`" tabindex="0">
                <p v-if="bodyLoading" class="status-line" data-testid="traffic-body-loading">读取中…</p>
                <ErrorState v-else-if="bodyError" :message="bodyError" retryable @retry="loadBody(bodyPart)" />
                <template v-else-if="currentBody">
                  <!-- 截断了就必须说：用户看到的不是全文 -->
                  <p v-if="currentBody.truncated" data-testid="traffic-body-truncated" class="status-line">正文过大（{{ currentBody.truncated.capped ? `不少于 ${currentBody.truncated.total}` : `共 ${currentBody.truncated.total}` }} 个字符），已截断显示前 {{ currentBody.truncated.shown }} 个字符</p>
                  <pre v-if="currentBody.text" data-testid="traffic-body" class="code-block">{{ currentBody.text }}</pre>
                  <p v-else data-testid="traffic-body-empty" class="status-line">{{ EMPTY_BODY_LABEL }}</p>
                </template>
                <EmptyState v-else title="正文未加载" description="按需读取，避免详情拖慢列表。" />
              </div>
            </template>
            <EmptyState v-else title="选择一条记录" description="查看接口元数据和请求 / 响应正文。" />
          </aside>
        </template>
      </SplitPane>
    </div>

    <!-- 删除只有一个确认框、两条入口（选择条与详情面板）：ids 在打开这一刻定下来 -->
    <ConfirmDialog
      :open="!!deleteRequest"
      title="删除记录"
      :message="deleteRequest?.message ?? ''"
      confirm-text="删除"
      tone="danger"
      test-id="traffic-confirm-delete"
      @confirm="runDelete"
      @cancel="deleteRequest = undefined"
    />

    <!-- 清空：整库记录一次删掉。没有上限可填，确认框里只有条数与「不可恢复」 -->
    <ConfirmDialog
      :open="clearOpen"
      title="清空历史记录"
      :message="clearMessage"
      confirm-text="清空"
      tone="danger"
      test-id="traffic-confirm-clear"
      @confirm="runClear"
      @cancel="closeClear"
    />
  </section>
</template>

<style scoped>
.status-cluster {
  align-items: center;
  display: flex;
  flex-wrap: wrap;
  gap: .45rem;
}

.traffic-stats {
  font-size: .78rem;
}

.panel-actions {
  margin-left: auto;
}

/* 筛选条：左边一个「筛选」标签，右边一行控件。原来控件被 margin-left:auto 顶到最右，
   而全局 select 是 width:100%，于是 select 独占一整行、整条带子高 150px，左侧还空着
   一大片——控件全挤在右边那一小块里。 */
.filter-label {
  color: var(--muted);
  flex: none;
  font-size: .85rem;
}

.filter-tools {
  flex: 1 1 auto;
}

.record-filter {
  flex: 0 1 12rem;
  min-width: 8rem;
}

/* 关键词是主筛选项：让它吃掉剩下的宽度，其余控件保持内容宽 */
.filter-keyword {
  flex: 1 1 16rem;
  max-width: 26rem;
}

.filter-tools select,
.filter-tools button {
  flex: none;
  width: auto;
}

.filter-actions {
  display: flex;
  flex: none;
  gap: .5rem;
}

.check-inline input[type='number'] {
  width: 5.5rem;
}

/* 面板内的原位提示（错误 / 失效选中） */
.panel-notice,
.error-line,
.notice-line {
  margin: .75rem 1.1rem 0;
}

/* 列表行：apiType / method / name / 状态徽标，第二行是时间、耗时与字节数 */
.record-list button {
  grid-template-columns: minmax(0, 1fr);
}

/* 一行 = 复选框列 + 行按钮，两个兄弟节点（input 放进 button 是无效 HTML，点击也会被按钮吃掉）。
   悬停与选中高亮打在整个 li 上，否则只有按钮那半边亮，复选框那一列看着像被切出去的另一条带子；
   按钮自己的底色随之让开（文字色仍由 base.css 的 button.selected 给）。 */
.record-item {
  align-items: center;
  border-radius: var(--radius-sm);
  display: grid;
  grid-template-columns: auto minmax(0, 1fr);
}

.record-item:hover {
  background: var(--panel-2);
}

.record-item.selected {
  background: var(--accent-soft);
}

.record-item .traffic-row:hover,
.record-item .traffic-row.selected {
  background: transparent;
}

.record-check {
  margin: 0 .15rem 0 .5rem;
}

/* 勾选栏：始终占位（items 为空时整条隐藏）。flex:none 是硬约束 —— 主面板是 flex 列，
   只有 .record-list 能吃掉剩余高度。 */
.selection-bar {
  align-items: center;
  border-bottom: 1px solid var(--border);
  display: flex;
  flex: none;
  flex-wrap: wrap;
  gap: .6rem;
  padding: .35rem .7rem;
}

.selection-bar .status-line {
  color: var(--muted);
  font-size: .78rem;
}

.selection-bar button {
  margin-left: auto;
}

/* 分页条：在列表滚动区之外（同 extract 的扫描结果分页：翻到底不该把翻页控件也滚走） */
.list-pager {
  align-items: center;
  border-top: 1px solid var(--border);
  display: flex;
  flex: none;
  flex-wrap: wrap;
  gap: .5rem;
  padding: .45rem .7rem;
}

.list-pager .status-line {
  color: var(--muted);
  font-size: .78rem;
  margin: 0 .2rem;
}

/* 全局 select / input 是 width:100%：不收回内容宽，跳转框会独占一整行 */
.list-pager input[type='number'] {
  width: 4.2rem;
}

/* 三段容器：选择条 / 列表 / 分页条。只有列表能滚，另外两段固定。 */
.list-column {
  display: flex;
  flex: 1 1 auto;
  flex-direction: column;
  min-height: 0;
  min-width: 0;
}

.record-head {
  align-items: center;
  display: flex;
  gap: .4rem;
}

.record-title {
  flex: 1 1 auto;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

/* 一行一条：URL 换行会把行高撑成两三倍，列表就没法一眼扫过去。完整值在悬停提示里。 */
.record-url {
  color: var(--muted);
  font-family: var(--mono);
  font-size: .74rem;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

/* AppID 每行都一样：降级成 meta 里最淡的一段，窄了先截它 */
.record-appid {
  color: var(--faint);
  flex: 0 1 auto;
  font-family: var(--mono);
  font-size: .72rem;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
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
  min-width: 0;
}

.record-duration {
  color: var(--accent-strong);
}

.panel-actions select {
  width: auto;
}

.traffic-detail-meta {
  align-items: center;
  display: flex;
  flex-wrap: wrap;
  gap: .4rem;
  margin: 0;
}

.traffic-detail-meta .status-badge {
  margin-left: 0;
}

.traffic-url {
  color: var(--muted);
  font-size: .8rem;
  margin: 0;
  overflow-wrap: anywhere;
}

/* 列表 / 详情分栏：面板本身是 flex 列，分栏要吃掉剩余高度 */
.traffic-split {
  flex: 1 1 auto;
  gap: 0;
  min-height: 0;
}

/* flex-direction: column 是必需的：主面板现在是三段（选择条 / 列表 / 分页条），
   少了这一条它们会横着排，列表也就不滚了。 */
.traffic-split :deep(.split-pane-main),
.traffic-split :deep(.split-pane-side) {
  display: flex;
  flex-direction: column;
  min-height: 0;
  min-width: 0;
}

.traffic-split .record-list,
.traffic-split .detail-pane {
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

.code-block {
  max-height: 360px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-word;
}

/* 小屏单列：筛选与操作各自占满一行，列表与详情叠起来后各自滚动 */
@media (max-width: 900px) {
  .traffic-page .panel {
    overflow-y: auto;
  }

  .traffic-split {
    flex: 0 0 auto;
  }

  .traffic-split .record-list {
    max-height: 22rem;
  }

  /* 窄屏翻页控件成组换行，不与「跳至」混排 */
  .list-pager {
    justify-content: flex-start;
  }

  .selection-bar .status-line {
    order: 3;
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

  /* 窄屏：按最小可用宽度自动换行（900px 宽时三个输入正好一行），不再让每个控件
     独占一整行——那样筛选条会涨到 229px，而这一档页面高度是钉死的，列表只剩 142px。
     这里改 flex 而不是 width：主轴上 flex-basis 压过 width。 */
  .record-filter,
  .filter-keyword {
    flex: 1 1 12rem;
    max-width: none;
  }

  /* 两个动作按钮整组换行，不与输入框混排 */
  .filter-actions {
    flex: 1 1 100%;
  }

  .panel-notice,
  .error-line,
  .notice-line {
    margin: .75rem .9rem 0;
  }
}
</style>
