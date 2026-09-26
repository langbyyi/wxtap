<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue';
import { backend } from '../api/bridge';
import { useCredentialStore } from '../stores/credentials';
import { messageOf, prettyJson } from '../utils/format';
import { copyText, notify } from '../utils/notify';
import PageHeader from '../components/PageHeader.vue';

type AKMode = 'oa' | 'mini' | 'work';
type AKResult = {
  mode: AKMode;
  valid: boolean;
  endpoint: string;
  http_status?: number;
  errcode?: number;
  errmsg?: string;
  expires_in?: number;
  token?: string;
  token_fingerprint?: string;
  body?: unknown;
};

type Param = { id: string; label: string; kind: string; required?: boolean; hint?: string; default?: string; shared?: boolean };
type Endpoint = {
  id: string; group: string; label: string; category: string; summary: string; method: string; path: string;
  json_body: boolean; returns: string; note?: string; params?: Param[];
};
type Group = { id: string; label: string };
type CallResult = {
  endpoint: string; label: string; method: string; url: string; http_status: number;
  errcode: number; errmsg: string; duration_ms: number; body?: unknown; image_data_url?: string;
};

// 最近一次执行的报文快照，请求/响应两栏显示的就是它——两栏都只放报文：成不成、
// 耗时多少由响应头与响应体自己说，不再另起一行结论。这一页不留历史列表、不落盘。
type Exchange = {
  kind: 'verify' | 'call';
  requestHead: string;
  requestBody: string;
  responseHead: string;
  body?: unknown;
  image?: string;
};

const modes: Array<{ value: AKMode; label: string; access: string; secret: string }> = [
  { value: 'mini', label: '微信小程序', access: 'AppID', secret: 'AppSecret' },
  { value: 'oa', label: '微信公众号', access: 'AppID', secret: 'AppSecret' },
  { value: 'work', label: '企业微信', access: 'CorpID', secret: 'CorpSecret' },
];

const credentials = useCredentialStore();
const fakeIP = ref('');
const busy = ref(false);
const result = ref<AKResult | null>(null);
const error = ref('');
// 请求/响应两栏显示的那一次执行。
const current = ref<Exchange | null>(null);

const modeConfig = computed(() => modes.find((item) => item.value === credentials.mode) ?? modes[0]);

async function verify() {
  if (busy.value) return;
  if (!credentials.accessKey.trim() || !credentials.secretKey.trim()) {
    error.value = `${modeConfig.value.access} 和 ${modeConfig.value.secret} 不能为空`;
    return;
  }
  busy.value = true;
  error.value = '';
  // 上一次的结果在发起新一次验证时即作废（与「执行」那边的 callResult 同一做法）：
  // 失败时留在屏上的旧 token 与「有效」会被读成这一次的结果——
  // 用户会把旧凭据的 token 复制走。
  result.value = null;
  current.value = null;
  try {
    // 一直取完整 token：它是这条链的中间产出，用户要能看到并复制它。
    result.value = await backend.call<AKResult>('ak.verify', {
      mode: credentials.mode,
      access_key: credentials.accessKey.trim(),
      secret_key: credentials.secretKey.trim(),
      fake_ip: fakeIP.value.trim(),
      include_token: true,
    });
    const verifyURL = hostAndTarget((result.value as AKResult & { request_url?: string }).request_url ?? result.value.endpoint);
    current.value = {
      kind: 'verify',
      requestHead: headLines(`GET ${verifyURL.target} HTTP/1.1`, verifyURL.host, undefined),
      requestBody: '',
      responseHead: headLines(`HTTP/1.1 ${result.value.http_status ?? '-'}`, '', undefined),
      body: result.value,
    };
    notify(result.value.valid ? '凭据有效，token 已取得' : '凭据无效或已失效', result.value.valid ? 'success' : 'info');
  } catch (reason) {
    error.value = messageOf(reason);
  } finally {
    busy.value = false;
  }
}

async function copyToken() {
  const token = result.value?.token;
  if (!token) {
    notify('尚未获取 token，请先点击「获取」', 'info');
    return;
  }
  await copyText(token, 'access_token');
}

// 请求段与响应段的拼法：起始行只放 path?query，host 进头——与真实报文一致，
// 里面的凭据与 token 按原样显示，不做掩码。
function hostAndTarget(url: string): { host: string; target: string } {
  try {
    const parsed = new URL(url);
    return { host: parsed.host, target: parsed.pathname + parsed.search };
  } catch {
    return { host: '', target: url };
  }
}

