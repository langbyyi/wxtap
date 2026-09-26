import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import TrafficView from './TrafficView.vue';
import PageHeader from '../components/PageHeader.vue';

const { call, listeners, notify, routeQuery } = vi.hoisted(() => ({
  call: vi.fn(),
  listeners: {} as Record<string, (payload: unknown) => void>,
  notify: vi.fn(),
  routeQuery: {} as Record<string, unknown>,
}));
// TrafficView 只读 route.query（?q= 预填关键词），其余路由能力用不到。
vi.mock('vue-router', () => ({ useRoute: () => ({ query: routeQuery }) }));
vi.mock('../api/bridge', () => ({
  backend: {
    call,
    on: vi.fn((event: string, listener: (payload: unknown) => void) => {
      listeners[event] = listener;
      return () => delete listeners[event];
    }),
  },
}));
// 提示是全局 toast 栈（utils/notify 的模块级状态）——断言调用而不是去读那份跨用例残留的状态。
vi.mock('../utils/notify', () => ({ notify }));

type TrafficRecord = {
  id: string;
  capturedAt?: string;
  apiType?: string;
  appId?: string;
  name?: string;
  method?: string;
  url?: string;
  status?: string;
  requestBytes?: number;
  responseBytes?: number;
  durationMs?: number;
};
type Params = Record<string, unknown>;
type Wrapper = ReturnType<typeof mount>;

enableAutoUnmount(afterEach);

function record(id: string, overrides: Partial<TrafficRecord> = {}): TrafficRecord {
  return {
    id,
    capturedAt: '2026-09-21T10:00:00Z',
    apiType: 'wx.request',
    name: id,
    method: 'POST',
    status: 'success',
    requestBytes: 12,
    responseBytes: 20,
    durationMs: 0,
    ...overrides,
  };
}

// 一页样本：total 缺省等于本页条数（一条就是一页），需要多页的用例自己给。
type PageFixture = { items: TrafficRecord[]; total?: number; page?: number; pageSize?: number };
let pageFor: (params: Params) => PageFixture;
let statFor: () => unknown;
let deleteFor: (params: Params) => unknown;
// traffic.getBody 经 Wails 走的是 base64（Go 侧的 []byte → JSON 字符串），测试按同一形状喂。
const bodyPayload = btoa('{"ok":true}');

const listCalls = () => call.mock.calls.filter(([method]) => method === 'traffic.list');
const lastListParams = () => listCalls().at(-1)?.[1] as Params;
const statsCalls = () => call.mock.calls.filter(([method]) => method === 'traffic.stats');
const bodyCalls = () => call.mock.calls.filter(([method]) => method === 'traffic.getBody');
const deleteCalls = () => call.mock.calls.filter(([method]) => method === 'traffic.delete');
const clearCalls = () => call.mock.calls.filter(([method]) => method === 'traffic.clear');
const rows = (wrapper: Wrapper) => wrapper.findAll('button.traffic-row');
const available = () => listeners['traffic:available']?.({ inserted: 1 });
// 后端回显请求里的窗口（钳制的活在后端做，这里照抄请求值），免得每个用例都手写 page/pageSize。
const paged = (params: Params, fixture: PageFixture) => ({
  items: fixture.items,
  total: fixture.total ?? fixture.items.length,
  page: fixture.page ?? Number(params.page ?? 1),
  pageSize: fixture.pageSize ?? Number(params.pageSize ?? 100),
});

