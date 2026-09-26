<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue';
import { backend } from '../api/bridge';
import { messageOf } from '../utils/format';
import { copyText } from '../utils/notify';
import { highlightLines, markHitText } from '../utils/code-highlight';
import CodeTabs from '../components/code-browser/CodeTabs.vue';
import ProjectTree from '../components/code-browser/ProjectTree.vue';
import PageHeader from '../components/PageHeader.vue';

type Node = { name: string; path: string; isDir: boolean; children?: Node[]; expanded?: boolean; loaded?: boolean };
// kind 决定编辑区画什么：text 走高亮源码，image 走内联图片，binary 只给一句说明。
type Tab = { name: string; path: string; content: string; language: string; kind: string; dataUrl: string; truncated: boolean; lines: number; hitLine?: number; hitText?: string };
type Project = { appid: string; name: string; path: string; mtime: number };

const root = ref('');
const projects = ref<Project[]>([]);
const selectedApp = ref('');
const tree = ref<Node[]>([]);
const tabs = ref<Tab[]>([]);
const active = ref('');
const query = ref('');
const regex = ref(false);
const searchResults = ref<{ file: string; line: number; text: string }[]>([]);
const error = ref('');
const loadingProjects = ref(false);
const openingProject = ref(false);
const searching = ref(false);
const SEARCH_DEBOUNCE_MS = 220;
let searchTimer: ReturnType<typeof setTimeout> | undefined;
let searchRequest = 0;
let projectRequest = 0;

const activeTab = computed(() => tabs.value.find((tab) => tab.path === active.value));
const currentProject = computed(() => projects.value.find((item) => item.appid === selectedApp.value));
const sidebarTitle = computed(() => (currentProject.value ? currentProject.value.name || currentProject.value.appid : '目录'));

function normalize(nodes: Node[]): Node[] {
  return nodes.map((node) => ({
    ...node,
    children: node.children ? normalize(node.children) : [],
    expanded: false,
    loaded: !!node.children,
  }));
}

function flattened(nodes: Node[], depth = 0): Array<{ node: Node; depth: number }> {
  // 不往节点上写 depth：computed 里改响应式状态是副作用，而且展开/折叠本来就是改
  // 节点自身，这里只描述「哪一层」，节点对象原样往下传（点击时还要改它）。
  return nodes.flatMap((node) => [
    { node, depth },
    ...(node.isDir && node.expanded ? flattened(node.children ?? [], depth + 1) : []),
  ]);
}

const visibleNodes = computed(() => flattened(tree.value));

function sameDir(left: string, right: string) {
  const normalize = (value: string) => value.trim().replace(/[\\/]+$/, '').replace(/\\/g, '/').toLowerCase();
  return !!left && !!right && normalize(left) === normalize(right);
}

function projectFilePath(projectPath: string, file: string) {
  const parts = file.trim().replace(/\\/g, '/').split('/').filter(Boolean);
  if (!parts.length || parts.some((part) => part === '..') || /^[A-Za-z]:$/.test(parts[0])) return '';
  return `${projectPath.replace(/[\\/]+$/, '')}/${parts.join('/')}`;
}

function resultPath(file: string) {
  const project = currentProject.value;
  if (!project) return file;
  const base = project.path.replace(/[\\/]+$/, '').replace(/\\/g, '/');
  const normalized = file.replace(/\\/g, '/');
  return normalized.toLowerCase().startsWith(`${base.toLowerCase()}/`) ? normalized.slice(base.length + 1) : file;
}

function resetSearch() {
  if (searchTimer) clearTimeout(searchTimer);
  searchTimer = undefined;
  searchRequest += 1;
  searching.value = false;
  query.value = '';
  searchResults.value = [];
  searchTruncated.value = false;
}

function clearOpenedProject() {
  root.value = '';
  tree.value = [];
  tabs.value = [];
  active.value = '';
}

// 只审计反编译产物：项目由 appid 打开，后端不会为代码浏览器弹出任意目录选择框。
async function loadProjects() {
  loadingProjects.value = true;
  error.value = '';
  try {
    const data = await backend.call<{ projects?: Project[] }>('code.projects');
    projects.value = data.projects ?? [];
    if (!projects.value.some((item) => item.appid === selectedApp.value)) {
      selectedApp.value = '';
      clearOpenedProject();
      resetSearch();
    }
    return true;
  } catch (reason) {
    error.value = `读取反编译产物失败: ${messageOf(reason)}`;
    return false;
  } finally {
    loadingProjects.value = false;
  }
}

async function reloadProjects() {
  const keep = selectedApp.value;
  if (!(await loadProjects())) return;
  const target = projects.value.find((item) => item.appid === keep) ?? projects.value[0];
  if (target) await selectProject(target.appid);
}

