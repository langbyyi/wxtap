<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue';
import { backend } from '../api/bridge';
import ConfirmDialog from '../components/ConfirmDialog.vue';
import { messageOf, prettyJson } from '../utils/format';
import { copyText, notify } from '../utils/notify';
import PageHeader from '../components/PageHeader.vue';
import ExtractLogPanel from '../components/extract/ExtractLogPanel.vue';
import ExtractSelectionPanel from '../components/extract/ExtractSelectionPanel.vue';
import ExtractFindingsPanel from '../components/extract/ExtractFindingsPanel.vue';
import type { ExtractApp as App, ExtractFinding as Finding, ExtractPackage as Package } from '../components/extract/types';

// 这里只声明页面真正读的字段：后端还给了 subject / icon_* / metadata_source /
// indexed / status，界面一个都不显示，不必在模型里中转。
type InventoryItem = { appid: string; name?: string; icon_data_url?: string; package_paths: string[]; decompiled: boolean; output_dir: string; mtime?: number; unsupported?: boolean };
type InventorySummary = { package_count: number };
type DirCandidate = { os: string; version: string; path: string; exists: boolean; selected: boolean; description: string };
type CleanupRequest =
  | { kind: 'delete'; app: App; title: string; message: string }
  | { kind: 'clear'; clearType: 'decompiled' | 'applet'; title: string; message: string };

const directory = ref('');
const apps = ref<App[]>([]);
const inventorySummary = ref<InventorySummary>({ package_count: 0 });
const dirCandidates = ref<DirCandidate[]>([]);
const status = ref('');
const error = ref('');
const progress = ref(0);
const running = ref('');
const findings = ref<Finding[]>([]);
const findingQuery = ref('');
const findingPage = ref(1);
// 每页条数按档位可调：命中上千条时，50/页要翻几十页才看完一遍。
const findingsPerPage = ref(50);
const resultApp = ref('');
const resultOutputDir = ref('');
const categoryFilter = ref('all');
const logs = ref<string[]>([]);
let loadedConfig: Record<string, unknown> = {};
let stopProgress = () => {};
let stopDone = () => {};
let stopScanDone = () => {};
let stopLog = () => {};
let stopAppInfo = () => {};
const cleanupRequest = ref<CleanupRequest | null>(null);
const appQuery = ref('');

// WeChat 4.x keeps every logged-in account inside its own hash directory,
// so the user directory is what pins down an Applet package path.
type AccountUser = { id: string; dir: string; appids: string[]; packages_dir?: string };
const accountUsers = ref<AccountUser[]>([]);
const pickedUser = ref('');

const visibleFindings = computed(() => {
  const needle = findingQuery.value.trim().toLowerCase();
  return findings.value.filter((finding) => {
    if (categoryFilter.value !== 'all' && finding.category !== categoryFilter.value) return false;
    if (!needle) return true;
    // value 是表格「匹配结果」列显示的内容：查找必须覆盖它，否则用户照着表里
    // 看到的匹配值去搜反而搜不到。
    return [finding.title, finding.category, finding.file, finding.value, finding.masked, finding.privilege ?? '']
      .some((value) => value.toLowerCase().includes(needle));
  });
});
const findingCategories = computed(() => {
  const categoryOrder = ['path', 'secret', 'oss', 'key', 'static', 'url', 'mobile', 'mail', 'sfz', 'jdbc', 'jwt'];
  const orderOf = (key: string) => {
    const index = categoryOrder.indexOf(key);
    return index < 0 ? categoryOrder.length : index;
  };
  const counts = new Map<string, number>();
  for (const finding of findings.value) counts.set(finding.category, (counts.get(finding.category) ?? 0) + 1);
  return [...counts.entries()]
    .map(([key, count]) => ({ key, label: categoryLabel(key), count }))
    .sort((left, right) => right.count - left.count || orderOf(left.key) - orderOf(right.key) || left.label.localeCompare(right.label));
});
const pagedFindings = computed(() => visibleFindings.value.slice((findingPage.value - 1) * findingsPerPage.value, findingPage.value * findingsPerPage.value));
const findingPageCount = computed(() => Math.max(1, Math.ceil(visibleFindings.value.length / findingsPerPage.value)));
const visibleApps = computed(() => {
  const needle = appQuery.value.trim().toLowerCase();
  // 列表只含有可反编译包的小程序（面板据此不再处理「原始包未缓存」的状态）；产物还在
  // 但原始包已被微信清理掉的小程序不进这个列表，页面模板还原不了的（后端标 unsupported，
  // 例如微信新版编译模板运行时生成的）也不进——列表就是总闸门，不在里面就不会走后续流程。
  const compilable = apps.value.filter((app) => app.packages.length > 0 && !app.unsupported);
  if (!needle) return compilable;
  return compilable.filter((app) => app.appid.toLowerCase().includes(needle) || app.name.toLowerCase().includes(needle));
});
const actionableApps = computed(() => apps.value.filter((app) => app.packages.length > 0 && !app.unsupported));

