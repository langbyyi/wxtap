<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue';

export type LogRow = {
  key: string | number;
  time?: string;
  level?: string;
  text?: string;
  /** 附加说明（例如小程序 console 的 source/line/stack） */
  extra?: string;
};

// 两页共用的日志面板：筛选、级别、计数、复制、清空、空态的交互只实现一次。
// 数据生命周期仍归各自的页面（运行日志来自 `log` 事件缓冲，Console 日志来自
// `console.list` 轮询），这里只管呈现与筛选。
const props = withDefaults(defineProps<{
  title: string;
  /** 已按各自上限裁剪过的行（面板再做关键词与级别筛选） */
  entries: LogRow[];
  /** 级别下拉里提供的档位，只列实际会出现的级别 */
  levels: string[];
  /** `log` / `console`：拼出列表、按钮、计数的 data-testid */
  testId: string;
  searchLabel: string;
  searchPlaceholder: string;
  emptyTitle: string;
  emptyHint: string;
  /** 计数分母（例如界面保留上限）；缺省用 entries.length */
  cap?: number;
  paused?: boolean;
  /** 暂停期间到达的新条数，界面据此给出「N 条新日志」 */
  pendingCount?: number;
  clearing?: boolean;
}>(), { cap: undefined, paused: false, pendingCount: 0, clearing: false });

const emit = defineEmits<{
  copy: [rows: LogRow[]];
  clear: [];
  togglePause: [];
}>();

const query = ref('');
const level = ref('all');

const levelCounts = computed(() => {
  const counts: Record<string, number> = {};
  for (const name of props.levels) counts[name] = 0;
  for (const row of props.entries) {
    const key = (row.level ?? '').toLowerCase();
    if (key in counts) counts[key] += 1;
  }
  return counts;
});

const filtered = computed(() => {
  const needle = query.value.trim().toLowerCase();
  const wanted = level.value;
  if (!needle && wanted === 'all') return props.entries;
  return props.entries.filter((row) => {
    const rowLevel = (row.level ?? '').toLowerCase();
    if (wanted !== 'all' && rowLevel !== wanted) return false;
    if (!needle) return true;
    return `${row.time ?? ''} ${rowLevel} ${row.text ?? ''} ${row.extra ?? ''}`.toLowerCase().includes(needle);
  });
});

const total = computed(() => props.entries.length);
const cap = computed(() => props.cap ?? props.entries.length);

// 自动滚动：贴底时新行到达就继续贴底；用户上滑阅读即解除贴住，浮出
// 「回到底部」。暂停本就冻结快照供阅读，不参与自动滚动。
const viewEl = ref<HTMLElement | null>(null);
const pinned = ref(true);
const PIN_THRESHOLD = 32;

function onScroll() {
  const el = viewEl.value;
  if (!el) return;
  pinned.value = el.scrollHeight - el.scrollTop - el.clientHeight < PIN_THRESHOLD;
}

function scrollToBottom() {
  const el = viewEl.value;
  if (!el) return;
  // 瞬时跳转而非平滑滚动：滚动动画期间的距离检查会把「贴底」误判为已离开。
  el.scrollTo?.({ top: el.scrollHeight });
}

watch(filtered, async () => {
  if (props.paused || !pinned.value) return;
  await nextTick();
  scrollToBottom();
}, { immediate: true });
</script>

