export type BackendErrorPayload = { code?: string; message: string; retryable?: boolean; details?: unknown };
export type BackendEnvelope<T> = { result: T } | { error: string | BackendErrorPayload };

export class BackendCallError extends Error {
  readonly code: string;
  readonly retryable: boolean;
  readonly details?: unknown;

  constructor(payload: BackendErrorPayload) {
    super(payload.message);
    this.name = 'BackendCallError';
    this.code = payload.code ?? 'BACKEND_ERROR';
    this.retryable = payload.retryable ?? false;
    this.details = payload.details;
  }
}

// Wails v2 injects window.go bindings and window.runtime into the webview.
// The Go shell exposes one bound method Call(method, paramsJSON) returning
// the same {result}|{error} IPC envelope. Without Wails (browser debug),
// calls fall back to a local mock — not a production path.
type WailsHost = {
  main?: {
    App?: {
      Call?(method: string, paramsJSON: string): Promise<string>;
    };
  };
};

type WailsRuntime = {
  EventsOn?(name: string, callback: (payload?: unknown) => void): () => void;
  ClipboardSetText?(text: string): Promise<boolean>;
};

declare global {
  interface Window {
    go?: WailsHost;
    runtime?: WailsRuntime;
    __onBackendEvent?: (name: string, payload: unknown) => void;
  }
}

export type BackendEventName =
  | 'app_info'
  | 'assets_progress'
  | 'cloud_capture'
  | 'cloud_update'
  | 'code_file_formatted'
  | 'code_format_done'
  | 'export_progress'
  | 'extract_done'
  | 'extract_log'
  | 'extract_progress'
  | 'extract_scan_done'
  | 'log'
  | 'navigate_progress'
  | 'status'
  | 'task'
  | 'traffic:available'
  | 'vconsole_result'
  | 'wxapi_capture'
  | 'wxapi_update';

export interface BackendBridge {
  call<T>(method: string, params?: Record<string, unknown>): Promise<T>;
  on<T = unknown>(event: BackendEventName | string, listener: (payload: T) => void): () => void;
}

const supportedMethods = new Set([
  'ak.verify',
  // 资产清单（contracts/assets）：scan 异步受理后由 assets_progress 事件收尾。
  'assets.export',
  'assets.list',
  'assets.scan',
  'cloud.call',
  'cloud.call_container',
  'cloud.clear',
  'cloud.export',
  // v0.1.0 冻结面（compat-surface.json 钉住）：进度实际走 export_progress 事件。
  'cloud.export.progress',
  'cloud.poll',
  'cloud.scan',
  'cloud.start',
  'cloud.stats',
  'cloud.stop',
  'cloudapi.start',
  'cloudapi.status',
  'cloudapi.stop',
  'code.expandDir',
  'code.formatAll',
  'code.formatFile',
  'code.openDir',
  'code.project',
  'code.projects',
  'code.readFile',
  'code.search',
  'console.clear',
  'console.list',
  'electron.detect',
  'electron.status',
  'engine.status',
  'engine.start',
  'engine.stop',
  'engine.vconsole',
  'wechat.status',
  'config.load',
  'config.save',
  'extract.browse',
  'extract.builtinPatterns',
  'extract.candidateDirs',
  'extract.clearOutput',
  'extract.decompile',
  'extract.decompileAll',
  'extract.defaultDir',
  'extract.delete',
  'extract.openDir',
  'extract.inventory',
  'extract.packages',
  'extract.scan',
  'fetch.md',
  'hook.inject',
  'hook.list',
  'hook.setGlobal',
  'log.clear',
  'log.list',
  'mcp.start',
  'mcp.status',
  'mcp.stop',
  'miniapp.list',
  'navigator.autoVisit',
  'navigator.autoVisitState',
  'navigator.disableRedirectGuard',
  'navigator.enableRedirectGuard',
  'navigator.getCurrentRoute',
  'navigator.guardState',
  'navigator.navigate',
  'navigator.pages',
  'navigator.stopAutoVisit',
  'node.detect',
  'node.status',
  'sessionkey.detect',
  'sessionkey.users',
  'sessionkey.scan',
  'sessionkey.scanDecompiled',
  'sessionkey.scanTraffic',
  'settings.getPaths',
  'shell.openDevtoolsWindow',
  'shell.openFolder',
  'shell.openUrl',
  'targets.list',
  'traffic.appids',
  'traffic.list',
  'traffic.curl',
  'traffic.delete',
  'traffic.exportHar',
  'traffic.getBody',
  'traffic.replay',
  'traffic.stats',
  'traffic.clear',
  'update.checkVersion',
  'update.downloadRelease',
  'update.restart',
  'update.status',
  'update.syncSkills',
  'update.syncWMPF',
  'wxapi.clear',
  'wxapi.poll',
  'wxapi.replay',
  'wxapi.start',
  'wxapi.stats',
  'wxapi.stop',
  'wxopen.call',
  'wxopen.endpoints',
]);

