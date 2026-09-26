<script setup lang="ts">
import { computed, h, onMounted, ref, type FunctionalComponent } from 'vue';
import { backend } from '../api/bridge';
import { messageOf } from '../utils/format';
import { copyText, notify } from '../utils/notify';
import PageHeader from '../components/PageHeader.vue';

type InlinePart = { text: string; href?: string; bold?: boolean; code?: boolean };
type Block =
  | { tag: 'h2' | 'h3' | 'p'; parts: InlinePart[] }
  | { tag: 'blockquote'; lines: InlinePart[][] }
  | { tag: 'ul' | 'ol'; items: InlinePart[][] };

// 与 latest.json 同一仓库的固定地址。保持与 update.ReleaseRepo、release.yml 的
// RELEASE_REPO 一致，文档随发布同步（docs/FEEDBACK.md）。
const feedbackUrl = 'https://raw.githubusercontent.com/langbyyi/wxtap/main/feedback.md';
// 提交反馈的入口是仓库的 Issues。
const issuesUrl = 'https://github.com/langbyyi/wxtap/issues';
// raw.githubusercontent.com 在国内并不稳定：把最近一次成功的内容留在
// localStorage，进页先上屏缓存再后台刷新，拉取失败时页面也仍然可用。
const cacheKey = 'feedback-doc-cache';
const markdown = ref('');
const error = ref('');
const loading = ref(true);
const loadedAt = ref('');

