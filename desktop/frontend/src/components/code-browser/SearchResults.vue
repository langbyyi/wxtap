<script setup lang="ts">
type SearchResult = { file: string; line: number; text: string };

defineProps<{
  results: SearchResult[];
  searching: boolean;
  truncated?: boolean;
  resultPath: (file: string) => string;
}>();

const emit = defineEmits<{ open: [result: SearchResult] }>();
</script>

<template>
  <div class="search-results">
    <p v-if="truncated && results.length" class="callout warning result-limit" role="status">已达 {{ results.length }} 条上限，匹配结果可能不止于此；请缩小关键词以获得更精确的结果。</p>
    <ul v-if="results.length">
      <li v-for="result in results" :key="result.file + ':' + result.line">
        <button type="button" :title="result.file" @click="emit('open', result)">
          <code class="result-file">{{ resultPath(result.file) }}:{{ result.line }}</code>
          <span class="result-text">{{ result.text }}</span>
        </button>
      </li>
    </ul>
    <div v-else class="empty-state">
      <strong>{{ searching ? '搜索中' : '没有匹配结果' }}</strong>
      <span>{{ searching ? '正在扫描当前小程序源码。' : '请更换关键词，或关闭正则模式后重试。' }}</span>
    </div>
  </div>
</template>

<style>
.search-results { flex: 1; overflow-y: auto; padding: .4rem; }
.result-limit { font-size: .74rem; margin: 0 0 .4rem; }
.search-results ul { display: grid; gap: .2rem; list-style: none; margin: 0; padding: 0; }
.search-results button { background: transparent; border: 0; border-radius: var(--radius-sm); color: var(--text); display: grid; font-weight: 400; gap: .1rem; padding: .3rem .5rem; text-align: left; width: 100%; }
.search-results button:hover { background: var(--panel-2); }
.result-file { color: var(--accent-strong); font-family: var(--mono); font-size: .74rem; }
.result-text { color: var(--muted); font-family: var(--mono); font-size: .76rem; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