async function selectProject(appid: string) {
  const project = projects.value.find((item) => item.appid === appid);
  if (!project) return;
  const request = ++projectRequest;
  selectedApp.value = project.appid;
  openingProject.value = true;
  error.value = '';
  resetSearch();
  clearOpenedProject();
  try {
    const data = await backend.call<{ root?: string; tree?: Node[] }>('code.project', { appid: project.appid });
    if (request !== projectRequest) return;
    root.value = data.root ?? '';
    tree.value = normalize(data.tree ?? []);
  } catch (reason) {
    if (request !== projectRequest) return;
    error.value = `打开反编译产物失败: ${messageOf(reason)}`;
  } finally {
    if (request === projectRequest) openingProject.value = false;
  }
}

// 一次渲染的窗口行数：正常源码也就几屏，超大文件不必一次生成几万个行节点。
// 窗口两端各有「还有 N 行未渲染」的入口，不把没渲染的部分藏起来。
const CHUNK_LINES = 4000;
// 从搜索结果跳进来时命中行上方多留的上下文
const HIT_LEAD_LINES = 200;

const codeTabs = ref<InstanceType<typeof CodeTabs> | null>(null);
const windowStart = ref(1);
const windowEnd = ref(CHUNK_LINES);
// 反编译产物常是几百 KB 的单行代码，换行开关会被反复拨动，和主题一样记住它。
const WRAP_KEY = 'code-wrap';
const wrap = ref(localStorage.getItem(WRAP_KEY) === '1');
watch(wrap, (value) => { localStorage.setItem(WRAP_KEY, value ? '1' : '0'); });
const searchTruncated = ref(false);
const totalLines = computed(() => activeTab.value?.lines ?? 0);
const activeDisplayPath = computed(() => (activeTab.value ? resultPath(activeTab.value.path) : ''));
// 每个标签只分词一次：反编译产物最大 1MB，重复着色浪费明显，WeakMap 随标签一起释放。
const highlightCache = new WeakMap<Tab, string[]>();
// 行号列、命中行高亮都按窗口首行的真实行号对齐，窗口滑动时不会错位。
const renderedRows = computed(() => {
  const tab = activeTab.value;
  // 图片的 data URL 和二进制说明都不是源码：切行只会切出一堆乱码，直接不渲染行。
  if (!tab || tab.kind !== 'text' || !tab.content) return null;
  let highlighted = highlightCache.get(tab);
  if (!highlighted) {
    highlighted = highlightLines(tab.content, tab.language);
    highlightCache.set(tab, highlighted);
  }
  const rows: Array<{ number: number; html: string }> = [];
  const end = Math.min(windowEnd.value, highlighted.length);
  for (let number = windowStart.value; number <= end; number += 1) {
    let html = highlighted[number - 1] ?? '';
    // 扫描结果跳转时带上匹配值：命中行内把匹配子串精确标出来（压缩成一整行的
    // 产物尤其依赖这个，否则「整行高亮」等于没高亮）。跨 token 标不上就留在整行高亮。
    if (number === tab.hitLine && tab.hitText) html = markHitText(html, tab.hitText);
    rows.push({ number, html });
  }
  return rows;
});

// 打开或切换标签后的窗口：默认从文件开头看起，从搜索/提取结果跳进来时以命中行为准。
function openWindow(hitLine?: number) {
  const total = totalLines.value;
  // 行号可能来自另一次反编译的产物（文件已经变了）。超出文件时回到开头并说明，
  // 否则会渲染出一片空白，还报出「上方还有 4799 行」这种按 3 行文件算不出的数。
  const stale = !!hitLine && total > 0 && hitLine > total;
  if (stale) error.value = `第 ${hitLine} 行超出文件范围（共 ${total} 行），已从文件开头显示。`;
  windowStart.value = !stale && hitLine && hitLine > HIT_LEAD_LINES ? hitLine - HIT_LEAD_LINES : 1;
  windowEnd.value = windowStart.value + CHUNK_LINES - 1;
}

function loadMore() {
  windowEnd.value += CHUNK_LINES;
}

// 向上加载一屏：视线落到新加载内容的开头，而不是停在刚才的位置。
function loadBefore() {
  windowStart.value = Math.max(1, windowStart.value - CHUNK_LINES);
  void nextTick(() => codeTabs.value?.scrollToTop());
}

// 打开文件、切换标签都走这里：窗口与滚动位置一起就位。
function showFile(path: string, hitLine?: number, hitText?: string) {
  active.value = path;
  openWindow(hitLine);
  // 等这一帧的 DOM 落地再定位，否则量到的还是上一个文件的高度
  void nextTick(() => codeTabs.value?.revealHit());
}

// 点标签切回已打开的文件：带着它自己的命中锚点，别把位置丢回文件开头。
function activateTab(path: string) {
  const tab = tabs.value.find((item) => item.path === path);
  showFile(path, tab?.hitLine, tab?.hitLine ? tab.hitText : undefined);
}

