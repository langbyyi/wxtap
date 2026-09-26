<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue';
import { backend } from '../api/bridge';
import AssetDetail from '../components/AssetDetail.vue';
import EmptyState from '../components/EmptyState.vue';
import ErrorState from '../components/ErrorState.vue';
import HighlightText from '../components/HighlightText.vue';
import PageHeader from '../components/PageHeader.vue';
import { useEngineStore } from '../stores/engine';
import { messageOf } from '../utils/format';
import { notify } from '../utils/notify';

// ---- 契约类型（contracts/assets）：形状以后端唯一权威契约为准 ----
type AssetSource = { type: string; ref: string };

type AssetItem = {
  id: string;
  kind: 'api' | 'static' | 'ws' | 'cloud' | string;
  url: string;
  host: string;
  path: string;
  method: string;
  sources: AssetSource[];
  hits: number;
  firstSeen: string;
  lastSeen: string;
  tags?: string[];
  trafficSeen: boolean;
};

// assets.list 的一页：hosts 汇总只反映后端当前的全量，与当页内容无关。
// appid / builtAt 标识这份清单属于哪个小程序、何时构建（恢复的存档保留原时间）。
type AssetsPage = { ok: boolean; total?: number; hosts?: Array<{ host: string; count: number }>; items?: AssetItem[]; error?: string; appid?: string; builtAt?: string };
// assets.scan 受理：{ok:true, async:true, taskId}；无来源 → {ok:false, error}
type ScanAccepted = { ok: boolean; async?: boolean; taskId?: string; error?: string };
// assets.export save:true：{ok:true, path} | {ok:false, reason:'用户取消'}
type ExportResult = { ok: boolean; path?: string; reason?: string; error?: string };
type AssetsProgressEvent = { status: 'working' | 'done' | 'error'; current: number; total: number; message?: string };
// 全局 task 事件（engine store 同款载荷）：assets_progress 丢失时的完成兜底。
type TaskEvent = { id: string; phase?: string; message?: string; error?: string };

const KINDS = [
  { key: 'api', label: 'API' },
  { key: 'static', label: '静态资源' },
  { key: 'ws', label: 'WebSocket' },
  { key: 'cloud', label: '云函数' },
] as const;

const EXPORT_FORMATS = [
  { key: 'nuclei', label: 'Nuclei（targets）' },
  { key: 'httpx', label: 'httpx（urls）' },
  { key: 'json', label: 'JSON' },
  { key: 'txt', label: 'TXT' },
  { key: 'csv', label: 'CSV' },
] as const;

const PAGE_SIZE = 100;
// 关键词防抖：停手 300ms 才重查，避免每个字符都打一次 IPC。
const KEYWORD_DEBOUNCE_MS = 300;
// 构建期间的兜底轮询：契约没有 assets.state，完成信号以 assets_progress 为主、
// task 事件兜底；轮询只负责把正在生成的清单刷进视图。
const BUILD_POLL_MS = 2000;

// ---- 结构树 ----
// 后端单次上限 500 条：结构树按 500 一轮循环拉全量再本地建树；
// TREE_MAX_ITEMS 是前端保险丝，超限时明说截断而不是悄悄少画。
const TREE_FETCH_LIMIT = 500;
const TREE_MAX_ITEMS = 5000;
// 子树资产数不超过该值的路径节点默认展开：小清单打开即是全貌，大清单保持收起。
const TREE_AUTO_EXPAND = 15;

type TreeNode = { name: string; children: Map<string, TreeNode>; assets: AssetItem[]; count: number };
type TreeRow =
  | { type: 'host'; key: string; host: string; count: number; depth: number }
  | { type: 'path'; key: string; name: string; count: number; depth: number }
  | { type: 'leaf'; key: string; asset: AssetItem; name: string; depth: number };

// 页面分组：同一反编译页面文件引用到的端点聚在一组，纯流量来源的端点单独一组。
type PageGroup = { page: string; assets: AssetItem[] };

const NO_PAGE_GROUP = '（仅流量来源）';

function kindLabel(key: string): string {
  return KINDS.find((item) => item.key === key)?.label ?? key;
}

// 方法徽章配色：读绿、写蓝、改黄、删红；"*"（仅代码已知，任意方法）走中性默认。
function methodClass(method: string): string {
  switch (method.toUpperCase()) {
    case 'GET':
    case 'HEAD':
      return 'method-read';
    case 'POST':
      return 'method-write';
    case 'PUT':
    case 'PATCH':
      return 'method-update';
    case 'DELETE':
      return 'method-delete';
    default:
      return '';
  }
}

