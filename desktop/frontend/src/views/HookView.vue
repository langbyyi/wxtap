<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import { backend } from '../api/bridge';
import PageHeader from '../components/PageHeader.vue';
import { useEngineStore } from '../stores/engine';
import { messageOf } from '../utils/format';
import { notify } from '../utils/notify';

// lastRun 是壳侧登记的「上一次注入的结局」：成功与否、落定值摘要、耗时。它跨
// realm 保留（realm 重建只让 injected 作废），因为「上次到底跑成什么样」仍然是
// 事实，而用户改脚本时最需要的就是这一行。
type HookRun = { at?: number; ok?: boolean; summary?: string; durationMs?: number };
type Script = {
  filename: string;
  global: boolean;
  injected: boolean;
  mtime?: number;
  stale?: boolean;
  lastRun?: HookRun;
};

const store = useEngineStore();
const connected = computed(() => store.status.miniapp);
const scripts = ref<Script[]>([]);
const search = ref('');
const loading = ref(false);
const injecting = ref('');
const error = ref('');
const scriptDir = ref('');
const filteredScripts = computed(() => {
  const needle = search.value.trim().toLowerCase();
  if (!needle) return scripts.value;
  return scripts.value.filter((script) => script.filename.toLowerCase().includes(needle));
});
const injectedCount = computed(() => scripts.value.filter((script) => script.injected).length);
const globalCount = computed(() => scripts.value.filter((script) => script.global).length);

/** 跑过一次（或者此刻就在页面里）之后，这个按钮的含义就是「再跑一遍」。 */
function injectLabel(script: Script): string {
  return script.injected || script.lastRun ? '重新注入' : '注入';
}

/** 今天只给时刻，更早的注入补上日期：列表里同一列既看时间又看日期会很吵。 */
function formatWhen(unix?: number): string {
  if (!unix) return '';
  const when = new Date(unix * 1000);
  const clock = when.toLocaleTimeString([], { hour12: false });
  if (when.toDateString() === new Date().toDateString()) return clock;
  return `${when.getMonth() + 1}-${when.getDate()} ${clock}`;
}

async function load() {
  loading.value = true;
  error.value = '';
  try {
    // injected / lastRun / stale 全部由后端登记，前端不再自己记忆：
    // 本地标记在小程序重载或注入失败后必然说错话。
    const result = await backend.call<{ scripts?: Script[] }>('hook.list');
    scripts.value = (result.scripts ?? []).map((script) => ({
      filename: script.filename,
      global: script.global === true,
      injected: script.injected === true,
      mtime: script.mtime,
      stale: script.stale === true,
      lastRun: script.lastRun,
    }));
  } catch (value) {
    error.value = messageOf(value);
  } finally {
    loading.value = false;
  }
}

async function loadScriptDir() {
  try {
    const paths = await backend.call<{ hook_scripts?: string }>('settings.getPaths');
    scriptDir.value = paths?.hook_scripts ?? '';
  } catch {
    scriptDir.value = '';
  }
}

async function openScriptDir() {
  if (!scriptDir.value) await loadScriptDir();
  if (!scriptDir.value) {
    notify('未能定位脚本目录', 'error');
    return;
  }
  try {
    await backend.call('shell.openFolder', { path: scriptDir.value });
  } catch (value) {
    notify(`打开目录失败：${messageOf(value)}`, 'error');
  }
}