function headLines(startLine: string, host: string, headers: Record<string, string> | undefined): string {
  const lines = [startLine];
  if (host) lines.push(`Host: ${host}`);
  for (const [name, value] of Object.entries(headers ?? {}).sort(([a], [b]) => a.localeCompare(b))) {
    lines.push(`${name}: ${value}`);
  }
  return lines.join('\n');
}

// 两栏各管各的复制：请求栏给报文，响应栏给结果体。
async function copyRequest() {
  const entry = current.value;
  if (!entry) return;
  await copyText([entry.requestHead, entry.requestBody].filter(Boolean).join('\n\n'), '请求报文');
}

async function copyResponse() {
  const entry = current.value;
  if (!entry) return;
  if (entry.image) {
    notify('图片响应无法以文本复制', 'info');
    return;
  }
  await copyText(prettyJson(entry.body ?? {}), '响应结果');
}

function clearResponse() {
  current.value = null;
  result.value = null;
  callResult.value = null;
  error.value = '';
}

// --- 官方接口 ---
const groups = ref<Group[]>([]);
const endpoints = ref<Endpoint[]>([]);
const selectedId = ref('');
// 自己画的下拉是否展开。选中、换个类型、按 Esc 都收起来。
const pickerOpen = ref(false);
const pickerMenu = ref<HTMLElement | null>(null);
const values = ref<Record<string, string>>({});
const callResult = ref<CallResult | null>(null);
const callBusy = ref(false);
const loading = ref(false);

// 动态参数：一个族里多个接口共用的入参（openid、翻页游标、日期区间这类）。填一次，
// 选中哪个接口就带上哪个——而不是每个接口各问一遍同样的值。共享标记由后端给。
const sharedValues = ref<Record<string, string>>({});
const sharedParams = computed(() => {
  const seen = new Map<string, Param>();
  for (const item of visible.value) {
    for (const param of item.params ?? []) {
      if (param.shared && !seen.has(param.id)) seen.set(param.id, param);
    }
  }
  return [...seen.values()];
});

function ownParams(endpoint: Endpoint | undefined): Param[] {
  return (endpoint?.params ?? []).filter((param) => !param.shared);
}

function sharedOf(endpoint: Endpoint | undefined): Param[] {
  return (endpoint?.params ?? []).filter((param) => param.shared);
}

// 第 1 步选定的凭据类型就是范围：一个类型只够得着自己的接口，所以这里不再提供
// "全部/按类型筛选"的第二遍选择，也就没有"类型不匹配"这种状态。
const visible = computed(() => endpoints.value.filter((item) => item.group === credentials.mode));
// 分组由后端给（category），前端不自己编类目——新增接口没分类会在 Go 测试里红。
const categoryGroups = computed(() => {
  const order: string[] = [];
  const byCategory = new Map<string, Endpoint[]>();
  for (const item of visible.value) {
    if (!byCategory.has(item.category)) {
      byCategory.set(item.category, []);
      order.push(item.category);
    }
    byCategory.get(item.category)!.push(item);
  }
  return order.map((category) => ({ category, items: byCategory.get(category) ?? [] }));
});
const familyLabel = computed(() => groups.value.find((item) => item.id === credentials.mode)?.label ?? modeConfig.value.label);
const selected = computed(() => endpoints.value.find((item) => item.id === selectedId.value) ?? null);
const missing = computed(() => {
  const endpoint = selected.value;
  if (!endpoint) return [];
  return (endpoint.params ?? [])
    .filter((param) => {
      const source = param.shared ? sharedValues.value : values.value;
      return !(source[param.id] ?? param.default ?? '').trim();
    })
    .filter((param) => param.required)
    .map((param) => param.label);
});
const canRun = computed(() => !!selected.value && !callBusy.value && credentials.complete && missing.value.length === 0);
// 详情块只列这个接口自己的入参：共享的那些已经在上面的「本族接口共用」里填过，
// 在这里再渲染一份不仅占地方，填进去也不会被 run() 采信。
const selectedParams = computed(() => ownParams(selected.value ?? undefined));

