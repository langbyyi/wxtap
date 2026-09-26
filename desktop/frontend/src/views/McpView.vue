<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue';
import { backend } from '../api/bridge';
import { messageOf } from '../utils/format';
import { copyText, notify } from '../utils/notify';
import PageHeader from '../components/PageHeader.vue';
import StateToggle from '../components/StateToggle.vue';

const running = ref(false);
const port = ref(9527);
const error = ref('');
const working = ref(false);
const refreshing = ref(false);
const command = ref('');

// 三种接入方式（stdio / Streamable HTTP / SSE）的端点都只由端口决定，不看服务有
// 没有在跑：配置片段本来就是这么算的，地址跟着一样算，页面才不会在点过「启动服
// 务」之后才凭空长出两行地址来。
const httpUrl = computed(() => `http://127.0.0.1:${port.value}/mcp`);
const sseUrl = computed(() => `http://127.0.0.1:${port.value}/sse`);
const configText = computed(() => JSON.stringify({ mcpServers: { wxtap: { url: httpUrl.value } } }, null, 2));
const sseConfigText = computed(() => JSON.stringify({ mcpServers: { wxtap: { url: sseUrl.value } } }, null, 2));
const cliCommand = computed(() => `claude mcp add --transport http wxtap ${httpUrl.value}`);
const stdioConfigText = computed(() => JSON.stringify({ mcpServers: { wxtap: { command: command.value || 'WxTap.exe', args: ['-mcp'] } } }, null, 2));

type McpStatus = { running?: boolean; available?: boolean; state?: string; kind?: string; port?: number; mode?: string; command?: string };
async function refresh() {
  refreshing.value = true;
  try {
    const status = await backend.call<McpStatus>('mcp.status');
    running.value = status.running === true;
    command.value = status.command ?? '';
    if (typeof status.port === 'number' && status.port > 0) port.value = status.port;
  } catch (reason) { error.value = messageOf(reason); } finally { refreshing.value = false; }
}
async function start() {
  working.value = true; error.value = '';
  try {
    const result = await backend.call<{ ok?: boolean }>('mcp.start', { port: port.value });
    if (!result.ok) throw new Error('MCP 服务启动失败');
    running.value = true;
    notify('MCP 服务已启动', 'success');
  } catch (reason) {
    error.value = messageOf(reason);
    notify(`MCP 启动失败：${messageOf(reason)}`, 'error');
  } finally { working.value = false; }
}
async function stop() {
  working.value = true; error.value = '';
  try {
    await backend.call('mcp.stop');
    running.value = false;
    notify('MCP 服务已停止');
  } catch (reason) {
    error.value = messageOf(reason);
    notify(`停止 MCP 失败：${messageOf(reason)}`, 'error');
  } finally { working.value = false; }
}
async function copyHttpUrl() {
  await copyText(httpUrl.value, 'MCP 地址');
}
async function copyConfig() {
  await copyText(configText.value, 'MCP 配置');
}
async function copyCli() {
  await copyText(cliCommand.value, 'CLI 命令');
}
async function copySseUrl() {
  await copyText(sseUrl.value, 'SSE 地址');
}
async function copySseConfig() {
  await copyText(sseConfigText.value, 'SSE 配置');
}
async function copyStdioConfig() {
  await copyText(stdioConfigText.value, 'stdio 配置');
}
async function copyCommand() {
  if (command.value) await copyText(command.value, '启动命令');
}

// 选中即复制：选区落在可复制的值里时，在选区上方浮出一个小按钮。右键复制负责
// 「整块拿走」，这个按钮负责「只拿走选中的那一段」——只剩右键时，用户选中一段
// 再右键拿到的是整块，与预期不符。
const selectionTip = ref<{ left: number; top: number; below: boolean; text: string } | null>(null);

