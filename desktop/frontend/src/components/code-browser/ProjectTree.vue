<script setup lang="ts">
import SearchResults from './SearchResults.vue';

type Node = { name: string; path: string; isDir: boolean; children?: Node[]; expanded?: boolean; loaded?: boolean };
type TreeRow = { node: Node; depth: number };
type SearchResult = { file: string; line: number; text: string };

defineProps<{
  sidebarTitle: string;
  active: string;
  query: string;
  regex: boolean;
  searchResults: SearchResult[];
  searching: boolean;
  searchTruncated: boolean;
  openingProject: boolean;
  projectsCount: number;
  visibleNodes: TreeRow[];
  resultPath: (file: string) => string;
}>();

const emit = defineEmits<{
  'update:query': [value: string];
  'update:regex': [value: boolean];
  search: [];
  searchNow: [];
  clearSearch: [];
  openResult: [result: SearchResult];
  select: [node: Node];
}>();

function testId(path: string) {
  return 'tree-' + path.replace(/[^A-Za-z0-9]/g, '_');
}
</script>

<template>
  <aside class="panel tree-panel">
    <header class="panel-header">
      <h2>{{ sidebarTitle }}</h2>
      <span v-if="query" class="status-line" role="status">{{ searching ? '搜索中…' : '命中 ' + searchResults.length + (searchTruncated ? '+' : '') + ' 处' }}</span>
      <!-- 项目选择与刷新挂在列表自己的头上：页面顶部不再为它们单独留一行。 -->
      <slot name="header-actions" />
    </header>

    <div class="sidebar-search">
      <input data-testid="code-search" :value="query" type="search" class="search-input" placeholder="在当前小程序内搜索代码…" aria-label="搜索代码" :disabled="!visibleNodes.length && !projectsCount" @input="emit('update:query', ($event.target as HTMLInputElement).value); emit('search')" @keydown.esc="emit('clearSearch')">
      <button v-if="query" class="ghost small" type="button" @click="emit('clearSearch')">清除</button>
      <label class="check-inline"><input data-testid="regex-search" :checked="regex" type="checkbox" :disabled="!visibleNodes.length && !projectsCount" @change="emit('update:regex', ($event.target as HTMLInputElement).checked); emit('searchNow')"> 正则</label>
    </div>

    <SearchResults v-if="query" :results="searchResults" :searching="searching" :truncated="searchTruncated" :result-path="resultPath" @open="emit('openResult', $event)" />

    <div v-if="openingProject" class="empty-state"><strong>正在加载源码树</strong><span>正在读取当前反编译产物。</span></div>
    <ul v-else class="tree">
      <li v-for="row in visibleNodes" :key="row.node.path" :style="{ paddingLeft: row.depth * 14 + 'px' }">
        <button :data-testid="testId(row.node.path)" type="button" :class="{ active: active === row.node.path }" :aria-expanded="row.node.isDir ? !!row.node.expanded : undefined" @click="emit('select', row.node)">
          <span class="tree-icon" aria-hidden="true">{{ row.node.isDir ? (row.node.expanded ? '▾' : '▸') : '·' }}</span>
          <span class="tree-name">{{ row.node.name }}</span>
        </button>
      </li>
      <li v-if="!visibleNodes.length"><div class="empty-state"><strong>{{ projectsCount ? '暂无可显示文件' : '暂无反编译产物' }}</strong><span>{{ projectsCount ? '当前小程序没有可浏览的源码文件。' : '请先在「反编译」页完成反编译。' }}</span></div></li>
    </ul>
  </aside>
</template>

<style>
.sidebar-search { align-items: center; border-bottom: 1px solid var(--border); display: flex; flex-wrap: wrap; gap: .4rem; padding: .5rem .6rem; }
.sidebar-search .search-input { flex: 1; max-width: none; min-width: 8rem; }
.tree-panel { display: flex; flex-direction: column; height: 100%; min-height: 0; overflow: hidden; }
.tree { flex: 1; list-style: none; margin: 0; overflow-y: auto; padding: .4rem; }
.tree > li > button { align-items: center; background: transparent; border: 0; border-radius: var(--radius-sm); color: var(--text); display: flex; font-weight: 400; gap: .35rem; justify-content: flex-start; padding: .3rem .5rem; text-align: left; width: 100%; }
.tree > li > button:hover { background: var(--panel-2); }
.tree > li > button.active { background: var(--accent-soft); color: var(--accent-strong); }
.tree-icon { color: var(--faint); flex: none; width: 1rem; }
.tree-name { font-family: var(--mono); font-size: .8rem; overflow-wrap: anywhere; }
</style>