// 注入了就不再禁止再点：脚本调试本来就是「改文件 → 重注 → 看结果」的循环，
// 而文件每次注入都是重新从磁盘读的，重注零成本。旧实现注入后把按钮禁用，
// 用户改完脚本只能重载小程序才能再跑一次。
async function inject(script: Script) {
  if (!connected.value) {
    notify('需要先连接小程序才能注入脚本', 'error');
    return;
  }
  injecting.value = script.filename;
  let failure = '';
  try {
    const result = await backend.call<{ summary?: string }>('hook.inject', { filename: script.filename });
    notify(`${script.filename} 已注入：${result?.summary ?? '无返回值'}`, 'success');
  } catch (value) {
    failure = messageOf(value);
    notify(`注入失败：${failure}`, 'error');
  } finally {
    // 成败都回读一次：结局、mtime 与 stale 都由后端登记，前端推一遍就会有两份真相。
    // 回读期间按钮保持「注入中…」，否则这半秒里连点两次会注入两遍。
    await load();
    // load() 开头会清掉上一次的错误，本次注入的失败要重新挂上，否则 alert 一闪而过。
    if (failure) error.value = failure;
    injecting.value = '';
  }
}

async function setGlobal(script: Script) {
  try {
    await backend.call('hook.setGlobal', { filename: script.filename, global: script.global });
  } catch (value) {
    script.global = !script.global;
    error.value = messageOf(value);
    notify(`保存全局设置失败：${messageOf(value)}`, 'error');
  }
}

onMounted(() => {
  void load();
  void loadScriptDir();
});
</script>

<template>
  <section class="hook-view" aria-labelledby="hook-title">
    <PageHeader title="注入脚本" title-id="hook-title" />

    <p v-if="error" class="error" role="alert">{{ error }}</p>

    <div class="panel">
      <header class="panel-header script-filter-bar">
        <span
          class="status-pill"
          :class="connected ? 'ok' : 'warn'"
          :title="connected ? '注入会立即在当前页面执行' : '注入需要已连接的小程序；未连接时仍可浏览脚本与设置全局开关'"
        >{{ connected ? '小程序已连接' : '未连接小程序' }}</span>
        <span class="status-pill" data-testid="injected-count">{{ injectedCount }} / {{ scripts.length }} 已注入</span>
        <span v-if="globalCount" class="status-pill" data-testid="global-count">{{ globalCount }} 个全局</span>
        <input v-model="search" type="search" placeholder="搜索脚本文件名" aria-label="搜索 Hook 脚本">
        <span class="subnav-count">{{ filteredScripts.length }} / {{ scripts.length }}</span>
        <RouterLink to="/console" class="ghost small" data-testid="open-console">脚本输出在 Console 页 →</RouterLink>
        <button data-testid="open-script-dir" class="ghost small" type="button" @click="openScriptDir">打开脚本目录</button>
        <button data-testid="refresh-hooks" class="secondary small" type="button" :disabled="loading" @click="load">{{ loading ? '加载中…' : '刷新列表' }}</button>
      </header>
      <ul v-if="filteredScripts.length" class="script-list">
        <li v-for="script in filteredScripts" :key="script.filename" class="script-row">
          <div class="script-info">
            <code class="script-name">{{ script.filename }}</code>
            <span class="status-pill" :class="script.injected ? 'ok' : 'off'">{{ script.injected ? '已注入' : '未注入' }}</span>
            <span
              v-if="script.stale"
              class="status-pill warn"
              :data-testid="`stale-${script.filename}`"
              title="文件在最近一次注入之后被改过：改动还没进小程序，重新注入才生效"
            >文件已更新</span>
          </div>
          <p class="script-run" :data-testid="`run-${script.filename}`">
            <template v-if="script.lastRun">
              <span :class="script.lastRun.ok ? 'run-ok' : 'run-fail'">{{ script.lastRun.ok ? '上次注入成功' : '上次注入失败' }}</span>
              <span v-if="script.lastRun.at"> · {{ formatWhen(script.lastRun.at) }}</span>
              <!-- 负数时长 = 脚本没跑起来（文件读不出来），显示 0ms 会假装它跑过 -->
              <span v-if="(script.lastRun.durationMs ?? -1) >= 0"> · {{ script.lastRun.durationMs }}ms</span>
              <span> · {{ script.lastRun.ok ? '返回' : '错误' }}：</span><code class="script-run-value" :title="script.lastRun.summary">{{ script.lastRun.summary || '无返回值' }}</code>
            </template>
            <span v-else>尚未注入过：点「注入」立刻在当前页面执行一次。</span>
          </p>
          <div class="script-actions">
            <label class="check-inline" title="勾选后，小程序每次重新加载都会自动注入这个脚本">
              <input :data-testid="`global-${script.filename}`" v-model="script.global" type="checkbox" @change="setGlobal(script)"> 全局
            </label>
            <button
              :data-testid="`inject-${script.filename}`"
              class="small"
              type="button"
              :title="connected ? '注入后无法撤回：代码会留在当前页面里，直到小程序重新加载或被切换；届时「已注入」标记也会一并复位' : '需要先连接小程序'"
              :disabled="!connected || injecting === script.filename"
              @click="inject(script)"
            >
              {{ injecting === script.filename ? '注入中…' : injectLabel(script) }}
            </button>
          </div>
        </li>
      </ul>
      <div v-else class="empty-state">
        <strong>{{ scripts.length ? '没有匹配的脚本' : '脚本目录中尚无 .js 文件' }}</strong>
        <span v-if="scripts.length">尝试缩短关键词或清空搜索条件。</span>
        <span v-else>将 .js 文件放入 <code>{{ scriptDir || '脚本目录' }}</code>，然后点击「刷新列表」。</span>
      </div>
    </div>

    <div class="panel">
      <div class="panel-body stack">
        <p class="status-line">
          脚本在<strong>小程序当前页面的 JS 上下文</strong>里直接执行 —— 与在小程序控制台里手动输入等效：可以修改 <code>wx.*</code>、可以向 <code>window</code> 挂载钩子，但没有 DOM 可以操作。
          已执行的代码无法撤回，只能等待小程序重新加载（届时「已注入」标记一并复位，「上次注入」的记录仍在）。
        </p>
        <p class="status-line">
          脚本里的 <code>console.log</code> 会带 <code>[文件名]</code> 前缀进 Console 页，报错也归到 <code>wxtap-user-script/文件名</code> 名下。
          注入时脚本被包在一层作用域里，所以 <code>var</code> / <code>function</code> 不会成为全局变量 —— 若需全局可见，请显式写 <code>window.x = ...</code>。
        </p>
        <p class="status-line">勾选「全局」后，小程序每次重新加载都会自动注入；脚本目录里新增或修改文件后点击「刷新列表」重新读取。</p>
      </div>
    </div>
  </section>
