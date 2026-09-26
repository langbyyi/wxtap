<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue';
import { backend } from '../api/bridge';
import { useEngineStore } from '../stores/engine';
import { isNoMiniappError, messageOf } from '../utils/format';
import { copyText, notify } from '../utils/notify';
import PageHeader from '../components/PageHeader.vue';
import NavigatePanel from '../components/navigator/NavigatePanel.vue';
import GuardPanel from '../components/navigator/GuardPanel.vue';
import RouteListPanel from '../components/navigator/RouteListPanel.vue';

type PageResponse = { pages?: string[]; tab_bar_pages?: string[]; current_route?: string };
type GuardState = { enabled?: boolean; redirects?: Array<{ type?: string; url?: string; time?: string }> };

const store = useEngineStore();
const connected = computed(() => store.status.miniapp);
const pages = ref<string[]>([]);
const tabs = ref<string[]>([]);
const currentRoute = ref('');
const loading = ref(false);
const guardLoading = ref(false);
const error = ref('');
const guard = ref(false);
const redirects = ref<Array<{ type?: string; url?: string; time?: string }>>([]);
const autoVisiting = ref(false);
const autoProgress = ref(0);
const autoCurrent = ref('');
const autoFailed = ref(0);
const autoTotal = ref(0);
let pollTimer: ReturnType<typeof setInterval> | undefined;
let pollInFlight = false;
let stopNavProgress = () => {};
let disposed = false;

const POLL_MS = 2000;

function fail(value: unknown) {
  error.value = messageOf(value);
}

async function load() {
  if (!connected.value) return;
  loading.value = true;
  error.value = '';
  try {
    const result = await backend.call<PageResponse>('navigator.pages');
    pages.value = result.pages ?? [];
    tabs.value = result.tab_bar_pages ?? [];
    currentRoute.value = result.current_route ?? '';
    // 防跳转的真实状态只有页面知道：首屏必须回读一次，否则重新进入本页会把
    // 仍然生效的拦截显示成「未开启」。遍历同理：它跑在 shell 里，离开页面再
    // 回来时不能默认已经停了。
    await Promise.all([refreshCurrentRoute(), loadGuard(), loadAutoVisitState()]);
  } catch (value) {
    fail(value);
    // 引擎在跑但小程序已断开时，顶栏状态是旧的：顺手校准一次。判定用的是**原始失败**
    // 而不是上面那行已经翻成中文的文案 —— 显示文案会随译文变，判据不能跟着变。
    if (isNoMiniappError(value)) await store.refreshStatus();
  } finally {
    loading.value = false;
  }
}

// 配置里的 current_route 只在「获取路由」那一步回读一次，页面自己跳转（含自动
// 重定向）之后就得靠运行时回读。轮询只问当前路由，不重取整份页面配置。
async function refreshCurrentRoute(quiet = false) {
  if (!connected.value) return;
  try {
    const result = await backend.call<{ route?: string }>('navigator.getCurrentRoute');
    if (disposed) return;
    currentRoute.value = result?.route || '';
  } catch (value) {
    if (!quiet) fail(value);
  }
}

// 防跳转只存在于小程序页面里，后端能回读的才是真的：页面重新加载后拦截已
// 失效，前端若还显示「已拦截」就是假状态。
async function loadGuard(quiet = false) {
  if (!connected.value) return;
  if (!quiet) guardLoading.value = true;
  try {
    const result = await backend.call<GuardState>('navigator.guardState');
    if (disposed) return;
    const enabled = result?.enabled === true;
    if (guard.value && !enabled) notify('页面已重新加载，防跳转拦截已失效', 'info');
    guard.value = enabled;
    redirects.value = result?.redirects ?? [];
  } catch (value) {
    if (!quiet) fail(value);
  } finally {
    if (!quiet) guardLoading.value = false;
  }
}

async function navigate(route: string, method: 'navigateTo' | 'reLaunch') {
  if (!route || !connected.value) return;
  error.value = '';
  try {
    await backend.call('navigator.navigate', { route, method });
    currentRoute.value = route;
    notify(method === 'reLaunch' ? `已重启到 ${route}` : `已跳转到 ${route}`, 'success');
    await refreshCurrentRoute(true);
  } catch (value) {
    fail(value);
  }
}

async function changeGuard(enabled: boolean) {
  if (!connected.value) return;
  error.value = '';
  try {
    await backend.call(enabled ? 'navigator.enableRedirectGuard' : 'navigator.disableRedirectGuard');
    await loadGuard();
  } catch (value) {
    // 勾选框由 model 驱动，改回原值即可让 DOM 真实回弹（勾选/取消是二值，
    // 原值就是它的反面）。
    guard.value = !enabled;
    fail(value);
  }
}

async function copyRoute(route: string) {
  await copyText(route, '路由');
}

