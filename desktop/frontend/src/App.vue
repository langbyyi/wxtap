<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue';
import { useRoute, useRouter } from 'vue-router';
import { backend } from './api/bridge';
import ActionToast from './components/ActionToast.vue';
import { useEngineStore } from './stores/engine';
import { messageOf } from './utils/format';
import { notify } from './utils/notify';
import mark from './assets/wxtap-mark.svg?raw';
import { navItemMatches } from './nav-match';

type MiniApp = { name?: string; appid?: string };
type NavItem = { path: string; label: string; hint: string };
type PaletteItem = NavItem & { groupLabel: string };
type NavGroup = { key: string; label: string; items: NavItem[] };

const groups: NavGroup[] = [
  {
    key: 'session',
    label: '连接',
    items: [
      { path: '/control', label: '状态', hint: '配置端口，启动或停止引擎，并查看微信与 Frida 的连接状态。' },
    ],
  },
  {
    key: 'runtime',
    label: '调试',
    items: [
      { path: '/navigator', label: '页面路由', hint: '回读小程序当前路由，跳转至指定页面，或按配置列表自动遍历全部页面。' },
      { path: '/console', label: 'Console 日志', hint: '小程序页面里的 console 输出与未捕获错误，可过滤、复制和清空。' },
      { path: '/devtools', label: 'DevTools', hint: '复制 DevTools 地址，或使用已配置的 Electron 打开。多个小程序时先选择目标。' },
      { path: '/vconsole', label: 'vConsole', hint: '开关微信小程序自带的调试面板。面板长在微信窗口里，本程序读不到它的内容 —— 要看日志请去 Console 日志页。' },
      { path: '/hook', label: '注入脚本', hint: '把 hook_scripts 目录里的 .js 注进小程序当前页面，可反复重注；勾「全局」则每次小程序重新加载都自动注入。脚本输出带 [文件名] 前缀进 Console 日志页。' },
    ],
  },
  {
    key: 'calls',
    label: '流量',
    items: [
      { path: '/wxapi', label: 'WxAPI', hint: '记录 wx.* 的参数和返回值，并按原参数重放。' },
      { path: '/cloud', label: '云函数', hint: '扫描、捕获、重放和导出云函数调用，也可在本机转发。' },
      { path: '/traffic', label: '历史记录', hint: '按页查看已保存的调用，可勾选删除；选中记录后才读取正文。' },
    ],
  },
  {
    key: 'source',
    label: '代码',
    items: [
      { path: '/extract', label: '反编译', hint: '从微信小程序包还原源码，并标出敏感信息。' },
      { path: '/code', label: '代码浏览', hint: '打开反编译结果，按目录和搜索查看源码。' },
      { path: '/assets', label: '资产清单', hint: '从反编译目录与抓包流量汇总 API、静态资源、WebSocket 与云函数资产，可筛选、分页与导出。' },
    ],
  },
  {
    key: 'keys',
    label: '利用',
    items: [
      { path: '/sessionkey', label: 'SessionKey', hint: '从报文或源码取出 session_key，对开放数据做 AES 加解密。' },
      { path: '/ak', label: '微信 AK', hint: '填写微信 AppID 和 AppSecret，向官方接口验证凭据是否有效。' },
    ],
  },
  {
    key: 'local',
    label: '系统',
    items: [
      { path: '/settings', label: '设置', hint: '查看运行目录，配置外部程序路径，并检查版本。' },
      { path: '/mcp', label: 'MCP 服务', hint: '启动服务后，外部智能体可以调试当前连接的小程序。' },
    ],
  },
  {
    key: 'help',
    label: '帮助',
    items: [
      { path: '/faq', label: '使用帮助', hint: '快速上手流程、各功能页的使用方法，以及报错原文对应的处理办法。' },
      { path: '/feedback', label: '交流反馈', hint: '交流群、问题反馈和更新说明。' },
    ],
  },
];