// 文档入口：按当前族的官方文档根 + 官方调试工具。这两个地址就是接口表里引用的
// 来源根，放在凭据下面，用户核对路径时不用自己找。
const docLinks = computed(() => {
  const family = {
    mini: { label: '小程序 API 文档', url: 'https://developers.weixin.qq.com/miniprogram/dev/OpenApiDoc/' },
    oa: { label: '公众号 API 文档', url: 'https://developers.weixin.qq.com/doc/offiaccount/' },
    work: { label: '企业微信 API 文档', url: 'https://developer.work.weixin.qq.com/document/path/' },
  }[credentials.mode];
  return [family, { label: '官方调试工具', url: 'https://mp.weixin.qq.com/debug/' }];
});

async function openLink(url: string) {
  try {
    await backend.call('shell.openUrl', { url });
  } catch (reason) {
    error.value = messageOf(reason);
  }
}

function defaultsFor(endpoint: Endpoint | undefined): Record<string, string> {
  const next: Record<string, string> = {};
  for (const param of ownParams(endpoint)) {
    if (param.default) next[param.id] = param.default;
  }
  return next;
}

function select(endpoint: Endpoint) {
  selectedId.value = endpoint.id;
  values.value = defaultsFor(endpoint);
  callResult.value = null;
  error.value = '';
}

// 下拉里选中的是 id：先找到接口再走同一条 select 路径。
function selectById(id: string) {
  const endpoint = endpoints.value.find((item) => item.id === id);
  if (endpoint) select(endpoint);
}

function pickEndpoint(id: string) {
  selectById(id);
  pickerOpen.value = false;
}

// 弹层在文档流里，展开时若被挤到可视区外就把它带进视野——否则点了没反应。
async function togglePicker() {
  pickerOpen.value = !pickerOpen.value;
  if (!pickerOpen.value) return;
  await nextTick();
  // jsdom 里没有 scrollIntoView，所以是可选调用（与 App.vue / CloudView 同）。
  pickerMenu.value?.scrollIntoView?.({ block: 'nearest' });
}

function onKeydown(event: KeyboardEvent) {
  if (event.key === 'Escape') pickerOpen.value = false;
}

async function load() {
  if (loading.value) return;
  loading.value = true;
  error.value = '';
  try {
    const payload = await backend.call<{ groups?: Group[]; endpoints?: Endpoint[] }>('wxopen.endpoints');
    groups.value = payload.groups ?? [];
    endpoints.value = payload.endpoints ?? [];
    if (!selectedId.value || !endpoints.value.some((item) => item.id === selectedId.value && item.group === credentials.mode)) {
      if (visible.value.length) select(visible.value[0]);
    }
  } catch (reason) {
    error.value = messageOf(reason);
  } finally {
    loading.value = false;
  }
}

async function run() {
  const endpoint = selected.value;
  if (!endpoint || callBusy.value) return;
  error.value = '';
  callResult.value = null;
  callBusy.value = true;
  try {
    const params: Record<string, string> = {};
    for (const param of ownParams(endpoint)) {
      const value = (values.value[param.id] ?? '').trim();
      if (value) params[param.id] = value;
    }
    // 动态参数按这个接口声明的那几个合并进来，未填的交给后端按必填校验。
    for (const param of sharedOf(endpoint)) {
      const value = (sharedValues.value[param.id] ?? param.default ?? '').trim();
      if (value) params[param.id] = value;
    }
    callResult.value = await backend.call<CallResult>('wxopen.call', {
      mode: credentials.mode,
      access_key: credentials.accessKey.trim(),
      secret_key: credentials.secretKey.trim(),
      endpoint: endpoint.id,
      params,
    });
    const call = callResult.value;
    const callURL = hostAndTarget(call.url);
    current.value = {
      kind: 'call',
      requestHead: headLines(`${call.method} ${callURL.target} HTTP/1.1`, callURL.host, (call as { request_headers?: Record<string, string> }).request_headers),
      requestBody: (call as { request_body?: string }).request_body ?? '',
      responseHead: headLines(`HTTP/1.1 ${call.http_status}`, '', (call as { response_headers?: Record<string, string> }).response_headers),
      body: call.body,
      image: call.image_data_url,
    };
    const failed = callResult.value.errcode !== 0;
    notify(failed ? `接口返回 errcode ${callResult.value.errcode}` : '接口调用成功', failed ? 'info' : 'success');
  } catch (reason) {
    error.value = messageOf(reason);
  } finally {
    callBusy.value = false;
  }
}