// lastSeen：三天内给相对时间（新旧的差别一眼可见），更早给本地「YYYY-MM-DD HH:mm」。
function formatLastSeen(value?: string): string {
  if (!value) return '—';
  const time = new Date(value).getTime();
  if (Number.isNaN(time)) return value;
  const elapsed = Date.now() - time;
  if (elapsed >= 0 && elapsed < 60_000) return '刚刚';
  if (elapsed >= 0 && elapsed < 3_600_000) return `${Math.floor(elapsed / 60_000)} 分钟前`;
  if (elapsed >= 0 && elapsed < 86_400_000) return `${Math.floor(elapsed / 3_600_000)} 小时前`;
  if (elapsed >= 0 && elapsed < 7 * 86_400_000) return `${Math.floor(elapsed / 86_400_000)} 天前`;
  const date = new Date(time);
  const pad = (num: number) => String(num).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

// ---- 构建卡片 ----
const buildDir = ref('');
const includeTraffic = ref(true);
const cloudFnsText = ref('');
const building = ref(false);
const buildTaskId = ref('');
const buildProgress = reactive({ current: 0, total: 0, message: '' });
const buildError = ref('');
const buildNotice = ref('');

// ---- 筛选 ----
const kindFilter = ref('');
const hostFilter = ref('');
const keyword = ref('');
let keywordTimer: ReturnType<typeof setTimeout> | undefined;

// ---- 目标小程序 ----
// 清单是单个小程序的档案：构建取流量按目标 appid 收窄，反编译目录自动对齐
// output/<appid>。目标默认跟随当前连接；断开后保持上次选择（config 持久化）。
type AppIDOption = { appid: string; name?: string; count?: number; connected?: boolean };
const engine = useEngineStore();
const targetAppID = ref('');
const appOptions = ref<AppIDOption[]>([]);
const knownProjects = ref<Array<{ appid?: string; name?: string; path?: string; mtime?: number }>>([]);
const cloudFnsByApp = ref<Record<string, string>>({});

// ---- 列表（明细表）----
const items = ref<AssetItem[]>([]);
const total = ref(0);
const hosts = ref<Array<{ host: string; count: number }>>([]);
const page = ref(1);
const pageCount = computed(() => Math.max(1, Math.ceil(total.value / PAGE_SIZE)));
const listLoading = ref(false);
const listError = ref('');
// 汇总 chips：total 与 byKind。契约的 list 响应不带 byKind，按 kind 各探一页（limit:1）
// 用 total 取数，host / 关键词筛选同步下发，保证 chips 与当前筛选一致。
const kindCounts = ref<Record<string, number>>({});
const expandedId = ref('');
// 清单归属与构建时间来自 assets.list 响应（恢复的存档保留原构建时间）。
// 切换目标后旧清单仍在（存档不会自动重建）：提示要指出错位，引导重新构建。
const builtAppID = ref('');
const builtAtISO = ref('');
const builtHint = computed(() => {
  if (!builtAtISO.value) return '';
  const builtName = builtAppID.value
    ? appOptions.value.find((option) => option.appid === builtAppID.value)?.name ?? builtAppID.value
    : '未指定小程序';
  const hint = `清单目标：${builtName} · 构建于 ${formatLastSeen(builtAtISO.value)}`;
  if (!targetAppID.value || targetAppID.value === builtAppID.value) return hint;
  const targetName = appOptions.value.find((option) => option.appid === targetAppID.value)?.name ?? targetAppID.value;
  return `${hint} —— 目标已切换为 ${targetName}，重新构建后更新`;
});

// ---- 视图 ----
// tree / pages 共用同一份全量数据（treeItems），切换不重拉；table 走后端分页。
const viewMode = ref<'tree' | 'pages' | 'table'>('tree');
const treeHosts = ref<TreeNode[]>([]);
const treeItems = ref<AssetItem[]>([]);
const treeLoaded = ref(false);
// 表格视图里的筛选/构建刷新不会覆盖树数据：树/页面切回去时按 stale 重新拉。
const treeStale = ref(true);
const expandedKeys = ref<Set<string>>(new Set());
const pageGroups = ref<PageGroup[]>([]);
const pageExpanded = ref<Set<string>>(new Set());
const treeLoading = ref(false);
const treeTruncated = ref(0);

// ---- 导出 ----
const exporting = ref(false);
const exportNotice = ref('');

let disposed = false;
let buildTimer: ReturnType<typeof setInterval> | undefined;
let stopAssetsProgress = () => {};
let stopTaskEvents = () => {};

// ---- assets.list：主查询（树/页面/表）+ chips 计数探针 ----
// 筛选快照：loadTree 一轮最多 10 次顺序 IPC，循环内必须用进入时的口径，
// 否则中途改筛选会把两套口径的条目拼进同一份数据。
function currentFilters(): { kind: string; host: string; query: string } {
  return {
    kind: kindFilter.value,
    host: hostFilter.value,
    query: keyword.value.trim(),
  };
}

function filterParams(filters: ReturnType<typeof currentFilters>, offset: number, limit: number): Record<string, unknown> {
  const params: Record<string, unknown> = { offset: Math.max(0, Math.floor(offset)), limit };
  if (filters.kind) params.kind = filters.kind;
  if (filters.host) params.host = filters.host;
  if (filters.query) params.query = filters.query;
  return params;
}

function listParams(offset: number, kindScope: string, limit: number): Record<string, unknown> {
  return filterParams({ ...currentFilters(), kind: kindScope }, offset, limit);
}

let listReloadPending = false;
async function loadAssets(targetPage: number) {
  if (listLoading.value) {
    // 在途请求用的还是旧口径：记账，收尾后回第 1 页按新口径补拉。
    listReloadPending = true;
    return;
  }
  listLoading.value = true;
  listError.value = '';
  try {
    const result = await backend.call<AssetsPage>('assets.list', listParams((targetPage - 1) * PAGE_SIZE, kindFilter.value, PAGE_SIZE));
    if (disposed) return;
    if (result?.ok === false) {
      listError.value = result.error || '读取资产列表失败';
      return;
    }
    total.value = typeof result?.total === 'number' ? result.total : 0;
    hosts.value = result?.hosts ?? [];
    items.value = result?.items ?? [];
    builtAppID.value = typeof result?.appid === 'string' ? result.appid : '';
    builtAtISO.value = typeof result?.builtAt === 'string' ? result.builtAt : '';
    page.value = Math.min(Math.max(1, Math.floor(targetPage)), pageCount.value);
  } catch (reason) {
    if (!disposed) listError.value = messageOf(reason);
  } finally {
    if (!disposed) listLoading.value = false;
    if (listReloadPending && !disposed) {
      listReloadPending = false;
      void loadAssets(1);
    }
  }
}

// 结构树拉全量：filters 全部下发给 assets.list，按 500 一轮翻到拉完或触顶。
let treeReloadPending = false;
async function loadTree() {
  if (treeLoading.value) {
    // 在途那轮是旧口径：置 stale 记账，收尾后按新口径补拉。
    treeReloadPending = true;
    treeStale.value = true;
    return;
  }
  treeLoading.value = true;
  listError.value = '';
  const scope = currentFilters();
  try {
    const collected: AssetItem[] = [];
    let totalSeen = 0;
    for (let offset = 0; ; offset += TREE_FETCH_LIMIT) {
      const result = await backend.call<AssetsPage>('assets.list', filterParams(scope, offset, TREE_FETCH_LIMIT));
      if (disposed) return;
      if (result?.ok === false) {
        listError.value = result.error || '读取资产列表失败';
        return;
      }
      totalSeen = typeof result?.total === 'number' ? result.total : 0;
      if (typeof result?.appid === 'string') builtAppID.value = result.appid;
      if (typeof result?.builtAt === 'string') builtAtISO.value = result.builtAt;
      hosts.value = result?.hosts ?? hosts.value;
      collected.push(...(result?.items ?? []));
      if (collected.length >= TREE_MAX_ITEMS) {
        treeTruncated.value = Math.max(0, totalSeen - collected.length);
        break;
      }
      // 空页说明数据已经见底（后端清单可能比翻页开始时小），按拉完处理、不报截断。
      if (!result?.items?.length || collected.length >= totalSeen) {
        treeTruncated.value = 0;
        break;
      }
    }
    total.value = totalSeen;
    treeItems.value = collected;
    treeLoaded.value = true;
    treeStale.value = false;
    buildTreeState(collected);
    buildPageState(collected);
  } catch (reason) {
    if (!disposed) listError.value = messageOf(reason);
  } finally {
    if (!disposed) treeLoading.value = false;
    if (treeReloadPending && !disposed) {
      treeReloadPending = false;
      void loadTree();
    }
  }
}

// host → 路径逐段 → 叶子资产；云函数的 path 没有前导斜杠，按单段处理自然落位。
function buildTreeState(newItems: AssetItem[]) {
  const roots = new Map<string, TreeNode>();
  for (const asset of newItems) {
    const host = asset.host || '(无主机)';
    let root = roots.get(host);
    if (!root) {
      root = { name: host, children: new Map(), assets: [], count: 0 };
      roots.set(host, root);
    }
    let node = root;
    for (const segment of asset.path.split('/').filter(Boolean)) {
      let child = node.children.get(segment);
      if (!child) {
        child = { name: segment, children: new Map(), assets: [], count: 0 };
        node.children.set(segment, child);
      }
      node = child;
    }
    node.assets.push(asset);
  }
  const hostList = [...roots.values()];
  for (const root of hostList) countAssets(root);
  // 展开状态按新一轮数据重建：主机全开，小路径子树跟着开，其余保持收起。
  const expanded = new Set<string>();
  for (const root of hostList) {
    const hostKey = `h:${root.name}`;
    expanded.add(hostKey);
    collectAutoExpand(root, hostKey, expanded);
  }
  treeHosts.value = hostList;
  expandedKeys.value = expanded;
}

function countAssets(node: TreeNode): number {
  let sum = node.assets.length;
  for (const child of node.children.values()) sum += countAssets(child);
  node.count = sum;
  return sum;
}

function collectAutoExpand(node: TreeNode, key: string, into: Set<string>) {
  for (const child of node.children.values()) {
    const childKey = `${key}/${child.name}`;
    if (child.count > TREE_AUTO_EXPAND) continue;
    into.add(childKey);
    collectAutoExpand(child, childKey, into);
  }
}

// 一个端点可能被多个页面引用：sources 里的每个 code 来源都是一个分组入口。
function pageRefsOf(asset: AssetItem): string[] {
  const pages: string[] = [];
  for (const source of asset.sources) {
    if (source.type !== 'code' || !source.ref || source.ref === 'cloud') continue;
    if (!pages.includes(source.ref)) pages.push(source.ref);
  }
  return pages;
}

function buildPageState(newItems: AssetItem[]) {
  const byPage = new Map<string, AssetItem[]>();
  for (const asset of newItems) {
    const pages = pageRefsOf(asset);
    if (!pages.length) pages.push(NO_PAGE_GROUP);
    for (const page of pages) {
      let list = byPage.get(page);
      if (!list) {
        list = [];
        byPage.set(page, list);
      }
      if (!list.some((item) => item.id === asset.id)) list.push(asset);
    }
  }
  const groups = [...byPage.entries()]
    .map(([page, assets]) => ({ page, assets }))
    .sort((a, b) => b.assets.length - a.assets.length || a.page.localeCompare(b.page))
    // 「仅流量来源」没有页面语义，排到最后
    .sort((a, b) => Number(a.page === NO_PAGE_GROUP) - Number(b.page === NO_PAGE_GROUP));
  // 展开状态按新一轮数据重建：小组自动展开，大组保持收起。
  const expanded = new Set<string>();
  for (const group of groups) {
    if (group.assets.length <= TREE_AUTO_EXPAND) expanded.add(pageKey(group.page));
  }
  pageGroups.value = groups;
  pageExpanded.value = expanded;
}

function pageKey(page: string): string {
  return `p:${page}`;
}

// 树压平成可见行：展开状态决定哪些层级露出来，模板只做一层 v-for。
const visibleTreeRows = computed<TreeRow[]>(() => {
  const rows: TreeRow[] = [];
  const expanded = expandedKeys.value;
  const sorted = [...treeHosts.value].sort((a, b) => b.count - a.count || a.name.localeCompare(b.name));
  for (const root of sorted) {
    const key = `h:${root.name}`;
    rows.push({ type: 'host', key, host: root.name, count: root.count, depth: 0 });
    if (!expanded.has(key)) continue;
    appendTreeChildren(root, key, 1, rows, expanded);
  }
  return rows;
});

// 页面分组压平成同样的行结构，模板与结构树共用一套渲染。
const visiblePageRows = computed<TreeRow[]>(() => {
  const rows: TreeRow[] = [];
  const expanded = pageExpanded.value;
  for (const group of pageGroups.value) {
    const key = pageKey(group.page);
    rows.push({ type: 'host', key, host: group.page, count: group.assets.length, depth: 0 });
    if (!expanded.has(key)) continue;
    const leaves = [...group.assets].sort((a, b) => a.method.localeCompare(b.method) || a.id.localeCompare(b.id));
    for (const asset of leaves) {
      // 同一资产可落多个已展开分组，key 必须带分组前缀，否则 v-for 重复 key。
      rows.push({ type: 'leaf', key: `${key}::l:${asset.id}`, asset, name: leafName(asset), depth: 1 });
    }
  }
  return rows;
});

const activeRows = computed<TreeRow[]>(() => (viewMode.value === 'pages' ? visiblePageRows.value : visibleTreeRows.value));

function appendTreeChildren(node: TreeNode, key: string, depth: number, rows: TreeRow[], expanded: Set<string>) {
  const children = [...node.children.values()].sort((a, b) => a.name.localeCompare(b.name));
  for (const child of children) {
    const childKey = `${key}/${child.name}`;
    rows.push({ type: 'path', key: childKey, name: child.name, count: child.count, depth });
    if (expanded.has(childKey)) appendTreeChildren(child, childKey, depth + 1, rows, expanded);
  }
  const leaves = [...node.assets].sort((a, b) => a.method.localeCompare(b.method) || a.id.localeCompare(b.id));
  for (const asset of leaves) {
    rows.push({ type: 'leaf', key: `l:${asset.id}`, asset, name: leafName(asset), depth });
  }
}

// 叶子名取路径最后一段；层级已经由父节点摆出来，行内不再重复整条路径。
function leafName(asset: AssetItem): string {
  const segments = asset.path.split('/').filter(Boolean);
  return segments[segments.length - 1] || asset.path || asset.url;
}

// 展开状态按视图各存一份：树和页面分组的键空间不同，切换视图互不干扰。
function activeExpanded(): Set<string> {
  return viewMode.value === 'pages' ? pageExpanded.value : expandedKeys.value;
}

function toggleGroup(key: string) {
  const current = activeExpanded();
  const next = new Set(current);
  if (next.has(key)) next.delete(key);
  else next.add(key);
  if (viewMode.value === 'pages') pageExpanded.value = next;
  else expandedKeys.value = next;
}

function expandAll() {
  const expanded = new Set<string>();
  if (viewMode.value === 'pages') {
    for (const group of pageGroups.value) expanded.add(pageKey(group.page));
  } else {
    for (const root of treeHosts.value) {
      const key = `h:${root.name}`;
      expanded.add(key);
      collectAllKeys(root, key, expanded);
    }
  }
  if (viewMode.value === 'pages') pageExpanded.value = expanded;
  else expandedKeys.value = expanded;
}

function collectAllKeys(node: TreeNode, key: string, into: Set<string>) {
  for (const child of node.children.values()) {
    const childKey = `${key}/${child.name}`;
    into.add(childKey);
    collectAllKeys(child, childKey, into);
  }
}

function collapseAll() {
  if (viewMode.value === 'pages') pageExpanded.value = new Set();
  else expandedKeys.value = new Set();
}

async function refreshCounts() {
  const scopes = ['', ...KINDS.map((item) => item.key)];
  const next: Record<string, number> = {};
  try {
    await Promise.all(scopes.map(async (scope) => {
      const result = await backend.call<AssetsPage>('assets.list', listParams(0, scope, 1));
      next[scope] = result?.ok === false ? 0 : (result?.total ?? 0);
    }));
    if (!disposed) kindCounts.value = next;
  } catch {
    // 计数只是 chips 的展示值：读不到就保留上一份，不打断列表。
  }
}

// 构建轮询用：数据可能在增长，表格保持当前页。
function refreshActive() {
  if (viewMode.value === 'table') {
    treeStale.value = true;
    void loadAssets(page.value);
  } else {
    void loadTree();
  }
}

// 筛选/构建完成等口径变化用：表格回第 1 页——按旧页码查新口径会出现
// 「第 1 / 1 页但列表为空」的错位。
function reload() {
  if (viewMode.value === 'table') {
    treeStale.value = true;
    void loadAssets(1);
  } else {
    void loadTree();
  }
  void refreshCounts();
}

function switchView(mode: 'tree' | 'pages' | 'table') {
  if (viewMode.value === mode) return;
  viewMode.value = mode;
  listError.value = '';
  if (mode === 'table') {
    void loadAssets(1);
    return;
  }
  // 树/页面分组共用同一份全量数据：没拉过、或表格期间筛选已变（stale），都要重拉。
  if (!treeLoaded.value || treeStale.value) void loadTree();
}

// 汇总 chips 是唯一的类型筛选入口：同值不重查，换值即刷新列表。
function pickKind(key: string) {
  if (kindFilter.value === key) return;
  kindFilter.value = key;
  reload();
}

function selectHost(event: Event) {
  hostFilter.value = (event.target as HTMLSelectElement).value;
  reload();
}

function goToPage(target: number) {
  const next = Math.min(Math.max(1, Math.floor(target)), pageCount.value);
  if (next === page.value) return;
  void loadAssets(next);
}

watch(keyword, () => {
  if (keywordTimer) clearTimeout(keywordTimer);
  keywordTimer = setTimeout(() => {
    keywordTimer = undefined;
    reload();
  }, KEYWORD_DEBOUNCE_MS);
});

// ---- assets.scan：异步受理，进度走 assets_progress，完成以事件为主、task 事件兜底 ----
async function startBuild() {
  if (building.value) return;
  buildError.value = '';
  buildNotice.value = '';
  const params: Record<string, unknown> = { includeTraffic: includeTraffic.value };
  // 清单按目标小程序构建：流量只取它的记录，构建结果也归档到它名下。
  if (targetAppID.value) params.appid = targetAppID.value;
  if (buildDir.value.trim()) params.dir = buildDir.value.trim();
  const functions = cloudFnsText.value.split(/[\s,，;；]+/).map((line) => line.trim()).filter(Boolean);
  if (functions.length) params.cloudFns = functions;
  // 云函数名随目标记入 config：下次构建同一小程序自动回填。
  if (targetAppID.value) {
    cloudFnsByApp.value = { ...cloudFnsByApp.value, [targetAppID.value]: cloudFnsText.value };
    persistTarget();
  }
  try {
    const accepted = await backend.call<ScanAccepted>('assets.scan', params);
    if (disposed) return;
    // 无来源（既没有反编译目录也没有流量）等后端拒绝：原样亮出来，不静默。
    if (accepted?.ok === false) {
      buildError.value = accepted.error || '构建失败';
      return;
    }
    building.value = true;
    buildTaskId.value = accepted.taskId ?? '';
    buildProgress.current = 0;
    buildProgress.total = 0;
    buildProgress.message = '';
    startBuildPolling();
  } catch (reason) {
    if (!disposed) buildError.value = messageOf(reason);
  }
}

function startBuildPolling() {
  if (buildTimer || disposed) return;
  buildTimer = setInterval(() => { refreshActive(); }, BUILD_POLL_MS);
}

function stopBuildPolling() {
  if (buildTimer) clearInterval(buildTimer);
  buildTimer = undefined;
}

function finishBuild(success: boolean, message = '') {
  building.value = false;
  stopBuildPolling();
  if (success) {
    buildNotice.value = '资产清单构建完成';
    notify('资产清单构建完成', 'success');
  } else {
    buildError.value = message || '构建失败';
  }
  reload();
}

function onAssetsProgress(event: AssetsProgressEvent) {
  buildProgress.current = event.current ?? 0;
  buildProgress.total = event.total ?? 0;
  buildProgress.message = event.message ?? '';
  if (event.status === 'working') {
    building.value = true;
    startBuildPolling();
    return;
  }
  finishBuild(event.status === 'done', event.message);
}

// 全局 task 帧：assets_progress 没到时由它宣布完成 / 失败（按 taskId 认领）。
function onTaskEvent(payload: TaskEvent) {
  if (!building.value || !buildTaskId.value || payload?.id !== buildTaskId.value) return;
  if (payload.phase === 'done') finishBuild(true);
  else if (payload.phase === 'failed' || payload.phase === 'cancelled') {
    finishBuild(false, payload.error || payload.message || '构建失败');
  }
}

// ---- assets.export：save:true 直接落盘，取消与成功都明说 ----
// 格式直接读事件目标（TrafficView 同款做法：v-model 的 change 与这里的 @change 谁先跑
// 不保证），导出完成后把下拉复位到占位项。
async function exportAssets(event: Event) {
  const target = event.target as HTMLSelectElement;
  const format = target.value;
  if (!format || exporting.value) return;
  exporting.value = true;
  exportNotice.value = '';
  buildError.value = '';
  const params: Record<string, unknown> = { format, save: true };
  if (kindFilter.value) params.kind = kindFilter.value;
  if (hostFilter.value) params.host = hostFilter.value;
  // 导出与视图同一筛选口径：界面上看到的范围就是导出的范围。
  try {
    const result = await backend.call<ExportResult>('assets.export', params);
    if (disposed) return;
    if (result?.ok) {
      exportNotice.value = `已保存到 ${result.path ?? ''}`;
      notify(`资产清单已保存到 ${result.path ?? ''}`, 'success');
    } else if (result?.reason) {
      exportNotice.value = `已取消导出：${result.reason}`;
    } else {
      notify(result?.error || '导出失败', 'error');
    }
  } catch (reason) {
    if (!disposed) notify(`导出失败：${messageOf(reason)}`, 'error');
  } finally {
    if (!disposed) {
      exporting.value = false;
      target.value = '';
    }
  }
}

// ---- 行详情：完整 URL + tags + sources（同一时间只开一条）----
function toggleExpand(id: string) {
  expandedId.value = expandedId.value === id ? '' : id;
}

// ---- 目标小程序：跟随连接、目录对齐、持久化 ----
function optionLabel(option: AppIDOption): string {
  const base = option.name ? `${option.name}（${option.appid}）` : option.appid;
  return option.connected ? `${base} · 已连接` : base;
}

// 反编译目录对齐目标——清单是单个小程序的档案，代码来源必须也是这个程序：
// 1) 目标有产物 → 精确取 output/<目标appid>（appid 匹配，不看 mtime）；
// 2) 目标没有产物 → 目录若是别的程序的产物则清空，绝不把别家代码扫进这份
//    清单（流量已按目标收窄，代码来源也要同口径）；手输的自定义路径不是
//    已知产物，视为有意填写，不动；
// 3) 未指定目标 → 保持旧口径：目录为空时退回最近 mtime 的产物。
function alignDirToTarget() {
  if (targetAppID.value) {
    const match = knownProjects.value.find((project) => project.appid === targetAppID.value && project.path);
    if (match?.path) {
      buildDir.value = match.path;
      return;
    }
    const current = buildDir.value.trim();
    if (current) {
      const currentMatch = knownProjects.value.find((project) => project.path === current);
      if (currentMatch?.appid && currentMatch.appid !== targetAppID.value) buildDir.value = '';
    }
    return;
  }
  const known = knownProjects.value.filter((project) => !!project.path);
  if (!buildDir.value.trim() && known.length) {
    buildDir.value = known.reduce((a, b) => ((b.mtime ?? 0) > (a.mtime ?? 0) ? b : a)).path ?? '';
  }
}

// 目标与云函数名写入 config.json：重启后据此恢复上次的目标与存档。空目标
// 不覆盖——选回「未指定」不该抹掉上次的小程序。
function persistTarget() {
  const appid = targetAppID.value;
  if (!appid) return;
  void backend
    .call('config.save', {
      assetTargetAppID: appid,
      assetCloudFns: { ...cloudFnsByApp.value, [appid]: cloudFnsText.value },
    })
    .catch(() => { /* 持久化失败只影响下次启动的回填 */ });
}

// 目标小程序的三处来源：当前连接的身份、流量库里出现过的 appid、已反编译
// 的产物目录（自带 appid 与昵称）。并集去重后进下拉。
async function bootstrapTarget() {
  const [configRes, projectsRes, appidRes] = await Promise.allSettled([
    backend.call<Record<string, unknown>>('config.load'),
    backend.call<{ projects?: Array<{ appid?: string; name?: string; path?: string; mtime?: number }> }>('code.projects'),
    backend.call<Array<{ appid: string; count?: number; lastSeen?: string }>>('traffic.appids'),
  ]);
  if (disposed) return;
  knownProjects.value = projectsRes.status === 'fulfilled' ? projectsRes.value?.projects ?? [] : [];
  const options = new Map<string, AppIDOption>();
  const connectedAppID = engine.status.appInfo?.appid || '';
  if (connectedAppID) {
    options.set(connectedAppID, { appid: connectedAppID, name: engine.status.appInfo?.name, connected: true });
  }
  if (appidRes.status === 'fulfilled' && Array.isArray(appidRes.value)) {
    for (const stat of appidRes.value) {
      if (!stat?.appid) continue;
      options.set(stat.appid, { ...(options.get(stat.appid) ?? { appid: stat.appid }), count: stat.count });
    }
  }
  for (const project of knownProjects.value) {
    if (!project.appid) continue;
    const option = options.get(project.appid) ?? { appid: project.appid };
    option.name = option.name || project.name;
    options.set(project.appid, option);
  }
  // 已连接的排最前，其余按 appid 稳定排序。
  appOptions.value = [...options.values()].sort((a, b) => Number(!!b.connected) - Number(!!a.connected) || a.appid.localeCompare(b.appid));

  const savedConfig = configRes.status === 'fulfilled' ? configRes.value ?? {} : {};
  const savedAppID = String(savedConfig.assetTargetAppID ?? '');
  cloudFnsByApp.value = (savedConfig.assetCloudFns as Record<string, string> | undefined) ?? {};
  // 上次目标即使已无流量/产物也要出现在下拉里，否则看不见也选不回。
  if (savedAppID && !options.has(savedAppID)) options.set(savedAppID, { appid: savedAppID });
  // 已连接的排最前，其余按 appid 稳定排序。
  appOptions.value = [...options.values()].sort((a, b) => Number(!!b.connected) - Number(!!a.connected) || a.appid.localeCompare(b.appid));

  // 目标优先级：当前连接 > 上次保存 > 空（跨程序兜底口径）。
  targetAppID.value = connectedAppID || savedAppID;
  const savedFns = targetAppID.value ? cloudFnsByApp.value[targetAppID.value] : '';
  if (savedFns && !cloudFnsText.value.trim()) cloudFnsText.value = savedFns;
  alignDirToTarget();
}

onMounted(() => {
  reload();
  stopAssetsProgress = backend.on<AssetsProgressEvent>('assets_progress', onAssetsProgress);
  stopTaskEvents = backend.on<TaskEvent>('task', onTaskEvent);
  void bootstrapTarget();
});

// 连接身份变化即跟随：目标始终对上正在调试（或最近调试）的小程序。
// 新身份若不在选项里（页面开着时新连了一个小程序），先补进下拉再切换。
watch(() => engine.status.appInfo?.appid, (appid) => {
  if (!appid || appid === targetAppID.value) return;
  if (!appOptions.value.some((option) => option.appid === appid)) {
    appOptions.value = [{ appid, name: engine.status.appInfo?.name, connected: true }, ...appOptions.value];
  }
  targetAppID.value = appid;
});

watch(targetAppID, (appid, previous) => {
  if (appid === previous) return;
  // 切换目标时回填该小程序上次用过的云函数名；没有存过的目标保持现状。
  const savedFns = appid ? cloudFnsByApp.value[appid] : undefined;
  if (savedFns) cloudFnsText.value = savedFns;
  alignDirToTarget();
  persistTarget();
});

onBeforeUnmount(() => {
  disposed = true;
  if (keywordTimer) clearTimeout(keywordTimer);
  stopBuildPolling();
  stopAssetsProgress();
  stopTaskEvents();
});
</script>

<template>
  <section class="assets-view" aria-labelledby="assets-title">
    <PageHeader title="资产清单" title-id="assets-title" />

    <!-- 构建卡片：无标题，单行紧凑，竖线分段 -->
    <div class="panel build-panel">
      <div class="panel-body">
        <div class="build-grid">
          <label class="field inline-field" for="assets-target"><span>目标小程序</span>
            <select id="assets-target" v-model="targetAppID" data-testid="assets-target" title="清单按该小程序构建：流量只取它的记录，反编译目录自动对齐；当前连接的小程序会自动选中">
              <option value="">未指定（流量跨程序）</option>
              <option v-for="option in appOptions" :key="option.appid" :value="option.appid">{{ optionLabel(option) }}</option>
            </select>
          </label>
          <span class="build-sep" aria-hidden="true"></span>
          <label class="field inline-field grow-dir" for="assets-dir"><span>反编译目录</span>
            <input id="assets-dir" v-model="buildDir" data-testid="assets-dir" placeholder="留空则仅从抓包流量构建" title="反编译产物目录，例如 extract 输出目录">
          </label>
          <span class="build-sep" aria-hidden="true"></span>
          <label class="check-inline" for="assets-include-traffic">
            <input id="assets-include-traffic" v-model="includeTraffic" data-testid="assets-include-traffic" type="checkbox">
            包含流量
          </label>
          <span class="build-sep" aria-hidden="true"></span>
          <label class="field inline-field grow-fns" for="assets-cloud-fns"><span>云函数（可选）</span>
            <input id="assets-cloud-fns" v-model="cloudFnsText" data-testid="assets-cloud-fns" placeholder="如 login, getOrder" spellcheck="false" title="多个云函数用逗号或换行分隔">
          </label>
          <button data-testid="assets-build" type="button" :disabled="building" @click="startBuild">{{ building ? '构建中…' : '构建清单' }}</button>
        </div>
        <p v-if="builtHint" data-testid="assets-built-hint" class="status-line built-hint">{{ builtHint }}</p>
        <div v-if="building" class="progress-line" data-testid="assets-progress">
          <progress :value="buildProgress.current" :max="buildProgress.total || 1" />
          <span class="status-line" data-testid="assets-progress-text">{{ buildProgress.current }}/{{ buildProgress.total }}{{ buildProgress.message ? ` · ${buildProgress.message}` : '' }}</span>
        </div>
        <p v-if="buildError" data-testid="assets-build-error" class="error error-line" role="alert">{{ buildError }}</p>
        <p v-if="buildNotice" data-testid="assets-build-notice" class="status-line notice-line" role="status">{{ buildNotice }}</p>
      </div>
    </div>

    <!-- 资产面板：视图切换 + 筛选 + 汇总 chips + 结构树/明细表 + 导出 -->
    <div class="panel assets-panel">
      <header class="panel-header">
        <div class="chip-row" role="group" aria-label="资产汇总，点击按类型筛选">
          <button type="button" class="chip" :class="{ active: kindFilter === '' }" :aria-pressed="kindFilter === ''" data-testid="assets-chip-total" @click="pickKind('')">全部 <span class="chip-count">{{ kindCounts[''] ?? total }}</span></button>
          <button v-for="kind in KINDS" :key="kind.key" type="button" class="chip" :class="{ active: kindFilter === kind.key }" :aria-pressed="kindFilter === kind.key" :data-testid="`assets-chip-${kind.key}`" @click="pickKind(kind.key)">
            {{ kind.label }} <span class="chip-count">{{ kindCounts[kind.key] ?? 0 }}</span>
          </button>
        </div>
        <div class="toolbar panel-actions">
          <div class="view-toggle" role="group" aria-label="视图切换">
            <button data-testid="assets-view-tree" type="button" :class="{ active: viewMode === 'tree' }" @click="switchView('tree')">结构树</button>
            <button data-testid="assets-view-pages" type="button" :class="{ active: viewMode === 'pages' }" @click="switchView('pages')">按页面</button>
            <button data-testid="assets-view-table" type="button" :class="{ active: viewMode === 'table' }" @click="switchView('table')">明细表</button>
          </div>
          <label class="check-inline">导出
            <select data-testid="assets-export-format" aria-label="选择导出格式" :disabled="exporting" @change="exportAssets">
              <option value="">选择格式…</option>
              <option v-for="format in EXPORT_FORMATS" :key="format.key" :value="format.key">{{ format.label }}</option>
            </select>
          </label>
        </div>
      </header>

      <header class="panel-header assets-filters">
        <span class="filter-label">筛选</span>
        <div class="toolbar filter-tools">
          <select :value="hostFilter" data-testid="assets-host-filter" aria-label="按主机筛选" @change="selectHost">
            <option value="">全部主机</option>
            <option v-for="entry in hosts" :key="entry.host" :value="entry.host">{{ entry.host }}（{{ entry.count }}）</option>
          </select>
          <input v-model="keyword" data-testid="assets-keyword" class="record-filter" type="search" placeholder="URL / 路径关键词" aria-label="关键词筛选">
        </div>
      </header>

      <p v-if="exportNotice" data-testid="assets-export-notice" class="status-line notice-line panel-notice" role="status">{{ exportNotice }}</p>
      <ErrorState v-if="listError" class="panel-notice" :message="listError" retryable @retry="reload" />

      <!-- 结构树 / 按页面：两视图共用同一套行渲染，靠 activeRows 切换内容 -->
      <template v-if="viewMode !== 'table'">
        <div class="tree-toolbar" data-testid="assets-tree-toolbar">
          <span class="status-line" data-testid="assets-total">共 {{ total }} 条</span>
          <span v-if="treeLoading" class="status-line" data-testid="assets-tree-loading">读取中…</span>
          <span v-if="treeTruncated > 0" class="status-line warn-line" data-testid="assets-tree-truncated">
            清单过大，{{ viewMode === 'pages' ? '页面分组' : '结构树' }}只展示前 {{ TREE_MAX_ITEMS }} 条；可用筛选缩小范围或切换明细表
          </span>
          <span class="tree-spacer"></span>
          <button class="ghost small" type="button" @click="expandAll">全部展开</button>
          <button class="ghost small" type="button" @click="collapseAll">全部收起</button>
        </div>
        <div v-if="activeRows.length" class="tree-wrap" :data-testid="viewMode === 'pages' ? 'assets-pages' : 'assets-tree'">
          <template v-for="row in activeRows" :key="row.key">
            <div
              v-if="row.type === 'host'"
              class="tree-row host-row"
              role="button"
              tabindex="0"
              :aria-expanded="(viewMode === 'pages' ? pageExpanded : expandedKeys).has(row.key)"
              :style="{ paddingLeft: `${0.7 + row.depth * 1.1}rem` }"
              @click="toggleGroup(row.key)"
              @keydown.enter.prevent="toggleGroup(row.key)"
              @keydown.space.prevent="toggleGroup(row.key)"
            >
              <span class="tree-twist">{{ (viewMode === 'pages' ? pageExpanded : expandedKeys).has(row.key) ? '▾' : '▸' }}</span>
              <span class="mono tree-name"><HighlightText :text="row.host" :term="keyword" /></span>
              <span class="tree-count">{{ row.count }}</span>
            </div>
            <div
              v-else-if="row.type === 'path'"
              class="tree-row path-row"
              role="button"
              tabindex="0"
              :aria-expanded="expandedKeys.has(row.key)"
              :style="{ paddingLeft: `${0.7 + row.depth * 1.1}rem` }"
              @click="toggleGroup(row.key)"
              @keydown.enter.prevent="toggleGroup(row.key)"
              @keydown.space.prevent="toggleGroup(row.key)"
            >
              <span class="tree-twist">{{ expandedKeys.has(row.key) ? '▾' : '▸' }}</span>
              <span class="mono tree-name"><HighlightText :text="row.name" :term="keyword" /></span>
              <span class="tree-count">{{ row.count }}</span>
            </div>
            <div v-else class="tree-row leaf-row" :data-testid="`assets-row-${row.asset.id}`" :style="{ paddingLeft: `${0.7 + row.depth * 1.1}rem` }">
              <span class="kind-tag" :class="row.asset.kind">{{ kindLabel(row.asset.kind) }}</span>
              <span class="record-badge mono" :class="methodClass(row.asset.method)">{{ row.asset.method || '—' }}</span>
              <span class="mono tree-name leaf-name" :title="row.asset.url"><HighlightText :text="row.name" :term="keyword" /></span>
              <span class="tree-count mono">×{{ row.asset.hits }}</span>
              <span class="leaf-time">{{ formatLastSeen(row.asset.lastSeen) }}</span>
              <button class="ghost small" :data-testid="`assets-expand-${row.asset.id}`" type="button" @click="toggleExpand(row.asset.id)">{{ expandedId === row.asset.id ? '收起' : '详情' }}</button>
              <AssetDetail v-if="expandedId === row.asset.id" class="leaf-detail" :asset="row.asset" :data-testid="`assets-detail-${row.asset.id}`" />
            </div>
          </template>
        </div>
        <EmptyState
          v-else-if="!listError && !treeLoading && !listLoading"
          title="还没有资产"
          description="先点击上方「构建清单」，从反编译目录或抓包流量构建；或放宽筛选条件后再试。"
        />
      </template>

      <!-- 明细表：平铺分页，走后端 offset -->
      <template v-else>
        <div v-if="items.length" class="table-wrap">
          <table class="data-table assets-table" data-testid="assets-table">
            <thead>
              <tr>
                <th>类型</th>
                <th>方法</th>
                <th>主机</th>
                <th>路径</th>
                <th class="num">命中</th>
                <th class="num">来源</th>
                <th>最近出现</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              <template v-for="asset in items" :key="asset.id">
                <tr :data-testid="`assets-row-${asset.id}`">
                  <td><span class="kind-tag" :class="asset.kind">{{ kindLabel(asset.kind) }}</span></td>
                  <td class="mono"><span class="record-badge" :class="methodClass(asset.method)">{{ asset.method || '—' }}</span></td>
                  <td class="mono" :title="asset.host"><HighlightText :text="asset.host" :term="keyword" /></td>
                  <td class="mono cell-path" :title="asset.url"><HighlightText :text="asset.path" :term="keyword" /></td>
                  <td class="mono num">{{ asset.hits }}</td>
                  <td class="mono num">{{ asset.sources.length }}</td>
                  <td :title="asset.lastSeen">{{ formatLastSeen(asset.lastSeen) }}</td>
                  <td>
                    <button class="ghost small" :data-testid="`assets-expand-${asset.id}`" type="button" @click="toggleExpand(asset.id)">
                      {{ expandedId === asset.id ? '收起' : '展开' }}
                    </button>
                  </td>
                </tr>
                <tr v-if="expandedId === asset.id" class="detail-row">
                  <td colspan="8">
                    <AssetDetail :asset="asset" :data-testid="`assets-detail-${asset.id}`" />
                  </td>
                </tr>
              </template>
            </tbody>
          </table>
        </div>
        <EmptyState
          v-else-if="!listError && !listLoading"
          title="还没有资产"
          description="先点击上方「构建清单」，从反编译目录或抓包流量构建；或放宽筛选条件后再试。"
        />

        <nav v-if="total > 0" class="list-pager" data-testid="assets-pager" aria-label="资产分页">
          <span class="status-line subnav-count" data-testid="assets-total">共 {{ total }} 条</span>
          <button class="ghost small" data-testid="assets-page-prev" type="button" :disabled="page <= 1 || listLoading" @click="goToPage(page - 1)">上一页</button>
          <span class="status-line subnav-count" data-testid="assets-page-indicator">第 {{ page }} / {{ pageCount }} 页 · 每页 {{ PAGE_SIZE }} 条</span>
          <button class="ghost small" data-testid="assets-page-next" type="button" :disabled="page >= pageCount || listLoading" @click="goToPage(page + 1)">下一页</button>
        </nav>
      </template>
    </div>
  </section>
</template>

<style scoped>
/* 构建卡片：无标题单行——竖线分隔「目录 | 流量 | 云函数」三段，
   两个输入框按比例吃满剩余宽度，仅极窄窗口才换行 */
.build-panel > .panel-body {
  padding-block: .8rem;
}

.build-grid {
  align-items: center;
  display: flex;
  flex-wrap: wrap;
  gap: .75rem;
}

.inline-field {
  align-items: center;
  display: flex;
  gap: .45rem;
  min-width: 0;
}

.inline-field > span {
  white-space: nowrap;
}

.inline-field input {
  flex: 1 1 auto;
  min-width: 0;
}

/* 目标下拉不参与弹性撑宽：按最宽选项自适应，超长 appid 截到上限 */
.inline-field select {
  flex: 0 1 auto;
  max-width: 16rem;
  width: auto;
}

.grow-dir {
  flex: 2.2 1 17rem;
}

.grow-fns {
  flex: 1.4 1 12rem;
}

.build-sep {
  background: var(--border);
  flex: none;
  height: 1.6rem;
  width: 1px;
}

/* 清单归属提示：构建行下方的小字 */
.built-hint {
  color: var(--muted);
  font-size: .78rem;
  margin: .45rem 0 0;
}

.field > span {
  color: var(--muted);
  font-size: .78rem;
  font-weight: 600;
}

.progress-line {
  align-items: center;
  display: flex;
  gap: .7rem;
  margin-top: .7rem;
}

.progress-line progress {
  flex: 1 1 auto;
}

.panel-actions {
  margin-left: auto;
}

.notice-line,
.error-line,
.panel-notice {
  margin: .75rem 1.1rem 0;
}

/* 筛选行 */
.assets-filters {
  border-top: 1px solid var(--border);
}

.filter-label {
  color: var(--muted);
  flex: none;
  font-size: .85rem;
}

.filter-tools {
  flex: 1 1 auto;
  margin-left: 0;
}

.filter-tools select,
.filter-tools .record-filter {
  width: auto;
}

.record-filter {
  flex: 1 1 14rem;
  max-width: 30rem;
  min-width: 9rem;
}

/* 汇总 chips：点击即按类型筛选，与下方类型下拉同步；计数来自 byKind 探针 */
.chip-row {
  align-items: center;
  display: flex;
  flex-wrap: wrap;
  gap: .35rem;
}

.chip-count {
  color: var(--muted);
  font-family: var(--mono);
  font-size: .72rem;
}

.chip-row .chip.active .chip-count {
  color: inherit;
}

/* 视图切换：分段控件——非激活中性底，激活实心 accent，避免全局按钮紫底喧宾夺主 */
.view-toggle {
  display: inline-flex;
  gap: .25rem;
}

.view-toggle button {
  background: var(--panel-2);
  border: 1px solid var(--border-strong);
  color: var(--muted);
}

.view-toggle button:hover:not(.active) {
  background: var(--panel-3);
  color: var(--text);
}

.view-toggle button.active,
.view-toggle button.active:hover {
  background: var(--accent);
  border-color: var(--accent);
  color: var(--accent-text);
}

/* 结构树 */
.tree-toolbar {
  align-items: center;
  display: flex;
  flex-wrap: wrap;
  gap: .5rem;
  padding: .5rem .7rem 0;
}

.tree-toolbar .status-line {
  color: var(--muted);
  font-size: .78rem;
  margin: 0;
}

.tree-toolbar .warn-line {
  color: var(--warning);
}

.tree-spacer {
  flex: 1 1 auto;
}

.tree-wrap {
  max-height: 34rem;
  overflow: auto;
  padding-bottom: .35rem;
}

.tree-row {
  align-items: baseline;
  display: flex;
  flex-wrap: wrap;
  gap: .5rem;
  padding-bottom: .22rem;
  padding-right: .7rem;
  padding-top: .22rem;
}

.tree-row.host-row {
  background: var(--panel-2);
  cursor: pointer;
  font-weight: 650;
  /* 滚动容器内的置顶行：长树滚动时主机上下文不丢 */
  position: sticky;
  top: 0;
  z-index: 2;
}

.tree-row.host-row:hover,
.tree-row.path-row:hover {
  background: var(--panel-3);
}

.tree-row.leaf-row:hover {
  background: var(--panel-2);
}

.tree-row:focus-visible {
  outline: 2px solid var(--accent);
  outline-offset: -2px;
}

.tree-row.path-row {
  color: var(--muted);
  cursor: pointer;
}

.tree-row.leaf-row {
  align-items: center;
}

.tree-twist {
  color: var(--muted);
  flex: none;
  width: 1em;
}

.tree-name {
  overflow-wrap: anywhere;
}

.tree-name.leaf-name {
  flex: 1 1 6rem;
  min-width: 4rem;
}

.tree-count {
  color: var(--muted);
  flex: none;
  font-family: var(--mono);
  font-size: .74rem;
  font-variant-numeric: tabular-nums;
}

.tree-row.host-row .tree-count,
.tree-row.path-row .tree-count {
  margin-left: auto;
}

.leaf-time {
  color: var(--muted);
  flex: none;
  font-size: .76rem;
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}

.leaf-detail {
  flex: 1 1 100%;
}

/* 资产表（明细表） */
.assets-panel .table-wrap {
  max-height: 34rem;
}

.kind-tag {
  border-radius: 99px;
  display: inline-block;
  font-size: .72rem;
  font-weight: 650;
  padding: .06rem .5rem;
  white-space: nowrap;
}

.kind-tag.api {
  background: var(--accent-soft);
  color: var(--accent-strong);
}

.kind-tag.static {
  background: var(--panel-3);
  color: var(--muted);
}

.kind-tag.ws {
  background: var(--warning-soft);
  color: var(--warning);
}

.kind-tag.cloud {
  background: var(--success-soft);
  color: var(--success);
}

/* 方法徽章配色：与 kind-tag 同一套 soft/strong 搭配 */
.record-badge.method-read {
  background: var(--success-soft);
  color: var(--success);
}

.record-badge.method-write {
  background: var(--accent-soft);
  color: var(--accent-strong);
}

.record-badge.method-update {
  background: var(--warning-soft);
  color: var(--warning);
}

.record-badge.method-delete {
  background: var(--danger-soft);
  color: var(--danger);
}

.cell-path {
  max-width: 24rem;
  overflow-wrap: anywhere;
}

/* 明细表数字列：右对齐 + 等宽数字，位数不同也能竖向对齐 */
.assets-table .num {
  font-variant-numeric: tabular-nums;
  text-align: right;
}

/* 行展开 */
.detail-row > td {
  background: var(--panel-2);
}

/* 导出下拉与分页 */
.panel-actions select {
  width: auto;
}

.list-pager {
  align-items: center;
  border-top: 1px solid var(--border);
  display: flex;
  flex-wrap: wrap;
  gap: .5rem;
  justify-content: flex-end;
  padding: .45rem .7rem;
}

.list-pager .status-line {
  color: var(--muted);
  font-size: .78rem;
  margin: 0 .3rem;
}
</style>