</template>

<style scoped>
.script-list {
  display: grid;
  list-style: none;
  margin: 0;
  padding: .35rem;
}

.script-row {
  align-items: center;
  border-radius: var(--radius-sm);
  display: grid;
  gap: .4rem .75rem;
  grid-template-columns: 1fr auto;
  padding: .55rem .7rem;
  transition: background .1s ease;
}

.script-row + .script-row {
  border-top: 1px solid var(--border);
}

.script-row:hover {
  background: var(--panel-2);
}

.script-info {
  align-items: center;
  display: flex;
  flex-wrap: wrap;
  gap: .6rem;
  grid-column: 1;
  grid-row: 1;
  min-width: 0;
}

.script-name {
  font-family: var(--mono);
  font-size: .85rem;
  overflow-wrap: anywhere;
}

/* 注入结局占一整行：返回值/错误文本要能读，挤在按钮旁边只会被截成省略号。 */
.script-run {
  color: var(--muted);
  font-size: .8rem;
  grid-column: 1 / -1;
  grid-row: 2;
  margin: 0;
  min-width: 0;
}

.script-run-value {
  font-family: var(--mono);
  overflow-wrap: anywhere;
}

.run-ok { color: var(--success); }
.run-fail { color: var(--danger); }

.script-actions {
  align-items: center;
  display: flex;
  flex-wrap: wrap;
  gap: .6rem;
  grid-column: 2;
  grid-row: 1;
}

.script-filter-bar input {
  max-width: 24rem;
}

.empty-state code {
  font-family: var(--mono);
  overflow-wrap: anywhere;
}
</style>