function clearAll() {
  credentials.clear();
  credentials.mode = 'mini';
  fakeIP.value = '';
  result.value = null;
  current.value = null;
  values.value = defaultsFor(selected.value ?? undefined);
  sharedValues.value = {};
  callResult.value = null;
  error.value = '';
}

// 换类型等于换范围：旧的选中项不属于新类型，必须重新选。
watch(() => credentials.mode, () => {
  pickerOpen.value = false;
  const next = visible.value[0];
  if (next) select(next);
  else {
    selectedId.value = '';
    values.value = {};
    callResult.value = null;
  }
});

onMounted(() => {
  load();
  window.addEventListener('keydown', onKeydown);
});
onBeforeUnmount(() => window.removeEventListener('keydown', onKeydown));
</script>

<template>
  <section class="ak-view page-workbench" aria-labelledby="ak-title">
    <PageHeader title="微信 AK" title-id="ak-title" />

    <p v-if="error" class="error error-line" role="alert" data-testid="ak-error">{{ error }}</p>

    <div class="ak-layout">
      <div class="controls">
        <!-- 类型与清空钉在左栏顶部：它们是整页的作用域，不该跟着表单滚走。
             类型只在这里选一次——它是凭据自身的一部分，也决定下面能看到哪些接口。 -->
        <div class="controls-bar">
          <div class="chip-row" role="tablist" aria-label="凭据类型">
            <button
              v-for="item in modes" :key="item.value" type="button" role="tab" class="chip"
              :class="{ active: credentials.mode === item.value }" :aria-selected="credentials.mode === item.value"
              :data-testid="`ak-mode-${item.value}`" @click="credentials.mode = item.value"
            >{{ item.label }}</button>
          </div>
          <button type="button" class="ghost danger small" data-testid="ak-clear" @click="clearAll">清空</button>
        </div>

        <div class="controls-scroll">
          <div class="panel input-panel">
          <form class="input-stack" @submit.prevent="verify">
            <label class="field" for="ak-access">
              <span>{{ modeConfig.access }}</span>
              <input id="ak-access" v-model="credentials.accessKey" data-testid="ak-access" autocomplete="off" spellcheck="false" placeholder="输入凭据 ID">
            </label>
            <label class="field" for="ak-secret">
              <span>{{ modeConfig.secret }}</span>
              <input id="ak-secret" v-model="credentials.secretKey" data-testid="ak-secret" type="password" autocomplete="new-password" spellcheck="false" placeholder="输入凭据密钥">
            </label>
            <label class="field" for="ak-fake-ip">
              <span>伪造 IP <span class="hint">(可选)</span></span>
              <input id="ak-fake-ip" v-model="fakeIP" data-testid="ak-fake-ip" autocomplete="off" spellcheck="false" placeholder="X-Forwarded-For">
            </label>
            <label class="field" for="ak-token">
              <span>access_token</span>
              <div class="token-row">
                <input id="ak-token" :value="result?.token ?? ''" data-testid="ak-token" readonly spellcheck="false" placeholder="点击「获取」以取得 token">
                <button type="button" class="ghost small" data-testid="ak-copy-token" :disabled="!result?.token" @click="copyToken">复制</button>
              </div>
            </label>
            <p v-if="result" class="status-line-inline" data-testid="ak-verify-status">
              <span class="status-pill" :class="result.valid ? 'ok' : 'off'" data-testid="ak-valid">{{ result.valid ? '有效' : '无效' }}</span>
              <span v-if="result.expires_in" class="hint-text">有效期 {{ result.expires_in }}s</span>
              <code class="hint-text" data-testid="ak-endpoint">{{ result.endpoint }}</code>
            </p>
            <button data-testid="ak-verify" type="submit" class="primary" :disabled="busy">{{ busy ? '获取中…' : result?.token ? '重新获取' : '获取' }}</button>
            <p class="doc-links">
              <button v-for="(link, index) in docLinks" :key="link.url" type="button" class="link-button" :data-testid="`ak-doc-${index}`" @click="openLink(link.url)">{{ link.label }}</button>
            </p>
          </form>
          </div>

        <div class="panel console-panel">
          <header class="panel-header">
            <h2>可利用接口 <span class="panel-note">{{ familyLabel }}</span> <span v-if="visible.length" class="subnav-count">{{ visible.length }}</span></h2>
            <button type="button" class="ghost small" data-testid="wxopen-reload" :disabled="loading" @click="load">{{ loading ? '加载中…' : '刷新' }}</button>
          </header>
          <div class="endpoint-picker">
            <!-- 下拉自己画：原生 select 的弹层由系统渲染，条目一多就顶出屏幕，也套不上深色
                 主题。弹层留在文档流里而不是绝对定位，所以在左栏里滚动不会被裁掉。 -->
            <div class="field">
              <span id="wxopen-endpoint-label">接口 <span v-if="visible.length" class="hint">（{{ visible.length }} 个可用）</span></span>
              <button
                type="button" class="picker-trigger" data-testid="wxopen-endpoint" :disabled="!visible.length"
                aria-haspopup="listbox" :aria-expanded="pickerOpen" aria-labelledby="wxopen-endpoint-label"
                @click="togglePicker"
              >
                <span class="picker-value">{{ selected ? `${selected.label} · ${selected.method} ${selected.path}` : '选择接口' }}</span>
                <span class="picker-caret" aria-hidden="true">▾</span>
              </button>
            </div>
            <div v-if="pickerOpen" ref="pickerMenu" class="picker-menu" role="listbox" aria-labelledby="wxopen-endpoint-label" data-testid="wxopen-endpoint-menu">
              <div v-for="group in categoryGroups" :key="group.category" class="picker-group" :data-testid="`wxopen-category-${group.category}`">
                <p class="picker-group-label">{{ group.category }}</p>
                <button
                  v-for="item in group.items" :key="item.id" type="button" role="option" class="picker-option"
                  :class="{ active: item.id === selectedId }" :aria-selected="item.id === selectedId"
                  :data-testid="`wxopen-endpoint-${item.id}`" @click="pickEndpoint(item.id)"
                >{{ item.label }} · {{ item.method }} {{ item.path }}</button>
              </div>
            </div>
            <p v-if="!visible.length" class="hint-text endpoint-empty" data-testid="wxopen-list-empty">{{ loading ? '正在读取接口表…' : '没有可用的接口，请先点击「刷新」。' }}</p>
          </div>

          <!-- 动态参数：本族多个接口共用的入参，填一次，相关接口都会带上。 -->
          <div v-if="sharedParams.length" class="dynamic-params">
            <p class="group-label">动态参数 <span class="hint">（本族接口共用，填一次即可）</span></p>
            <label v-for="param in sharedParams" :key="param.id" class="field" :for="`wxopen-shared-${param.id}`">
              <span>{{ param.label }}<span v-if="param.required" class="required">*</span><span v-if="param.hint" class="hint"> {{ param.hint }}</span></span>
              <input
                :id="`wxopen-shared-${param.id}`" v-model="sharedValues[param.id]" :data-testid="`wxopen-shared-${param.id}`"
                autocomplete="off" spellcheck="false" :placeholder="param.default || param.label"
              >
            </label>
          </div>

          <div v-if="selected" class="endpoint-detail">
            <div class="endpoint-head">
              <strong>{{ selected.label }}</strong>
              <span class="endpoint-path">{{ selected.method }} {{ selected.path }}</span>
            </div>
            <p class="endpoint-summary" data-testid="wxopen-summary">{{ selected.summary }}</p>
            <p v-if="selected.note" class="hint-text" data-testid="wxopen-note">{{ selected.note }}</p>
            <template v-if="selectedParams.length">
              <p class="group-label">本接口参数</p>
              <label v-for="param in selectedParams" :key="param.id" class="field" :for="`wxopen-param-${param.id}`">
                <span>{{ param.label }}<span v-if="param.required" class="required">*</span><span v-if="param.hint" class="hint"> {{ param.hint }}</span></span>
                <input
                  :id="`wxopen-param-${param.id}`" v-model="values[param.id]" :data-testid="`wxopen-param-${param.id}`"
                  autocomplete="off" spellcheck="false" :placeholder="param.default || param.label"
                >
              </label>
            </template>
            <div class="run-row">
              <button type="button" class="primary" data-testid="wxopen-run" :disabled="!canRun" @click="run">{{ callBusy ? '调用中…' : '执行' }}</button>
              <span v-if="missing.length" class="hint-text" data-testid="wxopen-missing">缺少参数：{{ missing.join('、') }}</span>
              <span v-else-if="!credentials.complete" class="hint-text">请先填写凭据。</span>
            </div>
          </div>
        </div>
        </div>
      </div>

      <!-- 请求区：这一次执行实际发出去的报文。 -->
      <div class="panel request-panel">
        <header class="panel-header">
          <h2>请求</h2>
          <div class="panel-actions">
            <button type="button" class="ghost small" data-testid="request-copy" :disabled="!current" @click="copyRequest">复制</button>
          </div>
        </header>
        <template v-if="current">
          <pre class="packet-head" data-testid="packet-request-head">{{ current.requestHead }}</pre>
          <pre v-if="current.requestBody" class="packet-head" data-testid="packet-request-body">{{ current.requestBody }}</pre>
        </template>
        <div v-else class="empty-state response-empty">
          <strong>尚无请求</strong>
          <span>点击「获取」以取得 token，或选择接口后点击「执行」。</span>
        </div>
      </div>

      <!-- 响应区：对应的响应报文与结果。 -->
      <div class="panel response-panel">
        <header class="panel-header">
          <h2>响应</h2>
          <div class="panel-actions">
            <button type="button" class="ghost small" data-testid="response-copy" :disabled="!current" @click="copyResponse">复制</button>
            <button type="button" class="ghost small" data-testid="response-clear" :disabled="!current" @click="clearResponse">清空</button>
          </div>
        </header>

        <template v-if="current">
          <pre class="packet-head" data-testid="packet-response-head">{{ current.responseHead }}</pre>
          <img v-if="current.image" class="result-image" :src="current.image" alt="接口返回的图片" data-testid="wxopen-image">
          <pre v-else class="result-output" :data-testid="current.kind === 'verify' ? 'ak-result' : 'wxopen-body'">{{ prettyJson(current.body ?? {}) }}</pre>
        </template>
        <div v-else class="empty-state response-empty">
          <strong>尚无响应</strong>
          <span>点击「获取」以取得 token，或选择接口后点击「执行」。</span>
        </div>
      </div>
    </div>
  </section>
