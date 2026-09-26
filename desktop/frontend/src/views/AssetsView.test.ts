import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import AssetsView from './AssetsView.vue';
import PageHeader from '../components/PageHeader.vue';

const { call, listeners, routerPush, engineHolder } = vi.hoisted(() => ({
  call: vi.fn(),
  listeners: {} as Record<string, (payload?: unknown) => void>,
  routerPush: vi.fn(),
  engineHolder: {} as { state?: { status: { miniapp: boolean; devtools: boolean; appInfo: null | { appid?: string; name?: string } } } },
}));
vi.mock('../api/bridge', () => ({
  backend: {
    call,
    on: vi.fn((event: string, listener: (payload?: unknown) => void) => {
      listeners[event] = listener;
      return () => delete listeners[event];
    }),
  },
}));
// 资产页只读 engine store 的 status.appInfo（当前连接的小程序身份）；用
// reactive 对象充当 store，测试直接改它来模拟连接/切换。
vi.mock('../stores/engine', async () => {
  const { reactive } = await import('vue');
  const state = reactive({ status: { miniapp: false, devtools: false, appInfo: null as null | { appid?: string; name?: string } } });
  engineHolder.state = state;
  return { useEngineStore: () => state };
});
// AssetDetail 的流量跳转用 useRouter；测试里只断言 push 的目标。
vi.mock('vue-router', () => ({ useRouter: () => ({ push: routerPush }) }));

enableAutoUnmount(afterEach);

const minutesAgo = (minutes: number) => new Date(Date.now() - minutes * 60_000).toISOString();

// 形状与 contracts/assets 严格一致的样本：一条 api（双来源 + tags）、一条 cloud。
const assetsItems = [
  {
    id: 'a-1', kind: 'api', url: 'https://api.example.com/v2/user/profile', host: 'api.example.com',
    path: '/v2/user/profile', method: 'GET',
    sources: [{ type: 'traffic', ref: 'seed-1' }, { type: 'code', ref: 'pages/index.js' }],
    hits: 42, firstSeen: minutesAgo(120), lastSeen: minutesAgo(2), tags: ['需要登录'],
  },
  {
    id: 'a-2', kind: 'cloud', url: 'cloud://env-1.login', host: 'env-1',
    path: 'login', method: 'CALL',
    sources: [{ type: 'traffic', ref: 'seed-3' }],
    hits: 23, firstSeen: minutesAgo(120), lastSeen: minutesAgo(30), tags: [],
  },
];

function mockHappyPath() {
  call.mockImplementation((method: string) => {
    if (method === 'assets.list') return Promise.resolve({ ok: true, total: 2, hosts: [{ host: 'api.example.com', count: 1 }, { host: 'env-1', count: 1 }], items: assetsItems });
    if (method === 'assets.scan') return Promise.resolve({ ok: true, async: true, taskId: 'assets-task-1' });
    if (method === 'assets.export') return Promise.resolve({ ok: true, path: 'C:/out/assets.nuclei' });
    // 产物列表默认有一份已反编译的小程序：目标下拉只列已反编译的程序
    if (method === 'code.projects') return Promise.resolve({ projects: [{ appid: 'wx-preview', name: '预览小程序', path: 'C:/out/wx-preview', mtime: 100 }] });
    return Promise.resolve({ ok: true });
  });
}

// 主查询（结构树 limit=500 / 明细表 limit=100）与 chips 计数探针（limit=1）都打 assets.list，按 limit 区分。
const listCalls = () => call.mock.calls.filter(([method]) => method === 'assets.list');
const mainListCalls = () => listCalls().filter(([, params]) => [100, 500].includes((params as { limit?: number })?.limit ?? 0));

beforeEach(() => {
  call.mockReset();
  routerPush.mockClear();
  if (engineHolder.state) engineHolder.state.status.appInfo = null;
  mockHappyPath();
});

afterEach(() => {
  vi.useRealTimers();
});

