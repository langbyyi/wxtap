<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref } from 'vue';
type Tab = { name: string; path: string; content: string; language: string; kind: string; dataUrl: string; truncated: boolean; lines: number; hitLine?: number; hitText?: string };
// 渲染窗口内的一行：number 是文件里的真实行号，html 是已转义、带语法着色
// 的 HTML（utils/code-highlight.ts 产出，只有 tok-* span 和文本）。
type Row = { number: number; html: string };

const props = defineProps<{
  tabs: Array<Pick<Tab, 'name' | 'path'>>;
  active: string;
  activeTab?: Tab;
  // 窗口内的行（大文件只渲染窗口内的一段），行号与着色都由父级算好；null 表示无可渲染内容
  rows?: Row[] | null;
  totalLines?: number;
  wrap?: boolean;
  // 工具栏显示的项目内相对路径（完整路径太长，省略号会吃掉文件名）
  displayPath?: string;
}>();

const emit = defineEmits<{
  select: [path: string];
  close: [path: string];
  closeAll: [];
  copyPath: [path: string];
  copyContent: [];
  'update:wrap': [value: boolean];
  loadBefore: [];
  loadMore: [];
  gotoHit: [];
  clearHit: [];
}>();

const codePre = ref<HTMLElement | null>(null);
// 图片与二进制文件没有行号、没有命中行，也没有换行开关可言：只有源码才走
// 下面那一整套行窗口。
const isSource = computed(() => (props.activeTab?.kind ?? 'text') === 'text');
const rows = computed(() => props.rows ?? []);
const firstLine = computed(() => rows.value[0]?.number ?? 1);
const lastRenderedLine = computed(() => (rows.value.length ? rows.value[rows.value.length - 1].number : 0));
// 行号列要装得下最长的行号：超过 5 位数的文件用固定宽度会被切掉。
const gutterWidth = computed(() => `${Math.max(3, String(Math.max(lastRenderedLine.value, props.totalLines ?? 0)).length + 1)}ch`);
// 一个反编译产物里同名文件很常见（根目录和子目录各有一个 page-frame.js）。重名时
// 标签带上父目录，否则两个页签长得一模一样，只能靠悬停分辨。
const labels = computed(() => {
  const seen = new Map<string, number>();
  for (const tab of props.tabs) seen.set(tab.name, (seen.get(tab.name) ?? 0) + 1);
  return new Map(props.tabs.map((tab) => [tab.path, (seen.get(tab.name) ?? 0) > 1 ? parentLabel(tab.path, tab.name) : tab.name]));
});

function parentLabel(path: string, name: string) {
  const parts = path.replace(/\\/g, '/').split('/').filter(Boolean);
  return parts.length > 1 ? `${parts[parts.length - 2]}/${name}` : name;
}

let flashTimer: ReturnType<typeof setTimeout> | undefined;

// 把命中行滚到视野中间。用 rect 差值而不是 offsetTop：代码区不是定位元素，
// offsetTop 量到的是页面顶部，会把滚动位置算到文件末尾去。
// 没有命中行时回到窗口开头（切换标签、向上加载后的落点）。
// 落位后命中行闪一下再褪回常亮高亮：单色背景在代码里并不显眼，闪动负责把视线带过去。
function revealHit() {
  void nextTick(() => {
    const pre = codePre.value;
    if (!pre) return;
    const hit = pre.querySelector<HTMLElement>('.code-line.is-hit');
    if (!hit) {
      pre.scrollTop = 0;
      return;
    }
    pre.scrollTop = Math.max(0, hit.getBoundingClientRect().top - pre.getBoundingClientRect().top + pre.scrollTop - pre.clientHeight / 2);
    hit.classList.remove('is-hit-flash');
    // 强制回流让同一行可以重复触发动画
    void hit.offsetWidth;
    hit.classList.add('is-hit-flash');
    if (flashTimer) clearTimeout(flashTimer);
    flashTimer = setTimeout(() => hit.classList.remove('is-hit-flash'), 1500);
  });
}
function scrollToTop() {
  void nextTick(() => {
    if (codePre.value) codePre.value.scrollTop = 0;
  });
}
defineExpose({ revealHit, scrollToTop });
onBeforeUnmount(() => {
  if (flashTimer) clearTimeout(flashTimer);
});
</script>

