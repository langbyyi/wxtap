<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue';
import { backend } from '../api/bridge';
import { useEngineStore } from '../stores/engine';
import { RouterLink } from 'vue-router';
import { describeTarget, inspectorUrlForTarget } from '../target-role';
import { WMPF_DEBUG_PORT } from '../ports';
import { messageOf } from '../utils/format';
import { copyText, notify } from '../utils/notify';
import PageHeader from '../components/PageHeader.vue';
import StateToggle from '../components/StateToggle.vue';

type Target = { targetId: string; type?: string; url?: string; title?: string };
type H5Session = { clientId: number; targetId: string; state: 'connecting' | 'connected' | 'closing' | 'closed' | 'error'; active: boolean; error: string; url: string; title: string };
// version 不再保留：界面从来没有渲染过它，留着只会让人以为在选择器里有构建号。
type MiniChoice = { appid: string; title: string; targetId: string };

const store = useEngineStore();
const error = ref('');
const refreshing = ref(false);
const opening = ref(false);
const electronPath = ref('');
const electronError = ref('');
const choices = ref<MiniChoice[]>([]);
const selectedApp = ref('');
const engineUp = computed(() => store.status.frida || store.status.devtools || store.status.miniapp);
const discoveredTargets = ref<Target[]>([]);
const targetError = ref('');
const targetsLoading = ref(false);
const targetsKnown = ref(false);
let targetsRevision = 0;
const targetSourceId = ref<number | null>(null);
const targetOriginId = ref<number | null>(null);
const targetRole = ref('');
const targetSearch = ref('');
const selectedTargetId = ref('');
const probePending = ref('');
const h5Sessions = ref<Record<string, H5Session>>({});
const h5Pending = ref<Record<string, 'start' | 'stop'>>({});
const h5Errors = ref<Record<string, string>>({});
const sessionsError = ref('');
let sessionsReading = false;
let sessionsRevision = 0;
type ProbeResult = { ok: boolean; text: string; url?: string; title?: string; readyState?: string; released?: boolean };
const probeMessages = ref<Record<string, ProbeResult>>({});
const h5Count = computed(() => discoveredTargets.value.filter((target) => describeTarget(target).role === 'H5 候选').length);
const targetRoles = computed(() => [...new Set(discoveredTargets.value.map((target) => describeTarget(target).role))]);
const visibleTargets = computed(() => {
  const keyword = targetSearch.value.trim().toLowerCase();
  return discoveredTargets.value.filter((target) => (!targetRole.value || describeTarget(target).role === targetRole.value)
    && (!keyword || [target.title, target.url, target.targetId].some((value) => value?.toLowerCase().includes(keyword))));
});
const selectedTarget = computed(() => visibleTargets.value.find((target) => target.targetId === selectedTargetId.value));
const selectedProbe = computed(() => selectedTarget.value ? probeMessages.value[selectedTarget.value.targetId] : undefined);
const selectedSession = computed(() => selectedTarget.value ? h5Sessions.value[selectedTarget.value.targetId] : undefined);

async function loadH5Sessions() {
  const clientId = targetSourceId.value;
  if (sessionsReading || clientId === null || !engineUp.value) return;
  const revision = sessionsRevision;
  sessionsReading = true;
  try {
    const result = await backend.call<{ sessions?: H5Session[] }>('targets.sessions');
    if (revision !== sessionsRevision || clientId !== targetSourceId.value) return;
    h5Sessions.value = Object.fromEntries((result.sessions ?? []).filter((session) => session.clientId === clientId).map((session) => [session.targetId, session]));
    for (const target of discoveredTargets.value) {
      const session = h5Sessions.value[target.targetId];
      if (!session?.active || session.state !== 'connected' || !session.url) continue;
      if (target.url !== session.url || target.title !== session.title) delete probeMessages.value[target.targetId];
      target.url = session.url;
      target.title = session.title ?? '';
    }
    sessionsError.value = '';
  } catch (reason) {
    if (revision === sessionsRevision) sessionsError.value = messageOf(reason);
  } finally { sessionsReading = false; }
}