describe('AssetsView', () => {
  it('uses the shared page header contract', () => {
    expect(mount(AssetsView).findComponent(PageHeader).exists()).toBe(true);
  });

  it('renders the inventory as a host-path tree with leaf details', async () => {
    const wrapper = mount(AssetsView);
    await flushPromises();

    // 主机行：count 相同按名称排序
    const hostNames = wrapper.findAll('.tree-row.host-row .tree-name').map((node) => node.text());
    expect(hostNames).toEqual(['api.example.com', 'env-1']);

    // 路径逐级成段：/v2/user/profile 拆成 v2 → user → profile，云函数 login 是单段
    const pathNames = wrapper.findAll('.tree-row.path-row .tree-name').map((node) => node.text());
    expect(pathNames).toEqual(['v2', 'user', 'profile', 'login']);

    // 叶子行：方法、命中、相对时间都在
    const first = wrapper.get('[data-testid="assets-row-a-1"]');
    expect(first.text()).toContain('API');
    expect(first.text()).toContain('GET');
    expect(first.get('.record-badge').classes()).toContain('method-read');
    expect(first.text()).toContain('42');
    // lastSeen 两分钟前 → 相对时间
    expect(first.text()).toContain('分钟前');
    expect(wrapper.get('[data-testid="assets-row-a-2"]').text()).toContain('云函数');

    // 汇总 chips：total 来自探针
    expect(wrapper.get('[data-testid="assets-chip-total"]').text()).toContain('2');

    // host 下拉来自 hosts 汇总
    const hostOptions = wrapper.get('[data-testid="assets-host-filter"]').findAll('option');
    expect(hostOptions.map((option) => option.text())).toEqual(['全部主机', 'api.example.com（1）', 'env-1（1）']);
  });

  it('filters by kind when a summary chip is clicked', async () => {
    const wrapper = mount(AssetsView);
    await flushPromises();

    await wrapper.get('[data-testid="assets-chip-api"]').trigger('click');
    await flushPromises();
    // chips 是唯一的类型筛选入口：高亮 + 主查询带上 kind=api
    expect(wrapper.get('[data-testid="assets-chip-api"]').classes()).toContain('active');
    const lastMain = mainListCalls().at(-1)?.[1] as { kind?: string };
    expect(lastMain?.kind).toBe('api');

    // 点回「全部」恢复无条件查询
    await wrapper.get('[data-testid="assets-chip-total"]').trigger('click');
    await flushPromises();
    const restored = mainListCalls().at(-1)?.[1] as { kind?: string };
    expect(restored?.kind).toBeUndefined();
  });

  it('targets the connected mini program and sends its appid with the build', async () => {
    engineHolder.state!.status.appInfo = { appid: 'wx-current', name: '演示小程序' };
    call.mockImplementation((method: string) => {
      if (method === 'code.projects') return Promise.resolve({ projects: [{ appid: 'wx-current', name: '演示小程序', path: 'C:/out/wx-current' }] });
      if (method === 'assets.list') return Promise.resolve({ ok: true, total: 0, hosts: [], items: [] });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(AssetsView);
    await flushPromises();

    // 目标下拉默认选中已连接（已反编译）的小程序，并标注已连接；没有「未指定」
    const select = wrapper.get<HTMLSelectElement>('[data-testid="assets-target"]');
    expect(select.element.value).toBe('wx-current');
    expect(select.text()).toContain('已连接');
    expect(select.text()).not.toContain('未指定');
    // 反编译目录不再手填：构建参数里也没有 dir
    expect(wrapper.find('[data-testid="assets-dir"]').exists()).toBe(false);
    await wrapper.get('[data-testid="assets-build"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('assets.scan', expect.objectContaining({ appid: 'wx-current' }));
    const scanParams = call.mock.calls.find(([method]) => method === 'assets.scan')?.[1] as Record<string, unknown>;
    expect(scanParams).not.toHaveProperty('dir');
    // 目标写入 config，重启后据此恢复
    expect(call).toHaveBeenCalledWith('config.save', expect.objectContaining({ assetTargetAppID: 'wx-current' }));
  });

  it('restores the last target from config and shows the built-at hint', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'config.load') {
        return Promise.resolve({ assetTargetAppID: 'wx-last', assetCloudFns: { 'wx-last': 'login' } });
      }
      if (method === 'code.projects') {
        return Promise.resolve({
          projects: [
            { appid: 'wx-last', name: '上次小程序', path: 'C:/out/wx-last' },
            { appid: 'wx-current', name: '当前小程序', path: 'C:/out/wx-current' },
          ],
        });
      }
      if (method === 'assets.list') {
        return Promise.resolve({ ok: true, total: 1, hosts: [], items: [assetsItems[1]], appid: 'wx-last', builtAt: '2026-09-26T00:00:00Z' });
      }
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(AssetsView);
    await flushPromises();

    // 上次目标回填，云函数名一并恢复；清单归属提示带目标与构建时间
    expect(wrapper.get<HTMLSelectElement>('[data-testid="assets-target"]').element.value).toBe('wx-last');
    expect(wrapper.get<HTMLInputElement>('[data-testid="assets-cloud-fns"]').element.value).toBe('login');
    expect(wrapper.get('[data-testid="assets-built-hint"]').text()).toContain('上次小程序');
    expect(wrapper.get('[data-testid="assets-built-hint"]').text()).toContain('构建于');

    // 切换目标后旧清单不自动重建：提示指出错位并引导重新构建。
    // （走真实路径：连接到另一个小程序 → 跟随逻辑自动切目标）
    engineHolder.state!.status.appInfo = { appid: 'wx-current', name: '当前' };
    await flushPromises();
    expect(wrapper.get<HTMLSelectElement>('[data-testid="assets-target"]').element.value).toBe('wx-current');
    expect(wrapper.get('[data-testid="assets-built-hint"]').text()).toContain('目标已切换为');
    expect(wrapper.get('[data-testid="assets-built-hint"]').text()).toContain('重新构建');
  });

  it('lists only decompiled programs, connected first, and builds without a dir param', async () => {
    engineHolder.state!.status.appInfo = { appid: 'wx-mine', name: '我的小程序' };
    call.mockImplementation((method: string) => {
      if (method === 'code.projects') {
        return Promise.resolve({
          projects: [
            { appid: 'wx-other', path: 'C:/out/wx-other', mtime: 200 },
            { appid: 'wx-mine', path: 'C:/out/wx-mine', mtime: 100 },
          ],
        });
      }
      if (method === 'assets.list') return Promise.resolve({ ok: true, total: 0, hosts: [], items: [] });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(AssetsView);
    await flushPromises();

    // 下拉只列已反编译的产物（占位 + 两个程序），已连接的排最前并标注
    const options = wrapper.get('[data-testid="assets-target"]').findAll('option');
    expect(options.map((option) => option.text())).toEqual(['请选择小程序', 'wx-mine · 已连接', 'wx-other']);
    expect(wrapper.find('[data-testid="assets-decompile-connected"]').exists()).toBe(false);

    // 构建带目标 appid，不带目录：目录由后端按 appid 解析
    await wrapper.get('[data-testid="assets-build"]').trigger('click');
    await flushPromises();
    const scanParams = call.mock.calls.find(([method]) => method === 'assets.scan')?.[1] as Record<string, unknown>;
    expect(scanParams).toMatchObject({ appid: 'wx-mine', includeTraffic: true });
    expect(scanParams).not.toHaveProperty('dir');
  });

  it('offers a one-click decompile for a connected program without output and selects it afterwards', async () => {
    // 连接中的 wx-b 还没有产物：不进下拉（没法选），露出一键反编译；
    // 反编译完成后刷新产物列表并自动选中它。
    const projects = [{ appid: 'wx-a', path: 'C:/out/wx-a', mtime: 100 }];
    call.mockImplementation((method: string) => {
      if (method === 'code.projects') return Promise.resolve({ projects: projects.map((project) => ({ ...project })) });
      if (method === 'extract.decompile') {
        projects.push({ appid: 'wx-b', path: 'C:/out/wx-b', mtime: 200 });
        return Promise.resolve({ ok: true, files_count: 12 });
      }
      if (method === 'assets.list') return Promise.resolve({ ok: true, total: 0, hosts: [], items: [] });
      return Promise.resolve({ ok: true });
    });
    engineHolder.state!.status.appInfo = { appid: 'wx-b', name: 'B' };
    const wrapper = mount(AssetsView);
    await flushPromises();

    expect(wrapper.get<HTMLSelectElement>('[data-testid="assets-target"]').element.value).toBe('');
    expect(wrapper.get('[data-testid="assets-decompile-connected"]').text()).toBe('反编译当前小程序');

    await wrapper.get('[data-testid="assets-decompile-connected"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('extract.decompile', { appid: 'wx-b' });
    expect(wrapper.get<HTMLSelectElement>('[data-testid="assets-target"]').element.value).toBe('wx-b');
    // 已反编译，入口随之消失
    expect(wrapper.find('[data-testid="assets-decompile-connected"]').exists()).toBe(false);
  });

  it('expands a leaf to show the full URL, tags and sources', async () => {
    const wrapper = mount(AssetsView);
    await flushPromises();
    expect(wrapper.find('[data-testid="assets-detail-a-1"]').exists()).toBe(false);

    await wrapper.get('[data-testid="assets-expand-a-1"]').trigger('click');
    const detail = wrapper.get('[data-testid="assets-detail-a-1"]');
    expect(detail.text()).toContain('https://api.example.com/v2/user/profile');
    expect(detail.text()).toContain('需要登录');
    expect(detail.text()).toContain('traffic → seed-1');
    expect(detail.text()).toContain('code → pages/index.js');
  });

  it('auto-expands small subtrees and supports expand/collapse all', async () => {
    // 一个 host：/a 下 1 条 + /big 下 19 条；big 子树超过自动展开阈值，默认收起
    const items = [
      { id: 'solo', kind: 'api', url: 'https://h.test/a', host: 'h.test', path: '/a', method: 'GET', sources: [], hits: 1, firstSeen: '', lastSeen: '' },
      ...Array.from({ length: 19 }, (_, i) => ({ id: `b-${i}`, kind: 'api', url: `https://h.test/big/p${i}`, host: 'h.test', path: `/big/p${i}`, method: 'GET', sources: [], hits: 1, firstSeen: '', lastSeen: '' })),
    ];
    call.mockImplementation((method: string, params: { limit?: number }) => {
      if (method === 'assets.list') {
        if (params?.limit === 1) return Promise.resolve({ ok: true, total: items.length, hosts: [], items: [] });
        return Promise.resolve({ ok: true, total: items.length, hosts: [{ host: 'h.test', count: items.length }], items });
      }
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(AssetsView);
    await flushPromises();

    // 小子树自动展开，大子树收起：只看到 /a 的叶子
    expect(wrapper.findAll('.tree-row.leaf-row')).toHaveLength(1);
    const bigRow = wrapper.findAll('.tree-row.path-row').find((row) => row.text().includes('big'));
    expect(bigRow).toBeDefined();
    // 收起的大子树行内不出叶子
    expect(wrapper.text()).not.toContain('https://h.test/big');

    await wrapper.get('[data-testid="assets-tree-toolbar"]').get('button:nth-of-type(1)').trigger('click');
    await flushPromises();
    expect(wrapper.findAll('.tree-row.leaf-row')).toHaveLength(20);

    await wrapper.get('[data-testid="assets-tree-toolbar"]').get('button:nth-of-type(2)').trigger('click');
    await flushPromises();
    expect(wrapper.findAll('.tree-row.leaf-row')).toHaveLength(0);
    expect(wrapper.findAll('.tree-row.host-row')).toHaveLength(1);
  });

  it('switches between the tree, per-page and flat table views', async () => {
    const wrapper = mount(AssetsView);
    await flushPromises();
    expect(wrapper.find('[data-testid="assets-tree"]').exists()).toBe(true);
    expect(wrapper.find('[data-testid="assets-pages"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="assets-table"]').exists()).toBe(false);

    await wrapper.get('[data-testid="assets-view-table"]').trigger('click');
    await flushPromises();
    expect(wrapper.find('[data-testid="assets-table"]').exists()).toBe(true);
    expect(wrapper.find('[data-testid="assets-tree"]').exists()).toBe(false);

    await wrapper.get('[data-testid="assets-view-tree"]').trigger('click');
    await flushPromises();
    expect(wrapper.find('[data-testid="assets-tree"]').exists()).toBe(true);
    expect(wrapper.find('[data-testid="assets-table"]').exists()).toBe(false);

    // 树 ↔ 页面分组共用同一份全量数据，切换不该再触发主查询
    const mainsBefore = mainListCalls().length;
    await wrapper.get('[data-testid="assets-view-pages"]').trigger('click');
    await flushPromises();
    expect(wrapper.find('[data-testid="assets-pages"]').exists()).toBe(true);
    expect(wrapper.find('[data-testid="assets-tree"]').exists()).toBe(false);
    expect(mainListCalls().length).toBe(mainsBefore);
  });

  it('groups assets by the page that references them in the pages view', async () => {
    const wrapper = mount(AssetsView);
    await flushPromises();

    await wrapper.get('[data-testid="assets-view-pages"]').trigger('click');
    await flushPromises();
    // a-1 有 code 来源 pages/index.js；a-2 只有流量来源，落进兜底组
    const groupNames = wrapper.findAll('.tree-row.host-row .tree-name').map((node) => node.text());
    expect(groupNames).toEqual(['pages/index.js', '（仅流量来源）']);
    expect(wrapper.get('[data-testid="assets-row-a-1"]').text()).toContain('GET');
    expect(wrapper.get('[data-testid="assets-row-a-2"]').text()).toContain('CALL');
  });

  it('toggles a group with the keyboard and reflects aria-expanded', async () => {
    const wrapper = mount(AssetsView);
    await flushPromises();
    const hostRow = wrapper.get('.tree-row.host-row');
    expect(hostRow.attributes('aria-expanded')).toBe('true');

    await hostRow.trigger('keydown.enter');
    await flushPromises();
    expect(wrapper.get('.tree-row.host-row').attributes('aria-expanded')).toBe('false');
    // api.example.com 收起后只剩 env-1 组的 a-2 叶子
    expect(wrapper.findAll('.tree-row.leaf-row')).toHaveLength(1);

    await wrapper.get('.tree-row.host-row').trigger('keydown.space');
    await flushPromises();
    expect(wrapper.findAll('.tree-row.leaf-row')).toHaveLength(2);
  });

  it('renders one leaf per page group when an asset is referenced by several pages', async () => {
    // 同一资产被两个页面引用：两个已展开分组各渲染一行，key 必须不冲突
    const shared = {
      id: 'a-9', kind: 'api', url: 'https://api.example.com/x', host: 'api.example.com', path: '/x',
      method: 'GET', hits: 1, firstSeen: '', lastSeen: '', trafficSeen: false,
      sources: [{ type: 'code', ref: 'pages/one.js' }, { type: 'code', ref: 'pages/two.js' }],
    };
    call.mockImplementation((method: string, params: { limit?: number }) => {
      if (method === 'assets.list') {
        if (params?.limit === 1) return Promise.resolve({ ok: true, total: 1, hosts: [], items: [] });
        return Promise.resolve({ ok: true, total: 1, hosts: [{ host: 'api.example.com', count: 1 }], items: [shared] });
      }
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(AssetsView);
    await flushPromises();

    await wrapper.get('[data-testid="assets-view-pages"]').trigger('click');
    await flushPromises();
    expect(wrapper.findAll('[data-testid="assets-row-a-9"]')).toHaveLength(2);
  });

  it('refetches with the kind and host filters pushed down to assets.list', async () => {
    const wrapper = mount(AssetsView);
    await flushPromises();
    expect(mainListCalls()).toHaveLength(1);

    await wrapper.get('[data-testid="assets-chip-api"]').trigger('click');
    await flushPromises();
    expect(mainListCalls()).toHaveLength(2);
    expect(mainListCalls()[1]?.[1]).toMatchObject({ kind: 'api', offset: 0, limit: 500 });

    await wrapper.get('[data-testid="assets-host-filter"]').setValue('api.example.com');
    await flushPromises();
    expect(mainListCalls()[2]?.[1]).toMatchObject({ kind: 'api', host: 'api.example.com', limit: 500 });
  });

  it('debounces the keyword filter by 300ms before refetching', async () => {
    vi.useFakeTimers();
    const wrapper = mount(AssetsView);
    await vi.advanceTimersByTimeAsync(50);
    const before = mainListCalls().length;

    await wrapper.get('[data-testid="assets-keyword"]').setValue('order');
    await vi.advanceTimersByTimeAsync(200);
    await wrapper.vm.$nextTick();
    // 停手不足 300ms：不重查
    expect(mainListCalls().length).toBe(before);

    await vi.advanceTimersByTimeAsync(200);
    await wrapper.vm.$nextTick();
    await vi.advanceTimersByTimeAsync(0);
    expect(mainListCalls().length).toBe(before + 1);
    expect(mainListCalls().at(-1)?.[1]).toMatchObject({ query: 'order', limit: 500, offset: 0 });
  });

  it('builds the inventory from the form values and reports completion', async () => {    const wrapper = mount(AssetsView);
    await flushPromises();

    await wrapper.get('[data-testid="assets-target"]').setValue('wx-preview');
    await wrapper.get('[data-testid="assets-cloud-fns"]').setValue('login\n getOrder \n\n');
    await wrapper.get('[data-testid="assets-build"]').trigger('click');
    await flushPromises();
    // 每行一个云函数名，去空白；空行丢弃；目录不传（后端按 appid 解析）
    expect(call).toHaveBeenCalledWith('assets.scan', { appid: 'wx-preview', includeTraffic: true, cloudFns: ['login', 'getOrder'] });
    expect(wrapper.get('[data-testid="assets-build"]').attributes('disabled')).toBeDefined();

    listeners.assets_progress?.({ status: 'working', current: 1, total: 4, message: '扫描代码' });
    await wrapper.vm.$nextTick();
    expect(wrapper.get('[data-testid="assets-progress-text"]').text()).toContain('1/4');
    expect(wrapper.get('[data-testid="assets-progress-text"]').text()).toContain('扫描代码');

    const listCallsBefore = mainListCalls().length;
    listeners.assets_progress?.({ status: 'done', current: 4, total: 4 });
    await flushPromises();
    expect(wrapper.get('[data-testid="assets-build-notice"]').text()).toContain('资产清单构建完成');
    expect(wrapper.get('[data-testid="assets-build"]').attributes('disabled')).toBeUndefined();
    // 完成后刷新当前视图
    expect(mainListCalls().length).toBeGreaterThan(listCallsBefore);
  });

  it('builds with traffic excluded when the checkbox is turned off', async () => {
    const wrapper = mount(AssetsView);
    await flushPromises();
    await wrapper.get('[data-testid="assets-target"]').setValue('wx-preview');
    await wrapper.get('[data-testid="assets-include-traffic"]').setValue(false);
    await wrapper.get('[data-testid="assets-build"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('assets.scan', { appid: 'wx-preview', includeTraffic: false });
  });

  it('announces build completion from the task event when assets_progress is missed', async () => {
    const wrapper = mount(AssetsView);
    await flushPromises();
    await wrapper.get('[data-testid="assets-target"]').setValue('wx-preview');
    await wrapper.get('[data-testid="assets-build"]').trigger('click');
    await flushPromises();
    expect(wrapper.get('[data-testid="assets-build"]').attributes('disabled')).toBeDefined();

    // 兜底：assets_progress 丢了，task 帧按 taskId 宣布完成
    listeners.task?.({ id: 'assets-task-1', phase: 'done' });
    await flushPromises();
    expect(wrapper.get('[data-testid="assets-build-notice"]').text()).toContain('资产清单构建完成');
    expect(wrapper.get('[data-testid="assets-build"]').attributes('disabled')).toBeUndefined();

    // 不认领别人的任务
    await wrapper.get('[data-testid="assets-build"]').trigger('click');
    await flushPromises();
    listeners.task?.({ id: 'someone-else', phase: 'failed', error: 'boom' });
    await flushPromises();
    expect(wrapper.find('[data-testid="assets-build-error"]').exists()).toBe(false);
    expect(wrapper.get('[data-testid="assets-build"]').attributes('disabled')).toBeDefined();
  });

  it('surfaces the scan rejection and list errors visibly', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'assets.scan') return Promise.resolve({ ok: false, error: '没有可用的资产来源：请先反编译或开始抓包' });
      if (method === 'assets.list') return Promise.resolve({ ok: false, error: 'history db locked' });
      if (method === 'code.projects') return Promise.resolve({ projects: [{ appid: 'wx-preview', path: 'C:/out/wx-preview' }] });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(AssetsView);
    await flushPromises();
    // 列表失败带重试
    expect(wrapper.get('[role="alert"]').text()).toContain('history db locked');

    await wrapper.get('[data-testid="assets-target"]').setValue('wx-preview');
    await wrapper.get('[data-testid="assets-build"]').trigger('click');
    await flushPromises();
    expect(wrapper.get('[data-testid="assets-build-error"]').text()).toBe('没有可用的资产来源：请先反编译或开始抓包');
  });

  it('exports in the selected format, honoring filters, and reports cancellation', async () => {
    const wrapper = mount(AssetsView);
    await flushPromises();

    await wrapper.get('[data-testid="assets-chip-api"]').trigger('click');
    await flushPromises();
    await wrapper.get('[data-testid="assets-export-format"]').setValue('nuclei');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('assets.export', { format: 'nuclei', save: true, kind: 'api' });
    expect(wrapper.get('[data-testid="assets-export-notice"]').text()).toContain('C:/out/assets.nuclei');
    // 导出完成后下拉复位到占位项
    expect((wrapper.get('[data-testid="assets-export-format"]').element as HTMLSelectElement).value).toBe('');

    call.mockImplementationOnce(() => Promise.resolve({ ok: false, reason: '用户取消' }));
    await wrapper.get('[data-testid="assets-export-format"]').setValue('csv');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('assets.export', { format: 'csv', save: true, kind: 'api' });
    expect(wrapper.get('[data-testid="assets-export-notice"]').text()).toContain('已取消导出：用户取消');
  });

  it('shows the empty-state guidance before any inventory exists', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'assets.list') return Promise.resolve({ ok: true, total: 0, hosts: [], items: [] });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(AssetsView);
    await flushPromises();

    expect(wrapper.find('[data-testid^="assets-row-"]').exists()).toBe(false);
    const empty = wrapper.get('.empty-state');
    expect(empty.text()).toContain('还没有资产');
    expect(empty.text()).toContain('构建清单');
  });

  it('caps the tree fetch and says so', async () => {
    const makeAsset = (index: number) => ({ id: `x-${index}`, kind: 'api', url: `https://h.test/group${index % 10}`, host: 'h.test', path: `/group${index % 10}`, method: 'GET', sources: [], hits: 1, firstSeen: '', lastSeen: '' });
    call.mockImplementation((method: string, params: { limit?: number; offset?: number }) => {
      if (method === 'assets.list') {
        const { limit, offset = 0 } = params ?? {};
        if (limit === 1) return Promise.resolve({ ok: true, total: 6000, hosts: [], items: [] });
        const size = Math.max(0, Math.min(500, 6000 - offset));
        return Promise.resolve({ ok: true, total: 6000, hosts: [], items: Array.from({ length: size }, (_, i) => makeAsset(offset + i)) });
      }
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(AssetsView);
    await flushPromises();

    expect(wrapper.get('[data-testid="assets-total"]').text()).toContain('6000');
    expect(wrapper.get('[data-testid="assets-tree-truncated"]').text()).toContain('5000');
  });

  it('refetches the tree after filters changed while in the table view', async () => {
    const wrapper = mount(AssetsView);
    await flushPromises();
    await wrapper.get('[data-testid="assets-view-table"]').trigger('click');
    await flushPromises();

    // 表格期间筛选只刷表格；切回树视图必须按新筛选重拉，不能端出旧数据
    await wrapper.get('[data-testid="assets-chip-api"]').trigger('click');
    await flushPromises();
    await wrapper.get('[data-testid="assets-view-tree"]').trigger('click');
    await flushPromises();

    const treeReload = mainListCalls().filter(
      ([, params]) => (params as { limit?: number; kind?: string })?.limit === 500 && (params as { kind?: string })?.kind === 'api',
    );
    expect(treeReload.length).toBeGreaterThanOrEqual(1);
    expect(treeReload.at(-1)?.[1]).toMatchObject({ kind: 'api', limit: 500, offset: 0 });
  });

  it('pages through the inventory with the backend offset in the table view', async () => {
    call.mockImplementation((method: string, params: { limit?: number; offset?: number }) => {
      if (method === 'assets.list') {
        const { limit, offset = 0 } = params ?? {};
        // 探针给 total；主查询只回第一页，保证结构树一次拉完不空转
        if (limit === 1) return Promise.resolve({ ok: true, total: 250, hosts: [], items: [] });
        return Promise.resolve({ ok: true, total: 250, hosts: [], items: offset === 0 ? assetsItems : [] });
      }
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(AssetsView);
    await flushPromises();

    // 空页收尾（第二页空）按「拉完」处理，不得误报截断
    expect(wrapper.find('[data-testid="assets-tree-truncated"]').exists()).toBe(false);

    await wrapper.get('[data-testid="assets-view-table"]').trigger('click');
    await flushPromises();
    expect(wrapper.get('[data-testid="assets-total"]').text()).toContain('共 250 条');
    expect(wrapper.get('[data-testid="assets-page-indicator"]').text()).toContain('第 1 / 3 页');

    await wrapper.get('[data-testid="assets-page-next"]').trigger('click');
    await flushPromises();
    const pageTwos = mainListCalls().filter(([, params]) => (params as { offset?: number })?.offset === 100);
    expect(pageTwos).toHaveLength(1);
    expect(wrapper.get('[data-testid="assets-page-indicator"]').text()).toContain('第 2 / 3 页');
  });
});