const store = useEngineStore();
const githubUrl = 'https://github.com/langbyyi/wxtap';
const appVersion = __WXTAP_VERSION__;
// 顶栏的更新提示。启动时静默检测一次，失败即静默——首次请求在国内网络下超时
// 是常态，不该在启动时弹窗打扰。已下载待重启比「可更新」更强，优先展示。
const updateNotice = ref('');
type UpdateCheck = { latest?: string; has_update?: boolean; error?: string };
type UpdateStatus = { staged?: boolean; version?: string };
async function checkForUpdate() {
  try {
    const status = await backend.call<UpdateStatus>('update.status');
    if (status.staged) {
      updateNotice.value = `重启生效 ${status.version ?? ''}`.trim();
      return;
    }
    const result = await backend.call<UpdateCheck>('update.checkVersion');
    if (result.has_update && result.latest) updateNotice.value = `可更新 ${result.latest}`;
  } catch {
    // 静默：更新检测失败不影响任何其他功能。
  }
}
const route = useRoute();
const router = useRouter();
const miniApp = ref<MiniApp>({});
const paletteOpen = ref(false);
const paletteQuery = ref('');
const paletteInput = ref<HTMLInputElement | null>(null);
const paletteTrigger = ref<HTMLButtonElement | null>(null);
const paletteIndex = ref(0);
const paletteItems: PaletteItem[] = groups.flatMap((group) => group.items.map((item) => ({ ...item, groupLabel: group.label })));
const paletteResults = computed(() => {
  const needle = paletteQuery.value.trim().toLowerCase();
  return paletteItems
    .filter((item) => !needle
      || item.label.toLowerCase().includes(needle)
      || item.path.toLowerCase().includes(needle)
      || item.groupLabel.toLowerCase().includes(needle))
    .slice(0, 8);
});
function togglePalette() {
  if (paletteOpen.value) {
    closePalette();
    return;
  }
  paletteOpen.value = true;
  paletteQuery.value = '';
  paletteIndex.value = 0;
}
function closePalette() {
  if (!paletteOpen.value) return;
  paletteOpen.value = false;
  void nextTick(() => paletteTrigger.value?.focus());
}
async function setPaletteIndex(index: number) {
  if (!paletteResults.value.length) return;
  paletteIndex.value = (index + paletteResults.value.length) % paletteResults.value.length;
  await nextTick();
  document.querySelector<HTMLElement>(`[data-palette-index="${paletteIndex.value}"]`)?.scrollIntoView?.({ block: 'nearest' });
}
function choosePalette(path: string) {
  paletteOpen.value = false;
  void router.push(path);
}
function paletteKeydown(event: KeyboardEvent) {
  if (event.key === 'Escape') { closePalette(); return; }
  if (event.key === 'ArrowDown') { event.preventDefault(); void setPaletteIndex(paletteIndex.value + 1); return; }
  if (event.key === 'ArrowUp') { event.preventDefault(); void setPaletteIndex(paletteIndex.value - 1); return; }
  if (event.key === 'Enter') {
    const item = paletteResults.value[paletteIndex.value] ?? paletteResults.value[0];
    if (item) choosePalette(item.path);
  }
}
watch(paletteQuery, () => { paletteIndex.value = 0; });
const theme = ref(localStorage.getItem('theme') === 'light' ? 'light' : 'dark');

const activeGroup = computed(() => {
  const path = route.path;
  return groups.find((group) => group.items.some((item) => navItemMatches(path, item.path))) ?? groups[0];
});

const navIntro = ref<{ label: string; hint: string } | null>(null);
const navIntroStyle = ref({ left: '0px', top: '0px' });
const navIntroElement = ref<HTMLElement | null>(null);
let navIntroToken = 0;

