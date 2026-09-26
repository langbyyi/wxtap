<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue';
import { backend } from '../api/bridge';
import { useEngineStore } from '../stores/engine';
import { RouterLink } from 'vue-router';
import { describeTarget, inspectorUrlForTarget } from '../target-role';
import { WMPF_DEBUG_PORT } from '../ports';
import { messageOf } from '../utils/format';
import { copyText, notify } from '../utils/notify';
import PageHeader from '../components/PageHeader.vue';

type Target = { targetId: string; type?: string; url?: string };
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
const several = computed(() => choices.value.length > 1);
const selectedChoice = computed(() => choices.value.find((item) => item.appid === selectedApp.value));
const currentChoice = computed(() => (several.value ? selectedChoice.value : choices.value[0]));
const cdpLabel = computed(() => {
  if (store.status.devtools) return '已连接';
  if (store.status.frida) return '等待调试器';
  return '未启动';
});
// 目标 id 只在多开（需要指定具体页面）时进 URL；单开时交给 DevTools 自己选。
const browserURL = computed(() => inspectorUrlForTarget(
  Number(store.cdpPort),
  several.value ? selectedChoice.value?.targetId ?? '' : '',
));

async function loadTargets() {
  if (!engineUp.value) {
    choices.value = [];
    selectedApp.value = '';
    return;
  }
  try {
    const [targets, miniapps] = await Promise.all([
      backend.call<{ targets?: Target[]; error?: string }>('targets.list'),
      backend.call<{ list?: { appid?: string; name?: string }[] }>('miniapp.list'),
    ]);
    // 后端把「引擎没起来 / CDP 查询失败」和「真的没有小程序」分开报：空数组
    // 只代表后者，否则页面会把失败说成「尚未检测到小程序」。后端文案里嵌着 Core 的
    // 原文（core error 1000: no miniapp connected），显示前走一遍共用译文。
    error.value = targets?.error ? messageOf(targets.error) : '';
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
    error.value = messageOf(reason);
  }
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
  refreshing.value = false;
  notify(store.status.devtools ? 'CDP 已连接' : cdpLabel.value, store.status.devtools ? 'success' : 'info');
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
});
watch(engineUp, (up) => {
  if (up) void loadTargets();
});
</script>

<template>
  <section class="devtools-view" aria-labelledby="devtools-title">
    <PageHeader title="DevTools" title-id="devtools-title" />

    <div class="panel">
      <header class="panel-header">
        <h2>调试地址</h2>
        <div class="status-row">
          <span class="status-pill" :class="store.status.miniapp ? 'ok' : 'off'">WMPF {{ WMPF_DEBUG_PORT }}：{{ store.status.miniapp ? '已连接' : (store.status.frida ? '等待小程序' : '未启动') }}</span>
          <span class="status-pill" :class="store.status.devtools ? 'ok' : 'off'" data-testid="cdp-state">CDP：{{ cdpLabel }}</span>
          <button class="ghost small" type="button" :disabled="refreshing" @click="refreshStatus">{{ refreshing ? '刷新中…' : '刷新状态' }}</button>
        </div>
      </header>
      <div class="panel-body stack">
        <p v-if="!store.status.frida && !store.status.devtools" class="callout warning">引擎未启动。请先在「状态」中启动。调试器完成连接后，CDP 状态更新为已连接。</p>
        <p v-else-if="!store.status.devtools" class="callout">
          <template v-if="electronPath">
            调试器尚未连接。DevTools 用独立窗口打开，将使用 <code>{{ electronPath }}</code>；devtools://
            地址无法直接在 Chrome/Edge 打开，跨机调试时可先复制地址。
          </template>
          <template v-else>
            调试器尚未连接。{{ electronError || '未找到可用的 Electron。' }}
            <RouterLink to="/settings">在「设置」里指定 Electron 路径</RouterLink>；devtools://
            地址无法直接在 Chrome/Edge 打开，跨机调试时可先复制地址。
          </template>
        </p>

        <div v-if="several" class="mini-choices" data-testid="mini-choices">
          <span>当前有 {{ choices.length }} 个小程序，请选择目标后再复制或打开</span>
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
        <p v-else-if="currentChoice" class="current-mini" data-testid="current-mini">
          当前小程序：<strong>{{ currentName }}</strong>
          <span v-if="currentName !== currentChoice.appid">{{ currentChoice.appid }}</span>
        </p>
        <p v-else-if="engineUp && !error" class="muted">尚未检测到小程序。以下地址对应当前调试端口。</p>

        <div class="browser-url">
          <code data-testid="devtools-browser-url">{{ browserURL }}</code>
          <button v-if="electronPath" data-testid="open-electron" type="button" :disabled="opening || (several && !selectedChoice)" @click="openElectron">{{ opening ? '正在打开…' : '用 Electron 打开' }}</button>
          <button data-testid="open-devtools" class="secondary" type="button" :disabled="several && !selectedChoice" @click="copyAddress">复制地址</button>
        </div>
        <p v-if="error" class="error" role="alert">{{ error }}</p>
      </div>
    </div>
  </section>
</template>

<style scoped>
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

