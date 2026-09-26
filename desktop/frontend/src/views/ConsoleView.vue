<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue';
import LogPanel, { type LogRow } from '../components/LogPanel.vue';
import PageHeader from '../components/PageHeader.vue';
import { CONSOLE_LIMIT, useEngineStore } from '../stores/engine';
import { messageOf } from '../utils/format';
import { copyText, notify } from '../utils/notify';

const store = useEngineStore();
const connected = computed(() => store.status.miniapp);
// 暂停 = 停止轮询。拉取按序号增量进行，暂停期间后端照常缓冲，恢复时会把
// 暂停期间的记录一次补齐，不会丢行也不会重复。
const paused = ref(false);
const clearing = ref(false);
const miniappLabel = computed(() => store.status.appInfo?.name?.trim() || store.status.appInfo?.appid?.trim() || '');
const currentAppId = computed(() => store.status.appInfo?.appid?.trim() || '');
const LEVELS = ['log', 'info', 'warn', 'error', 'debug'];
const POLL_MS = 1000;

function extraText(record: { appId?: string; extra?: Record<string, string> }): string {
  const parts: string[] = [];
  for (const [key, value] of Object.entries(record.extra ?? {})) {
    if (value !== '') parts.push(`${key}: ${value}`);
  }
  // 环形缓冲跨小程序共享：切换目标后，上一个应用的记录还在列表里。appId
  // 与当前应用不同时标注出来，免得两段输出被误读成同一个页面打的。
  if (record.appId && currentAppId.value && record.appId !== currentAppId.value) {
    parts.push(`appId: ${record.appId}`);
  }
  return parts.join(' · ');
}

// 筛选与呈现交给共享的 LogPanel；这里只把缓冲映射成行，key 用后端序号，
// 裁剪头部时不会错位。
const rows = computed<LogRow[]>(() => store.consoleRecords.map((record, index) => ({
  key: record.seq ?? `row-${index}`,
  time: record.timestamp,
  level: (record.level ?? 'log').toLowerCase(),
  text: record.text,
  extra: extraText(record) || undefined,
})));

async function copyRows(filtered: LogRow[]) {
  const text = filtered
    .map((row) => `${row.time ?? ''} [${row.level ?? ''}] ${row.text ?? ''}${row.extra ? ` (${row.extra})` : ''}`)
    .join('\n');
  await copyText(text, 'Console 日志');
}

async function clear() {
  clearing.value = true;
  try {
    await store.clearConsole();
    notify('Console 日志已清空');
  } catch (error) {
    notify(`清空失败：${messageOf(error)}`, 'error');
  } finally {
    clearing.value = false;
  }
}

let timer: ReturnType<typeof setInterval> | undefined;

onMounted(() => {
  void store.pollConsole();
  timer = setInterval(() => {
    if (!paused.value) void store.pollConsole();
  }, POLL_MS);
});

onBeforeUnmount(() => {
  if (timer) clearInterval(timer);
  timer = undefined;
});
</script>

<template>
  <section class="console-view" aria-labelledby="console-title">
    <PageHeader title="Console 日志" title-id="console-title" />

    <p v-if="store.consoleError" class="error" role="alert">{{ store.consoleError }}</p>

    <LogPanel
      title="Console 日志"
      test-id="console"
      :entries="rows"
      :levels="LEVELS"
      :cap="CONSOLE_LIMIT"
      search-label="搜索 Console 日志"
      search-placeholder="搜索日志内容"
      empty-title="暂无 Console 输出"
      empty-hint="连接小程序并触发页面逻辑后，console.log 与未捕获错误会显示在这里。WxTap 自身的日志见「状态」页的运行日志。"
      :paused="paused"
      :clearing="clearing"
      @copy="copyRows"
      @clear="clear"
      @toggle-pause="paused = !paused"
    >
      <template #status>
        <span
          class="status-pill"
          :class="connected ? 'ok' : 'off'"
          :title="connected ? '日志来自小程序页面上下文' : '未连接小程序：只能查看已缓冲的记录'"
        >{{ connected ? '捕获中' : '未连接' }}</span>
        <span v-if="miniappLabel" class="status-pill">{{ miniappLabel }}</span>
      </template>
      <template #notice>
        <p v-if="store.consoleDropped" class="callout warning" data-testid="console-dropped">
          已丢弃 {{ store.consoleDropped }} 条：后端缓冲上限 1000 条，暂停过久或小程序刷新过快时，最旧的记录将被淘汰。
        </p>
      </template>
    </LogPanel>
  </section>
</template>