const supportedEvents = new Set<BackendEventName>([
  'app_info',
  'assets_progress',
  'cloud_capture',
  'cloud_update',
  'code_file_formatted',
  'code_format_done',
  'export_progress',
  'extract_done',
  'extract_log',
  'extract_progress',
  'extract_scan_done',
  'log',
  'navigate_progress',
  'status',
  'task',
  'traffic:available',
  'vconsole_result',
  'wxapi_capture',
  'wxapi_update',
]);

const mockEventCompletions: Partial<Record<string, Array<[BackendEventName, unknown]>>> = {
  'engine.vconsole': [['vconsole_result', { ok: true, enable: true }]],
  'cloud.export': [
    ['export_progress', { status: 'done', path: '' }],
  ],
  // 无壳预览里没有真实的遍历 goroutine：补一发完成事件，否则界面会永远停在「遍历中」。
  'navigator.autoVisit': [['navigate_progress', { progress: 100, current: '', done: true, total: 1, failed: 0 }]],
  // 预览里同样没有真实的构建 goroutine：assets.scan 受理（async:true）后补一发
  // 完成事件，否则页面会永远停在「构建中」。
  'assets.scan': [['assets_progress', { status: 'done', current: 4, total: 4 }]],
};

// 预览模式下「代码浏览」页的样本：语法高亮、命中行定位、窗口加载控件都要
// 有真实形状的内容才看得出来。内容刻意带一处明文 secret（第 4 行），配合
// #/code?...&line=4&value=preview-secret-abcdef 的 hash 就能在无壳预览里
// 完整走一遍「扫描结果 → 源码定位」的体验。
const PREVIEW_ROOT = 'C:/wxtap-preview/wxdeadbeefdeadbeef';

const PREVIEW_FILES: Record<string, { content: string; language: string; kind?: string; dataUrl?: string }> = {
  'app.js': {
    language: 'javascript',
    content: `// 预览样本：反编译产物的常见形状（配置 + 明文密钥线索）
const appConfig = {
  appId: "wxdeadbeefdeadbeef",
  AppSecret: "preview-secret-abcdef",
  apiBase: "https://api.example.com/v2",
  retry: 3
};

function login(token) {
  return wx.request({
    url: appConfig.apiBase + "/login",
    header: { Authorization: "Bearer " + token },
    success(res) { console.log("login ok", res.statusCode); },
    fail(err) { console.warn("login failed", err); }
  });
}

App({ onLaunch() { login(appConfig.AppSecret); } });
`,
  },
  'app.json': {
    language: 'json',
    content: `{
  "pages": ["pages/index/index"],
  "window": {
    "navigationBarTitleText": "预览示例",
    "navigationBarBackgroundColor": "#101828"
  },
  "requiredPrivateInfos": ["getLocation"]
}
`,
  },
  'app.wxss': {
    language: 'css',
    content: `/* 预览样本：wxss 常见结构 */
page {
  background: #f3f3f9;
  font-size: 28rpx;
}

.card {
  border-radius: 12rpx;
  box-shadow: 0 2px 8px rgba(0, 0, 0, .08);
}
`,
  },
  'pages/index.js': {
    language: 'javascript',
    content: `Page({
  data: { title: "预览示例" },
  onTap() { wx.navigateTo({ url: "/pages/login/login" }); }
});
`,
  },
  'pages/index.wxml': {
    language: 'xml',
    content: `<view class="card">
  <text>{{ title }}</text>
  <button bindtap="onTap">提交</button>
  <!-- 预览样本 -->
</view>
`,
  },
  // 产物里的图片样本：预览模式要能看出图片走的是图片面板，而不是把字节当源码。
  'static/logo.png': {
    language: 'image/png',
    kind: 'image',
    content: '',
    // 48x48 的深蓝底 + 浅蓝方块，一眼能认出是画出来的图
    dataUrl: 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAADAAAAAwCAYAAABXAvmHAAAAUElEQVR42u3YMREAIAwAsUpgRAKSUIjNVgUccBleQNaP1ke+XAAAAAAAAAAAHATMlVsDAAAAAAAAAAAAAAAAAAAAALgT4MwBAAAAAAAAfAQoBJqNIgLs4KgAAAAASUVORK5CYII=',
  },
};