<template>
  <div class="panel log-panel">
    <header class="panel-header">
      <h2>
        {{ title }}
        <span class="subnav-count" :data-testid="`${testId}-count`">{{ filtered.length }} / {{ cap }}</span>
        <span v-if="paused" class="status-pill warn">已暂停</span>
      </h2>
      <slot name="status" />
      <div class="toolbar log-toolbar">
        <div class="log-filters">
          <input v-model="query" class="log-search" type="search" :placeholder="searchPlaceholder" :aria-label="searchLabel">
          <select v-model="level" aria-label="日志级别">
            <option value="all">全部级别（{{ total }}）</option>
            <option v-for="name in levels" :key="name" :value="name">{{ name }}（{{ levelCounts[name] }}）</option>
          </select>
        </div>
        <div class="log-actions">
          <button :data-testid="`pause-${testId}`" class="ghost small" type="button" @click="emit('togglePause')">
            {{ paused ? '恢复' : '暂停' }}
          </button>
          <button
            v-if="paused && pendingCount > 0"
            :data-testid="`jump-${testId}`"
            class="ghost small log-jump"
            type="button"
            @click="emit('togglePause')"
          >{{ pendingCount }} 条新日志</button>
          <button :data-testid="`copy-${testId}`" class="ghost small" type="button" :disabled="!filtered.length" @click="emit('copy', filtered)">复制</button>
          <button :data-testid="`clear-${testId}`" class="ghost small" type="button" :disabled="clearing || !total" @click="emit('clear')">
            {{ clearing ? '清空中…' : '清空' }}
          </button>
        </div>
      </div>
    </header>
    <slot name="notice" />
    <div v-if="filtered.length" class="log-body">
      <ul ref="viewEl" :data-testid="`${testId}-list`" class="log-view" role="log" @scroll="onScroll">
        <li v-for="row in filtered" :key="row.key">
          <span class="log-time">{{ row.time }}</span>
          <strong class="log-level" :class="row.level">{{ row.level }}</strong>
          <span class="log-message">{{ row.text }}</span>
          <span v-if="row.extra" class="log-extra">{{ row.extra }}</span>
        </li>
      </ul>
      <button
        v-if="!pinned"
        :data-testid="`bottom-${testId}`"
        class="log-bottom"
        type="button"
        @click="pinned = true; scrollToBottom()"
      >回到底部 ↓</button>
    </div>
    <div v-else-if="total" class="empty-state">
      <strong>没有匹配的日志</strong>
      <span>调整关键词或日志级别后重试。</span>
    </div>
    <div v-else class="empty-state">
      <strong>{{ emptyTitle }}</strong>
      <span>{{ emptyHint }}</span>
    </div>
  </div>
</template>

<style scoped>
.log-body {
  position: relative;
}

/* 上滑阅读后浮出，点击跳回最新一行 */
.log-bottom {
  background: var(--panel-3);
  border: 1px solid var(--border-strong);
  border-radius: 99px;
  box-shadow: var(--shadow);
  color: var(--accent);
  cursor: pointer;
  font: inherit;
  font-size: .76rem;
  font-weight: 600;
  padding: .22rem .75rem;
  position: absolute;
  bottom: .6rem;
  right: .9rem;
}

.log-toolbar {
  align-items: center;
  flex: 1 1 auto;
  gap: .5rem;
  justify-content: space-between;
  min-width: 0;
}

.log-filters {
  align-items: center;
  display: flex;
  flex: 1 1 auto;
  gap: .5rem;
  min-width: 0;
}

.log-search {
  flex: 1 1 9rem;
  max-width: 24rem;
  min-width: 0;
}

.log-toolbar select {
  flex: none;
  width: auto;
}

.log-actions {
  display: flex;
  flex: none;
  gap: .35rem;
}

/* 「N 条新日志」用主色把自己和普通 ghost 按钮区分开 */
.log-jump {
  color: var(--accent);
  font-variant-numeric: tabular-nums;
}

.log-extra {
  color: var(--muted);
  flex-basis: 100%;
  font-size: .74rem;
  overflow-wrap: anywhere;
}

.log-panel :deep(.log-view) {
  max-height: 32rem;
}

.log-panel :deep(.log-view li) {
  flex-wrap: wrap;
}

.log-panel :deep(.log-view .log-message) {
  flex: 1 1 auto;
  min-width: 0;
}

@media (max-width: 720px) {
  .log-toolbar {
    align-items: stretch;
    flex-direction: column;
  }

  .log-search {
    max-width: none;
  }

  .log-actions {
    justify-content: flex-end;
  }
}
</style>
