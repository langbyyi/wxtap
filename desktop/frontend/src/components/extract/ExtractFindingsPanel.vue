<script setup lang="ts">
import type { ExtractFinding } from './types';

// 每页档位：几千条命中时 50/页要翻几十页，给几档让用户自己权衡（一页的渲染量有界）
const PAGE_SIZES = [25, 50, 100, 200];

// pageOffset / pageSize 给了缺省值：这两个值是「分页大小属于页面」时代的产物，页面已经显式
// 传入；给默认值让只关心展示的调用方（测试、将来的纯展示用法）不必知道分页细节。
withDefaults(defineProps<{
  resultApp: string;
  findingQuery: string;
  findingCategories: { key: string; label: string; count: number }[];
  categoryFilter: string;
  visibleFindings: ExtractFinding[];
  pagedFindings: ExtractFinding[];
  findingPage: number;
  findingPageCount: number;
  // 本页第一行在整份结果里的序号偏移：分页大小属于页面，面板不该自己再写一遍
  pageOffset?: number;
  // 每页条数：分页大小属于页面（ExtractView 决定），面板只负责展示与上报
  pageSize?: number;
  resultOutputDir: string;
}>(), { pageSize: 50, pageOffset: 0 });

const emit = defineEmits<{
  'update:findingQuery': [value: string];
  'update:categoryFilter': [value: string];
  copyResults: [];
  copyFinding: [finding: ExtractFinding];
  viewSource: [finding: ExtractFinding];
  setPage: [page: number];
  setPageSize: [size: number];
}>();
</script>

<template>
  <div class="panel findings-panel" aria-live="polite">
    <header class="panel-header">
      <h2>敏感信息提取结果<span v-if="resultApp"> · {{ resultApp }}</span></h2>
      <div class="toolbar">
        <input v-if="resultApp" :value="findingQuery" class="search-input" type="search" aria-label="搜索敏感信息" placeholder="查找…" data-testid="finding-search" @input="emit('update:findingQuery', ($event.target as HTMLInputElement).value)">
        <button data-testid="copy-extract-results" class="secondary small" type="button" @click="emit('copyResults')">复制</button>
      </div>
    </header>

    <template v-if="resultApp">
      <div class="panel-section stack">
        <div class="category-tabs" data-testid="category-tabs">
          <button v-for="item in findingCategories" :key="item.key" class="chip" :class="{ active: categoryFilter === item.key }" :data-testid="`category-${item.key}`" type="button" @click="emit('update:categoryFilter', item.key)">{{ item.label }} <span class="chip-count">{{ item.count }}</span></button>
        </div>
      </div>

      <template v-if="visibleFindings.length">
        <div class="table-wrap">
          <table class="data-table findings-table" data-testid="findings-table">
            <thead><tr><th class="index-col">序号</th><th>匹配结果</th><th>文件路径</th><th>查看源码</th></tr></thead>
            <tbody>
              <tr v-for="(finding, index) in pagedFindings" :key="finding.id" :data-testid="`finding-${finding.id}`">
                <td class="mono">{{ pageOffset + index + 1 }}</td>
                <td><code class="finding-value">{{ finding.value }}</code></td>
                <td class="mono">{{ finding.file }}:{{ finding.line }}</td>
                <td><div class="row-actions"><button class="secondary small" type="button" @click="emit('copyFinding', finding)">复制</button><button class="ghost small" type="button" :disabled="!resultOutputDir" @click="emit('viewSource', finding)">查看源码</button></div></td>
              </tr>
            </tbody>
          </table>
        </div>
        <!-- 分页留在表格的滚动区之外：表格滚到下面时，翻页控件不该跟着滚走 -->
        <!-- 只要有命中就显示这一条：条数、每页大小与翻页放在一起，单页时也看得到总数与页大小 -->
        <nav v-if="visibleFindings.length" class="finding-pagination" aria-label="扫描结果分页">
          <span>共 {{ visibleFindings.length }} 条<template v-if="findingPageCount > 1">，第 {{ findingPage }} / {{ findingPageCount }} 页</template></span>
          <label class="check-inline">每页
            <select :value="pageSize" data-testid="findings-page-size" aria-label="每页条数" @change="emit('setPageSize', Number(($event.target as HTMLSelectElement).value))">
              <option v-for="size in PAGE_SIZES" :key="size" :value="size">{{ size }}</option>
            </select>
          </label>
          <template v-if="findingPageCount > 1">
            <button class="ghost small" type="button" :disabled="findingPage === 1" @click="emit('setPage', findingPage - 1)">上一页</button>
            <button class="ghost small" type="button" :disabled="findingPage === findingPageCount" @click="emit('setPage', findingPage + 1)">下一页</button>
          </template>
        </nav>
      </template>
      <p v-else class="status-line panel-section" role="status">当前筛选下没有结构化发现。</p>

    </template>

    <div v-else class="empty-state findings-empty"><strong>等待选择小程序</strong><span>选择一个小程序并提取敏感信息后，结果将在这里显示。</span></div>
  </div>