<template>
  <div v-if="tabs.length" role="tablist" class="tab-strip">
    <div v-for="tab in tabs" :key="tab.path" class="tab" :class="{ active: active === tab.path }">
      <button type="button" class="tab-select" role="tab" :title="tab.path" :aria-selected="active === tab.path" @click="emit('select', tab.path)" @auxclick.middle.prevent="emit('close', tab.path)">{{ labels.get(tab.path) }}</button>
      <button type="button" class="tab-close" :aria-label="'关闭 ' + labels.get(tab.path)" @click="emit('close', tab.path)">×</button>
    </div>
    <button class="ghost small tab-close-all" data-testid="close-all-tabs" type="button" title="关闭全部标签" @click="emit('closeAll')">关闭全部</button>
  </div>
  <template v-if="activeTab">
    <header class="editor-toolbar">
      <span class="mono-path" :title="activeTab.path">{{ displayPath || activeTab.path }}</span>
      <span class="status-line editor-meta">{{ activeTab.language }}<template v-if="activeTab.lines"> · {{ activeTab.lines }} 行</template></span>
      <div v-if="isSource && activeTab.hitLine" class="hit-tools" data-testid="hit-tools">
        <span class="hit-chip" data-testid="hit-line-chip">命中行 {{ activeTab.hitLine }}</span>
        <button data-testid="goto-hit" class="ghost small" type="button" title="跳转至命中行" @click="emit('gotoHit')">回到命中行</button>
        <button data-testid="clear-hit" class="ghost small" type="button" title="移除命中行标记" @click="emit('clearHit')">清除高亮</button>
      </div>
      <div class="actions">
        <button data-testid="copy-file-path" class="ghost small" type="button" @click="emit('copyPath', activeTab.path)">复制路径</button>
        <button data-testid="copy-file-content" class="ghost small" type="button" :disabled="!activeTab.content" @click="emit('copyContent')">复制内容</button>
        <label v-if="isSource" class="check-inline wrap-toggle" title="长行按窗口宽度折行显示"><input data-testid="wrap-toggle" type="checkbox" :checked="wrap" @change="emit('update:wrap', ($event.target as HTMLInputElement).checked)"> 自动换行</label>
      </div>
    </header>
    <p v-if="activeTab.truncated" class="callout warning file-warning">文件超过 1MB，仅显示前 1MB 内容；搜索与源码定位仍可正常使用。</p>
    <!-- 图片直接画出来；二进制文件给一句话说明。把 PNG 的字节塞进高亮器只会
         得到满屏乱码，正是这个分支要避免的。 -->
    <img v-if="activeTab.kind === 'image' && activeTab.dataUrl" class="image-preview" data-testid="code-image" :src="activeTab.dataUrl" :alt="activeTab.name">
    <div v-else-if="!isSource" class="empty-state code-empty" data-testid="binary-notice">
      <strong>{{ activeTab.kind === 'image' ? '图片未显示' : '无法按源码显示' }}</strong>
      <span>{{ activeTab.content || '该文件没有可显示的源码内容。' }}</span>
    </div>
    <template v-else>
      <button v-if="firstLine > 1" class="ghost small code-window-more" data-testid="load-before" type="button" @click="emit('loadBefore')">↑ 上方还有 {{ firstLine - 1 }} 行未渲染 · 加载上一屏</button>
      <pre v-if="activeTab.content" ref="codePre" data-testid="code-content" class="code-content" :class="{ 'is-wrap': wrap }"><span v-for="row in rows" :key="row.number" class="code-line" :class="{ 'is-hit': row.number === activeTab.hitLine }" :data-line="row.number"><span class="code-gutter" :style="{ width: gutterWidth }" aria-hidden="true">{{ row.number }}</span><span class="code-text" v-html="row.html"></span></span></pre>
      <div v-else class="empty-state code-empty"><strong>空文件</strong><span>该反编译产物不包含任何内容。</span></div>
      <button v-if="totalLines && lastRenderedLine < totalLines" class="ghost small code-window-more" data-testid="load-more" type="button" @click="emit('loadMore')">↓ 下方还有 {{ totalLines - lastRenderedLine }} 行未渲染 · 继续加载</button>
    </template>
  </template>