</template>

<style scoped>
/* 三栏就是默认形态，不做响应式折叠：窗口再窄也是 控制 | 请求 | 响应，先让控制栏收窄
   （下限 240px），而不是把三块叠成一列。控制栏用 fr 而不是固定上限，正是为了在窄窗口
   下把空间让给两块报文——固定上限时它不肯让，报文会被压到 200px 以下。
   报文面板里的内容都会换行（起始行、头、体都是 pre-wrap + break-all）。

   整页不滚（page-workbench）：三栏各自占满高度、各自滚自己的。左栏滚到哪，请求/响应
   都留在原位——它们是一次执行的快照，跟着表单滑走就没法边填边看了。 */
.ak-layout { align-items: stretch; display: grid; gap: 14px; grid-template-columns: minmax(240px, .75fr) minmax(0, 1fr) minmax(0, 1fr); min-height: 0; }
.controls { display: flex; flex-direction: column; gap: 14px; min-width: 0; overflow: hidden; }
.controls-bar { align-items: center; display: flex; flex-wrap: wrap; gap: .5rem; justify-content: space-between; }
.controls-scroll { display: flex; flex: 1 1 auto; flex-direction: column; gap: 14px; min-height: 0; overflow-y: auto; }
/* 面板要能被压到栏宽：不然里面那条 nowrap 的接口名会把面板顶宽，整栏出横向滚动条。 */
.controls-scroll > * { min-width: 0; }
.request-panel, .response-panel { display: flex; flex-direction: column; min-width: 0; overflow-y: auto; }