describe('TrafficView', () => {
  beforeEach(() => {
    pageFor = () => ({ items: [] });
    statFor = () => ({ records: 0 });
    deleteFor = () => ({ deleted: 1 });
    for (const key of Object.keys(routeQuery)) delete routeQuery[key];
    call.mockReset();
    notify.mockReset();
    call.mockImplementation((method: string, params: Params = {}) => {
      if (method === 'traffic.list') return Promise.resolve(paged(params, pageFor(params)));
      if (method === 'traffic.stats') return Promise.resolve(statFor());
      if (method === 'traffic.getBody') return Promise.resolve(bodyPayload);
      if (method === 'traffic.delete') return Promise.resolve(deleteFor(params));
      return Promise.reject(new Error(`unexpected ${method}`));
    });
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('keeps the page skeleton and never reads a body before a record is selected', async () => {
    pageFor = () => ({ items: [record('rec-1')] });
    const wrapper = mount(TrafficView);
    await flushPromises();

    expect(wrapper.element.tagName).toBe('SECTION');
    expect(wrapper.classes()).toContain('page-workbench');
    expect(wrapper.findComponent(PageHeader).exists()).toBe(true);
    expect(wrapper.get('#traffic-title').text()).toBe('历史记录');

    // 操作并入内容区：页头不承载工具条
    expect(wrapper.find('.page-toolbar').exists()).toBe(false);
    for (const testId of ['traffic-refresh', 'traffic-pager', 'traffic-clear-filters']) {
      expect(wrapper.find(`.panel [data-testid="${testId}"]`).exists()).toBe(true);
    }
    expect(wrapper.get('[data-testid="traffic-refresh"]').text()).toBe('刷新');

    expect(listCalls()).toHaveLength(1);
    expect(listCalls()[0]?.[1]).toEqual(expect.objectContaining({ page: 1, pageSize: 100 }));
    expect(rows(wrapper)).toHaveLength(1);
    expect(wrapper.get('.traffic-detail-panel').text()).toContain('选择一条记录');
    expect(bodyCalls()).toHaveLength(0);
  });

  it('sends appId as a traffic.list parameter instead of filtering the loaded pages locally', async () => {
    pageFor = (params) => (params.appId
      ? { items: [record('rec-1', { appId: 'wxone' })] }
      : { items: [record('rec-1', { appId: 'wxone' }), record('rec-2', { appId: 'wxtwo' })] });
    const wrapper = mount(TrafficView);
    await flushPromises();
    // 留空时不带 appId：空串是「按空 appid 精确匹配」，会把结果筛成空集
    expect(Object.keys(listCalls()[0]?.[1] as Params)).not.toContain('appId');
    expect(rows(wrapper)).toHaveLength(2);

    await wrapper.get('[data-testid="traffic-app-id"]').setValue('wxone');
    await flushPromises();

    expect(listCalls()).toHaveLength(2);
    expect(lastListParams()).toEqual(expect.objectContaining({ appId: 'wxone', page: 1 }));
    // 筛的是后端：列表只显示后端为这个 appId 返回的那一页
    expect(rows(wrapper)).toHaveLength(1);

    // 清空 → 参数整条去掉，列表跟着回到不过滤的结果
    await wrapper.get('[data-testid="traffic-app-id"]').setValue('');
    await flushPromises();
    expect(listCalls()).toHaveLength(3);
    expect(Object.keys(lastListParams() as Params)).not.toContain('appId');
    expect(rows(wrapper)).toHaveLength(2);
  });

  it('offers a per-program picker fed by traffic.appids and filters through the same appId', async () => {
    pageFor = (params) => (params.appId
      ? { items: [record('rec-1', { appId: 'wxone' })] }
      : { items: [record('rec-1', { appId: 'wxone' }), record('rec-2', { appId: 'wxtwo' })] });
    call.mockImplementation((method: string, params: Params = {}) => {
      if (method === 'traffic.appids') return Promise.resolve([{ appid: 'wxone', count: 12, lastSeen: '2026-09-26T00:00:00Z' }]);
      if (method === 'traffic.list') return Promise.resolve(paged(params, pageFor(params)));
      if (method === 'traffic.stats') return Promise.resolve(statFor());
      return Promise.reject(new Error(`unexpected ${method}`));
    });
    const wrapper = mount(TrafficView);
    await flushPromises();

    // 下拉来自 traffic.appids：全部小程序 + 每个小程序（带记录数）
    const picker = wrapper.get('[data-testid="traffic-appid-picker"]');
    expect(picker.findAll('option').map((option) => option.text())).toEqual(['全部小程序', 'wxone（12）']);
    // 选中即精确过滤，与手输 AppID 走同一参数
    await picker.setValue('wxone');
    await flushPromises();
    expect(lastListParams()).toEqual(expect.objectContaining({ appId: 'wxone', page: 1 }));
    expect(rows(wrapper)).toHaveLength(1);

    // 选回「全部小程序」= 清空 appId
    await picker.setValue('');
    await flushPromises();
    expect(Object.keys(lastListParams() as Params)).not.toContain('appId');
    expect(rows(wrapper)).toHaveLength(2);
  });

  it('clears the stale selection and returns to page 1 when the appId changes', async () => {
    pageFor = (params) => (Number(params.page) > 1
      ? { items: [record('rec-9')], total: 300 }
      : { items: [record('rec-1'), record('rec-2')], total: 300 });
    const wrapper = mount(TrafficView);
    await flushPromises();
    await wrapper.get('[data-testid="traffic-page-next"]').trigger('click');
    await flushPromises();
    expect(rows(wrapper)).toHaveLength(1);

    await wrapper.get('[data-testid="traffic-rec-9"]').trigger('click');
    expect(wrapper.get('.traffic-detail-panel').text()).toContain('rec-9');

    await wrapper.get('[data-testid="traffic-app-id"]').setValue('wxone');
    await flushPromises();

    // 换条件等于重来：回第 1 页重取，不是接着上一页往下翻
    expect(lastListParams()).toEqual(expect.objectContaining({ appId: 'wxone', page: 1 }));
    expect(rows(wrapper)).toHaveLength(2);
    expect(wrapper.find('[data-testid="traffic-rec-9"]').exists()).toBe(false);
    // 选中项属于旧条件的结果集：按 reset 语义清空详情，不留下「看不见」的记录
    expect(wrapper.get('.traffic-detail-panel').text()).toContain('选择一条记录');
  });

  it('does not lose an appId change that lands while a request is already in flight', async () => {
    let round = 0;
    let release: (() => void) | undefined;
    call.mockImplementation((method: string, params: Params = {}) => {
      if (method === 'traffic.list') {
        round += 1;
        const page = { items: [record(`rec-${String(params.appId ?? 'all')}`)] };
        // 第二次请求挂在空中，模拟后端稍后才回
        if (round === 2) return new Promise((resolve) => { release = () => resolve(page); });
        return Promise.resolve(page);
      }
      if (method === 'traffic.stats') return Promise.resolve({ records: 0 });
      return Promise.resolve({});
    });
    const wrapper = mount(TrafficView);
    await flushPromises();
    expect(wrapper.find('[data-testid="traffic-rec-all"]').exists()).toBe(true);

    // 第一次改 appId：这次请求还没落地
    await wrapper.get('[data-testid="traffic-app-id"]').setValue('wx');
    expect(listCalls()).toHaveLength(2);

    // 落地之前又改一次：在飞请求的互斥挡掉了这次重取，但变化不能就这么丢掉
    await wrapper.get('[data-testid="traffic-app-id"]').setValue('wxone');
    expect(listCalls()).toHaveLength(2);

    release?.();
    await flushPromises();

    // 在飞请求落地后补取的是**当前** appId 的第一页，而不是被挡掉的那次变化或旧的 'wx'
    expect(lastListParams()).toEqual(expect.objectContaining({ appId: 'wxone', page: 1 }));
    expect(wrapper.find('[data-testid="traffic-rec-wxone"]').exists()).toBe(true);
    expect(wrapper.find('[data-testid="traffic-rec-all"]').exists()).toBe(false);
  });

  it('distinguishes an empty filtered result from an empty store', async () => {
    pageFor = (params) => (params.appId ? { items: [] } : { items: [record('rec-1')] });
    const wrapper = mount(TrafficView);
    await flushPromises();

    await wrapper.get('[data-testid="traffic-app-id"]').setValue('wxnone');
    await flushPromises();

    expect(wrapper.get('.empty-state').text()).toContain('没有匹配的记录');
  });

  it('shows the AppID from the record and only falls back to the rid segment when it is empty', async () => {
    pageFor = () => ({
      items: [
        record('wx.request-wxrid-1758600000002-3', { appId: 'wxexplicit', name: '两个来源都有' }),
        record('wx.request-wxfallback-1758600000001-2', { name: '只有 rid' }),
        record('rec-3', { name: '两处都没有' }),
      ],
    });
    const wrapper = mount(TrafficView);
    await flushPromises();

    // 记录自带 appId 优先于 rid 里的那一段
    const explicit = wrapper.get('[data-testid="traffic-wx.request-wxrid-1758600000002-3"]').text();
    expect(explicit).toContain('wxexplicit');
    expect(explicit).not.toContain('wxrid');
    // 老记录没有 appId → 退回 rid 第二段 <apiType>-<appId>-<ts>-<seq>
    expect(wrapper.get('[data-testid="traffic-wx.request-wxfallback-1758600000001-2"]').text()).toContain('wxfallback');
    // 两处都取不到：不显示，也不把整条 rid 当成 AppID
    expect(wrapper.get('[data-testid="traffic-rec-3"]').text()).not.toContain('rec-3');

    await wrapper.get('[data-testid="traffic-wx.request-wxrid-1758600000002-3"]').trigger('click');
    expect(wrapper.get('.traffic-detail-meta').text()).toContain('wxexplicit');

    await wrapper.get('[data-testid="traffic-rec-3"]').trigger('click');
    expect(wrapper.get('.traffic-detail-meta').text()).not.toContain('rec-3');
  });

  it('reads each body part on demand, once per record, and clears it when another record is picked', async () => {
    pageFor = () => ({ items: [record('rec-1'), record('rec-2')] });
    const wrapper = mount(TrafficView);
    await flushPromises();

    await wrapper.get('[data-testid="traffic-rec-1"]').trigger('click');
    expect(bodyCalls()).toHaveLength(0);
    expect(wrapper.get('.traffic-detail-panel').text()).toContain('正文未加载');

    await wrapper.get('[data-testid="load-request-body"]').trigger('click');
    await flushPromises();
    expect(bodyCalls()).toHaveLength(1);
    expect(bodyCalls()[0]?.[1]).toEqual({ id: 'rec-1', part: 'request' });
    expect(wrapper.get('[data-testid="traffic-body"]').text()).toContain('"ok": true');

    // 同一条记录的同一种正文只读一次：标签页来回切换不重复请求
    await wrapper.get('[data-testid="load-response-body"]').trigger('click');
    await flushPromises();
    await wrapper.get('[data-testid="load-request-body"]').trigger('click');
    await flushPromises();
    expect(bodyCalls()).toHaveLength(2);

    // 切换记录：上一条的正文与加载态一起清掉
    await wrapper.get('[data-testid="traffic-rec-2"]').trigger('click');
    expect(wrapper.find('[data-testid="traffic-body"]').exists()).toBe(false);
    expect(wrapper.get('.traffic-detail-panel').text()).toContain('正文未加载');

    await wrapper.get('[data-testid="load-request-body"]').trigger('click');
    await flushPromises();
    expect(bodyCalls()[2]?.[1]).toEqual({ id: 'rec-2', part: 'request' });
  });

  it('replaces the page instead of accumulating rows, and reports the window', async () => {
    pageFor = (params) => (Number(params.page) > 1
      ? { items: [record('rec-3')], total: 300 }
      : { items: [record('rec-1'), record('rec-2')], total: 300 });
    const wrapper = mount(TrafficView);
    await flushPromises();
    expect(rows(wrapper)).toHaveLength(2);
    expect(wrapper.get('[data-testid="traffic-count"]').text()).toContain('共 300 条');
    expect(wrapper.get('[data-testid="traffic-page-indicator"]').text()).toContain('第 1 / 3 页');

    await wrapper.get('[data-testid="traffic-page-next"]').trigger('click');
    await flushPromises();

    expect(listCalls()[1]?.[1]).toEqual(expect.objectContaining({ page: 2, pageSize: 100 }));
    // 一页就是全部内容：上一页的行必须离开 DOM，不能像游标追加那样越翻越长
    expect(rows(wrapper)).toHaveLength(1);
    expect(wrapper.find('[data-testid="traffic-rec-1"]').exists()).toBe(false);
    expect(wrapper.get('[data-testid="traffic-page-indicator"]').text()).toContain('第 2 / 3 页');
  });

  it('refuses to page past the end and clamps a jump into range', async () => {
    pageFor = (params) => {
      const page = Number(params.page);
      return { items: [record(`rec-${page}`)], total: 300 };
    };
    const wrapper = mount(TrafficView);
    await flushPromises();

    // 末页：下一页 / 末页都禁用，首页 / 上一页可用
    await wrapper.get('[data-testid="traffic-page-last"]').trigger('click');
    await flushPromises();
    expect(lastListParams()).toEqual(expect.objectContaining({ page: 3 }));
    expect(wrapper.get('[data-testid="traffic-page-next"]').attributes('disabled')).toBeDefined();
    expect(wrapper.get('[data-testid="traffic-page-last"]').attributes('disabled')).toBeDefined();
    expect(wrapper.get('[data-testid="traffic-page-prev"]').attributes('disabled')).toBeUndefined();

    // 跳转越界钳回末页；空框（清掉数字后失焦）不发起请求
    await wrapper.get('[data-testid="traffic-page-jump"]').setValue('99');
    await wrapper.get('[data-testid="traffic-page-jump"]').trigger('keyup.enter');
    await flushPromises();
    expect(lastListParams()).toEqual(expect.objectContaining({ page: 3 }));
    const calls = listCalls().length;
    await wrapper.get('[data-testid="traffic-page-jump"]').setValue('');
    await wrapper.get('[data-testid="traffic-page-jump"]').trigger('keyup.enter');
    await flushPromises();
    expect(listCalls()).toHaveLength(calls);
  });

  it('clamps the page back into range when a delete empties the last page', async () => {
    // 第 3 页那一行被删掉后整页就不存在了：后端如实回新的 total，前端必须改取末页
    // 而不是停在一个空页上（空页会被读成「没有记录」，而库里明明还有）。
    let total = 250;
    pageFor = (params) => {
      const page = Number(params.page);
      if (page > 1 && total <= 100) return { items: [], total, page };
      return { items: [record(`rec-${page}`)], total, page };
    };
    const wrapper = mount(TrafficView);
    await flushPromises();
    await wrapper.get('[data-testid="traffic-page-last"]').trigger('click');
    await flushPromises();
    expect(lastListParams()).toEqual(expect.objectContaining({ page: 3 }));

    deleteFor = () => { total = 10; return { deleted: 249 }; };
    await wrapper.get('[data-testid="traffic-rec-3"]').trigger('click');
    await wrapper.get('[data-testid="traffic-delete-one"]').trigger('click');
    await wrapper.get('[data-testid="traffic-confirm-delete"]').trigger('click');
    await flushPromises();

    // 删除后先按原页码重取（后端回空页 + 真总数），再收敛到末页
    expect(listCalls().at(-1)?.[1]).toEqual(expect.objectContaining({ page: 1 }));
    expect(wrapper.get('[data-testid="traffic-page-indicator"]').text()).toContain('第 1 / 1 页');
    expect(rows(wrapper)).toHaveLength(1);
  });

  it('resets the page, the list and the selection together on refresh', async () => {
    pageFor = (params) => (Number(params.page) > 1
      ? { items: [record('rec-3')], total: 300 }
      : { items: [record('rec-1'), record('rec-2')], total: 300 });
    const wrapper = mount(TrafficView);
    await flushPromises();
    await wrapper.get('[data-testid="traffic-page-next"]').trigger('click');
    await flushPromises();
    expect(rows(wrapper)).toHaveLength(1);
    // 翻页时勾选清空（勾选只覆盖本页），这里先勾上再刷新，验证刷新也清
    await wrapper.get('[data-testid="traffic-check-rec-3"]').setValue(true);
    expect(wrapper.get('[data-testid="traffic-selected-count"]').text()).toContain('已选 1 条');
    await wrapper.get('[data-testid="traffic-rec-3"]').trigger('click');
    await wrapper.get('[data-testid="load-request-body"]').trigger('click');
    await flushPromises();
    expect(wrapper.find('[data-testid="traffic-body"]').exists()).toBe(true);

    await wrapper.get('[data-testid="traffic-refresh"]').trigger('click');
    await flushPromises();

    expect(lastListParams()).toEqual(expect.objectContaining({ page: 1 }));
    expect(rows(wrapper)).toHaveLength(2);
    expect(wrapper.find('[data-testid="traffic-rec-3"]').exists()).toBe(false);
    // 选中的那条仍在第一页里，但 reset 必须连选中与正文一起清掉（不是靠「失效选中」兜底）
    expect(wrapper.get('[data-testid="traffic-rec-1"]').classes()).not.toContain('selected');
    expect(wrapper.get('.traffic-detail-panel').text()).toContain('选择一条记录');
    expect(wrapper.find('[data-testid="traffic-body"]').exists()).toBe(false);
    expect(wrapper.get('[data-testid="traffic-selected-count"]').text()).toContain('已选 0 条');
  });

  it('drops a selection that the new server-side filter no longer returns', async () => {
    pageFor = (params) => (params.status === 'fail'
      ? { items: [record('rec-2', { status: 'fail' })] }
      : { items: [record('rec-1'), record('rec-2', { status: 'fail' })] });
    const wrapper = mount(TrafficView);
    await flushPromises();
    await wrapper.get('[data-testid="traffic-rec-1"]').trigger('click');
    expect(wrapper.get('.traffic-detail-panel').text()).toContain('rec-1');

    await wrapper.get('[data-testid="traffic-status"]').setValue('fail');
    await flushPromises();

    expect(listCalls()[1]?.[1]).toEqual(expect.objectContaining({ status: 'fail', page: 1 }));
    expect(rows(wrapper)).toHaveLength(1);
    expect(wrapper.get('.traffic-detail-panel').text()).toContain('选择一条记录');
  });

  it('clears every filter and reloads from the first page', async () => {
    pageFor = () => ({ items: [record('rec-1')] });
    const wrapper = mount(TrafficView);
    await flushPromises();

    await wrapper.get('[data-testid="traffic-keyword"]').setValue('ledger');
    await wrapper.get('[data-testid="traffic-api-type"]').setValue('wx.request');
    await wrapper.get('[data-testid="traffic-app-id"]').setValue('wxone');
    // 状态是下拉框：选中即生效，用的必须是这次选中的值（不是上一个状态）
    await wrapper.get('[data-testid="traffic-status"]').setValue('fail');
    await flushPromises();
    expect(lastListParams()).toEqual(expect.objectContaining({ status: 'fail', apiType: 'wx.request', appId: 'wxone', page: 1 }));

    await wrapper.get('[data-testid="traffic-apply-filters"]').trigger('click');
    await flushPromises();
    expect(lastListParams()).toEqual(expect.objectContaining({ query: 'ledger', apiType: 'wx.request', status: 'fail', appId: 'wxone', page: 1 }));

    await wrapper.get('[data-testid="traffic-clear-filters"]').trigger('click');
    await flushPromises();
    expect(lastListParams()).toEqual(expect.objectContaining({ query: '', apiType: '', status: '', page: 1 }));
    // 清空 appId 是「去掉这个参数」，不是发一个空串
    expect(Object.keys(lastListParams() as Params)).not.toContain('appId');
    expect((wrapper.get('[data-testid="traffic-app-id"]').element as HTMLInputElement).value).toBe('');
    expect(rows(wrapper)).toHaveLength(1);
  });

  it('shows the duration only when it is a real measurement', async () => {
    pageFor = () => ({
      items: [record('rec-1', { durationMs: 1830 }), record('rec-2', { durationMs: 0 }), record('rec-3')],
    });
    const wrapper = mount(TrafficView);
    await flushPromises();

    const durations = wrapper.findAll('.record-duration');
    expect(durations).toHaveLength(1);
    expect(durations[0]?.text()).toBe('1.8s');
    expect(wrapper.get('[data-testid="traffic-rec-1"]').text()).toContain('12 B');
  });

  it('keys the rows by record id, so dropping one row does not rebuild the rest', async () => {
    vi.useFakeTimers();
    let round = 0;
    pageFor = () => {
      round += 1;
      return round === 1
        ? { items: [record('wx.request-wxone-1-1'), record('wx.request-wxtwo-1-2')] }
        : { items: [record('wx.request-wxtwo-1-2')] };
    };
    const wrapper = mount(TrafficView);
    await flushPromises();
    const second = wrapper.get('[data-testid="traffic-wx.request-wxtwo-1-2"]').element;

    // 自动刷新换掉第一页、少了一行：留下来的行必须还是原来那个 DOM 节点（键是记录 id，不是下标）
    available();
    await flushPromises();

    expect(rows(wrapper)).toHaveLength(1);
    expect(wrapper.get('[data-testid="traffic-wx.request-wxtwo-1-2"]').element).toBe(second);
  });

  it('renders the empty state when nothing has been captured', async () => {
    const wrapper = mount(TrafficView);
    await flushPromises();
    expect(wrapper.get('.empty-state').text()).toContain('暂无历史记录');
  });

  it('exports a HAR from the toolbar and reports the saved path, the cancel, and the failure', async () => {
    pageFor = () => ({ items: [record('rec-1')] });
    let harFor: (params: Params) => unknown = () => ({ ok: true, path: 'C:/wxtap/session.har' });
    call.mockImplementation((method: string, params: Params = {}) => {
      if (method === 'traffic.exportHar') return Promise.resolve(harFor(params));
      if (method === 'traffic.list') return Promise.resolve(paged(params, pageFor(params)));
      if (method === 'traffic.stats') return Promise.resolve(statFor());
      return Promise.reject(new Error(`unexpected ${method}`));
    });
    const wrapper = mount(TrafficView);
    await flushPromises();

    const exportButton = () => wrapper.get('[data-testid="traffic-export-har"]');
    await exportButton().trigger('click');
    await flushPromises();
    expect(call.mock.calls.filter(([method]) => method === 'traffic.exportHar')).toHaveLength(1);
    expect(notify).toHaveBeenCalledWith('已导出 C:/wxtap/session.har', 'success');

    // 用户取消是回答不是错误：明说取消，不报 error。
    harFor = () => ({ ok: false, reason: '用户取消' });
    await exportButton().trigger('click');
    await flushPromises();
    expect(notify).toHaveBeenCalledWith('已取消导出', 'info');

    // 后端失败必须可见。
    harFor = () => Promise.reject(new Error('磁盘已满'));
    await exportButton().trigger('click');
    await flushPromises();
    expect(notify).toHaveBeenCalledWith('导出失败：磁盘已满', 'error');
  });

  it('reports a list failure with a retryable error state', async () => {
    let failing = true;
    call.mockImplementation((method: string, params: Params = {}) => {
      if (method === 'traffic.list') {
        return failing ? Promise.reject(new Error('storage down')) : Promise.resolve(pageFor(params));
      }
      if (method === 'traffic.stats') return Promise.resolve({ records: 0 });
      return Promise.resolve({});
    });
    const wrapper = mount(TrafficView);
    await flushPromises();
    expect(wrapper.get('.error-state').text()).toContain('storage down');

    failing = false;
    pageFor = () => ({ items: [record('rec-1')] });
    await wrapper.get('.error-state button').trigger('click');
    await flushPromises();
    expect(rows(wrapper)).toHaveLength(1);
    expect(wrapper.find('.error-state').exists()).toBe(false);
  });

  it('throttles traffic:available refreshes to one per window', async () => {
    vi.useFakeTimers();
    pageFor = () => ({ items: [record('rec-1')] });
    mount(TrafficView);
    await flushPromises();
    const before = listCalls().length;

    available();
    await flushPromises();
    expect(listCalls()).toHaveLength(before + 1);

    // 窗口内的一串事件只换来窗口末尾的一次重取
    available();
    available();
    await flushPromises();
    expect(listCalls()).toHaveLength(before + 1);

    await vi.advanceTimersByTimeAsync(2000);
    expect(listCalls()).toHaveLength(before + 2);
  });

  it('keeps the open detail and its body across an automatic refresh', async () => {
    vi.useFakeTimers();
    pageFor = () => ({ items: [record('rec-1'), record('rec-2')] });
    const wrapper = mount(TrafficView);
    await flushPromises();

    await wrapper.get('[data-testid="traffic-rec-1"]').trigger('click');
    await wrapper.get('[data-testid="load-request-body"]').trigger('click');
    await flushPromises();
    expect(wrapper.find('[data-testid="traffic-body"]').exists()).toBe(true);
    const reads = bodyCalls().length;

    available();
    await flushPromises();

    expect(wrapper.get('[data-testid="traffic-rec-1"]').classes()).toContain('selected');
    expect(wrapper.get('.traffic-detail-panel').text()).toContain('rec-1');
    expect(wrapper.find('[data-testid="traffic-body"]').exists()).toBe(true);
    expect(bodyCalls()).toHaveLength(reads);
    expect(wrapper.find('[data-testid="traffic-notice"]').exists()).toBe(false);
  });

  it('clears a selection the automatic refresh no longer returns, and says so', async () => {
    vi.useFakeTimers();
    let round = 0;
    pageFor = () => {
      round += 1;
      return round === 1
        ? { items: [record('rec-1')] }
        : { items: [record('rec-2')] };
    };
    const wrapper = mount(TrafficView);
    await flushPromises();
    await wrapper.get('[data-testid="traffic-rec-1"]').trigger('click');
    expect(wrapper.get('.traffic-detail-panel').text()).toContain('rec-1');

    available();
    await flushPromises();

    expect(wrapper.get('.traffic-detail-panel').text()).toContain('选择一条记录');
    expect(wrapper.get('[data-testid="traffic-notice"]').text()).toContain('已不在本页');
  });

  it('refreshes only the stats once the user has paged past the first page', async () => {
    vi.useFakeTimers();
    pageFor = (params) => (Number(params.page) > 1
      ? { items: [record('rec-3')], total: 300 }
      : { items: [record('rec-1')], total: 300 });
    const wrapper = mount(TrafficView);
    await flushPromises();
    await wrapper.get('[data-testid="traffic-page-next"]').trigger('click');
    await flushPromises();
    await wrapper.get('[data-testid="traffic-rec-3"]').trigger('click');

    const lists = listCalls().length;
    const stats = statsCalls().length;
    available();
    await flushPromises();

    // 第 N 页只刷统计：offset 窗口会随新记录移动，重取会把用户正在看的这几行换掉，
    // 也会把他弹回页首（roadmap 2.3 明确不许）。
    expect(listCalls()).toHaveLength(lists);
    expect(statsCalls()).toHaveLength(stats + 1);
    expect(rows(wrapper)).toHaveLength(1);
    expect(wrapper.get('[data-testid="traffic-rec-3"]').classes()).toContain('selected');
  });

  it('shows the record count and time range, and tolerates missing overload counters', async () => {
    statFor = () => ({ records: 12, oldestCapturedAt: '2026-09-01T00:00:00Z', newestCapturedAt: '2026-09-21T10:00:00Z' });
    const wrapper = mount(TrafficView);
    await flushPromises();
    expect(wrapper.get('[data-testid="traffic-stats"]').text()).toContain('12');
    expect(wrapper.find('[data-testid="traffic-dropped"]').exists()).toBe(false);
    wrapper.unmount();

    // R12（roadmap 2.4）：冻结名是 droppedRecords / droppedUpdates，两组互补不重叠，直接相加；
    // 字段缺失时按 0 处理，不报错
    statFor = () => ({ records: 12, droppedRecords: 4, droppedUpdates: 1 });
    const second = mount(TrafficView);
    await flushPromises();
    expect(second.get('[data-testid="traffic-dropped"]').text()).toContain('5');
    second.unmount();

    // 旧名（dropped / pageDroppedRecords / pageDroppedUpdates）已经不在契约里：只给旧名
    // 不能显示成丢弃总数 —— 否则改坏了字段名也看不出来
    statFor = () => ({ records: 12, dropped: 4, pageDroppedRecords: 2, pageDroppedUpdates: 1 });
    const third = mount(TrafficView);
    await flushPromises();
    expect(third.find('[data-testid="traffic-dropped"]').exists()).toBe(false);
  });

  it('keeps browsing the list when traffic.stats is not available yet', async () => {
    call.mockImplementation((method: string, params: Params = {}) => {
      if (method === 'traffic.stats') return Promise.reject(new Error('Unsupported backend method: traffic.stats'));
      if (method === 'traffic.list') return Promise.resolve(pageFor(params));
      return Promise.resolve({});
    });
    const wrapper = mount(TrafficView);
    await flushPromises();

    pageFor = () => ({ items: [record('rec-1')] });
    await wrapper.get('[data-testid="traffic-refresh"]').trigger('click');
    await flushPromises();
    expect(rows(wrapper)).toHaveLength(1);
    expect(wrapper.find('.error-state').exists()).toBe(false);
  });

  it('re-attaches the selection to the refreshed record so the detail shows the same settled facts as the row', async () => {
    let round = 0;
    pageFor = () => {
      round += 1;
      // 第一次是第一页的旧事实（调用还在进行中），刷新后这条已经落定
      return round === 1
        ? { items: [record('rec-1', { status: 'pending', durationMs: 0 })] }
        : { items: [record('rec-1', { status: 'success', durationMs: 1830 })] };
    };
    const wrapper = mount(TrafficView);
    await flushPromises();

    await wrapper.get('[data-testid="traffic-rec-1"]').trigger('click');
    expect(wrapper.get('.traffic-detail-meta').text()).toContain('等待中');
    expect(wrapper.get('.traffic-detail-meta').text()).not.toContain('1.8s');

    available();
    await flushPromises();

    // 列表行与详情必须是同一份事实：选中项要重新挂到新页里的新对象上，不能继续持有旧 summary
    const row = wrapper.get('[data-testid="traffic-rec-1"]').text();
    const detail = wrapper.get('.traffic-detail-meta').text();
    for (const text of [row, detail]) {
      expect(text).toContain('成功');
      expect(text).toContain('1.8s');
    }
    // 记录还在结果里，详情不该被关掉
    expect(wrapper.find('[data-testid="traffic-notice"]').exists()).toBe(false);
  });

  it('does not let an unrelated in-flight request swallow an automatic refresh', async () => {
    let release: (() => void) | undefined;
    let round = 0;
    pageFor = () => ({ items: [record('rec-1')] });
    call.mockImplementation((method: string, params: Params = {}) => {
      if (method === 'traffic.list') {
        round += 1;
        const page = pageFor(params);
        // 第二次请求挂在空中，模拟后端稍后才回
        if (round === 2) return new Promise((resolve) => { release = () => resolve(page); });
        return Promise.resolve(page);
      }
      if (method === 'traffic.stats') return Promise.resolve(statFor());
      return Promise.resolve({});
    });
    const wrapper = mount(TrafficView);
    await flushPromises();
    expect(listCalls()).toHaveLength(1);

    // 用户点的刷新还挂在空中
    await wrapper.get('[data-testid="traffic-refresh"]').trigger('click');
    expect(listCalls()).toHaveLength(2);

    // 在飞期间到达的事件：这次刷新不能被无声吞掉
    available();
    await flushPromises();
    expect(listCalls()).toHaveLength(2);

    release?.();
    await flushPromises();

    // 落地后补取一次，否则这串事件落地后就没人再取
    expect(listCalls()).toHaveLength(3);
    expect(lastListParams()).toEqual(expect.objectContaining({ page: 1 }));
    expect(rows(wrapper)).toHaveLength(1);
  });

  it('treats a filter input that was never applied as a new query when paging', async () => {
    pageFor = (params) => (Number(params.page) > 1
      ? { items: [record('rec-next')], total: 300 }
      : { items: [record('rec-1')], total: 300 });
    const wrapper = mount(TrafficView);
    await flushPromises();
    expect(rows(wrapper)).toHaveLength(1);

    // 输入关键词但没回车、没点「应用筛选」：条件已经变了，翻页就是一次新查询
    await wrapper.get('[data-testid="traffic-keyword"]').setValue('ledger');
    await wrapper.get('[data-testid="traffic-page-next"]').trigger('click');
    await flushPromises();

    // 语义：改条件即新查询 —— 回第 1 页按新条件取，不把新条件的结果接到旧列表后面（混排）
    expect(lastListParams()).toEqual(expect.objectContaining({ query: 'ledger', page: 1 }));
    expect(wrapper.find('[data-testid="traffic-rec-next"]').exists()).toBe(false);
    expect(rows(wrapper)).toHaveLength(1);
  });

  it('does not describe an empty list with a filter the user never applied', async () => {
    const wrapper = mount(TrafficView);
    await flushPromises();
    expect(wrapper.get('.empty-state').text()).toContain('暂无历史记录');

    // 只在关键词框里打字：列表内容没变，措辞也不该跟着变
    await wrapper.get('[data-testid="traffic-keyword"]').setValue('ledger');
    await flushPromises();
    expect(wrapper.get('.empty-state').text()).toContain('暂无历史记录');

    await wrapper.get('[data-testid="traffic-apply-filters"]').trigger('click');
    await flushPromises();
    expect(wrapper.get('.empty-state').text()).toContain('没有匹配的记录');
  });

  it('truncates an oversized body instead of parsing and rendering all of it', async () => {
    const limit = 2 * 1024 * 1024;
    const oversized = 'a'.repeat(limit + 5);
    call.mockImplementation((method: string, params: Params = {}) => {
      if (method === 'traffic.list') return Promise.resolve(pageFor(params));
      if (method === 'traffic.stats') return Promise.resolve(statFor());
      if (method === 'traffic.getBody') return Promise.resolve(btoa(oversized));
      return Promise.reject(new Error(`unexpected ${method}`));
    });
    pageFor = () => ({ items: [record('rec-1')] });
    const wrapper = mount(TrafficView);
    await flushPromises();

    await wrapper.get('[data-testid="traffic-rec-1"]').trigger('click');
    await wrapper.get('[data-testid="load-request-body"]').trigger('click');
    await flushPromises();

    // 截断必须说出来：用户看到的不是全文
    expect(wrapper.get('[data-testid="traffic-body-truncated"]').text()).toContain(String(limit + 5));
    const shown = wrapper.get('[data-testid="traffic-body"]').element.textContent ?? '';
    expect(shown).toHaveLength(limit);
    // 截的是**解码后**的正文，不是 base64 串（base64 的 'aaaa…' 以 'YW' 开头）
    expect(shown.startsWith('aaa')).toBe(true);
    expect(wrapper.find('[data-testid="traffic-body-empty"]').exists()).toBe(false);
  }, 20000);

  it('shows a dedicated state for a missing or empty body instead of the literal ""', async () => {
    pageFor = () => ({ items: [record('rec-1')] });
    // null = IPC 上的「没有正文」（旧代码渲染成字面量 `""`）；'' = 真的 0 字节正文
    for (const payload of [null, '']) {
      call.mockImplementation((method: string, params: Params = {}) => {
        if (method === 'traffic.list') return Promise.resolve(pageFor(params));
        if (method === 'traffic.stats') return Promise.resolve(statFor());
        if (method === 'traffic.getBody') return Promise.resolve(payload);
        return Promise.reject(new Error(`unexpected ${method}`));
      });
      const wrapper = mount(TrafficView);
      await flushPromises();

      await wrapper.get('[data-testid="traffic-rec-1"]').trigger('click');
      await wrapper.get('[data-testid="load-request-body"]').trigger('click');
      await flushPromises();

      expect(wrapper.get('[data-testid="traffic-body-empty"]').text()).toBe('（正文为空）');
      expect(wrapper.find('[data-testid="traffic-body"]').exists()).toBe(false);
      expect(wrapper.get('#traffic-body-panel').text()).not.toContain('""');
      wrapper.unmount();
    }
  });

  it('refetches the body after the selected row settles', async () => {
    let settled = false;
    pageFor = () => ({
      items: [record('rec-1', settled ? { status: 'success', durationMs: 1800 } : { status: 'pending' })],
    });
    call.mockImplementation((method: string, params: Params = {}) => {
      if (method === 'traffic.list') return Promise.resolve(pageFor(params));
      if (method === 'traffic.stats') return Promise.resolve({ records: 1 });
      // 落定前后端对这一侧返回 nil（正文还没入库），落定后才有内容
      if (method === 'traffic.getBody') return Promise.resolve(settled ? bodyPayload : null);
      return Promise.resolve({});
    });
    const wrapper = mount(TrafficView);
    await flushPromises();

    await rows(wrapper)[0].trigger('click');
    await wrapper.get('[data-testid="load-response-body"]').trigger('click');
    await flushPromises();
    // pending 时期的「没有正文」不是最终事实
    expect(wrapper.get('[data-testid="traffic-body-empty"]').text()).toBe('（正文为空）');

    settled = true;
    available();
    await flushPromises();

    // 落定会作废那份空正文缓存并重取一次（落定是终态，不会反复重取）
    expect(bodyCalls()).toHaveLength(2);
    expect(wrapper.get('[data-testid="traffic-body"]').text()).toContain('ok');
  });

  it('caps the decode itself and never claims a total it did not read', async () => {
    // 超过解码护栏的正文：展示上限拦不住解码开销，护栏必须在 atob 之前就生效；此时真实总长度
    // 无从得知，提示只能说「不少于」（此前它会拿被截断后的长度冒充总数）。
    const huge = btoa('a'.repeat(5 * 1024 * 1024));
    pageFor = () => ({ items: [record('rec-1')] });
    call.mockImplementation((method: string, params: Params = {}) => {
      if (method === 'traffic.list') return Promise.resolve(pageFor(params));
      if (method === 'traffic.stats') return Promise.resolve({ records: 1 });
      if (method === 'traffic.getBody') return Promise.resolve(huge);
      return Promise.resolve({});
    });
    const wrapper = mount(TrafficView);
    await flushPromises();

    await rows(wrapper)[0].trigger('click');
    await wrapper.get('[data-testid="load-response-body"]').trigger('click');
    await flushPromises();

    const hint = wrapper.get('[data-testid="traffic-body-truncated"]').text();
    expect(hint).toContain('不少于');
    expect(hint).not.toContain('共 ');
  });

  it('loads the body tab that was clicked while another read was in flight', async () => {
    pageFor = () => ({ items: [record('rec-1')] });
    let release: (() => void) | undefined;
    call.mockImplementation((method: string, params: Params = {}) => {
      if (method === 'traffic.list') return Promise.resolve(pageFor(params));
      if (method === 'traffic.stats') return Promise.resolve({ records: 1 });
      if (method === 'traffic.getBody') {
        // 请求正文挂在空中，用户在这个窗口里切到响应正文
        if (params.part === 'request') return new Promise((resolve) => { release = () => resolve(bodyPayload); });
        return Promise.resolve(btoa('{"side":"response"}'));
      }
      return Promise.resolve({});
    });
    const wrapper = mount(TrafficView);
    await flushPromises();
    await rows(wrapper)[0].trigger('click');

    await wrapper.get('[data-testid="load-request-body"]').trigger('click');
    await wrapper.get('[data-testid="load-response-body"]').trigger('click');
    release?.();
    await flushPromises();

    // 被挡下的那一侧要补读：否则界面会一直停在「正文未加载」，用户得再点一次
    expect(wrapper.get('[data-testid="traffic-body"]').text()).toContain('side');
    expect(bodyCalls().map(([, params]) => (params as Params).part)).toEqual(['request', 'response']);
  });


  // 详情面板的高度归正文（与 WxAPI / 云函数同一套规则）：记录身份、正文标签固定在面板里，
  // 只有标了 .detail-body 的那块滚。这页在浏览器预览里拿不到记录数据、滚动没法实测，
  // 这条结构断言就是它的守卫。
  it('keeps the detail head and tabs outside the scrolling body', async () => {
    pageFor = () => ({ items: [record('rec-1')] });
    const wrapper = mount(TrafficView);
    await flushPromises();
    await wrapper.get('[data-testid="traffic-rec-1"]').trigger('click');
    await flushPromises();

    const body = wrapper.get('[data-testid="traffic-detail"] .detail-body');
    expect(body.attributes('id')).toBe('traffic-body-panel');
    expect(body.find('.detail-tabs').exists()).toBe(false);
    expect(wrapper.find('[data-testid="traffic-detail"] > .detail-tabs').exists()).toBe(true);
    expect(wrapper.find('[data-testid="traffic-detail"] > .traffic-detail-meta').exists()).toBe(true);
    // 删除按钮也在滚动区之外：它作用的是面板里这条记录，不该跟着正文滚走
    expect(wrapper.find('[data-testid="traffic-detail"] > .detail-actions [data-testid="traffic-delete-one"]').exists()).toBe(true);
  });

  // 数据多了靠键盘走：↑/↓ 逐行、Home/End 到两端。范围是**本页**：一页就是全部内容。
  it('walks the list with the keyboard within the current page', async () => {
    pageFor = () => ({ items: [record('rec-1'), record('rec-2'), record('rec-3')] });
    const wrapper = mount(TrafficView);
    await flushPromises();

    await wrapper.get('[data-testid="traffic-rec-1"]').trigger('click');
    await wrapper.get('[data-testid="traffic-rec-1"]').trigger('keydown', { key: 'ArrowDown' });
    await flushPromises();
    expect(wrapper.get('[data-testid="traffic-rec-2"]').classes()).toContain('selected');

    await wrapper.get('[data-testid="traffic-rec-2"]').trigger('keydown', { key: 'End' });
    await flushPromises();
    expect(wrapper.get('[data-testid="traffic-rec-3"]').classes()).toContain('selected');

    await wrapper.get('[data-testid="traffic-rec-3"]').trigger('keydown', { key: 'Home' });
    await flushPromises();
    expect(wrapper.get('[data-testid="traffic-rec-1"]').classes()).toContain('selected');
  });

  // 每页条数下推给 traffic.list（后端把 pageSize 钳在 1..1000）：换大小等于换窗口，
  // 当前页的意义变了，必须回第 1 页重取。
  it('pushes the page size down to traffic.list and refetches from the first page', async () => {
    pageFor = (params) => ({ items: [record(`rec-${String(params.page)}`)], total: 300 });
    const wrapper = mount(TrafficView);
    await flushPromises();
    await wrapper.get('[data-testid="traffic-page-next"]').trigger('click');
    await flushPromises();
    expect(lastListParams()).toEqual(expect.objectContaining({ page: 2, pageSize: 100 }));

    await wrapper.get('[data-testid="traffic-page-limit"]').setValue('200');
    await flushPromises();
    expect(lastListParams()).toEqual(expect.objectContaining({ page: 1, pageSize: 200 }));
  });

  // 密度开关（数据多的列表共用）：默认紧凑，关掉后列表不再带紧凑类。
  it('defaults to compact rows and drops the class when the toggle is cleared', async () => {
    const wrapper = mount(TrafficView);
    await flushPromises();
    const list = () => wrapper.get('.record-list');
    expect(wrapper.get('[data-testid="traffic-density"]').element).toHaveProperty('checked', true);
    expect(list().classes()).toContain('is-compact');

    await wrapper.get('[data-testid="traffic-density"]').setValue(false);
    expect(list().classes()).not.toContain('is-compact');
  });

  it('selects the whole page, counts it, and clears the selection when the page changes', async () => {
    pageFor = (params) => (Number(params.page) > 1
      ? { items: [record('rec-3')], total: 300 }
      : { items: [record('rec-1'), record('rec-2')], total: 300 });
    const wrapper = mount(TrafficView);
    await flushPromises();
    expect(wrapper.get('[data-testid="traffic-selected-count"]').text()).toContain('已选 0 条');

    await wrapper.get('[data-testid="traffic-check-rec-1"]').setValue(true);
    expect(wrapper.get('[data-testid="traffic-selected-count"]').text()).toContain('已选 1 条');
    // 半选：全选本页的框自己不勾上
    const selectPage = wrapper.get('[data-testid="traffic-select-page"]').element as HTMLInputElement;
    expect(selectPage.checked).toBe(false);
    expect(selectPage.indeterminate).toBe(true);

    await wrapper.get('[data-testid="traffic-select-page"]').setValue(true);
    expect(wrapper.get('[data-testid="traffic-selected-count"]').text()).toContain('已选 2 条');

    // 换页清空：勾选只覆盖本页，留着会让「删除选中」作用于用户已经看不到的行
    await wrapper.get('[data-testid="traffic-page-next"]').trigger('click');
    await flushPromises();
    expect(wrapper.get('[data-testid="traffic-selected-count"]').text()).toContain('已选 0 条');
    expect((wrapper.get('[data-testid="traffic-check-rec-3"]').element as HTMLInputElement).checked).toBe(false);
  });

  it('deletes the checked rows after a confirmation and reloads the page it was on', async () => {
    pageFor = () => ({ items: [record('rec-1'), record('rec-2'), record('rec-3')], total: 3 });
    deleteFor = (params) => ({ deleted: (params.ids as string[]).length });
    const wrapper = mount(TrafficView);
    await flushPromises();

    await wrapper.get('[data-testid="traffic-check-rec-1"]').setValue(true);
    await wrapper.get('[data-testid="traffic-check-rec-2"]').setValue(true);
    expect(wrapper.get('[data-testid="traffic-delete-selected"]').text()).toContain('删除选中 (2)');

    // 确认之前一条都不删
    await wrapper.get('[data-testid="traffic-delete-selected"]').trigger('click');
    expect(deleteCalls()).toHaveLength(0);
    expect(wrapper.get('.modal-card').text()).toContain('将删除 2 条');

    pageFor = () => ({ items: [record('rec-3')], total: 1 });
    const lists = listCalls().length;
    await wrapper.get('[data-testid="traffic-confirm-delete"]').trigger('click');
    await flushPromises();

    expect(deleteCalls()[0]?.[1]).toEqual({ ids: ['rec-1', 'rec-2'] });
    expect(notify).toHaveBeenCalledWith('已删除 2 条历史记录', 'success');
    // 删除后留在同一页重取（这里是第 1 页），并清空勾选
    expect(listCalls().length).toBeGreaterThan(lists);
    expect(wrapper.get('[data-testid="traffic-selected-count"]').text()).toContain('已选 0 条');
    expect(rows(wrapper)).toHaveLength(1);
  });

  // 「清空」取代了原来的「清理」：没有保留条数 / 保留天数可填，也就没有「框上的数字与实际会删掉
  // 的东西对不上」这类事 —— 用户确认的就是「全部删除」这一件事。
  it('clears every record after a confirmation that names the count', async () => {
    pageFor = () => ({ items: [record('rec-1'), record('rec-2')], total: 2 });
    statFor = () => ({ records: 2 });
    call.mockImplementation((method: string, params: Params = {}) => {
      if (method === 'traffic.list') return Promise.resolve(paged(params, pageFor(params)));
      if (method === 'traffic.stats') return Promise.resolve(statFor());
      if (method === 'traffic.clear') return Promise.resolve({ deleted: 2 });
      return Promise.reject(new Error(`unexpected ${method}`));
    });
    const wrapper = mount(TrafficView);
    await flushPromises();

    await wrapper.get('[data-testid="traffic-clear-open"]').trigger('click');
    // 确认框里写明条数与不可恢复；点确认之前一条都不删
    expect(wrapper.get('.modal-card').text()).toContain('将删除全部 2 条历史记录');
    expect(wrapper.get('.modal-card').text()).toContain('不可恢复');
    expect(clearCalls()).toHaveLength(0);

    pageFor = () => ({ items: [], total: 0 });
    const lists = listCalls().length;
    await wrapper.get('[data-testid="traffic-confirm-clear"]').trigger('click');
    await flushPromises();

    // 清空不带任何参数：没有上限可填，也就没有参数可以填错
    expect(clearCalls()).toHaveLength(1);
    expect(clearCalls()[0]?.[1]).toBeUndefined();
    expect(notify).toHaveBeenCalledWith('已清空 2 条历史记录', 'success');
    expect(listCalls().length).toBeGreaterThan(lists);
    expect(rows(wrapper)).toHaveLength(0);
  });

  it('says the space was not reclaimed when the store was cleared but recycling failed', async () => {
    pageFor = () => ({ items: [record('rec-1')], total: 1 });
    statFor = () => ({ records: 1 });
    call.mockImplementation((method: string, params: Params = {}) => {
      if (method === 'traffic.list') return Promise.resolve(paged(params, pageFor(params)));
      if (method === 'traffic.stats') return Promise.resolve(statFor());
      if (method === 'traffic.clear') return Promise.resolve({ deleted: 1, reclamationFailed: true });
      return Promise.reject(new Error(`unexpected ${method}`));
    });
    const wrapper = mount(TrafficView);
    await flushPromises();

    await wrapper.get('[data-testid="traffic-clear-open"]').trigger('click');
    await wrapper.get('[data-testid="traffic-confirm-clear"]').trigger('click');
    await flushPromises();

    // 记录删了就是删了：成功提示照常，空间没还回去另说一条
    expect(notify).toHaveBeenCalledWith('已清空 1 条历史记录', 'success');
    expect(notify).toHaveBeenCalledWith('记录已清空，但空间回收失败', 'error');
  });

  it('deletes the record open in the detail pane and closes the detail', async () => {
    let round = 0;
    pageFor = () => {
      round += 1;
      return round === 1 ? { items: [record('rec-1'), record('rec-2')], total: 2 } : { items: [record('rec-2')], total: 1 };
    };
    deleteFor = () => ({ deleted: 1 });
    const wrapper = mount(TrafficView);
    await flushPromises();
    await wrapper.get('[data-testid="traffic-rec-1"]').trigger('click');
    await wrapper.get('[data-testid="load-request-body"]').trigger('click');
    await flushPromises();
    expect(wrapper.find('[data-testid="traffic-body"]').exists()).toBe(true);

    await wrapper.get('[data-testid="traffic-delete-one"]').trigger('click');
    expect(wrapper.get('.modal-card').text()).toContain('将删除 1 条');
    await wrapper.get('[data-testid="traffic-confirm-delete"]').trigger('click');
    await flushPromises();

    expect(deleteCalls()[0]?.[1]).toEqual({ ids: ['rec-1'] });
    // 删掉的正是详情里那条：详情与正文一起关掉，不留下一条已经不存在的记录
    expect(wrapper.get('.traffic-detail-panel').text()).toContain('选择一条记录');
    expect(wrapper.find('[data-testid="traffic-body"]').exists()).toBe(false);
  });

  it('keeps the selection across a background refresh but drops the rows that are gone', async () => {
    let round = 0;
    pageFor = () => {
      round += 1;
      // 第二次刷新时 rec-1 已被别的窗口删掉
      return round === 1
        ? { items: [record('rec-1'), record('rec-2')], total: 2 }
        : { items: [record('rec-2')], total: 1 };
    };
    const wrapper = mount(TrafficView);
    await flushPromises();
    await wrapper.get('[data-testid="traffic-check-rec-1"]').setValue(true);
    await wrapper.get('[data-testid="traffic-check-rec-2"]').setValue(true);
    expect(wrapper.get('[data-testid="traffic-selected-count"]').text()).toContain('已选 2 条');

    available();
    await flushPromises();

    // 后台刷新不动勾选（用户勾好的行不该被一次自动刷新悄悄取消），但已经不存在的行要摘掉
    expect(wrapper.get('[data-testid="traffic-selected-count"]').text()).toContain('已选 1 条');
    expect((wrapper.get('[data-testid="traffic-check-rec-2"]').element as HTMLInputElement).checked).toBe(true);
  });

  it('reports a failed delete without losing the selection', async () => {
    pageFor = () => ({ items: [record('rec-1')], total: 1 });
    deleteFor = () => { throw new Error('数据库忙'); };
    const wrapper = mount(TrafficView);
    await flushPromises();

    await wrapper.get('[data-testid="traffic-check-rec-1"]').setValue(true);
    await wrapper.get('[data-testid="traffic-delete-selected"]').trigger('click');
    await wrapper.get('[data-testid="traffic-confirm-delete"]').trigger('click');
    await flushPromises();

    expect(wrapper.get('[data-testid="traffic-delete-error"]').text()).toContain('数据库忙');
    // 勾选留着：重试就是再点一次
    expect(wrapper.get('[data-testid="traffic-selected-count"]').text()).toContain('已选 1 条');
  });

  it('does not claim a reclaim failure the delete never reported', async () => {
    pageFor = () => ({ items: [record('rec-1')], total: 1 });
    deleteFor = () => ({ deleted: 1, reclamationFailed: false });
    const wrapper = mount(TrafficView);
    await flushPromises();

    await wrapper.get('[data-testid="traffic-check-rec-1"]').setValue(true);
    await wrapper.get('[data-testid="traffic-delete-selected"]').trigger('click');
    await wrapper.get('[data-testid="traffic-confirm-delete"]').trigger('click');
    await flushPromises();

    expect(notify).toHaveBeenCalledWith('已删除 1 条历史记录', 'success');
    expect(notify).not.toHaveBeenCalledWith('记录已删除，但空间回收失败', 'error');
  });

  it('prefills the keyword filter from the ?q= route query before the first load', async () => {
    routeQuery.q = 'api.example.com/v1/token';
    pageFor = () => ({ items: [record('r1')] });
    const wrapper = mount(TrafficView);
    await flushPromises();

    // 预填发生在首查之前：第一次 traffic.list 就带 query
    expect(lastListParams()).toMatchObject({ query: 'api.example.com/v1/token' });
    // 输入框里可见，用户能改
    expect((wrapper.get('[data-testid="traffic-keyword"]').element as HTMLInputElement).value).toBe('api.example.com/v1/token');
  });
});