async function showNavIntro(label: string, hint: string, event: MouseEvent | FocusEvent) {
  const token = ++navIntroToken;
  navIntro.value = { label, hint };
  await nextTick();
  if (token !== navIntroToken) return;
  const anchor = event.currentTarget as HTMLElement | null;
  const tip = navIntroElement.value;
  if (!anchor || !tip) return;
  const rect = anchor.getBoundingClientRect();
  const box = tip.getBoundingClientRect();
  const left = Math.min(Math.max(12, rect.left), window.innerWidth - box.width - 12);
  const below = rect.bottom + 8;
  const top = below + box.height > window.innerHeight - 12
    ? Math.max(12, rect.top - box.height - 8)
    : below;
  navIntroStyle.value = { left: `${Math.round(left)}px`, top: `${Math.round(top)}px` };
}

function hideNavIntro() {
  navIntroToken += 1;
  navIntro.value = null;
}

function openGithub() {
  backend.call('shell.openUrl', { url: githubUrl }).catch((reason) => {
    notify(`打开 GitHub 失败：${messageOf(reason)}`, 'error');
  });
}

function applyTheme() {
  document.documentElement.dataset.theme = theme.value;
}

function toggleTheme() {
  theme.value = theme.value === 'dark' ? 'light' : 'dark';
  localStorage.setItem('theme', theme.value);
  applyTheme();
}

const stopAppInfo = backend.on<MiniApp>('app_info', (next) => { miniApp.value = next; });

function isTypingTarget(target: EventTarget | null) {
  const element = target as HTMLElement | null;
  if (!element) return false;
  const tag = element.tagName;
  return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || element.isContentEditable;
}

// '/' 聚焦当前页面的搜索框，跳过长列表里的鼠标操作
function focusPageSearch() {
  const input = document.querySelector<HTMLInputElement>('input[type="search"], input[placeholder*="搜索"], input[placeholder*="筛选"]');
  if (!input || input.disabled) return false;
  input.focus();
  input.select?.();
  return true;
}

function onGlobalKeydown(event: KeyboardEvent) {
  if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 'k') {
    event.preventDefault();
    togglePalette();
    return;
  }
  if (event.key === '/' && !event.ctrlKey && !event.metaKey && !event.altKey && !isTypingTarget(event.target)) {
    if (focusPageSearch()) event.preventDefault();
  }
}
watch(() => route.path, () => { closePalette(); hideNavIntro(); });
watch(paletteOpen, async (open) => {
  if (!open) return;
  await nextTick();
  paletteInput.value?.focus();
});
onMounted(() => {
  applyTheme();
  store.listen();
  // 启动即拉一次连接状态。轮询只在身份变化时推 app_info，
  // 页面重载后要靠这次 engine.status 里的 appInfo 把顶栏补上。
  void store.load().then(() => {
    const info = store.status.appInfo;
    if (!info || miniApp.value.name || miniApp.value.appid) return;
    miniApp.value = { name: info.name, appid: info.appid };
  });
  void checkForUpdate();
  window.addEventListener('keydown', onGlobalKeydown);
  window.addEventListener('resize', hideNavIntro);
  window.addEventListener('scroll', hideNavIntro, true);
});
onBeforeUnmount(() => {
  stopAppInfo();
  hideNavIntro();
  window.removeEventListener('keydown', onGlobalKeydown);
  window.removeEventListener('resize', hideNavIntro);
  window.removeEventListener('scroll', hideNavIntro, true);
});
</script>