// A hand-typed or auto-detected directory must not leave a stale user selected.
watch([() => directory.value, () => accountUsers.value], syncPickedUser);
watch([categoryFilter, findingQuery], () => { findingPage.value = 1; });

// Detection feeds the header picker; the same user directory is what pins
// down an Applet package path on WeChat 4.x installs.
async function detectAccounts() {
  try {
    const res = await backend.call<{ dir: string; exists: boolean }>('sessionkey.detect');
    if (!res.exists) {
      accountUsers.value = [];
      return;
    }
    const list = await backend.call<{ users: AccountUser[] }>('sessionkey.users');
    accountUsers.value = list.users ?? [];
    await refresh();
  } catch (reason) {
    error.value = `检测本机微信账号失败: ${messageOf(reason)}`;
  }
}

function sameDir(left: string, right: string) {
  const normalize = (value: string) => value.trim().replace(/[\\/]+$/, '').replace(/\\/g, '/').toLowerCase();
  return !!left && !!right && normalize(left) === normalize(right);
}

// Keep the picker honest when the directory was typed or auto-detected
// instead of chosen from the list.
function syncPickedUser() {
  const match = accountUsers.value.find((user) => user.packages_dir && sameDir(user.packages_dir, directory.value));
  pickedUser.value = match?.id ?? '';
}

async function applyUser() {
  const user = accountUsers.value.find((item) => item.id === pickedUser.value);
  if (!user?.packages_dir) return;
  await selectDirectory(user.packages_dir, `已切换到用户 ${user.id} 的包目录`);
}

// Escape drops the pointer/keyboard reveal without closing the page.
function dismissHelp(event: KeyboardEvent) {
  (event.target as HTMLElement | null)?.blur();
}

function grouped(items: Package[]) {
  const byId = new Map<string, App>();
  for (const pkg of items) {
    const old = byId.get(pkg.appid);
    if (old) {
      old.packages.push(pkg);
      old.mtime = Math.max(old.mtime, pkg.mtime ?? 0);
      old.decompiled ||= !!pkg.decompiled;
      old.outputDir ||= pkg.output_dir ?? '';
      old.unsupported ||= !!pkg.unsupported;
    } else {
      byId.set(pkg.appid, {
        appid: pkg.appid,
        name: pkg.name || pkg.appid,
        iconDataURL: '',
        iconBroken: false,
        packages: [pkg],
        decompiled: !!pkg.decompiled,
        scanned: false,
        mtime: pkg.mtime ?? 0,
        outputDir: pkg.output_dir ?? '',
        unsupported: !!pkg.unsupported,
      });
    }
  }
  return [...byId.values()].sort((a, b) => b.mtime - a.mtime);
}

async function save() {
  await backend.call('config.save', {
    ...loadedConfig,
    extract_packages_dir: directory.value,
  });
}