function syncSelectionTip() {
  const selection = window.getSelection();
  const range = selection && selection.rangeCount > 0 ? selection.getRangeAt(0) : null;
  // 取 range.toString() 而不是 selection.toString()：同一个选区两者同值，
  // 但 range 的文本在 jsdom 里也能拿到，这一段的测试才跑得起来。
  const text = range?.toString().trim() ?? '';
  const anchor = selection?.anchorNode ?? null;
  const host = anchor instanceof Element ? anchor : anchor?.parentElement ?? null;
  if (!range || !text || !host?.closest('.copy-on-context')) {
    selectionTip.value = null;
    return;
  }
  // 视口坐标（position: fixed）：跟着选区走就不必换算页面的滚动量
  const rect = range.getBoundingClientRect();
  const below = rect.top < 56;
  selectionTip.value = {
    left: Math.min(Math.max(rect.left + rect.width / 2, 48), window.innerWidth - 48),
    top: below ? rect.bottom + 8 : rect.top - 8,
    below,
    text,
  };
}

function hideSelectionTip() {
  selectionTip.value = null;
}

async function copySelection() {
  const text = selectionTip.value?.text ?? '';
  selectionTip.value = null;
  if (text) await copyText(text, '选中内容');
}

// 复制必须发生在 mousedown。Chromium 在点这块浮标时会把选区收掉——mousedown 和
// mouseup 都 preventDefault 也挡不住——收掉后 selectionchange 立刻把浮标清空，
// 等 click 触发时按钮已经不在 DOM 里了，文本自然也是空的。
function copySelectionDown(event: MouseEvent) {
  event.preventDefault();
  if (event.button !== 0) return;
  void copySelection();
}

function dismissSelectionTip(event: KeyboardEvent) {
  if (event.key === 'Escape') hideSelectionTip();
}
async function refreshStatus() {
  await refresh();
  notify(running.value ? 'MCP 服务正在运行' : 'MCP 服务未运行', running.value ? 'success' : 'info');
}
onMounted(refresh);

// 三个监听都挂在 document/window 上，所以离开这一页时必须摘掉，否则别的页面
// 选词也会触发这里的计算。scroll 用捕获阶段：滚动可能发生在内层容器里。
onMounted(() => {
  document.addEventListener('selectionchange', syncSelectionTip);
  document.addEventListener('keydown', dismissSelectionTip);
  window.addEventListener('scroll', hideSelectionTip, { capture: true, passive: true });
  window.addEventListener('resize', hideSelectionTip);
});

onBeforeUnmount(() => {
  document.removeEventListener('selectionchange', syncSelectionTip);
  document.removeEventListener('keydown', dismissSelectionTip);
  window.removeEventListener('scroll', hideSelectionTip, { capture: true });
  window.removeEventListener('resize', hideSelectionTip);
});
</script>

