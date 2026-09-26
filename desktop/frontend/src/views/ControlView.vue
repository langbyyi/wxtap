<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue';
import { RouterLink } from 'vue-router';
import { useEngineStore, LOG_LIMIT, type LogEntry } from '../stores/engine';
import { copyText, notify } from '../utils/notify';
import LogPanel, { type LogRow } from '../components/LogPanel.vue';
import TaskProgress from '../components/TaskProgress.vue';
import PageHeader from '../components/PageHeader.vue';
import ErrorState from '../components/ErrorState.vue';
import StateToggle from '../components/StateToggle.vue';

const store = useEngineStore();

// 暂停时冻结当前快照给用户细看；store 继续收日志（自身有 500 条上限），恢复时切回实时流。
// 删掉「自动滚动」后，没有暂停就无法在日志洪流里读一条 error。
const paused = ref(false);
const frozenLogs = ref<LogEntry[]>([]);
const pauseSeq = ref(0);
const sourceLogs = computed(() => (paused.value ? frozenLogs.value : store.logs));
const versionSupport = computed(() => {
  const host = store.wechatHost;
  if (!host.version) {
    return { label: '未检测', ok: false, title: host.note || '尚未读取到微信构建号' };
  }
  const file = `addresses.${host.version}.json`;
  if (host.addressTable) {
    return { label: '支持', ok: true, title: `构建 ${host.version} · PID ${host.pid} · ${file}` };
  }
  const missing = `构建 ${host.version} · PID ${host.pid} · 缺少 ${file}`;
  return { label: '不支持', ok: false, title: host.note ? `${missing}。${host.note}` : missing };
});
// 面板负责筛选与呈现，这里只把缓冲映射成行（先前的筛选/级别计数与 Console 页
// 是两份几乎一样的实现，已收进共享 LogPanel）。
const logRows = computed<LogRow[]>(() => sourceLogs.value.map((entry, index) => ({
  key: entry.seq ?? `row-${index}`,
  time: entry.time,
  level: (entry.level ?? 'info').toLowerCase(),
  text: entry.message,
})));

// 暂停期间到达的新条数。按 seq 比较而非长度——日志超限会从头裁剪，长度可能不升反降
const pendingCount = computed(() => {
  if (!paused.value) return 0;
  return store.logs.filter((entry) => (entry.seq ?? 0) > pauseSeq.value).length;
});
function pauseLogs() {
  frozenLogs.value = store.logs.slice();
  pauseSeq.value = store.logs.at(-1)?.seq ?? 0;
  paused.value = true;
}
function resumeLogs() {
  paused.value = false;
  frozenLogs.value = [];
  pauseSeq.value = 0;
}
function togglePause() {
  if (paused.value) resumeLogs();
  else pauseLogs();
}
const allChannelsReady = computed(() => store.status.frida && store.status.miniapp && store.status.devtools);
const activeTasks = computed(() => Object.values(store.tasks).filter((task) => !['done', 'failed', 'cancelled'].includes(task.phase)));
function clearLogs() {
  // 清空时一并解除暂停，否则用户看到的还是暂停前的旧快照，会和「已清空」提示矛盾
  resumeLogs();
  store.clearLogs();
  notify('运行日志已清空');
}

async function copyLogs(rows: LogRow[]) {
  await copyText(rows.map((row) => `${row.time ?? ''} ${row.level ?? ''} ${row.text ?? ''}`).join('\n'), '运行日志');
}

const cdpPortValid = computed(() => {
  const port = Number(store.cdpPort);
  return Number.isInteger(port) && port >= 1 && port <= 65535;
});
let weChatStatusTimer: ReturnType<typeof setInterval> | undefined;

onMounted(() => {
  store.load();
  void store.loadWeChatStatus();
  weChatStatusTimer = setInterval(() => void store.loadWeChatStatus(), 1000);
});
onBeforeUnmount(() => {
  if (weChatStatusTimer) clearInterval(weChatStatusTimer);
  weChatStatusTimer = undefined;
});
</script>