</template>

<style>
.tab-close-all { align-self: center; margin-left: auto; }
.tab-strip { border-bottom: 1px solid var(--border); display: flex; flex-wrap: wrap; gap: .15rem; padding: .35rem .5rem 0; }
.tab { align-items: center; border-radius: var(--radius-sm) var(--radius-sm) 0 0; color: var(--muted); display: inline-flex; }
.tab:hover, .tab.active { background: var(--panel-2); color: var(--text); }
.tab-select, .tab-close { background: transparent; border: 0; color: inherit; }
.tab-select { border-radius: inherit; font-weight: 500; padding: .4rem .35rem .4rem .7rem; }
.tab-close { color: var(--faint); padding: .2rem .45rem; }
.editor-toolbar { align-items: center; border-bottom: 1px solid var(--border); display: flex; flex-wrap: wrap; gap: .75rem; justify-content: space-between; padding: .5rem .9rem; }
.wrap-toggle { color: var(--muted); font-size: .78rem; white-space: nowrap; }
.hit-tools { align-items: center; display: inline-flex; gap: .4rem; }
.hit-chip { background: var(--accent-soft); border-radius: 999px; color: var(--accent-strong); font-size: .74rem; padding: .12rem .6rem; white-space: nowrap; }
.file-warning { margin: .75rem .9rem 0; }
.code-empty { flex: 1; }
/* 内联图片：按原始像素显示（小图标不会被拉糊），装不下时按比例缩到编辑区里，
   margin: auto 负责居中。 */
.image-preview { align-self: center; display: block; margin: auto; max-height: 100%; max-width: 100%; min-height: 0; }
.code-window-more { border-radius: 0; font-size: .78rem; margin: 0; padding: .35rem .9rem; text-align: center; width: 100%; }
.code-window-more:hover { background: var(--panel-2); }
.mono-path { color: var(--muted); font-family: var(--mono); font-size: .76rem; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.code-content { border: 0; border-radius: 0; flex: 1; font-size: .8rem; margin: 0; max-height: none; min-height: 0; overflow: auto; tab-size: 4; white-space: pre; }
.code-content.is-wrap, .code-content.is-wrap .code-text { overflow-wrap: anywhere; white-space: pre-wrap; }
.code-line { display: flex; min-height: 1.35em; }
.code-line:hover { background: var(--panel-2); }
.code-gutter { border-right: 1px solid var(--border); color: var(--faint); flex: none; font-variant-numeric: tabular-nums; margin-right: .7rem; padding-right: .7rem; text-align: right; user-select: none; }
.code-text { white-space: pre; }
/* 语法着色：颜色都来自主题变量（base.css 的 --tok-*），这里只负责挂到类上。 */
.code-content .tok-kw { color: var(--tok-keyword); }
.code-content .tok-str { color: var(--tok-string); }
.code-content .tok-num { color: var(--tok-number); }
.code-content .tok-comment { color: var(--tok-comment); font-style: italic; }
.code-content .tok-tag { color: var(--tok-tag); }
.code-content .tok-attr { color: var(--tok-attr); }
.code-content .tok-fn { color: var(--tok-fn); }
.code-content .tok-lit { color: var(--tok-lit); }
.code-content .tok-expr { color: var(--tok-expr); }
.code-line.is-hit { background: var(--accent-soft); box-shadow: inset 3px 0 0 var(--accent); }
.code-line.is-hit .code-gutter { color: var(--accent-strong); }
/* 命中值在行内的精确落点：扫描结果跳转后直接看到匹配的子串。 */
.code-text mark.code-hit-text { background: var(--accent); border-radius: 2px; color: var(--accent-text); padding: 0 .05em; }
/* 跳转落位时的闪动：亮起后褪回常亮高亮，把视线带到目标行。 */
.code-line.is-hit-flash { animation: code-hit-flash 1.4s ease-out 1; }
@keyframes code-hit-flash {
  0%, 35% { background: var(--accent-flash); }
  100% { background: var(--accent-soft); }
}
</style>