function gotoHit() {
  codeTabs.value?.revealHit();
}

// 命中标记看完就撤：留在标签上会一直挡着后续代码的底色。
function clearHit() {
  const tab = activeTab.value;
  if (!tab) return;
  tab.hitLine = undefined;
  tab.hitText = undefined;
}

async function select(node: Node, hitLine?: number, hitText?: string) {
  if (node.isDir) {
    if (!node.loaded) {
      try {
        const data = await backend.call<{ children?: Node[] }>('code.expandDir', { path: node.path });
        node.children = normalize(data.children ?? []);
        node.loaded = true;
      } catch (reason) {
        error.value = `展开目录失败: ${messageOf(reason)}`;
        return;
      }
    }
    node.expanded = !node.expanded;
    return;
  }
  const existing = tabs.value.find((tab) => tab.path === node.path);
  if (existing) {
    // 同一文件再次被点到（或从搜索结果再命中一次）时刷新高亮行并跳过去
    existing.hitLine = hitLine;
    existing.hitText = hitLine ? hitText : undefined;
    showFile(node.path, hitLine);
    return;
  }
  try {
    const data = await backend.call<{ content?: string; size?: number; language?: string; truncated?: boolean; kind?: string; dataUrl?: string }>('code.readFile', { path: node.path });
    error.value = '';
    const content = data.content ?? '';
    const kind = data.kind ?? 'text';
    tabs.value.push({
      name: node.name,
      path: node.path,
      content,
      kind,
      dataUrl: data.dataUrl ?? '',
      language: data.language ?? 'text',
      truncated: data.truncated ?? (data.size ?? 0) > 1024 * 1024,
      lines: kind === 'text' && content ? content.split('\n').length : 0,
      hitLine,
      hitText: hitLine ? hitText : undefined,
    });
    showFile(node.path, hitLine);
  } catch (reason) {
    error.value = `读取文件失败: ${messageOf(reason)}`;
  }
}

function close(path: string) {
  const index = tabs.value.findIndex((tab) => tab.path === path);
  // 关一个已经不在的标签不该顺手关掉别的（splice(-1) 会删掉最后一个）
  if (index < 0) return;
  tabs.value.splice(index, 1);
  if (active.value !== path) return;
  const next = tabs.value[Math.max(0, index - 1)];
  if (next) activateTab(next.path);
  else active.value = '';
}

function closeAll() {
  tabs.value = [];
  active.value = '';
}

function clearSearch() {
  resetSearch();
}

function scheduleSearch() {
  searchRequest += 1;
  if (searchTimer) clearTimeout(searchTimer);
  searchTimer = setTimeout(() => {
    searchTimer = undefined;
    void search();
  }, SEARCH_DEBOUNCE_MS);
}

async function search() {
  if (searchTimer) clearTimeout(searchTimer);
  searchTimer = undefined;
  const request = ++searchRequest;
  searching.value = false;
  if (!root.value || !query.value.trim()) {
    searchResults.value = [];
    searchTruncated.value = false;
    return;
  }
  searching.value = true;
  try {
    const data = await backend.call<{ results?: typeof searchResults.value; error?: string; truncated?: boolean }>('code.search', {
      root: root.value,
      query: query.value,
      regex: regex.value,
    });
    if (request !== searchRequest) return;
    searchResults.value = data.results ?? [];
    // 后端命中上限时结果只是前 N 条，面板要如实说明，别当成完整结果
    searchTruncated.value = data.truncated ?? false;
    error.value = data.error ?? '';
  } catch (reason) {
    if (request !== searchRequest) return;
    error.value = `搜索失败: ${messageOf(reason)}`;
  } finally {
    if (request === searchRequest) searching.value = false;
  }
}

async function openResult(result: { file: string; line?: number }) {
  // 字面搜索时查询串就出现在命中行里：带上它，命中行内的匹配子串会被精确标出
  //（正则查询没有固定的字面量，只做整行高亮）。
  const needle = !regex.value ? query.value.trim() : '';
  await select({ name: result.file.split(/[\\/]/).pop() ?? result.file, path: result.file, isDir: false }, result.line, needle || undefined);
}

async function copyPath(path: string, label = '文件路径') {
  await copyText(path, label);
}

async function copyActiveContent() {
  const tab = activeTab.value;
  if (!tab) return;
  await copyText(tab.content, `${tab.name} 内容`);
}