async function toggleH5(target: Target) {
  const clientId = targetSourceId.value;
  if (clientId === null || h5Pending.value[target.targetId] || probePending.value || !engineUp.value) return;
  const active = h5Sessions.value[target.targetId]?.active === true;
  if (!active && !electronPath.value) return;
  const revision = sessionsRevision;
  h5Pending.value[target.targetId] = active ? 'stop' : 'start';
  delete h5Errors.value[target.targetId];
  try {
    if (active) await backend.call('targets.close', { clientId, targetId: target.targetId });
    else await backend.call('shell.openDevtoolsWindow', { cdp_port: Number(store.cdpPort), electron_path: electronPath.value, client_id: clientId, target_id: target.targetId });
    if (revision === sessionsRevision) await loadH5Sessions();
  } catch (reason) {
    if (revision === sessionsRevision) h5Errors.value[target.targetId] = messageOf(reason);
  } finally {
    if (revision === sessionsRevision) delete h5Pending.value[target.targetId];
  }
}

async function copyTarget() {
  if (!selectedTarget.value) return;
  await copyText(JSON.stringify({ clientId: targetOriginId.value, target: selectedTarget.value,
    verification: selectedProbe.value ?? null, debugging: selectedSession.value ?? null }, null, 2), '目标信息');
}
type PausePolicy = { clientId: number | null; appid: string; name: string; enabled: boolean; known: boolean; busy: boolean; error: string };
const emptyPausePolicy = (): PausePolicy => ({ clientId: null, appid: '', name: '', enabled: false, known: false, busy: false, error: '' });
const pausePolicy = ref<PausePolicy>(emptyPausePolicy());
const pausePending = ref<'' | 'start' | 'stop'>('');
let pauseRevision = 0;
let pauseReading = false;
let pauseTimer: ReturnType<typeof setInterval> | undefined;
const pauseStateLabel = computed(() => pausePending.value || pausePolicy.value.busy ? '正在设置…'
  : !pausePolicy.value.known ? '状态未确认' : pausePolicy.value.enabled ? '已跳过暂停' : '正常暂停');

async function loadPausePolicy() {
  if (pauseReading || pausePending.value) return;
  const revision = pauseRevision;
  if (!engineUp.value) { pausePolicy.value = emptyPausePolicy(); return; }
  pauseReading = true;
  try {
    const result = await backend.call<PausePolicy>('debugger.pausePolicy');
    if (revision === pauseRevision) pausePolicy.value = { ...emptyPausePolicy(), ...result };
  } catch (reason) {
    if (revision === pauseRevision) pausePolicy.value = { ...emptyPausePolicy(), error: messageOf(reason) };
  } finally { pauseReading = false; }
}

async function setPausePolicy(enabled: boolean) {
  const clientId = pausePolicy.value.clientId;
  if (clientId === null || pausePending.value || pausePolicy.value.busy) return;
  const revision = ++pauseRevision;
  const previousEnabled = pausePolicy.value.enabled;
  pausePending.value = enabled ? 'start' : 'stop';
  pausePolicy.value = { ...pausePolicy.value, enabled, known: false, error: '' };
  try {
    const result = await backend.call<PausePolicy>('debugger.pausePolicy', { clientId, enabled });
    if (revision === pauseRevision) pausePolicy.value = result;
  } catch (reason) {
    if (revision === pauseRevision) pausePolicy.value = { ...pausePolicy.value, enabled: previousEnabled, error: messageOf(reason) };
  } finally {
    pausePending.value = '';
  }
}
const several = computed(() => choices.value.length > 1);
const selectedChoice = computed(() => choices.value.find((item) => item.appid === selectedApp.value));
const currentChoice = computed(() => (several.value ? selectedChoice.value : choices.value[0]));
const cdpLabel = computed(() => {
  if (store.status.devtools) return '已连接';
  if (engineUp.value) return '未连接';
  return '未启动';
});
// 目标 id 只在多开（需要指定具体页面）时进 URL；单开时交给 DevTools 自己选。
const browserURL = computed(() => inspectorUrlForTarget(
  Number(store.cdpPort),
  several.value ? selectedChoice.value?.targetId ?? '' : '',
));