</template>

<style scoped>
/* 结果表是这一页的主体：面板吃满分给它的高度，滚动发生在表格自己身上，
   而不是让整页滚走——那样滚到下面时列名就没了，看不出哪列是哪列。 */
.findings-panel { display: flex; flex-direction: column; min-height: 0; overflow: hidden; }

/* 表头/筛选/分页是固定框，只有表格滚动 */
.findings-panel > .panel-header,
.findings-panel > .panel-section,
.findings-panel > .finding-pagination { flex: none; }

/* 表头这排只是标题、查找和复制，高度按内容压到最小，省下的都给结果表。标题里的
   appid 长短不一，换行会把这一排顶成两行（按钮跟着掉下去），所以标题自己截断、
   这一排不换行。分类页签自带内边距，section 的 1.1rem 会在页签和表头之间留空带。 */
.findings-panel > .panel-header { flex-wrap: nowrap; gap: .5rem; padding: .45rem .7rem; }
.findings-panel > .panel-header h2 { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.findings-panel > .panel-header .toolbar { flex: none; }
.findings-panel > .panel-section { padding: 0; }

.findings-panel .table-wrap { flex: 1 1 auto; }

/* border-collapse 的表格里，粘住的单元格带不走边框，用 inset 阴影把表头下沿补回来；
   表头必须自带底色，否则行会从它下面透出来。 */
.findings-table thead th {
  background: var(--panel-2);
  box-shadow: inset 0 -1px 0 var(--border);
  position: sticky;
  top: 0;
  z-index: 1;
}

.search-input { max-width: 16rem; min-width: 11rem; }
.findings-table td:first-child, .findings-table td:last-child { white-space: nowrap; }
.row-actions { display: flex; flex-wrap: nowrap; gap: .35rem; }
.finding-pagination { align-items: center; border-top: 1px solid var(--border); color: var(--muted); display: flex; font-size: .78rem; gap: .45rem; justify-content: flex-end; padding: .55rem .7rem; }
.finding-pagination span { margin-right: auto; }
/* 全局 select 是 width:100%：在分页条里会吃掉整行，收成内容宽 */
.finding-pagination select { width: auto; }
.category-tabs { align-items: stretch; border-bottom: 1px solid var(--border); display: flex; gap: .35rem; overflow-x: auto; padding: 0 .5rem; }
.category-tabs .chip { align-items: center; background: transparent; border: 0; border-bottom: 2px solid transparent; border-radius: 0; color: var(--muted); display: inline-flex; flex: none; gap: .35rem; padding: .5rem .6rem .4rem; white-space: nowrap; }
.category-tabs .chip:hover { background: var(--panel-2); color: var(--text); }
.category-tabs .chip.active { background: transparent; border-bottom-color: var(--accent); color: var(--accent); }
.chip-count { color: var(--muted); font-family: var(--mono); font-size: .72rem; }
.findings-table .index-col { width: 2.6rem; }
.finding-value { display: block; font-family: var(--mono); font-size: .78rem; margin-top: .15rem; overflow-wrap: anywhere; }
.findings-empty { margin: 3rem 1rem; }
</style>