<template>
  <section class="mcp-view" aria-labelledby="mcp-title">
    <PageHeader title="MCP 服务" title-id="mcp-title" />

    <div class="stack">
      <div class="panel">
        <header class="panel-header"><h2>服务控制</h2><div class="toolbar"><span class="status-pill" :class="running ? 'ok' : 'off'">{{ running ? '运行中' : '已停止' }}</span><button data-testid="refresh-mcp" class="ghost small" type="button" :disabled="refreshing" @click="refreshStatus">{{ refreshing ? '自检中…' : '连接自检' }}</button></div></header>
        <div class="panel-body stack">
          <label class="field mcp-port-field" for="mcp-port">
            <span>服务端口</span>
            <input id="mcp-port" data-testid="mcp-port" v-model.number="port" type="number" min="1024" max="65535" :disabled="running || working">
          </label>
          <div class="actions">
            <StateToggle
              test-id="mcp-toggle"
              :active="running"
              :pending="working ? (running ? 'stop' : 'start') : ''"
              start-label="启动服务"
              stop-label="停止服务"
              @start="start"
              @stop="stop"
            />
          </div>
          <p v-if="error" class="error" role="alert">{{ error }}</p>
        </div>
      </div>

      <!-- 三种接入方式平级：每块先给自己那一项可直接复制的值（stdio 是启动命令，
           HTTP / SSE 是端点地址），再给客户端配置片段。类型名写在标题上，块内不再
           插说明句；地址也不在「服务控制」里等启动后才冒出来。 -->
      <div v-if="command" class="panel">
        <header class="panel-header"><h2>stdio</h2></header>
        <div class="panel-body stack">
          <pre data-testid="mcp-command" class="copy-on-context" aria-label="MCP stdio 启动命令" title="右键复制命令（选中后可只复制片段）" tabindex="0" @contextmenu.prevent="copyCommand" @keydown.enter.prevent="copyCommand">{{ command }}</pre>
          <pre data-testid="mcp-stdio-config" class="copy-on-context" aria-label="MCP stdio 配置" title="右键复制配置（选中后可只复制片段）" tabindex="0" @contextmenu.prevent="copyStdioConfig" @keydown.enter.prevent="copyStdioConfig">{{ stdioConfigText }}</pre>
        </div>
      </div>

      <div class="panel">
        <header class="panel-header"><h2>Streamable HTTP</h2></header>
        <div class="panel-body stack">
          <code data-testid="mcp-url" class="url-value copy-on-context" title="右键复制 Streamable HTTP 地址（选中后可只复制片段）" tabindex="0" @contextmenu.prevent="copyHttpUrl" @keydown.enter.prevent="copyHttpUrl">{{ httpUrl }}</code>
          <pre data-testid="mcp-config" class="copy-on-context" aria-label="MCP 配置" title="右键复制配置（选中后可只复制片段）" tabindex="0" @contextmenu.prevent="copyConfig" @keydown.enter.prevent="copyConfig">{{ configText }}</pre>
          <code data-testid="mcp-cli-command" class="url-value copy-on-context" title="右键复制命令（选中后可只复制片段）" tabindex="0" @contextmenu.prevent="copyCli" @keydown.enter.prevent="copyCli">{{ cliCommand }}</code>
        </div>
      </div>

      <div class="panel">
        <header class="panel-header"><h2>SSE</h2></header>
        <div class="panel-body stack">
          <code data-testid="mcp-sse-url" class="url-value copy-on-context" title="右键复制 SSE 地址（选中后可只复制片段）" tabindex="0" @contextmenu.prevent="copySseUrl" @keydown.enter.prevent="copySseUrl">{{ sseUrl }}</code>
          <pre data-testid="mcp-sse-config" class="copy-on-context" aria-label="MCP SSE 配置" title="右键复制配置（选中后可只复制片段）" tabindex="0" @contextmenu.prevent="copySseConfig" @keydown.enter.prevent="copySseConfig">{{ sseConfigText }}</pre>
        </div>
      </div>
    </div>

    <!-- 见 copySelectionDown 的注释：复制挂在 mousedown 上，不是 click -->
    <button
      v-if="selectionTip"
      class="secondary small selection-copy"
      :class="{ below: selectionTip.below }"
      type="button"
      data-testid="copy-selection"
      :style="{ left: `${selectionTip.left}px`, top: `${selectionTip.top}px` }"
      @mousedown="copySelectionDown"
      @click.prevent
    >复制</button>
  </section>
</template>

<style scoped>
.mcp-port-field {
  max-width: 16rem;
}

/* 这一页的值都是只读的地址与配置，三种复制入口：右键拿整块、Tab 聚焦后回车
   拿整块、选中一段后浮出的按钮只拿选中的那一段。前两个不占版面，第三个只在
   真的选中时才出现，所以页面上没有常驻按钮。
   浮标用 fixed + 视口坐标（选区 rect 就是视口坐标，不必换算滚动量）；滚动时
   位置会过期，所以 scroll 一律收起。 */
.copy-on-context {
  cursor: context-menu;
}

.selection-copy {
  position: fixed;
  transform: translate(-50%, -100%);
  z-index: 30;
}

.selection-copy.below {
  transform: translate(-50%, 0);
}

.url-value {
  background: var(--panel-2);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  font-family: var(--mono);
  font-size: .82rem;
  overflow-wrap: anywhere;
  padding: .35rem .6rem;
}
</style>