async function loadTargets() {
  const revision = ++targetsRevision;
  targetsKnown.value = false;
  targetError.value = '';
  discoveredTargets.value = [];
  choices.value = [];
  targetSourceId.value = null;
  targetOriginId.value = null;
  selectedTargetId.value = '';
  probeMessages.value = {};
  if (!engineUp.value) {
    targetsLoading.value = false;
    selectedApp.value = '';
    return;
  }
  targetsLoading.value = true;
  try {
    const [targets, miniapps] = await Promise.all([
      backend.call<{ targets?: Target[]; error?: string; clientId?: number; locked?: boolean }>('targets.list'),
      backend.call<{ list?: { id?: number; locked?: boolean; appid?: string; name?: string }[] }>('miniapp.list').catch(() => ({ list: [] })),
    ]);
    if (revision !== targetsRevision) return;
    // 后端把「引擎没起来 / CDP 查询失败」和「真的没有小程序」分开报：空数组
    // 只代表后者，否则页面会把失败说成「尚未检测到小程序」。后端文案里嵌着 Core 的
    // 原文（core error 1000: no miniapp connected），显示前走一遍共用译文。
    targetError.value = targets?.error ? messageOf(targets.error) : '';
    if (targetError.value) return;
    discoveredTargets.value = targets.targets ?? [];
    targetsKnown.value = true;
    targetOriginId.value = Number.isSafeInteger(targets.clientId) && (targets.clientId ?? 0) > 0 ? targets.clientId! : null;
    targetSourceId.value = targets.locked === true ? targetOriginId.value : null;
    // 名字取 Core 解析出的身份（__wxConfig 昵称），不取 CDP 目标标题：后者是
    // WMPF 给 appservice 帧起的壳名（AppIndex / GameIndex），不是小程序名。
    const named = new Map<string, string>();
    for (const entry of miniapps?.list ?? []) {
      if (entry.appid && entry.name) named.set(entry.appid, entry.name);
    }
    const seen = new Set<string>();
    const next: MiniChoice[] = [];
    for (const target of targets.targets ?? []) {
      const info = describeTarget(target);
      if (!info.miniappPage || info.appid === '' || seen.has(info.appid)) continue;
      seen.add(info.appid);
      next.push({
        appid: info.appid,
        title: named.get(info.appid) ?? '',
        targetId: target.targetId,
      });
    }
    choices.value = next;
    if (!next.some((item) => item.appid === selectedApp.value)) selectedApp.value = next[0]?.appid ?? '';
  } catch (reason) {
    if (revision === targetsRevision) targetError.value = messageOf(reason);
  } finally {
    if (revision === targetsRevision) targetsLoading.value = false;
  }
}

async function probeH5(target: Target) {
  const clientId = targetSourceId.value;
  if (clientId === null || probePending.value || h5Pending.value[target.targetId] || h5Sessions.value[target.targetId]?.active || !engineUp.value) return;
  const revision = targetsRevision;
  probePending.value = target.targetId;
  delete probeMessages.value[target.targetId];
  try {
    const result = await backend.call<{ clientId: number; targetId: string; verified: boolean; released: boolean; url: string; title: string; readyState: string }>('targets.probe', { clientId, targetId: target.targetId });
    if (revision !== targetsRevision) return;
    if (result?.clientId !== clientId || result.targetId !== target.targetId || result.verified !== true || result.released !== true) {
      throw new Error('H5 验证或临时会话释放未获确认');
    }
    probeMessages.value[target.targetId] = { ok: true, text: `验证成功；临时会话已释放 · ${result.title || result.url} · ${result.readyState}`,
      url: result.url, title: result.title, readyState: result.readyState, released: result.released };
  } catch (reason) {
    if (revision === targetsRevision) probeMessages.value[target.targetId] = { ok: false, text: messageOf(reason) };
  } finally { probePending.value = ''; }
}

/** 显示名一律走 Core 解析出的身份：status 里的 appInfo 是最新的一份（顶栏用的
 *  同一个值），miniapp.list 覆盖多开时其余小程序。目标标题永远不当名字用。 */
function displayName(choice: MiniChoice): string {
  const info = store.status.appInfo;
  if (info?.name && info.appid === choice.appid) return info.name;
  return choice.title || choice.appid;
}

const currentName = computed(() => (currentChoice.value ? displayName(currentChoice.value) : ''));

async function refreshStatus() {
  refreshing.value = true;
  await store.load();
  await loadTargets();
  await loadPausePolicy();
  refreshing.value = false;
  notify(`DevTools ${cdpLabel.value}`, store.status.devtools ? 'success' : 'info');
}

function choose(appid: string) {
  selectedApp.value = appid;
}