async function refresh() {
  error.value = '';
  status.value = '';
  try {
    const user = accountUsers.value.find((item) => item.id === pickedUser.value)
      ?? accountUsers.value.find((item) => item.packages_dir && sameDir(item.packages_dir, directory.value));
    const data = await backend.call<{ items?: InventoryItem[]; summary?: InventorySummary }>('extract.inventory', { dir: directory.value, user_dir: user?.dir ?? '' });
    if (!data.items) {
      const listing = await backend.call<{ packages?: Package[] }>('extract.packages', { dir: directory.value });
      apps.value = grouped(listing.packages ?? []);
      // 与「可反编译」同一个判据：还原不了的小程序不计进数量。
      inventorySummary.value = { package_count: apps.value.filter((app) => !app.unsupported).length };
      return;
    }
    apps.value = data.items.map((item) => ({
      appid: item.appid, name: item.name || item.appid,
      iconDataURL: item.icon_data_url || '',
      iconBroken: false,
      packages: item.package_paths.map((path) => ({ appid: item.appid, path })),
      decompiled: item.decompiled, scanned: !!scanStore.value[item.appid], mtime: item.mtime ?? 0, outputDir: item.output_dir,
      unsupported: !!item.unsupported,
    }));
    inventorySummary.value = data.summary ?? { package_count: 0 };
  } catch (reason) {
    error.value = `扫描目录失败: ${messageOf(reason)}`;
  }
}

async function chooseDirectory() {
  try {
    const data = await backend.call<{ dir?: string }>('extract.browse', { dir: directory.value });
    if (data.dir) {
      directory.value = data.dir;
      await save();
      await refresh();
    }
  } catch (reason) {
    error.value = `选择目录失败: ${messageOf(reason)}`;
  }
}

async function loadCandidates() {
  try {
    const data = await backend.call<{ dirs?: DirCandidate[]; default?: string }>('extract.candidateDirs');
    dirCandidates.value = data.dirs ?? [];
    return data;
  } catch {
    dirCandidates.value = [];
    return {};
  }
}

// 切换包目录的唯一入口：写入目录、持久化、重新加载小程序列表。
async function selectDirectory(path: string, message: string) {
  clearResults();
  directory.value = path;
  await save();
  await refresh();
  notify(message, 'success');
}

async function useCandidate(candidate: DirCandidate) {
  if (!candidate.exists) return;
  await selectDirectory(candidate.path, `已选择 ${candidate.version}`);
}

async function decompileAll() {
  if (running.value || !actionableApps.value.length) return;
  error.value = '';
  running.value = 'all';
  progress.value = 0;
  status.value = `正在反编译全部 ${actionableApps.value.length} 个可用小程序包`;
  try {
    const data = await backend.call<{
      results?: Array<{ appid: string; ok: boolean; name?: string; files_count?: number; error?: string }>;
      succeeded?: number;
      failed?: number;
    }>('extract.decompileAll', { dir: directory.value });
    for (const item of data.results ?? []) {
      const app = apps.value.find((candidate) => candidate.appid === item.appid);
      if (app && item.ok) {
        app.decompiled = true;
        if (item.name) app.name = item.name;
      }
      if (item.error) logs.value.push(`${item.appid}: ${item.error}`);
    }
    await refresh();
    progress.value = 100;
    const succeeded = data.succeeded ?? 0;
    const failed = data.failed ?? 0;
    status.value = `反编译完成：成功 ${succeeded} 个，失败 ${failed} 个`;
    notify(status.value, failed ? 'error' : 'success');
  } catch (reason) {
    error.value = `批量反编译失败: ${messageOf(reason)}`;
    notify(error.value, 'error');
  } finally {
    if (running.value === 'all') running.value = '';
  }
}

async function decompile(app: App) {
  if (running.value) return;
  error.value = '';
  running.value = app.appid;
  progress.value = 0;
  status.value = `正在反编译 ${app.appid}`;
  try {
    const data = await backend.call<{ files_count?: number; name?: string }>('extract.decompile', {
      dir: directory.value,
      appid: app.appid,
    });
    app.decompiled = true;
    if (data.name) app.name = data.name;
    progress.value = 100;
    status.value = `反编译完成：${data.files_count ?? 0} 个文件`;
    notify(status.value, 'success');
  } catch (reason) {
    error.value = `反编译失败: ${messageOf(reason)}`;
    notify(error.value, 'error');
  } finally {
    if (running.value === app.appid) running.value = '';
  }
}