<template>
  <div class="app-shell">
    <header class="app-topbar">
      <div class="app-brand">
        <span class="app-brand-mark" aria-hidden="true" v-html="mark" />
        <div>
          <div class="app-brand-name">WxTap</div>
          <div class="app-brand-tagline">小程序调试</div>
        </div>
      </div>
      <nav class="app-groups" aria-label="功能分组">
        <RouterLink
          v-for="group in groups"
          :key="group.key"
          :to="group.items[0].path"
          class="app-group"
          :data-testid="`group-${group.key}`"
          :class="{ 'is-active': activeGroup.key === group.key }"
        >{{ group.label }}</RouterLink>
      </nav>
      <div class="topbar-right">
        <!-- 核心能探到 __wxConfig 昵称时展示昵称，否则回退到 appid -->
        <div v-if="miniApp.name || miniApp.appid" class="topbar-miniapp" data-testid="current-miniapp">
          <strong>{{ miniApp.name || miniApp.appid }}</strong>
        </div>
        <span class="topbar-version" data-testid="app-version">{{ appVersion }}</span>
        <RouterLink v-if="updateNotice" to="/settings" class="status-pill warn topbar-update" data-testid="update-badge" :title="updateNotice">
          {{ updateNotice }}
        </RouterLink>
        <button class="topbar-github" type="button" data-testid="github-link" aria-label="GitHub" title="github.com/langbyyi/wxtap" @click="openGithub">
          <svg viewBox="0 0 16 16" aria-hidden="true"><path fill="currentColor" d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82A7.7 7.7 0 0 1 8 3.87c.68.003 1.36.092 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0 0 16 8c0-4.42-3.58-8-8-8z"/></svg>
        </button>
        <button
          ref="paletteTrigger"
          class="ghost small palette-trigger"
          type="button"
          title="快速跳转 (Ctrl+K 或 ⌘K)"
          aria-controls="command-palette"
          :aria-expanded="paletteOpen"
          @click="togglePalette"
        >跳转 <kbd>Ctrl/⌘ K</kbd></button>
        <button data-testid="theme-toggle" class="secondary small" type="button" @click="toggleTheme">
          {{ theme === 'dark' ? '浅色' : '深色' }}
        </button>
      </div>
    </header>
    <nav class="app-subnav" :aria-label="activeGroup.label">
      <RouterLink
        v-for="item in activeGroup.items"
        :key="item.path"
        :to="item.path"
        :data-testid="`nav-${item.path.slice(1)}`"
        :aria-describedby="navIntro?.label === item.label ? 'nav-intro' : undefined"
        active-class="is-active"
        @mouseenter="showNavIntro(item.label, item.hint, $event)"
        @focus="showNavIntro(item.label, item.hint, $event)"
        @mouseleave="hideNavIntro"
        @blur="hideNavIntro"
      >{{ item.label }}</RouterLink>
    </nav>
    <div
      v-if="navIntro"
      id="nav-intro"
      ref="navIntroElement"
      class="nav-intro"
      data-testid="nav-intro"
      role="tooltip"
      :style="navIntroStyle"
    >
      <strong>{{ navIntro.label }}</strong>
      <p>{{ navIntro.hint }}</p>
    </div>
    <div v-if="paletteOpen" class="palette-backdrop" @click.self="closePalette">
      <div id="command-palette" class="palette-card" role="dialog" aria-modal="true" aria-label="快速跳转">
        <input
          ref="paletteInput"
          v-model="paletteQuery"
          class="palette-input"
          placeholder="输入页面名称或路径，按 Enter 跳转…"
          aria-label="搜索页面"
          @keydown="paletteKeydown"
        >
        <ul class="palette-list">
          <li v-for="(item, index) in paletteResults" :key="item.path">
            <RouterLink
              :to="item.path"
              :data-palette-index="index"
              :class="{ 'is-active': index === paletteIndex }"
              @mousemove="paletteIndex = index"
              @click="closePalette"
            >
              <span class="palette-item-label">
                <strong>{{ item.label }}</strong>
                <small>{{ item.groupLabel }}</small>
              </span>
              <code>{{ item.path }}</code>
            </RouterLink>
          </li>
          <li v-if="!paletteResults.length" class="palette-empty">没有匹配的页面</li>
        </ul>
        <p class="palette-hint">↑↓ 选择 · Enter 打开 · Esc 关闭 · 在页面上按 <kbd>/</kbd> 直接定位搜索框</p>
      </div>
    </div>

    <main class="app-content app-main">
      <RouterView v-slot="{ Component }">
        <KeepAlive include="ExtractView">
          <component :is="Component" />
        </KeepAlive>
      </RouterView>
    </main>
    <ActionToast />
  </div>
</template>