async function loadElectron() {
  // Electron 不必先在设置里填：后端按 设置 → PATH → 常见安装位置 解析
  // （electron_runtime.go），这里只把解析结果显示出来。
  try {
    const runtime = await backend.call<{ path?: string; error?: string }>('electron.status');
    electronPath.value = runtime?.path?.trim() ?? '';
    electronError.value = runtime?.error?.trim() ?? '';
  } catch (reason) {
    electronPath.value = '';
    electronError.value = messageOf(reason);
  }
}

async function copyAddress() {
  if (several.value && !selectedChoice.value) return;
  await copyText(browserURL.value, 'DevTools 地址');
}

// Electron 是唯一的窗口打开方式：devtools:// 是特权 scheme，浏览器会丢弃
// 以启动参数传入的 devtools:// 地址（手动粘到地址栏同样大多被拒）。
async function openElectron() {
  if (!electronPath.value || (several.value && !selectedChoice.value)) return;
  opening.value = true;
  error.value = '';
  try {
    await backend.call('shell.openDevtoolsWindow', {
      cdp_port: Number(store.cdpPort),
      electron_path: electronPath.value,
      target_id: several.value ? selectedChoice.value?.targetId ?? '' : '',
    });
    notify('已用 Electron 打开 DevTools', 'success');
  } catch (reason) {
    error.value = messageOf(reason);
    notify(`打开 DevTools 失败：${messageOf(reason)}`, 'error');
  } finally {
    opening.value = false;
  }
}

onMounted(() => {
  void loadElectron();
  if (engineUp.value) void loadTargets();
  void loadPausePolicy();
  pauseTimer = setInterval(() => { void loadPausePolicy(); void loadH5Sessions(); }, 1000);
});
onBeforeUnmount(() => { ++targetsRevision; ++pauseRevision; ++sessionsRevision; clearInterval(pauseTimer); });
watch(targetSourceId, () => {
  ++sessionsRevision; h5Sessions.value = {}; h5Pending.value = {}; h5Errors.value = {}; sessionsError.value = '';
  void loadH5Sessions();
});
watch(engineUp, () => {
  ++pauseRevision;
  pausePolicy.value = emptyPausePolicy();
  void loadPausePolicy();
});
watch([engineUp, () => store.status.miniapp], ([up, connected], [, wasConnected]) => {
  if (up && wasConnected && !connected) {
    ++targetsRevision;
    discoveredTargets.value = [];
    choices.value = [];
    selectedApp.value = '';
    targetsKnown.value = false;
    targetsLoading.value = false;
    targetSourceId.value = null;
    targetOriginId.value = null;
    selectedTargetId.value = '';
    probeMessages.value = {};
    targetError.value = '小程序连接已断开，请重新连接后刷新目标清单。';
    return;
  }
  void loadTargets();
});
watch(() => pausePolicy.value.clientId, (clientId) => {
  if (targetsLoading.value || (targetsKnown.value && clientId !== targetSourceId.value)) void loadTargets();
});
</script>