async function scan(app: App) {
  if (running.value && running.value !== app.appid) return;
  if (!app.decompiled) { error.value = '请先反编译此小程序'; return; }
  error.value = '';
  running.value = app.appid;
  progress.value = 0;
  status.value = `正在扫描 ${app.appid}`;
  try {
    await backend.call('extract.scan', { dir: directory.value, appid: app.appid });
  } catch (reason) {
    error.value = `扫描失败: ${messageOf(reason)}`;
    running.value = '';
  }
}

// 提取结果只保留在内存里：扫描完成即刷新右侧面板；扫过的小程序可以直接
// 用「查看提取结果」切回去，没扫过的需要重扫。
type ScanPayload = { findings?: Finding[]; output_dir?: string };
const scanStore = ref<Record<string, ScanPayload>>({});

function applyScanResult(appid: string, data: ScanPayload) {
  findings.value = data.findings ?? [];
  categoryFilter.value = 'all';
  findingQuery.value = '';
  findingPage.value = 1;
  resultOutputDir.value = data.output_dir ?? '';
  resultApp.value = appid;
}

// 切换入口：把本次会话里已扫描过的结果换到右侧面板，不触发重新扫描。
function showResults(app: App) {
  error.value = '';
  const data = scanStore.value[app.appid];
  if (!data) {
    status.value = `${app.appid} 本次会话尚未扫描`;
    return;
  }
  applyScanResult(app.appid, data);
}

function clearResults() {
  findings.value = [];
  resultOutputDir.value = '';
  resultApp.value = '';
}
function requestRemove(app: App) {
  cleanupRequest.value = {
    kind: 'delete',
    app,
    title: '删除小程序',
    message: `删除 ${app.appid} 的原始包和解包结果？`,
  };
}

function requestClear(clearType: 'decompiled' | 'applet') {
  const label = clearType === 'decompiled' ? '所有解包和扫描数据' : 'Applet 目录中的全部文件';
  cleanupRequest.value = {
    kind: 'clear',
    clearType,
    title: '清理输出',
    message: `确定清理${label}？此操作不可恢复。`,
  };
}

async function confirmCleanup() {
  const request = cleanupRequest.value;
  if (!request) return;
  cleanupRequest.value = null;
  try {
    if (request.kind === 'delete') {
      await backend.call('extract.delete', { dir: directory.value, appid: request.app.appid });
      delete scanStore.value[request.app.appid];
      if (resultApp.value === request.app.appid) clearResults();
      await refresh();
      return;
    }

    await backend.call('extract.clearOutput', { type: request.clearType, dir: directory.value });
    if (request.clearType === 'applet') apps.value = [];
    else apps.value.forEach((app) => { app.decompiled = false; app.scanned = false; });
    scanStore.value = {};
    clearResults();
    status.value = '清理完成';
    notify(status.value, 'success');
  } catch (reason) {
    error.value = request.kind === 'delete'
      ? `删除失败: ${messageOf(reason)}`
      : `清理失败: ${messageOf(reason)}`;
    notify(error.value, 'error');
  }
}

async function copyLogs() {
  if (!logs.value.length) return;
  await copyText(logs.value.join('\n'), '解包日志');
}

function clearLogs() {
  logs.value = [];
  notify('解包日志已清空');
}

async function copyResults() {
  if (!visibleFindings.value.length) {
    notify('没有可复制的扫描结果', 'error');
    return;
  }
  await copyText(prettyJson(visibleFindings.value), '当前筛选结果');
}

// 输出目录是按小程序划分的，入口放在列表每一行，不再提供页面级按钮。
async function openAppOutput(app: App) {
  if (!app.outputDir) return;
  try {
    await backend.call('extract.openDir', { path: app.outputDir });
    notify(`已打开 ${app.name} 的输出目录`);
  } catch (reason) {
    error.value = `打开目录失败: ${messageOf(reason)}`;
    notify(error.value, 'error');
  }
}