<template>
  <section class="control-view page-workbench" aria-labelledby="control-title">
    <PageHeader title="状态" title-id="control-title" />

    <div class="control-grid">
      <div class="control-row">
        <div class="panel">
          <header class="panel-header"><h2>引擎控制</h2></header>
          <div class="panel-body stack">
            <div class="engine-row">
              <label class="field" for="cdp-port">
                <span>CDP 端口</span>
                <input id="cdp-port" data-testid="cdp-port" v-model.number="store.cdpPort" inputmode="numeric" min="1" max="65535" type="number" :aria-invalid="!cdpPortValid">
              </label>
              <div class="actions">
                <StateToggle
                  test-id="engine-toggle"
                  :active="store.status.frida"
                  :pending="store.starting ? 'start' : store.stopping ? 'stop' : ''"
                  :disabled="!store.status.frida && !cdpPortValid"
                  start-label="启动引擎"
                  stop-label="停止引擎"
                  @start="store.start"
                  @stop="store.stop"
                />
              </div>
            </div>
            <p v-if="!cdpPortValid" class="error" role="status">端口需为 1-65535 的整数，当前值不会被应用。</p>
            <ErrorState v-if="store.error" data-testid="engine-error" :message="store.error" :retryable="store.errorRetryable" @retry="store.retryFailed" />
          </div>
        </div>

        <dl class="status-card panel">
          <header class="panel-header"><h2>组件状态</h2></header>
          <div class="panel-body">
            <div class="status-row" data-testid="wechat-host">
              <dt>版本支持</dt>
              <dd :class="versionSupport.ok ? 'connected' : 'disconnected'">
                <span class="status-pill" :class="versionSupport.ok ? 'ok' : 'off'" :title="versionSupport.title">{{ versionSupport.label }}</span>
              </dd>
            </div>
            <div class="status-row">
              <dt>微信</dt>
              <dd data-testid="wechat-status" :class="store.wechatStatusError ? 'disconnected' : store.status.wechatRunning ? 'connected' : 'disconnected'">
                <span class="status-pill" :class="store.wechatStatusError ? 'off' : store.status.wechatRunning ? 'ok' : 'off'" :title="store.wechatStatusError || store.wechatHost.path">{{ store.wechatStatusError ? '检测失败' : store.status.wechatRunning ? '运行中' : '未运行' }}</span>
              </dd>
            </div>
            <div class="status-row">
              <dt>Frida 注入</dt>
              <dd data-testid="frida-status" :class="store.status.frida ? 'connected' : 'disconnected'">
                <span class="status-pill" :class="store.status.frida ? 'ok' : 'off'">{{ store.status.frida ? '已连接' : '未连接' }}</span>
              </dd>
            </div>
          </div>
        </dl>
      </div>

      <aside v-if="allChannelsReady" class="callout ready-note" data-testid="ready-note">
        三条通道已就绪，可以开始调试：前往
        <RouterLink to="/navigator">页面路由</RouterLink> 跳转页面，或在
        <RouterLink to="/hook">注入脚本</RouterLink> 中注入脚本。
      </aside>

      <TaskProgress :tasks="activeTasks" />

      <LogPanel
        title="运行日志"
        test-id="log"
        :entries="logRows"
        :levels="['info', 'warn', 'error']"
        :cap="LOG_LIMIT"
        search-label="筛选运行日志"
        search-placeholder="筛选日志"
        empty-title="暂无日志"
        empty-hint="启动引擎后，运行输出会实时显示在这里。小程序自身的 console 输出见「Console 日志」页。"
        :paused="paused"
        :pending-count="pendingCount"
        @copy="copyLogs"
        @clear="clearLogs"
        @toggle-pause="togglePause"
      >
        <template #notice>
          <p v-if="store.logsDropped || store.logsTrimmed" class="callout warning" data-testid="log-dropped">
            已丢弃 {{ store.logsDropped + store.logsTrimmed }} 条：超出缓冲上限的日志不会再显示，完整记录仍在日志文件里，清空面板可重新计数。
          </p>
        </template>
      </LogPanel>
    </div>
  </section>
</template>

<style scoped>
/* 本页是 page-workbench：顶部两块面板固定高度，运行日志吃掉窗口剩余高度，
   列表只在面板内部滚，整页不滚。 */
.control-grid {
  display: flex;
  flex: 1 1 auto;
  flex-direction: column;
  gap: .8rem;
  min-height: 0;
  overflow: hidden;
}

/* 顶部固定：窗口变矮时不许被日志面板挤扁。 */
.control-row,
.control-grid > .ready-note,
.control-grid > .task-progress {
  flex: none;
}

/* 日志面板：头部（筛选/复制/清空）固定，只有列表滚。 */
.control-grid > .log-panel {
  display: flex;
  flex: 1 1 auto;
  flex-direction: column;
  min-height: 0;
  overflow: hidden;
}

.control-grid > .log-panel :deep(.log-body) {
  display: flex;
  flex: 1 1 auto;
  min-height: 0;
}

/* 面板已经吃满剩余高度，列表不再各自封顶（base.css 的 18rem / 面板的 32rem）。 */
.control-grid > .log-panel :deep(.log-view) {
  flex: 1 1 auto;
  max-height: none;
  min-height: 0;
}

.control-grid > .log-panel :deep(.empty-state) {
  flex: 1 1 auto;
}

/* 顶部两块面板压扁：只留够点按和读状态的高度。 */
.control-row > .panel > .panel-header {
  padding: .45rem 1.1rem;
}

.control-row > .panel > .panel-body {
  padding: .6rem 1.1rem;
}

.control-row > .panel > .panel-body.stack {
  gap: .6rem;
}

/* 端口和启停按钮并排：这一行的高度只取决于输入框，别按竖排累加。 */
.engine-row {
  align-items: end;
  display: flex;
  flex-wrap: wrap;
  gap: .6rem;
}

.engine-row .field {
  flex: 1 1 10rem;
}

.ready-note {
  border-left-color: var(--success);
}

.ready-note a {
  color: var(--accent-strong);
  font-weight: 600;
}

.control-row {
  align-items: start;
  display: grid;
  gap: 1rem;
  grid-template-columns: minmax(0, 1.4fr) minmax(16rem, 1fr);
}

.control-row > .panel {
  margin: 0;
  min-width: 0;
}

.status-card {
  margin: 0;
}

.status-row {
  align-items: center;
  border-bottom: 1px solid var(--border);
  display: flex;
  justify-content: space-between;
  padding: .25rem 0;
}

.status-row:last-child {
  border-bottom: 0;
  padding-bottom: 0;
}

.status-row:first-child {
  padding-top: 0;
}

.status-row dt {
  color: var(--muted);
  font-weight: 600;
}

.status-row dd {
  margin: 0;
}




@media (max-width: 860px) {
  .control-row {
    grid-template-columns: 1fr;
  }
}

/* ≤760px base.css 让整页滚动（page-workbench 退回高度自适应）：日志列表要恢复
   自己的上限，否则几千行会把页面撑到滚不完。 */
@media (max-width: 760px) {
  .control-grid {
    overflow: visible;
  }

  .control-grid > .log-panel :deep(.log-view) {
    max-height: 32rem;
  }
}
</style>

