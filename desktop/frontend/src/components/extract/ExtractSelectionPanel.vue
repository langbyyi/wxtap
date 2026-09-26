<script setup lang="ts">
import type { ExtractApp } from './types';

defineProps<{
  apps: ExtractApp[];
  resultApp: string;
  query: string;
  running: string;
  actionableCount: number;
  formatTime: (mtime: number) => string;
  primaryLabel: (app: ExtractApp) => string;
  appStatusLabel: (app: ExtractApp) => string;
}>();

/** The count of wxapkg files sits on the card's second line, next to the time. */
function packagesLabel(app: ExtractApp): string {
  return `${app.packages.length} 个 wxapkg 文件`;
}

const emit = defineEmits<{
  'update:query': [value: string];
  decompileAll: [];
  runPrimary: [app: ExtractApp];
  openOutput: [app: ExtractApp];
  remove: [app: ExtractApp];
  iconError: [app: ExtractApp];
}>();
</script>

<template>
  <div class="panel app-list-panel">
    <header class="panel-header">
      <h2 data-testid="inventory-title">小程序 <span v-if="apps.length" class="subnav-count">{{ apps.length }}</span></h2>
      <button data-testid="decompile-all" type="button" :disabled="!!running || !actionableCount" @click="emit('decompileAll')">反编译全部</button>
    </header>
    <div class="panel-section">
      <input :value="query" class="search-input" type="search" aria-label="搜索 AppID" placeholder="搜索 AppID" data-testid="extract-search" @input="emit('update:query', ($event.target as HTMLInputElement).value)">
    </div>
    <div v-if="apps.length" class="app-cards">
      <article v-for="app in apps" :key="app.appid" class="app-card" :class="{ active: resultApp === app.appid }" :data-testid="`app-${app.appid}`">
        <div v-if="app.iconDataURL && !app.iconBroken" class="app-card-icon-wrap">
          <img class="app-card-icon" :src="app.iconDataURL" alt="小程序图标" :data-testid="`app-icon-${app.appid}`" @error="emit('iconError', app)">
        </div>
        <div v-else class="app-card-icon app-card-icon-fallback" :data-testid="`app-icon-fallback-${app.appid}`" aria-label="通用小程序图标">wx</div>
        <p class="app-card-name">{{ app.appid }}</p>
        <div class="app-card-actions">
          <button class="small" type="button" :data-testid="`primary-${app.appid}`" :disabled="!!running" @click="emit('runPrimary', app)">{{ primaryLabel(app) }}</button>
          <button class="icon-action" type="button" title="打开输出目录" aria-label="打开输出目录" :data-testid="`open-${app.appid}`" :disabled="!app.decompiled" @click="emit('openOutput', app)">
            <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M3 7h6l2 2h10v10H3z" /></svg>
          </button>
          <button class="icon-action danger" type="button" title="删除" aria-label="删除" :data-testid="`delete-${app.appid}`" @click="emit('remove', app)">
            <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M5 7h14M10 7V5h4v2M7 7l1 13h8l1-13" /></svg>
          </button>
        </div>
        <div class="app-card-meta">
          <span>{{ packagesLabel(app) }}<template v-if="app.mtime"> · {{ formatTime(app.mtime) }}</template></span>
          <span class="status-pill" :class="app.scanned ? 'ok' : app.decompiled ? 'warn' : 'off'">{{ appStatusLabel(app) }}</span>
        </div>
      </article>
    </div>
    <p v-else class="status-line panel-section">{{ query ? '没有匹配的小程序。' : '请选择微信用户目录。' }}</p>
  </div>
</template>

<style scoped>
/* 卡片列表随小程序数量增长：让它自己滚，别把页面顶长、把右边的结果表挤下去
   （搜索框留在滚动区外面，滚到列表底部时还能改搜索词）。 */
.app-list-panel { display: flex; flex-direction: column; min-height: 0; overflow: hidden; }
.app-list-panel > .panel-header,
.app-list-panel > .panel-section { flex: none; }
.app-list-panel .app-cards { min-height: 0; overflow: auto; }
.app-cards { display: grid; gap: .45rem; padding: 0 .8rem .8rem; }
/* 两行：图标横跨两行，右边第一行 appid + 按钮，第二行「包数 · 时间」+ 状态。
   每张卡上都重复的「小程序」标签去掉了 —— 面板标题已经写着。 */
.app-card { align-items: center; background: var(--panel-2); border: 1px solid var(--border); border-radius: var(--radius-sm); column-gap: .5rem; display: grid; grid-template-columns: auto minmax(0, 1fr) auto; padding: .45rem .6rem; row-gap: .05rem; }
.app-card.active { border-color: var(--accent); box-shadow: var(--ring); }
.app-card-icon-wrap, .app-card-icon-fallback { align-self: center; grid-column: 1; grid-row: 1 / span 2; }
.app-card-icon-wrap { align-items: center; display: flex; gap: .2rem; }
.app-card-icon { border: 1px solid var(--border); border-radius: .5rem; height: 1.85rem; object-fit: cover; width: 1.85rem; }
.app-card-icon-fallback { align-items: center; background: var(--accent-soft); color: var(--accent); display: flex; font-family: var(--mono); font-size: .68rem; justify-content: center; }
.app-card-name { font-size: .84rem; font-weight: 600; grid-column: 2; grid-row: 1; margin: 0; min-width: 0; overflow-wrap: anywhere; }
.app-card-actions { display: flex; gap: .25rem; grid-column: 3; grid-row: 1; }
.icon-action { align-items: center; background: var(--panel); border: 1px solid var(--border); border-radius: var(--radius-sm); color: var(--muted); display: inline-flex; height: 1.55rem; justify-content: center; padding: 0; width: 1.55rem; }
.icon-action svg { fill: none; height: .85rem; stroke: currentColor; stroke-linecap: round; stroke-linejoin: round; stroke-width: 1.6; width: .85rem; }
.icon-action:hover:not(:disabled) { background: var(--accent-soft); border-color: var(--accent); color: var(--accent); }
.icon-action.danger:hover:not(:disabled) { background: var(--danger-soft); border-color: var(--danger); color: var(--danger); }
.app-card-meta { align-items: center; color: var(--muted); display: flex; font-size: .7rem; gap: .4rem; grid-column: 2 / -1; grid-row: 2; justify-content: space-between; }
</style>