// 内置分类的中文名；自定义分类原样显示。
const categoryLabels: Record<string, string> = {
  secret: '账号密码', jwt: 'JWT', url: 'URL', path: 'Path',
  mobile: '手机号', mail: '邮箱', sfz: '身份证', oss: '媒体资源', key: 'key', jdbc: 'JDBC', static: '静态资源',
};

function categoryLabel(key: string) {
  return categoryLabels[key] ?? key;
}

function formatTime(mtime: number) {
  if (!mtime) return '';
  const date = new Date(mtime * 1000);
  const pad = (value: number) => String(value).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}/${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

// 反编译输出目录交给代码浏览器，hash 路由下用查询串传递。value 是行内精确
// 标记用的匹配原文（hash 不出网，且该值本就显示在结果面板里）；压缩成一整行
// 的产物只有靠它才能看清匹配落点。
function viewSource(finding: Finding) {
  if (!resultOutputDir.value) return;
  const params = new URLSearchParams({ root: resultOutputDir.value, file: finding.file, line: String(finding.line), value: finding.value.slice(0, 300) });
  window.location.hash = `#/code?${params.toString()}`;
}

function primaryLabel(app: App) {
  if (!app.decompiled) return '反编译';
  return scanStore.value[app.appid] ? '查看' : '扫描';
}

function runPrimary(app: App) {
  if (!app.decompiled) { void decompile(app); return; }
  if (scanStore.value[app.appid]) { showResults(app); return; }
  void scan(app);
}

// 上面的列表只含有可反编译包的小程序，所以这里只剩这三种状态。
function appStatusLabel(app: App) {
  if (app.scanned) return '已扫描';
  if (app.decompiled) return '已反编译';
  return '待反编译';
}

function setFindingPage(page: number) {
  findingPage.value = Math.min(Math.max(1, page), findingPageCount.value);
}

// 换每页大小就回第一页：否则当前页可能已越界（第 5 页 × 200 条 > 总条数），
// 会渲染出一张空表，看着像「没有命中」。
function setFindingPageSize(size: number) {
  if (!Number.isFinite(size) || size <= 0 || size === findingsPerPage.value) return;
  findingsPerPage.value = size;
  findingPage.value = 1;
}

async function copyFinding(finding: Finding) {
  await copyText(finding.value, '匹配结果');
}

async function initialize() {
  try {
    const config = await backend.call<Record<string, unknown>>('config.load');
    loadedConfig = config;
    directory.value = String(config.extract_packages_dir ?? '');
    const candidateData = await loadCandidates();
    if (!directory.value) {
      const found = await backend.call<{ dir?: string }>('extract.defaultDir');
      directory.value = found.dir || candidateData.default || '';
    }
    await refresh();
  } catch (reason) {
    error.value = `初始化失败: ${messageOf(reason)}`;
  }
}

onMounted(() => {
  stopAppInfo = backend.on<{ appid?: string; name?: string; icon?: string }>('app_info', (event) => {
    const app = apps.value.find((item) => item.appid === event.appid);
    if (!app) return;
    if (event.name && event.name !== app.appid) app.name = event.name;
    if (event.icon) {
      app.iconDataURL = event.icon;
      app.iconBroken = false;
    }
  });
  stopProgress = backend.on<{ percent?: number; text?: string }>('extract_progress', (event) => {
    progress.value = event.percent ?? progress.value;
    status.value = event.text ?? status.value;
  });
  // 后端反编译失败走 promise 拒绝（单个）或 ok:false + failed 计数（批量），不会带 error 字段。
  stopDone = backend.on<{ appid?: string }>('extract_done', () => {
    running.value = '';
    void refresh();
  });
  // 扫描失败以后端事件而非 promise 拒绝报错：这里必须把失败态落到界面，
  // 否则小程序会被误标成「已扫描：0 条匹配结果」。
  stopScanDone = backend.on<{ appid?: string; error?: string; findings?: Finding[]; output_dir?: string; findings_count?: number; result?: { files_scanned?: number } }>('extract_scan_done', (event) => {
    if (event.error) {
      error.value = `扫描失败: ${event.error}`;
      running.value = '';
      notify(error.value, 'error');
      return;
    }
    const app = apps.value.find((item) => item.appid === event.appid);
    if (app) app.scanned = true;
    // 扫描完成即把明细填进右侧面板，不需要再点一次“查看结果”。
    if (event.appid) {
      const payload = { findings: event.findings, output_dir: event.output_dir };
      scanStore.value[event.appid] = payload;
      applyScanResult(event.appid, payload);
    }
    running.value = '';
    progress.value = 100;
    status.value = `扫描完成：${event.result?.files_scanned ?? 0} 个文件，${event.findings_count ?? 0} 条匹配结果`;
    notify(status.value, 'success');
  });
  stopLog = backend.on<{ message?: string }>('extract_log', (event) => {
    if (event.message) logs.value.push(event.message);
  });
  void initialize();
  void detectAccounts();
});

onBeforeUnmount(() => {
  stopAppInfo();
  stopProgress();
  stopDone();
  stopScanDone();
  stopLog();
});
</script>

<template>
  <section class="extract-view page-workbench" aria-labelledby="extract-title">
    <PageHeader title="反编译" title-id="extract-title" />
    <div class="stack">
      <div class="panel directory-panel">
        <header class="panel-header">
          <h2>包目录</h2>
          <div class="toolbar directory-actions" data-testid="directory-actions">
            <div data-testid="inventory-summary">
              <select
                v-model="pickedUser"
                class="user-picker"
                data-testid="user-directory"
                aria-label="本机微信用户目录"
                :disabled="!accountUsers.length"
                @change="applyUser"
              >
                <option value="">{{ accountUsers.length ? '选择本机用户目录' : '未检测到本机微信账号' }}</option>
                <option v-for="user in accountUsers" :key="user.id" :value="user.id" :disabled="!user.packages_dir">
                  {{ user.id }}{{ user.packages_dir ? '' : '（无包目录）' }}
                </option>
              </select>
              <button class="secondary small" type="button" data-testid="refresh-accounts" @click="detectAccounts">重新检测</button>
              <span class="status-pill">{{ inventorySummary.package_count }} 个小程序</span>
            </div>
            <button class="small" data-testid="browse-extract" type="button" @click="chooseDirectory">选择 Applet</button>
            <button data-testid="refresh-extract" class="secondary small" type="button" @click="refresh">刷新</button>
            <button data-testid="clear-decompiled" class="danger small" type="button" @click="requestClear('decompiled')">清空反编译文件</button>
            <button data-testid="clear-applet" class="danger small" type="button" @click="requestClear('applet')">清空 Applet 目录</button>
          </div>
        </header>
        <div class="panel-body stack">
          <div class="field">
            <div class="field-row">
              <input id="extract-directory" data-testid="extract-directory" aria-label="包目录" v-model="directory" @change="save(); refresh()">
              <div class="path-help">
                <button
                  type="button"
                  class="help-dot"
                  aria-label="微信 Applet 目录说明"
                  data-testid="applet-dir-help"
                  @keydown.esc="dismissHelp"
                >?</button>
                <div class="path-help-pop">
                  <div class="path-help-body">
                    <p>微信 Applet 目录下一般包含如下文件：<code>./wx..../100/__APP__.wxapkg</code></p>
                    <p>Windows v3：<code>C:\Users\xxx\Documents\WeChat Files\Applet</code>（手动定位：PC 微信 → 设置 → 文件管理）</p>
                    <p>Windows v4：<code>C:\Users\xxx\AppData\Roaming\Tencent\xwechat\radium\Applet\packages</code></p>
                    <p>Windows v4 最新版：<code>C:\Users\xxx\AppData\Roaming\Tencent\xwechat\radium\users\xxx_32位随机字符_xxx\Applet\packages</code></p>
                    <p>macOS v3：<code>/Users/xxx/Library/Containers/com.tencent.xinWeChat/Data/.wxapplet/packages</code></p>
                    <p>macOS v4：<code>/Users/xxx/Library/Containers/com.tencent.xinWeChat/Data/Documents/app_data/radium/Applet/packages</code></p>
                    <p>macOS v4 最新版：<code>/Users/xxx/Library/Containers/com.tencent.xinWeChat/Data/Documents/app_data/radium/users/xxx_32位随机字符_xxx/applet/packages</code></p>
                    <ul v-if="dirCandidates.length" class="candidate-list">
                      <li v-for="candidate in dirCandidates" :key="candidate.path">
                        <div>
                          <strong>{{ candidate.version }}</strong>
                          <span>{{ candidate.description }}</span>
                          <code>{{ candidate.path }}</code>
                        </div>
                        <button class="ghost small" type="button" :disabled="!candidate.exists" @click="useCandidate(candidate)">
                          {{ candidate.exists ? '使用' : '未找到' }}
                        </button>
                      </li>
                    </ul>
                  </div>
                </div>
              </div>
            </div>
          </div>
          <p v-if="error" class="error" role="alert">{{ error }}</p>
          <p v-if="status" class="status-line" role="status">{{ status }}<span v-if="running">（{{ progress }}%）</span></p>
          <div v-if="running" class="progress-track" aria-hidden="true"><div class="progress-fill" :style="{ width: `${progress}%` }" /></div>
        </div>
      </div>

      <div class="extract-workspace">
        <ExtractSelectionPanel
          :apps="visibleApps"
          :result-app="resultApp"
          :query="appQuery"
          :running="running"
          :actionable-count="actionableApps.length"
          :format-time="formatTime"
          :primary-label="primaryLabel"
          :app-status-label="appStatusLabel"
          @update:query="appQuery = $event"
          @decompile-all="decompileAll"
          @run-primary="runPrimary"
          @open-output="openAppOutput"
          @remove="requestRemove"
          @icon-error="($event.iconBroken = true)"
        />

        <ExtractFindingsPanel
          :result-app="resultApp"
          :finding-query="findingQuery"
          :finding-categories="findingCategories"
          :category-filter="categoryFilter"
          :visible-findings="visibleFindings"
          :paged-findings="pagedFindings"
          :finding-page="findingPage"
          :finding-page-count="findingPageCount"
          :page-offset="(findingPage - 1) * findingsPerPage"
          :page-size="findingsPerPage"
          :result-output-dir="resultOutputDir"
          @update:finding-query="findingQuery = $event"
          @update:category-filter="categoryFilter = $event"
          @copy-results="copyResults"
          @copy-finding="copyFinding"
          @view-source="viewSource"
          @set-page="setFindingPage"
          @set-page-size="setFindingPageSize"
        />
      </div>
      <ExtractLogPanel :logs="logs" @copy="copyLogs" @clear="clearLogs" />

    </div>
    <ConfirmDialog
      :open="!!cleanupRequest"
      :title="cleanupRequest?.title ?? ''"
      :message="cleanupRequest?.message ?? ''"
      confirm-text="确认"
      tone="danger"
      test-id="confirm-extract"
      @confirm="confirmCleanup"
      @cancel="cleanupRequest = null"
    />
  </section>
</template>

<style scoped>
/* 包目录整块（表头 + 表体）压到最小：这一页的主戏是下面的小程序列表和结果表，
   这排只用来选目录的控件不该占掉半屏。全局的 .panel-header / .panel-body
   内边距是别的页面也用的，所以这里只在自己的面板上收紧。 */
.directory-panel > .panel-header { gap: .4rem; padding: .45rem .8rem; }
.directory-panel > .panel-body { padding: .55rem .8rem; }
.directory-panel > .panel-body.stack { gap: .4rem; }
.directory-panel .field { gap: .2rem; }
.directory-panel .toolbar { gap: .35rem; }
.directory-panel input { padding: .3rem .55rem; }

[data-testid="inventory-summary"] {
  align-items: center;
  display: grid;
  gap: .45rem;
  grid-template-columns: minmax(14rem, 19rem) auto auto;
}

.user-picker { min-width: 0; width: 100%; }

.directory-actions { justify-content: flex-end; }

@media (max-width: 900px) {
  .directory-actions { justify-content: flex-start; width: 100%; }
  [data-testid="inventory-summary"] { grid-template-columns: minmax(0, 1fr) auto; width: min(100%, 32rem); }
  .user-picker { grid-column: 1 / -1; }
}
.progress-track {
  background: var(--panel-2);
  border: 1px solid var(--border);
  border-radius: 99px;
  height: .5rem;
  overflow: hidden;
}

.progress-fill {
  background: var(--accent);
  height: 100%;
  transition: width .3s ease;
}

.field-row {
  align-items: center;
  display: flex;
  gap: .45rem;
  position: relative;
}

.field-row > input {
  flex: 1;
  min-width: 0;
}

.path-help { flex: none; }

.help-dot {
  align-items: center;
  background: var(--panel-2);
  border: 1px solid var(--border-strong);
  border-radius: 50%;
  color: var(--muted);
  cursor: pointer;
  display: inline-flex;
  font-size: .72rem;
  font-weight: 700;
  height: 1.15rem;
  justify-content: center;
  padding: 0;
  width: 1.15rem;
}

.help-dot:hover:not(:disabled),
.path-help:focus-within .help-dot {
  background: var(--accent-soft);
  border-color: var(--accent);
  color: var(--accent);
}

.path-help-pop {
  display: none;
  left: 0;
  padding-top: .45rem;
  position: absolute;
  right: 0;
  top: 100%;
  z-index: 30;
}

.path-help:hover .path-help-pop,
.path-help:focus-within .path-help-pop {
  display: block;
}

.path-help-body {
  background: var(--panel-2);
  border: 1px solid var(--border-strong);
  border-radius: var(--radius-sm);
  box-shadow: var(--shadow);
  display: grid;
  font-family: var(--mono);
  font-size: .74rem;
  gap: .3rem;
  padding: .6rem .7rem;
}

.path-help-body p {
  color: var(--text);
  font-size: .74rem;
  margin: 0;
  overflow-wrap: anywhere;
}

.path-help-body p code {
  color: var(--muted);
  font-family: var(--mono);
  font-size: .74rem;
}

.candidate-list {
  display: grid;
  gap: .45rem;
  list-style: none;
  margin: .65rem 0 0;
  padding: 0;
}

.candidate-list li {
  align-items: center;
  background: var(--panel-2);
  border-radius: var(--radius-sm);
  display: flex;
  gap: .7rem;
  justify-content: space-between;
  padding: .5rem .65rem;
}

.candidate-list li > div {
  display: grid;
  gap: .08rem;
  min-width: 0;
}

.candidate-list span,
.candidate-list code {
  color: var(--muted);
  font-size: .76rem;
  overflow-wrap: anywhere;
}

/* 这一页的高度模型：包目录是设置区，按内容高就够；剩下的高度全给工作区（左列表 +
   右结果），它才是主要展示区。不这么写的话，grid 的默认 stretch 会把页面多出来的
   高度平摊给两行——包目录下面空一大片，真正要看的结果反而只剩一条。 */
.extract-view > .stack {
  align-content: start;
  grid-template-rows: auto minmax(0, 1fr) auto;
}

.extract-workspace {
  align-items: stretch;
  display: grid;
  gap: .85rem;
  /* 左列宽度按卡片的 appid 定：名称换行会在中间折断（wx1bbbfe598e128d74 断成
     两行）。18 位 appid 约 131px，卡片两侧的图标与按钮吃掉约 208px，所以 23rem
     才留得住一行——从右边的结果表借这点宽度，比让 appid 折行划算。 */
  grid-template-columns: minmax(0, 23rem) minmax(0, 1fr);
  min-height: 0;
}

@media (max-width: 1180px) {
  .extract-workspace { grid-template-columns: 1fr; }
  /* 单列时两栏上下叠，两行都按内容高：结果表给自己一个有限的滚动区，
     表头才会固定在它的顶端，而不是被整页滚动带走。
     这里写的是子组件的根元素（Vue 会把父组件的 scope 也打在子组件根节点上），
     所以 .findings-panel 能命中——不是笔误。 */
  .extract-workspace .findings-panel { max-height: 60vh; }
}
</style>