<template>
  <section class="devtools-view page-workbench" aria-labelledby="devtools-title">
    <PageHeader title="DevTools" title-id="devtools-title" />

    <div class="panel pause-panel">
      <div class="panel-body pause-row">
        <h2>暂停控制</h2>
        <span data-testid="pause-target" :title="pausePolicy.appid">{{ pausePolicy.name || pausePolicy.appid || (pausePolicy.clientId === null ? '未锁定' : `连接 ${pausePolicy.clientId}`) }}</span>
        <span v-if="pausePolicy.known || pausePending || pausePolicy.busy || pausePolicy.error" data-testid="pause-state">{{ pauseStateLabel }}</span>
        <StateToggle test-id="pause-toggle" :active="pausePolicy.known && pausePolicy.enabled" :aria-pressed="pausePolicy.known ? pausePolicy.enabled : 'mixed'" :pending="pausePending || (pausePolicy.busy ? (pausePolicy.enabled ? 'start' : 'stop') : '')" :disabled="!engineUp || pausePolicy.clientId === null" :start-label="pausePolicy.known ? '跳过全部暂停' : '恢复正常暂停'" stop-label="恢复正常暂停" start-title="仅作用于锁定目标；跳过模式会恢复当前暂停，并跳过 debugger、正常断点和异常暂停。状态未确认时先恢复正常暂停。" stop-title="恢复当前锁定目标的正常断点和异常暂停。" starting-label="正在设置…" stopping-label="正在恢复…" @start="setPausePolicy(pausePolicy.known)" @stop="setPausePolicy(false)" />
      </div>
      <p v-if="pausePolicy.error" data-testid="pause-error" class="error pause-error" role="alert">{{ pausePolicy.error }}</p>
    </div>

    <div class="panel">
      <header class="panel-header">
        <h2>调试地址</h2>
        <p v-if="!several && currentChoice" class="current-mini" data-testid="current-mini" :title="currentChoice.appid"><strong>{{ currentName }}</strong></p>
        <div class="status-row">
          <span class="status-pill" :class="store.status.miniapp ? 'ok' : 'off'">WMPF {{ WMPF_DEBUG_PORT }}：{{ store.status.miniapp ? '已连接' : (store.status.frida ? '等待小程序' : '未启动') }}</span>
          <span class="status-pill" :class="store.status.devtools ? 'ok' : 'off'" data-testid="cdp-state">DevTools：{{ cdpLabel }}</span>
          <button class="ghost small" type="button" :disabled="refreshing" @click="refreshStatus">{{ refreshing ? '刷新中…' : '刷新状态' }}</button>
        </div>
      </header>
      <div class="panel-body stack">
        <p v-if="!electronPath && engineUp" class="error">{{ electronError || '未找到可用的 Electron。' }} <RouterLink to="/settings">设置</RouterLink></p>

        <div v-if="several" class="mini-choices" data-testid="mini-choices">
          <span>{{ choices.length }} 个小程序</span>
          <button
            v-for="item in choices"
            :key="item.appid"
            type="button"
            :data-testid="`mini-${item.appid}`"
            :class="{ selected: item.appid === selectedApp }"
            @click="choose(item.appid)"
          >
            <strong>{{ displayName(item) }}</strong>
            <span>{{ item.appid }}</span>
          </button>
        </div>

        <div class="browser-url">
          <code data-testid="devtools-browser-url">{{ browserURL }}</code>
          <button v-if="electronPath" data-testid="open-electron" type="button" :title="electronPath" :disabled="opening || (several && !selectedChoice)" @click="openElectron">{{ opening ? '正在打开…' : '用 Electron 打开' }}</button>
          <button data-testid="open-devtools" class="secondary" type="button" :disabled="several && !selectedChoice" @click="copyAddress">复制地址</button>
        </div>
        <p v-if="error" class="error" role="alert">{{ error }}</p>
      </div>
    </div>
    <div class="panel target-panel" data-testid="target-diagnostics">
      <header class="panel-header target-header">
        <h2>目标诊断</h2>
        <div v-if="targetsKnown && discoveredTargets.length" class="target-filters">
          <select v-model="targetRole" data-testid="target-role-filter" aria-label="目标类型"><option value="">全部类型</option><option v-for="role in targetRoles" :key="role" :value="role">{{ role }}</option></select>
          <input v-model="targetSearch" data-testid="target-search" type="search" placeholder="标题 / URL / ID" aria-label="搜索目标">
        </div>
        <span v-if="targetsKnown" data-testid="target-summary">{{ targetRole || targetSearch.trim() ? `${visibleTargets.length} / ` : '' }}{{ discoveredTargets.length }} 个目标 · {{ h5Count }} 个 H5 候选</span>
      </header>
      <div class="panel-body stack">
        <p v-if="!engineUp" class="muted">未连接</p>
        <p v-else-if="targetsLoading" role="status">正在读取目标…</p>
        <p v-else-if="targetError" class="error" role="alert">{{ targetError }}</p>
        <template v-else-if="targetsKnown">
          <div v-if="visibleTargets.length" class="target-table">
            <table class="data-table">
              <colgroup><col class="col-type"><col class="col-title"><col><col class="col-id"><col class="col-action"></colgroup>
              <thead><tr><th scope="col">类型</th><th scope="col">标题</th><th scope="col">URL</th><th scope="col">目标 ID</th><th scope="col" title="验证通过不表示常驻调试已连接；调试会打开选定 H5 的独立 DevTools 窗口。">操作</th></tr></thead>
              <tbody><tr v-for="target in visibleTargets" :key="target.targetId" data-testid="target-row" :class="{ selected: selectedTargetId === target.targetId }">
                <td :title="describeTarget(target).role">{{ describeTarget(target).role }}</td><td :title="target.title"><button class="target-title" type="button" :data-testid="`target-select-${target.targetId}`" :aria-pressed="selectedTargetId === target.targetId" @click="selectedTargetId = selectedTargetId === target.targetId ? '' : target.targetId">{{ target.title || '无标题' }}</button></td><td :title="target.url"><code>{{ target.url || '无 URL' }}</code></td><td :title="target.targetId"><code>{{ target.targetId }}</code></td>
                <td><template v-if="describeTarget(target).role === 'H5 候选' || h5Sessions[target.targetId]?.active">
                  <StateToggle class="small" :test-id="`target-debug-${target.targetId}`" :active="h5Sessions[target.targetId]?.active === true" :pending="h5Pending[target.targetId] || (h5Sessions[target.targetId]?.state === 'connecting' ? 'start' : h5Sessions[target.targetId]?.state === 'closing' ? 'stop' : '')" :disabled="targetSourceId === null || !!probePending || (!h5Sessions[target.targetId]?.active && !electronPath)" start-label="调试" :stop-label="h5Sessions[target.targetId]?.state === 'error' ? '重试释放' : '断开'" starting-label="连接中…" stopping-label="释放中…" :start-title="electronPath ? '打开选定 H5 的独立 DevTools' : '未找到 Electron'" stop-title="关闭此 H5 调试连接并确认会话释放" @start="toggleH5(target)" @stop="toggleH5(target)" />
                  <button v-if="describeTarget(target).role === 'H5 候选'" class="secondary small" type="button" :data-testid="`target-probe-${target.targetId}`" :title="targetSourceId === null ? '未锁定小程序连接' : '只读验证临时 H5 会话并释放'" :disabled="targetSourceId === null || !!probePending || !!h5Pending[target.targetId] || h5Sessions[target.targetId]?.active" @click="probeH5(target)">{{ probePending === target.targetId ? '验证中…' : '验证' }}</button>
                  <span v-if="h5Errors[target.targetId] || h5Sessions[target.targetId]?.error" class="probe-result error" role="alert" :title="h5Errors[target.targetId] || h5Sessions[target.targetId]?.error">失败</span>
                  <span v-if="probeMessages[target.targetId]" class="probe-result" :title="probeMessages[target.targetId]!.text" :aria-label="probeMessages[target.targetId]!.text" :role="probeMessages[target.targetId]!.ok ? 'status' : 'alert'" :class="{ error: !probeMessages[target.targetId]!.ok }">{{ probeMessages[target.targetId]!.ok ? '通过' : '失败' }}</span>
                </template><span v-else class="muted">—</span></td>
              </tr></tbody>
            </table>
          </div>
          <p v-else-if="discoveredTargets.length" class="muted">无匹配目标</p>
          <p v-if="sessionsError" class="error" role="alert">{{ sessionsError }}</p>
          <div v-if="selectedTarget" data-testid="target-detail" class="target-detail">
            <div class="detail-heading"><strong>{{ selectedProbe?.ok ? selectedProbe.title || selectedTarget.title || '无标题' : selectedTarget.title || '无标题' }}</strong><button class="secondary small" data-testid="target-copy" type="button" @click="copyTarget">复制信息</button></div>
            <dl>
              <div><dt>目标</dt><dd><code>{{ selectedTarget.targetId }}</code> · {{ selectedTarget.type }} · 连接 {{ targetOriginId ?? '—' }}</dd></div>
              <template v-if="selectedSession"><div><dt>调试</dt><dd :class="{ error: selectedSession.error }">{{ selectedSession.error || ({ connecting: '连接中', connected: '已连接', closing: '释放中', closed: '已释放', error: '连接失败' }[selectedSession.state]) }}</dd></div><div v-if="selectedSession.url"><dt>调试 URL</dt><dd><code>{{ selectedSession.url }}</code></dd></div></template>
              <div v-if="h5Errors[selectedTarget.targetId]"><dt>调试</dt><dd class="error">{{ h5Errors[selectedTarget.targetId] }}</dd></div>
              <template v-if="selectedProbe?.ok"><div><dt>实际 URL</dt><dd><code>{{ selectedProbe.url }}</code></dd></div><div><dt>加载状态</dt><dd>{{ selectedProbe.readyState }} · 会话已释放</dd></div></template>
              <div v-else-if="selectedProbe"><dt>验证</dt><dd class="error">{{ selectedProbe.text }}</dd></div>
              <div><dt>URL</dt><dd><code>{{ selectedTarget.url || '—' }}</code></dd></div>
            </dl>
          </div>
        </template>
      </div>
    </div>
  </section>