function previewTree() {
  return [
    { name: 'app.js', path: `${PREVIEW_ROOT}/app.js`, isDir: false },
    { name: 'app.json', path: `${PREVIEW_ROOT}/app.json`, isDir: false },
    { name: 'app.wxss', path: `${PREVIEW_ROOT}/app.wxss`, isDir: false },
    {
      name: 'pages',
      path: `${PREVIEW_ROOT}/pages`,
      isDir: true,
      children: [
        { name: 'index.js', path: `${PREVIEW_ROOT}/pages/index.js`, isDir: false },
        { name: 'index.wxml', path: `${PREVIEW_ROOT}/pages/index.wxml`, isDir: false },
      ],
    },
    {
      name: 'static',
      path: `${PREVIEW_ROOT}/static`,
      isDir: true,
      children: [
        { name: 'logo.png', path: `${PREVIEW_ROOT}/static/logo.png`, isDir: false },
      ],
    },
  ];
}

async function browserMock<T>(method: string, params?: Record<string, unknown>): Promise<T> {
  for (const [eventName, payload] of mockEventCompletions[method] ?? []) {
    setTimeout(() => window.__onBackendEvent?.(eventName, payload), 0);
  }
  if (method === 'engine.status') {
    // 预览按「已连接示例小程序」返回：资产页的目标跟随、目录按 appid 对齐、
    // 流量页的小程序筛选都走真实链路可看。
    return { frida: true, miniapp: true, devtools: false, appInfo: { appid: 'wxdeadbeefdeadbeef', name: '预览示例小程序' } } as T;
  }

  if (method === 'config.load') {
    return {} as T;
  }

  if (method === 'traffic.appids') {
    // 预览里只有一个示例小程序，计数与流量页的 mock 记录数对齐。
    return [{ appid: 'wxdeadbeefdeadbeef', count: 5, lastSeen: new Date().toISOString() }] as T;
  }

  if (method === 'settings.getPaths') {
    return {} as T;
  }

  if (method === 'fetch.md') {
    // 与 docs/FEEDBACK.md 的发布内容保持同构：预览时看到的排版与链接即真实排版
    // 与链接，别再放 example.com 之类的占位地址——用户会真的点进去。
    return {
      text: '# 交流反馈\n\n> 开发预览展示的是本地占位文档；正式内容以仓库的 feedback.md 为准。\n\n## 反馈渠道\n\n- 问题反馈：提交 [Issue](https://github.com/langbyyi/wxtap/issues)，应用内「提交 Issue」按钮直达同一地址。\n- 交流群：待补充。\n\n## 更新说明\n\n各版本更新内容见 [Releases](https://github.com/langbyyi/wxtap/releases) 页；`latest.json` 的 `notes` 字段即当次发布说明。',
    } as T;
  }

  // 预览模式下给接口台一份样本，否则页面只剩空列表，看不出布局
  if (method === 'wxopen.endpoints') {
    // 浏览器里没有 Go 后端，接口表用目录快照回答。快照由
    // desktop/wxopen_preview_contract_test.go 从真实目录生成并校验，所以预览和
    // 应用不会各说各话——手写样本正是"预览里只有两条小程序接口"的来源。
    // 动态引入：只有真走到预览这条路才加载它，应用包里不带这份数据。
    const { default: catalog } = await import('../data/wxopen-endpoints.json');
    return catalog as T;
  }

  // 「微信 AK」页在预览模式下的样本：否则验证与调用都只回 {ok:true}，
  // 记录区看不出样子。
  if (method === 'ak.verify') {
    return { mode: 'mini', valid: true, endpoint: 'https://api.weixin.qq.com/cgi-bin/token', http_status: 200, errcode: 0, errmsg: 'ok', expires_in: 7200, token: 'TOKEN-preview-abcdefghijklmnop', token_fingerprint: 'abcd1234' } as T;
  }
  if (method === 'wxopen.call') {
    return { endpoint: 'mini-code-unlimited', label: '生成小程序码（不限量）', method: 'POST', url: 'https://api.weixin.qq.com/wxa/getwxacodeunlimit?access_token=TOKEN-a1b2c3d4e5f6', http_status: 200, errcode: 0, errmsg: '', duration_ms: 88, body: { errcode: 0, content_type: 'image/jpeg' } } as T;
  }
  // 预览没有历史库：这里给一份样本，否则「历史记录」页永远是空列表，行高、省略、
  // 徽标堆叠这些排版问题在预览里根本看不出来。形状严格按 contracts/traffic.ts 的
  // TrafficSummary：字段少一个页面就会渲染成「未知」，多一个也不是后端会给的东西。
  // 分页按请求里的 page / pageSize 真切片（总数报 137，样本只有 5 条）：页码、末页禁用、
  // 跳转这些交互在预览里才走得通。
  if (method === 'traffic.list') {
    const at = (minutesAgo: number) => new Date(Date.now() - minutesAgo * 60_000).toISOString();
    // wx.request 的 name 由钩子拼成「方法 + URL」（见 core/hooks/wxapi.js），url 字段本身
    // 只有云函数记录才带 —— 样本两种都给，页面两种排版都要能看。
    const items = [
      { id: '1-1', seq: 1, capturedAt: at(2), apiType: 'wx.request', appId: 'wxdeadbeefdeadbeef', name: 'POST https://api.example.com/v2/order/create?from=miniapp&channel=scan', method: 'POST', status: 'success', requestBytes: 312, responseBytes: 1840, durationMs: 128 },
      { id: '1-2', seq: 2, capturedAt: at(3), apiType: 'wx.request', appId: 'wxdeadbeefdeadbeef', name: 'GET https://api.example.com/v2/user/profile', method: 'GET', status: 'success', requestBytes: 96, responseBytes: 640, durationMs: 43 },
      { id: '1-3', seq: 3, capturedAt: at(5), apiType: 'wx.request', appId: '', name: 'POST https://third.example.net/collect', method: 'POST', status: 'fail', requestBytes: 204, responseBytes: 88, durationMs: 5012 },
      { id: '1-4', seq: 4, capturedAt: at(6), apiType: 'cloud.function', appId: 'wxdeadbeefdeadbeef', name: 'login', method: 'POST', url: 'https://service.example.com/login', status: 'success', requestBytes: 128, responseBytes: 456, durationMs: 240 },
      { id: '1-5', seq: 5, capturedAt: at(8), apiType: 'cloud.database.get', appId: 'wxdeadbeefdeadbeef', name: 'db.collection("orders").get', status: 'pending', requestBytes: 64, responseBytes: 0, durationMs: 0 },
    ];
    const paging = params as { page?: number; pageSize?: number } | undefined;
    const page = Math.max(1, Math.floor(paging?.page ?? 1));
    const pageSize = Math.max(1, Math.floor(paging?.pageSize ?? 100));
    return { items: items.slice((page - 1) * pageSize, page * pageSize), total: 137, page, pageSize } as T;
  }
  if (method === 'traffic.getBody') return '' as T;
  // 统计与上面那份样本对得上，免得顶栏说「库里 0 条」而列表里有 5 行。
  if (method === 'traffic.stats') {
    return { records: 137, oldestCapturedAt: new Date(Date.now() - 8 * 60_000).toISOString(), newestCapturedAt: new Date().toISOString(), bytes: 49152 } as T;
  }
  // 预览里照实报 0：没有真记录可数，编一个删除数会让人以为清空了什么。
  if (method === 'traffic.clear') return { deleted: 0 } as T;
  if (method === 'traffic.delete') return { deleted: 0 } as T;

  if (method === 'traffic.curl') {
    // 与 contracts/traffic 一致：bash 是 *nix 形式，cmd 是 Windows 形式；页面复制 bash。
    return {
      ok: true,
      bash: "curl -sS -X GET 'https://api.example.com/v2/user/profile?uid=1001' -H 'Authorization: Bearer seed-token'",
      cmd: "curl.exe -sS -X GET \"https://api.example.com/v2/user/profile?uid=1001\" -H \"Authorization: Bearer seed-token\"",
    } as T;
  }
  if (method === 'traffic.replay') {
    return { ok: true, status: 200, elapsedMs: 128, body: '{"code":0,"data":{"uid":1001,"nickname":"alice"}}', truncated: false } as T;
  }
  if (method === 'traffic.exportHar') return { ok: true, path: 'C:/wxtap-preview/session.har' } as T;

  // 资产清单（契约：contracts/audit.ts）。分页与筛选按请求参数真切片，与 traffic.list
  // 的 mock 同一做法：页码、总数与筛选在预览里才走得通。lastSeen 用相对当前时间的
  // ISO 串，「相对时间」列才看得出新旧的差别。
  if (method === 'assets.list') {
    const at = (hoursAgo: number) => new Date(Date.now() - hoursAgo * 3_600_000).toISOString();
    const items = [
      {
        id: 'a-1', kind: 'api', url: 'https://api.example.com/v2/user/profile', host: 'api.example.com',
        path: '/v2/user/profile', method: 'GET',
        sources: [{ type: 'traffic', ref: 'seed-1' }, { type: 'code', ref: 'pages/index.js' }],
        hits: 42, firstSeen: at(26), lastSeen: at(1), tags: ['需要登录'], trafficSeen: true,
      },
      {
        id: 'a-2', kind: 'api', url: 'https://api.example.com/v2/order/create', host: 'api.example.com',
        path: '/v2/order/create', method: 'POST',
        sources: [{ type: 'traffic', ref: 'seed-2' }],
        hits: 17, firstSeen: at(26), lastSeen: at(3), tags: [], trafficSeen: true,
      },
      {
        id: 'a-3', kind: 'static', url: 'https://cdn.example.com/static/app.config.json', host: 'cdn.example.com',
        path: '/static/app.config.json', method: 'GET',
        sources: [{ type: 'code', ref: 'app.js' }],
        hits: 5, firstSeen: at(50), lastSeen: at(50), tags: ['配置'], trafficSeen: false,
      },
      {
        id: 'a-4', kind: 'ws', url: 'wss://ws.example.com/conn', host: 'ws.example.com',
        path: '/conn', method: 'GET',
        sources: [{ type: 'code', ref: 'utils/socket.js' }],
        hits: 9, firstSeen: at(26), lastSeen: at(2), tags: ['长连接'], trafficSeen: false,
      },
      {
        id: 'a-5', kind: 'cloud', url: 'cloud://env-1.login', host: 'env-1', path: 'login', method: 'CALL',
        sources: [{ type: 'traffic', ref: 'seed-3' }, { type: 'code', ref: 'pages/login.js' }],
        hits: 23, firstSeen: at(26), lastSeen: at(4), tags: ['云函数'], trafficSeen: true,
      },
    ];
    const filter = (params ?? {}) as { kind?: string; host?: string; query?: string; offset?: number; limit?: number };
    const needle = (filter.query ?? '').trim().toLowerCase();
    const matched = items.filter((item) => (!filter.kind || item.kind === filter.kind)
      && (!filter.host || item.host === filter.host)
      && (!needle || `${item.url} ${item.path} ${item.method}`.toLowerCase().includes(needle)));
    const counts = new Map<string, number>();
    for (const item of items) counts.set(item.host, (counts.get(item.host) ?? 0) + 1);
    const hosts = [...counts.entries()].map(([host, count]) => ({ host, count })).sort((a, b) => b.count - a.count);
    const offset = Math.max(0, Math.floor(filter.offset ?? 0));
    const limit = Math.max(1, Math.floor(filter.limit ?? 100));
    return { ok: true, total: matched.length, hosts, items: matched.slice(offset, offset + limit), appid: 'wxdeadbeefdeadbeef', builtAt: new Date(Date.now() - 3_600_000).toISOString() } as T;
  }
  if (method === 'assets.scan') return { ok: true, async: true, taskId: 'assets-preview' } as T;
  if (method === 'assets.export') {
    const format = String((params as { format?: string } | undefined)?.format ?? 'json');
    return { ok: true, path: `C:/wxtap-preview/assets.${format}` } as T;
  }

  // wxapi.poll 的响应契约是数组（没有记录就是空数组）；预览模式返回 {ok:true}
  // 会让页面插进一条假记录，也和契约不符。
  if (method === 'wxapi.poll') return [] as T;
  // cloud.poll 与 wxapi.poll 同契约：响应永远是数组。
  if (method === 'cloud.poll') return [] as T;

  // 浏览器预览没有引擎：捕获统计按「已运行」返回，否则页面会把开启捕获判成失败。
  if (method === 'wxapi.stats') return { running: true, pending: 0, dropped: 0, ack: 0, updateAck: 0 } as T;
  if (method === 'cloud.stats') return { running: true, pending: 0, dropped: 0, ack: 0, updateAck: 0 } as T;

  // 代码浏览：一个假想产物足够撑起目录、高亮与命中定位的预览。
  if (method === 'code.projects') {
    return { projects: [{ appid: 'wxdeadbeefdeadbeef', name: '预览示例小程序', path: PREVIEW_ROOT, mtime: Math.floor(Date.now() / 1000) }] } as T;
  }
  if (method === 'code.project') {
    return { root: PREVIEW_ROOT, tree: previewTree() } as T;
  }
  if (method === 'code.readFile') {
    const relative = String(params?.path ?? '').replace(/\\/g, '/').slice(PREVIEW_ROOT.length + 1);
    const file = PREVIEW_FILES[relative] ?? PREVIEW_FILES['app.js'];
    return { content: file.content, size: file.content.length, language: file.language, truncated: false, kind: file.kind ?? 'text', dataUrl: file.dataUrl ?? '' } as T;
  }
  if (method === 'code.search') {
    return {
      results: [
        { file: `${PREVIEW_ROOT}/app.js`, line: 4, text: '  AppSecret: "preview-secret-abcdef",' },
        { file: `${PREVIEW_ROOT}/pages/index.js`, line: 3, text: 'onTap() { wx.navigateTo({ url: "/pages/login/login" }); }' },
      ],
      truncated: false,
    } as T;
  }

  return { ok: true } as T;
}