// 遍历状态跑在 shell 里：重进页面要回读，否则界面显示「未在遍历」而 shell 正
// 在走，点「开始遍历」会被静默忽略（后端返回 started=false）。
async function loadAutoVisitState() {
  // 不按 connected 提前返回：遍历跑在 shell 里，小程序断连后它可能还在走，
  // 界面必须如实显示（否则会停在「停止访问」或谎报正常）。
  try {
    const result = await backend.call<{ visiting?: boolean }>('navigator.autoVisitState');
    autoVisiting.value = result?.visiting === true;
  } catch {
    // 回读失败不改变界面：下一次轮询/操作会纠正。
  }
}

async function startAutoVisit() {
  if (!connected.value) return;
  error.value = '';
  autoFailed.value = 0;
  autoCurrent.value = '';
  autoProgress.value = 0;
  autoTotal.value = pages.value.length;
  // 立即进入遍历态：等 RPC 返回再亮按钮会让点击看起来没反应。
  autoVisiting.value = true;
  try {
    // 后端在页面列表为空时会直接报错，而不是静默走完一轮 0 页。
    const result = await backend.call<{ started?: boolean }>('navigator.autoVisit');
    notify(result?.started === false ? '后端已在遍历中，进度会继续显示' : '已开始遍历页面');
  } catch (value) {
    fail(value);
    autoVisiting.value = false;
  }
}

async function stopAutoVisit() {
  try {
    await backend.call('navigator.stopAutoVisit');
    notify('已停止页面遍历');
  } finally {
    autoVisiting.value = false;
    await loadAutoVisitState();
  }
}

async function poll() {
  if (disposed || pollInFlight || !connected.value) return;
  pollInFlight = true;
  try {
    await refreshCurrentRoute(true);
    if (guard.value) await loadGuard(true);
  } finally {
    pollInFlight = false;
  }
}

watch(connected, (running) => {
  if (running) {
    void load();
  } else {
    pages.value = [];
    tabs.value = [];
    currentRoute.value = '';
    guard.value = false;
    redirects.value = [];
    void loadAutoVisitState();
  }
});

onMounted(async () => {
  stopNavProgress = backend.on<{ progress?: number; current?: string; done?: boolean; failed?: number; total?: number }>('navigate_progress', (event) => {
    autoProgress.value = event.progress ?? 0;
    autoCurrent.value = event.current ?? '';
    autoFailed.value = event.failed ?? 0;
    autoTotal.value = event.total ?? autoTotal.value;
    if (event.done) {
      autoVisiting.value = false;
      const failed = event.failed ?? 0;
      notify(failed ? `页面遍历完成，${failed} 页失败` : '页面遍历完成', failed ? 'info' : 'success');
    }
  });
  await load();
  if (disposed) return;
  pollTimer = setInterval(() => void poll(), POLL_MS);
});

onBeforeUnmount(() => {
  disposed = true;
  if (pollTimer) clearInterval(pollTimer);
  stopNavProgress();
});
</script>

<template>
  <section class="navigator-view page-workbench" aria-labelledby="navigator-title">
    <PageHeader title="页面路由" title-id="navigator-title" />

    <p v-if="error" class="error" role="alert">{{ error }}</p>

    <div class="navigator-grid">
      <div class="stack navigator-main">
        <RouteListPanel
          :pages="pages"
          :tab-bar-pages="tabs"
          :current-route="currentRoute"
          :connected="connected"
          :loading="loading"
          @navigate="navigate"
          @copy="copyRoute"
          @refresh="load"
        />
      </div>

      <div class="stack navigator-side">
        <NavigatePanel
          :connected="connected"
          :loading="loading"
          :auto-visiting="autoVisiting"
          :auto-progress="autoProgress"
          :auto-current="autoCurrent"
          :auto-failed="autoFailed"
          :auto-total="autoTotal"
          @start-auto-visit="startAutoVisit"
          @stop-auto-visit="stopAutoVisit"
        />
        <GuardPanel
          v-model="guard"
          :connected="connected"
          :redirects="redirects"
          :loading="guardLoading"
          @change="changeGuard"
          @refresh="loadGuard()"
        />
      </div>
    </div>
  </section>
</template>

<style scoped>
/* 本页是 page-workbench：满高 flex 列，主栏的面板吃满剩余高度，列表内部滚动、
   动作条钉在面板底部 —— 不再需要把整页滑到底才够得着「跳转到选中」。 */
.navigator-grid {
  display: grid;
  flex: 1 1 auto;
  gap: 1rem;
  grid-template-columns: minmax(0, 1.2fr) minmax(18rem, .8fr);
  min-height: 0;
}

.navigator-main,
.navigator-side {
  min-height: 0;
  min-width: 0;
}

/* 侧栏两块面板保持自然高度：主栏被撑满时它们不跟着拉长。它们加起来比一屏高
   （拦截记录最多 200 条）时，本页已不再整页滚动，所以侧栏必须自己滚——
   否则底部会被 page-workbench 的 overflow:hidden 裁掉且滚不到。 */
.navigator-side {
  align-content: start;
  overflow-y: auto;
}

@media (max-width: 900px) {
  /* 单列后两栏叠起来放不进一屏，改成整块滚动；列表自身限高，动作条仍在它下面 */
  .navigator-grid {
    grid-template-columns: 1fr;
    overflow-y: auto;
  }
}
</style>