/* 空态撑满面板并居中：请求/响应是这一页常驻的两块，不该随内容多少忽高忽低。 */
.request-panel > .empty-state,
.response-panel > .empty-state { align-content: center; flex: 1 1 auto; }

.input-stack { display: flex; flex-direction: column; gap: 10px; padding: 12px 14px 14px; }
.hint-text { color: var(--faint); font-size: 12px; line-height: 1.5; margin: 0; }
.hint { color: var(--faint); font-weight: 400; }
.required { color: var(--danger); margin-left: 2px; }
.panel-actions { align-items: center; display: flex; flex-wrap: wrap; gap: .45rem; }
.panel-note { color: var(--faint); font-size: 12px; font-weight: 400; }

.token-row { align-items: center; display: flex; gap: .35rem; }
.token-row input { flex: 1; min-width: 0; }
.status-line-inline { align-items: center; display: flex; flex-wrap: wrap; gap: .5rem; margin: 0; }
.status-line-inline code { color: var(--faint); font-family: var(--mono); overflow-wrap: anywhere; }

.endpoint-path { color: var(--faint); font-family: var(--mono); font-size: 11px; overflow-wrap: anywhere; }
.endpoint-picker { padding: 12px 14px 0; }
.picker-trigger {
  align-items: center;
  background: var(--panel-2);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text);
  cursor: pointer;
  display: flex;
  font: inherit;
  gap: .5rem;
  justify-content: space-between;
  /* .field 是 grid：不给 0 下限的话，里面那条 nowrap 的接口名会把这一格顶宽。 */
  min-width: 0;
  padding: 8px 10px;
  text-align: left;
  width: 100%;
}
.picker-trigger:hover:not(:disabled) { border-color: var(--accent); }
.picker-trigger:disabled { cursor: not-allowed; opacity: .6; }
.picker-value { font-size: 13px; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.picker-caret { color: var(--faint); flex: none; font-size: 11px; }
.picker-menu {
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  margin-top: 6px;
  /* 上限跟着视口走，条目再多也不会顶出屏幕；超出部分自己滚（只竖着滚，接口名会折行）。 */
  max-height: min(45vh, 320px);
  overflow-x: hidden;
  overflow-y: auto;
  padding: 4px;
}
.picker-group + .picker-group { border-top: 1px solid var(--border); margin-top: 4px; padding-top: 4px; }
.picker-group-label { color: var(--faint); font-size: 11px; font-weight: 650; letter-spacing: .04em; margin: 4px 8px; }
.picker-option {
  background: none;
  border: 0;
  border-radius: var(--radius-sm);
  color: var(--text);
  cursor: pointer;
  display: block;
  font-size: 13px;
  padding: 6px 8px;
  text-align: left;
  width: 100%;
}
.picker-option:hover { background: var(--panel-2); }
.picker-option.active { background: var(--accent-soft); color: var(--accent-strong); }
.endpoint-empty { padding: .8rem 14px 0; }

.endpoint-detail { border-top: 1px solid var(--border); display: flex; flex-direction: column; gap: 9px; padding: 12px 14px; }
.endpoint-head { align-items: baseline; display: flex; flex-wrap: wrap; gap: .5rem; }
.endpoint-head strong { font-size: 13px; }
.endpoint-summary { color: var(--muted); font-size: 13px; line-height: 1.6; margin: 0; }
.group-label { color: var(--faint); font-size: 11px; font-weight: 650; letter-spacing: .04em; margin: 2px 0 0; }
.dynamic-params { border-bottom: 1px solid var(--border); display: flex; flex-direction: column; gap: 8px; padding: 10px 14px 12px; }
.doc-links { display: flex; flex-wrap: wrap; gap: .8rem; margin: 0; }
.link-button { background: none; border: 0; color: var(--accent); cursor: pointer; font-size: 12px; padding: 0; text-decoration: underline; }
.link-button:hover { color: var(--accent-strong); }
.run-row { align-items: center; display: flex; flex-wrap: wrap; gap: .6rem; }
.run-row .primary { padding: 8px 26px; }

.response-panel { display: flex; flex-direction: column; min-width: 0; }
.packet-label { color: var(--faint); font-size: 11px; font-weight: 650; letter-spacing: .04em; margin: 10px 14px 0; }
.packet-head {
  background: var(--panel-2);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  font-family: var(--mono);
  font-size: 12px;
  margin: 4px 14px 0;
  overflow: auto;
  padding: 8px 10px;
  white-space: pre-wrap;
  word-break: break-all;
}
.response-empty { margin: 0; padding: 1.6rem 1rem; }
.result-output {
  background: var(--panel-3);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  font-family: var(--mono);
  font-size: 12px;
  margin: 10px 14px 14px;
  max-height: min(420px, calc(100vh - 320px));
  min-height: 160px;
  overflow: auto;
  padding: 12px;
  white-space: pre-wrap;
  word-break: break-word;
}
.result-image {
  align-self: center;
  background: #fff;
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  margin: 10px 14px 14px;
  max-width: min(100%, 320px);
  padding: 8px;
}

@media (max-width: 1100px) {
  .result-output { max-height: 320px; }
}
</style>