// 小程序反编译页通过 hash 查询指定要审计的小程序（以及可选的具体文件）；
// 没有指定时默认打开最近一次反编译的产物。
async function applyIncomingTarget() {
  await loadProjects();
  const hash = window.location.hash;
  const separator = hash.indexOf('?');
  const query = separator >= 0 ? new URLSearchParams(hash.slice(separator + 1)) : null;
  const wanted = (query?.get('root') ?? '').trim();
  const file = (query?.get('file') ?? '').trim();
  const line = Number.parseInt(query?.get('line') ?? '', 10);
  // 匹配值用来在命中行内做子串级标记；超长截断（只影响高亮精度，不影响定位）。
  const value = (query?.get('value') ?? '').slice(0, 300);
  const requested = wanted ? projects.value.find((item) => sameDir(item.path, wanted)) : undefined;
  // 指定产物已不在（换了包、清过输出）时退到最近一次产物，而不是把页面留在空白状态。
  const project = requested ?? projects.value[0];
  if (!project) return;
  await selectProject(project.appid);
  if (wanted && !requested) {
    // 文件路径属于那次产物，落到别的产物上就不能再拿去跳转
    error.value = '未找到指定的反编译产物，已打开最近一次的产物。';
    return;
  }
  if (!file) return;
  const target = projectFilePath(project.path, file);
  if (!target) {
    error.value = '源码路径无效，已拒绝打开。';
    return;
  }
  await select({ name: file.split(/[\\/]/).pop() ?? file, path: target, isDir: false }, Number.isFinite(line) && line > 0 ? line : undefined, value || undefined);
}

onMounted(() => {
  void applyIncomingTarget();
});

onBeforeUnmount(() => {
  if (searchTimer) clearTimeout(searchTimer);
  searchTimer = undefined;
  searchRequest += 1;
  projectRequest += 1;
});
</script>

<template>
  <section class="code-browser-view page-workbench" aria-labelledby="code-title">
    <PageHeader title="代码浏览" title-id="code-title" />

    <p v-if="error" class="error" role="alert">{{ error }}</p>

    <div class="browser-layout">
      <ProjectTree
        :sidebar-title="sidebarTitle"
        :active="active"
        :query="query"
        :regex="regex"
        :search-results="searchResults"
        :searching="searching"
        :search-truncated="searchTruncated"
        :opening-project="openingProject"
        :projects-count="projects.length"
        :visible-nodes="visibleNodes"
        :result-path="resultPath"
        @update:query="query = $event"
        @update:regex="regex = $event"
        @search="scheduleSearch"
        @search-now="search"
        @clear-search="clearSearch"
        @open-result="openResult"
        @select="select"
      >
        <template #header-actions>
          <select v-if="projects.length" v-model="selectedApp" class="project-select" data-testid="code-project" aria-label="选择小程序" :disabled="loadingProjects || openingProject" @change="selectProject(selectedApp)">
            <option v-for="project in projects" :key="project.appid" :value="project.appid">{{ project.name || project.appid }} · {{ project.appid }}</option>
          </select>
          <button data-testid="refresh-projects" class="secondary small" type="button" :disabled="loadingProjects || openingProject" @click="reloadProjects">{{ loadingProjects || openingProject ? '加载中…' : '刷新列表' }}</button>
        </template>
      </ProjectTree>

      <main class="panel editor-panel">
        <CodeTabs
          ref="codeTabs"
          v-model:wrap="wrap"
          :tabs="tabs"
          :active="active"
          :active-tab="activeTab"
          :rows="renderedRows"
          :total-lines="totalLines"
          :display-path="activeDisplayPath"
          @select="activateTab"
          @close="close"
          @close-all="closeAll"
          @copy-path="copyPath"
          @copy-content="copyActiveContent"
          @load-before="loadBefore"
          @load-more="loadMore"
          @goto-hit="gotoHit"
          @clear-hit="clearHit"
        />
        <div v-if="!activeTab" class="empty-state editor-empty">
          <strong>尚未打开文件</strong>
          <span>在左侧目录中点击文件开始浏览代码。</span>
        </div>
      </main>
    </div>
  </section>
</template>

<style scoped>
/* 目录树、搜索结果、代码区各自带着自己的样式（ProjectTree / SearchResults /
   CodeTabs 里）：scoped 的重复规则匹配不到子组件内部的元素，写在这里只会变成
   没人执行的死样式，所以这里只负责本页自己的骨架与布局。 */

/* 现在挂在窄侧栏的面板头里：让它跟着可用宽度收缩，而不是撑出横向滚动。 */
.project-select {
  flex: 1 1 8rem;
  max-width: 100%;
  min-width: 0;
  width: auto;
}

.browser-layout {
  align-items: stretch;
  display: grid;
  gap: 1rem;
  grid-template-columns: minmax(14rem, 20rem) minmax(0, 1fr);
  height: 100%;
  min-height: 0;
}

.editor-panel {
  display: flex;
  flex-direction: column;
  min-height: 0;
  overflow: hidden;
}

.editor-empty {
  flex: 1;
}

@media (max-width: 900px) {
  .browser-layout {
    grid-template-columns: 1fr;
  }
}
</style>