</template>

<style scoped>
.devtools-view { display: flex; flex-direction: column; min-height: 0; }
.devtools-view > .panel { margin: 0; flex: none; }
.devtools-view > .panel + .panel { margin-top: .6rem; }
.devtools-view .panel-header { padding: .5rem .8rem; gap: .6rem; }
.devtools-view .panel-body { padding: .5rem .8rem; gap: .4rem; }
.pause-row { display: flex; align-items: center; gap: .7rem; }
.pause-row h2 { margin: 0; font-size: 1rem; white-space: nowrap; }
.pause-row [data-testid="pause-target"] { margin-right: auto; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.pause-row [data-testid="pause-state"] { margin-left: auto; white-space: nowrap; color: var(--muted); }
.pause-row button { flex: none; }
.pause-error { margin: 0; padding: 0 .8rem .5rem; }
.devtools-view > .target-panel { display: flex; flex-direction: column; flex: 1 1 auto; min-height: 0; }
.target-panel .panel-body { display: flex; flex-direction: column; flex: 1 1 auto; min-height: 0; overflow: hidden; }
.target-table { overflow: auto; min-height: 0; flex: 1 1 auto; }
.target-table table { width: 100%; min-width: 50rem; table-layout: fixed; }
.target-table .col-type { width: 6rem; }
.target-table .col-title { width: 20%; }
.target-table .col-id { width: 16rem; }
.target-table .col-action { width: 12rem; }
.target-table td .small + .small { margin-left: .3rem; }
.target-table th { position: sticky; top: 0; background: var(--panel); z-index: 1; }
.target-table td, .target-table th { padding: .3rem .5rem; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.target-table td code { font-size: .78rem; }
.target-header { flex-wrap: wrap; }
.target-header > span { margin-left: auto; font-size: .82rem; white-space: nowrap; }
.target-filters { display: flex; gap: .4rem; min-width: 0; }
.target-filters input { width: 12rem; min-width: 0; }
.target-filters input, .target-filters select { padding: .25rem .4rem; font-size: .82rem; }
.target-title { display: block; width: 100%; padding: 0; border: 0; background: transparent; color: inherit; text-align: left; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font: inherit; }
.target-title:hover { color: var(--accent); }
.target-table tr.selected { background: var(--accent-soft); }
.target-detail { flex: none; max-height: 40%; overflow: auto; border-top: 1px solid var(--border); padding-top: .4rem; font-size: .82rem; }
.detail-heading { display: flex; align-items: center; gap: .5rem; }
.detail-heading strong { flex: 1; min-width: 0; overflow-wrap: anywhere; }
.target-detail dl { margin: .3rem 0 0; }
.target-detail dl > div { display: flex; gap: .6rem; margin-top: .2rem; }
.target-detail dt { flex: none; width: 4rem; color: var(--muted); }
.target-detail dd { margin: 0; min-width: 0; overflow-wrap: anywhere; }
.probe-result { margin-left: .4rem; font-size: .8rem; }
.status-row,
.browser-url {
  align-items: center;
  display: flex;
  gap: .6rem;
}

.browser-url {
  align-items: stretch;
}

.current-mini,
.muted {
  margin: 0;
}

.current-mini span,
.muted {
  color: var(--muted);
}

.current-mini span {
  font-family: var(--mono);
  font-size: .82rem;
  margin-left: .4rem;
}

.browser-url code {
  flex: 1;
  min-width: 0;
  overflow-wrap: anywhere;
}

.mini-choices {
  display: flex;
  flex-wrap: wrap;
  gap: .5rem;
}

.mini-choices > span {
  align-self: center;
  color: var(--muted);
  font-size: .82rem;
}

.mini-choices button {
  display: grid;
  justify-items: start;
  text-align: left;
}

.mini-choices button.selected {
  border-color: var(--accent);
}

.mini-choices button span {
  color: var(--muted);
  font-family: var(--mono);
  font-size: .75rem;
}
</style>

