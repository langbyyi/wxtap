<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue';

const props = defineProps<{
  pages: string[];
  tabBarPages: string[];
  currentRoute: string;
  connected: boolean;
  loading: boolean;
}>();

const emit = defineEmits<{
  navigate: [route: string, method: 'navigateTo' | 'reLaunch'];
  copy: [route: string];
  refresh: [];
}>();

const query = ref('');
const selected = ref('');

const filteredPages = computed(() => {
  const needle = query.value.trim().toLowerCase();
  if (!needle) return props.pages;
  return props.pages.filter((route) => route.toLowerCase().includes(needle));
});
// 选中项只跟随路由列表本身：列表换了（拉取/切换小程序）就回到当前路由或首项，
// 轮询刷新不会打断用户已经在看的那一行。
watch(() => props.pages, () => {
  if (selected.value && props.pages.includes(selected.value)) return;
  selected.value = props.currentRoute || props.pages[0] || '';
});

function selectByOffset(offset: number) {
  const list = filteredPages.value;
  if (!list.length) return;
  const current = selected.value ? list.indexOf(selected.value) : -1;
  const index = current === -1
    ? (offset > 0 ? 0 : list.length - 1)
    : Math.min(Math.max(current + offset, 0), list.length - 1);
  selected.value = list[index] ?? '';
  void nextTick(() => {
    document.querySelector<HTMLElement>(`[data-route-index="${index}"]`)?.scrollIntoView?.({ block: 'nearest' });
  });
}

function navigateSelected(method: 'navigateTo' | 'reLaunch') {
  if (selected.value) emit('navigate', selected.value, method);
}
</script>

<template>
  <div class="panel routes-panel">
    <header class="panel-header">
      <h2>路由列表</h2>
      <div class="toolbar">
        <span class="status-pill" :class="connected ? 'ok' : 'off'">{{ connected ? '小程序已连接' : '未连接小程序' }}</span>
        <span v-if="currentRoute" class="status-pill ok">当前路由：/{{ currentRoute }}</span>
        <span v-else class="status-pill off">当前路由：--</span>
        <input v-model="query" class="route-filter" placeholder="搜索路由…" aria-label="搜索路由">
        <button v-if="query" class="ghost small" type="button" @click="query = ''">清除</button>
        <span v-if="query" class="subnav-count">{{ filteredPages.length }} / {{ pages.length }}</span>
        <span v-if="filteredPages.length > 1" class="status-line key-hint">↑↓ 选择</span>
        <button type="button" class="secondary small" :disabled="loading || !connected" @click="emit('refresh')">{{ loading ? '加载中…' : '获取路由' }}</button>
      </div>
    </header>
    <div class="route-list" aria-label="路由列表">
      <button
        v-for="(route, index) in filteredPages"
        :key="route"
        :data-route-index="index"
        type="button"
        :class="{ selected: selected === route }"
        :title="route === currentRoute ? '小程序当前页面；单击选择，双击或按 Enter 跳转' : '单击选择，双击或按 Enter 跳转'"
        @click="selected = route"
        @keydown.enter.prevent="emit('navigate', route, 'navigateTo')"
        @keydown.down.prevent="selectByOffset(1)"
        @keydown.up.prevent="selectByOffset(-1)"
        @dblclick="emit('navigate', route, 'navigateTo')"
      >
        <span v-if="route === currentRoute" class="current-badge" title="小程序当前显示的页面">当前</span>
        <span v-if="tabBarPages.includes(route)" class="tab-badge">Tab</span>
        <code>{{ route }}</code>
      </button>
      <div v-if="!filteredPages.length" class="empty-state">
        <strong>{{ pages.length ? '没有匹配的路由' : '暂无路由' }}</strong>
        <span>{{ pages.length ? '清空搜索条件后查看全部路由。' : '连接小程序后点击「获取路由」。' }}</span>
      </div>
    </div>
    <div class="route-actions">
      <button data-testid="navigate-selected" type="button" :disabled="!selected || !connected" @click="navigateSelected('navigateTo')">跳转到选中</button>
      <button type="button" class="secondary" :disabled="!selected || !connected" @click="navigateSelected('reLaunch')">重启到选中</button>
      <button data-testid="copy-route" type="button" class="secondary" :disabled="!selected" @click="selected && emit('copy', selected)">复制路由</button>
    </div>
  </div>
</template>

<style scoped>
.key-hint {
  font-size: .78rem;
  white-space: nowrap;
}

.route-filter {
  max-width: 12rem;
}

/* 面板吃满主栏高度：只让列表滚，动作条钉在面板底部 —— 它是选完路由马上要用的
   东西，不该滑到底才出现。 */
.routes-panel {
  display: flex;
  flex-direction: column;
  min-height: 0;
  overflow: hidden;
}

.route-list {
  display: grid;
  flex: 1 1 auto;
  gap: .15rem;
  min-height: 0;
  overflow-y: auto;
  padding: .4rem;
}

.route-list > button {
  align-items: center;
  background: transparent;
  border: 0;
  border-radius: var(--radius-sm);
  color: var(--text);
  display: flex;
  font-weight: 400;
  gap: .5rem;
  justify-content: flex-start;
  padding: .45rem .6rem;
  text-align: left;
  width: 100%;
}

.route-list > button:hover {
  background: var(--panel-2);
}

.route-list > button.selected {
  background: var(--accent-soft);
  color: var(--accent-strong);
}

.route-list code {
  font-family: var(--mono);
  font-size: .82rem;
  overflow-wrap: anywhere;
}

.tab-badge {
  background: var(--warning-soft);
  border-radius: 4px;
  color: var(--warning);
  flex: none;
  font-size: .68rem;
  font-weight: 700;
  padding: .05rem .35rem;
}

/* 「当前」是运行时事实（来自页面回读），与用户选中的那一行是两回事：早先只有
   选中高亮，当前页在列表里看不出来，容易被读成同一个东西。 */
.current-badge {
  background: var(--accent-soft);
  border-radius: 4px;
  color: var(--accent-strong);
  flex: none;
  font-size: .68rem;
  font-weight: 700;
  padding: .05rem .35rem;
}

/* 选中项的动作条贴着列表底部，避免用户先选再满页找按钮 */
.route-actions {
  border-top: 1px solid var(--border);
  display: flex;
  flex: none;
  flex-wrap: wrap;
  gap: .5rem;
  padding: .6rem .7rem;
}

/* 小屏两栏叠起来后列表不再有「面板剩余高度」可分，改回限高自己滚：
   动作条仍紧贴在列表下面，而不是被一长串路由推到屏幕外。 */
@media (max-width: 900px) {
  .routes-panel {
    overflow: visible;
  }

  .route-list {
    max-height: 22rem;
  }
}
</style>