export function createBackendBridge(): BackendBridge {
  const listeners = new Map<BackendEventName, Set<(payload: unknown) => void>>();

  if (typeof window !== 'undefined') {
    window.__onBackendEvent = (name, payload) => {
      if (!supportedEvents.has(name as BackendEventName)) {
        return;
      }

      for (const listener of listeners.get(name as BackendEventName) ?? []) {
        try {
          listener(payload);
        } catch (error) {
          console.error(`Backend event listener failed for ${name}`, error);
        }
      }
    };

    // Inside the Wails webview, pipe runtime events into the dispatcher.
    const wailsRuntime = window.runtime;
    if (wailsRuntime?.EventsOn) {
      for (const eventName of supportedEvents) {
        wailsRuntime.EventsOn(eventName, (payload?: unknown) => {
          window.__onBackendEvent?.(eventName, payload);
        });
      }
    }
  }

  return {
    async call<T>(method: string, params?: Record<string, unknown>): Promise<T> {
      if (!supportedMethods.has(method)) {
        throw new Error(`Unsupported backend method: ${method}`);
      }

      const wails = typeof window === 'undefined' ? undefined : window.go?.main?.App;

      if (wails?.Call) {
        const response = await wails.Call(method, JSON.stringify(params ?? {}));
        return parseEnvelope<T>(response);
      }

      return browserMock<T>(method, params);
    },
    on<T>(event: BackendEventName | string, listener: (payload: T) => void): () => void {
      if (!supportedEvents.has(event as BackendEventName)) {
        throw new Error(`Unsupported backend event: ${event}`);
      }

      const eventListeners = listeners.get(event as BackendEventName) ?? new Set();
      eventListeners.add(listener as (payload: unknown) => void);
      listeners.set(event as BackendEventName, eventListeners);

      return () => eventListeners.delete(listener as (payload: unknown) => void);
    },
  };
}

function parseEnvelope<T>(response: string): T {
  let envelope: BackendEnvelope<T>;

  try {
    envelope = JSON.parse(response) as BackendEnvelope<T>;
  } catch {
    throw new Error('Invalid backend response JSON');
  }

  if ('error' in envelope) {
    if (typeof envelope.error === 'string') throw new BackendCallError({ message: envelope.error });
    // 展开顺序决定胜负：后端给空 message 时，兜底文案必须留下。
    throw new BackendCallError({ ...envelope.error, message: envelope.error.message || '后端调用失败' });
  }

  return envelope.result;
}

export const backend = createBackendBridge();