// 渲染的是自己发布的文档，但仍按不可信输入对待：一切内容都走文本节点，
// 链接只接受 http(s) 绝对地址，其余 markdown 记号原样显示。
const inlinePattern = /\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)|\*\*([^*]+)\*\*|`([^`]+)`/g;
function inlineParts(line: string): InlinePart[] {
  const output: InlinePart[] = [];
  const push = (part: InlinePart) => {
    if (part.text || part.href) output.push(part);
  };
  inlinePattern.lastIndex = 0;
  let last = 0;
  let match: RegExpExecArray | null;
  while ((match = inlinePattern.exec(line))) {
    push({ text: line.slice(last, match.index) });
    if (match[2] !== undefined) output.push({ text: match[1] ?? '', href: match[2] });
    else if (match[3] !== undefined) push({ text: match[3], bold: true });
    else push({ text: match[4] ?? '', code: true });
    last = inlinePattern.lastIndex;
  }
  push({ text: line.slice(last) });
  return output;
}

function appendItem(blocks: Block[], tag: 'ul' | 'ol', text: string) {
  const lastBlock = blocks[blocks.length - 1];
  if (lastBlock && lastBlock.tag === tag) lastBlock.items.push(inlineParts(text));
  else blocks.push({ tag, items: [inlineParts(text)] });
}

function parseBlocks(doc: string): Block[] {
  const blocks: Block[] = [];
  for (const rawLine of doc.split(/\r?\n/)) {
    const line = rawLine.trim();
    if (!line) continue;
    const heading = /^(#{1,6}) (.+)$/.exec(line);
    if (heading) {
      blocks.push({ tag: heading[1].length <= 2 ? 'h2' : 'h3', parts: inlineParts(heading[2]) });
      continue;
    }
    if (line.startsWith('> ')) {
      const parts = inlineParts(line.slice(2));
      const lastBlock = blocks[blocks.length - 1];
      if (lastBlock && lastBlock.tag === 'blockquote') lastBlock.lines.push(parts);
      else blocks.push({ tag: 'blockquote', lines: [parts] });
      continue;
    }
    const unordered = /^[-*] (.+)$/.exec(line);
    if (unordered) {
      appendItem(blocks, 'ul', unordered[1]);
      continue;
    }
    const ordered = /^\d+[.、] (.+)$/.exec(line);
    if (ordered) {
      appendItem(blocks, 'ol', ordered[1]);
      continue;
    }
    blocks.push({ tag: 'p', parts: inlineParts(line) });
  }
  return blocks;
}

const blocks = computed<Block[]>(() => parseBlocks(markdown.value));

// 行内片段渲染器：链接/加粗/代码都是新建元素，不存在 HTML 注入路径。
const InlineParts: FunctionalComponent<{ parts: InlinePart[] }> = ({ parts }) =>
  h(
    'span',
    parts.map((part) => {
      if (part.href) return h('a', { href: part.href, target: '_blank', rel: 'noopener noreferrer' }, part.text);
      if (part.bold) return h('strong', part.text);
      if (part.code) return h('code', part.text);
      return part.text;
    }),
  );
InlineParts.props = { parts: { type: Array, required: true } };

function readCache(): { text: string; at: string } | null {
  try {
    const parsed = JSON.parse(localStorage.getItem(cacheKey) ?? '') as { text?: unknown; at?: unknown };
    if (typeof parsed.text === 'string' && typeof parsed.at === 'string') return { text: parsed.text, at: parsed.at };
  } catch {
    // 没有缓存或缓存损坏，都当作没有缓存
  }
  return null;
}

async function load(manual = false) {
  loading.value = true;
  error.value = '';
  try {
    markdown.value = (await backend.call<{ text?: string }>('fetch.md', { url: feedbackUrl })).text ?? '';
    loadedAt.value = new Date().toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' });
    try {
      localStorage.setItem(cacheKey, JSON.stringify({ text: markdown.value, at: loadedAt.value }));
    } catch {
      // 缓存放不进去就放弃缓存，刷新机制不受影响
    }
    // 进页面的一次不提示，只有手动刷新才提示，避免每次切换页面都被打扰
    if (manual) notify('反馈内容已更新', 'success');
  } catch (reason) {
    error.value = messageOf(reason);
    notify(`反馈内容加载失败：${messageOf(reason)}`, 'error');
  } finally {
    loading.value = false;
  }
}

async function copyContent() {
  if (markdown.value) await copyText(markdown.value, '反馈内容');
}

function openSource() {
  backend.call('shell.openUrl', { url: feedbackUrl }).catch((reason) => {
    notify(`打开原文档失败：${messageOf(reason)}`, 'error');
  });
}

function openIssues() {
  backend.call('shell.openUrl', { url: issuesUrl }).catch((reason) => {
    notify(`打开 Issue 页失败：${messageOf(reason)}`, 'error');
  });
}

// 正文链接交给系统浏览器：Wails 的 WebView2 宿主会吞掉新窗口，所以真实壳里
// 拦下点击走 shell.openUrl；无壳的浏览器调试保留默认跳转便于开发。
function openLink(event: MouseEvent) {
  if (!window.go?.main?.App?.Call) return;
  const anchor = (event.target as HTMLElement | null)?.closest('a');
  const href = anchor?.getAttribute('href');
  if (!href) return;
  event.preventDefault();
  backend.call('shell.openUrl', { url: href }).catch((reason) => {
    notify(`打开链接失败：${messageOf(reason)}`, 'error');
  });
}

// 有缓存就先同步上屏（不等网络），onMounted 里仍会后台刷新一次
const cached = readCache();
if (cached) {
  markdown.value = cached.text;
  loadedAt.value = cached.at;
}
onMounted(() => void load());
</script>

<template>
  <section class="feedback-view" aria-labelledby="feedback-title">
    <PageHeader title="交流反馈" title-id="feedback-title" />

    <p v-if="loading && !markdown" class="status-line" role="status">加载中…</p>
    <div v-else-if="error && !markdown" class="panel">
      <div class="panel-body stack">
        <p class="error" role="alert">{{ error }}</p>
        <div class="actions">
          <button type="button" @click="load(true)">重试</button>
          <button type="button" class="secondary" @click="openIssues">提交 Issue</button>
          <button type="button" class="secondary" @click="openSource">打开原文档</button>
        </div>
      </div>
    </div>
    <article v-else class="panel markdown">
      <header class="panel-header">
        <h2>交流反馈</h2>
        <div class="toolbar">
          <span v-if="loadedAt" class="status-line">最近更新：{{ loadedAt }}</span>
          <button class="small" type="button" @click="openIssues">提交 Issue</button>
          <button v-if="markdown" class="secondary small" type="button" @click="copyContent">复制内容</button>
          <button class="secondary small" type="button" :disabled="loading" @click="load(true)">{{ loading ? '加载中…' : '重新加载' }}</button>
          <button class="secondary small" type="button" @click="openSource">打开原文档</button>
        </div>
      </header>
      <p v-if="error" class="panel-body error" role="alert">刷新失败，下面显示的仍是上次内容：{{ error }}</p>
      <div v-if="blocks.length" class="panel-body" @click="openLink">
        <template v-for="(block, index) in blocks" :key="index">
          <p v-if="block.tag === 'p'"><InlineParts :parts="block.parts" /></p>
          <h2 v-else-if="block.tag === 'h2'"><InlineParts :parts="block.parts" /></h2>
          <h3 v-else-if="block.tag === 'h3'"><InlineParts :parts="block.parts" /></h3>
          <blockquote v-else-if="block.tag === 'blockquote'">
            <p v-for="(quoteLine, quoteIndex) in block.lines" :key="quoteIndex"><InlineParts :parts="quoteLine" /></p>
          </blockquote>
          <ul v-else-if="block.tag === 'ul'" class="md-list">
            <li v-for="(item, itemIndex) in block.items" :key="itemIndex"><InlineParts :parts="item" /></li>
          </ul>
          <ol v-else-if="block.tag === 'ol'" class="md-list">
            <li v-for="(item, itemIndex) in block.items" :key="itemIndex"><InlineParts :parts="item" /></li>
          </ol>
        </template>
      </div>
      <div v-else class="empty-state">
        <strong>暂无内容</strong>
        <span>反馈文档暂时无法读取，请稍后重试。</span>
      </div>
    </article>
  </section>
</template>

<style scoped>
.markdown h2 {
  font-size: 1.05rem;
  font-weight: 650;
  margin: 1.25rem 0 .5rem;
}

.markdown h2:first-child {
  margin-top: 0;
}

.markdown h3 {
  font-size: .95rem;
  font-weight: 650;
  margin: 1rem 0 .4rem;
}

.markdown p {
  color: var(--muted);
  margin: .4rem 0;
  max-width: 44rem;
}

.markdown .md-list {
  color: var(--muted);
  margin: .4rem 0;
  padding-left: 1.5rem;
}

.markdown .md-list li {
  margin: .2rem 0;
  max-width: 44rem;
}

.markdown blockquote {
  border-left: 2px solid var(--border);
  color: var(--faint);
  margin: .6rem 0;
  padding-left: .8rem;
}

.markdown blockquote p {
  margin: .2rem 0;
}

.markdown strong {
  color: var(--text);
}

.markdown code {
  background: var(--panel-2);
  border-radius: var(--radius-sm);
  font-family: var(--mono);
  font-size: .86em;
  padding: .05em .3em;
}

.markdown a {
  color: var(--accent-strong);
  text-decoration: none;
}

:root[data-theme='light'] .markdown a {
  color: var(--accent);
}

.markdown a:hover {
  text-decoration: underline;
}
</style>
