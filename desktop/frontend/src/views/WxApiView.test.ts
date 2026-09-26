import { createTestingPinia } from '@pinia/testing';
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import WxApiView from './WxApiView.vue';
import PageHeader from '../components/PageHeader.vue';
import { DETAIL_TEXT_LIMIT } from '../utils/format';
import { useEngineStore } from '../stores/engine';

const { call, listeners } = vi.hoisted(() => ({ call: vi.fn(), listeners: {} as Record<string, (payload: unknown) => void> }));
vi.mock('../api/bridge', () => ({
  backend: {
    call,
    on: vi.fn((event: string, listener: (payload: unknown) => void) => {
      listeners[event] = listener;
      return () => delete listeners[event];
    }),
  },
}));

type Pinia = ReturnType<typeof createTestingPinia>;

enableAutoUnmount(afterEach);

function mountWxApiView(pinia: Pinia = createTestingPinia({ createSpy: vi.fn, stubActions: false })) {
  return mount(WxApiView, { global: { plugins: [pinia] } });
}

const pollCalls = () => call.mock.calls.filter(([method]) => method === 'wxapi.poll').length;
const rows = (wrapper: ReturnType<typeof mountWxApiView>) => wrapper.findAll('[data-testid^="wxapi-record-"]');

/** 批量队列 120ms 冲刷一次；fake timers 下用它同时推进微任务。 */
const flushQueue = (wrapper: ReturnType<typeof mountWxApiView>, extraMs = 200) =>
  vi.advanceTimersByTimeAsync(extraMs).then(() => wrapper.vm.$nextTick());

/** 轮询在 2000ms 触发，再等一次队列冲刷才能看到记录进列表。 */
const pollTick = (wrapper: ReturnType<typeof mountWxApiView>) =>
  flushQueue(wrapper, 2000).then(() => flushQueue(wrapper, 200));

describe('WxApiView', () => {
  beforeEach(() => {
    call.mockImplementation((method: string) => {
      if (method === 'wxapi.stats') return Promise.resolve({ running: true, pending: 0, dropped: 0, ack: 0, updateAck: 0 });
      return Promise.resolve({ ok: true });
    });
  });

  afterEach(() => {
    vi.useRealTimers();
    call.mockReset();
  });

  it('uses the shared page header contract', () => {
    expect(mount(WxApiView, { global: { plugins: [createTestingPinia({ createSpy: vi.fn, stubActions: false })] } }).findComponent(PageHeader).exists()).toBe(true);
  });

  it('keeps the capture actions inside the panel and shows the record counters', async () => {
    vi.useFakeTimers();
    const wrapper = mountWxApiView();

    // 页头不再承载操作，三个按钮并入面板
    expect(wrapper.find('.page-toolbar').exists()).toBe(false);
    const panel = wrapper.get('.panel');
    for (const testId of ['wxapi-toggle', 'wxapi-toggle', 'clear-wxapi']) {
      expect(panel.find(`[data-testid="${testId}"]`).exists()).toBe(true);
    }
    expect(wrapper.get('[data-testid="wxapi-capture-status"]').classes()).toContain('off');

    listeners.wxapi_capture?.([
      { rid: 'r1', type: 'wx.request', name: 'ledger', status: 'success' },
      { rid: 'r2', type: 'wx.storage', name: 'setStorage', status: 'success' },
      { rid: 'r3', type: 'wx.auth', name: 'login', status: 'pending' },
    ]);
    await flushQueue(wrapper);

    expect(wrapper.get('[data-testid="wxapi-count"]').text()).toContain('3 / 3');
    expect(wrapper.get('[data-testid="category-all"] .chip-count').text()).toBe('3');
    expect(wrapper.get('[data-testid="category-request"] .chip-count').text()).toBe('1');
    expect(wrapper.get('[data-testid="category-cache"] .chip-count').text()).toBe('1');
    expect(wrapper.get('[data-testid="category-native"] .chip-count').text()).toBe('1');
  });

  it('starts capture, polls, accepts events, filters and clears records', async () => {
    vi.useFakeTimers();
    const wrapper = mountWxApiView();
    await wrapper.get('[data-testid="wxapi-toggle"]').trigger('click');
    expect(call).toHaveBeenCalledWith('wxapi.start');
    await flushPromises();
    listeners.wxapi_capture?.([{ type: 'request', name: 'request', data: { url: '/x' }, status: 'success', timestamp: 'now' }]);
    await flushQueue(wrapper);
    expect(wrapper.text()).toContain('request');
    await wrapper.get('[data-testid="wxapi-search"]').setValue('request');
    await vi.advanceTimersByTimeAsync(2000);
    expect(call).toHaveBeenCalledWith('wxapi.poll', { limit: 500 });
    await wrapper.get('[data-testid="clear-wxapi"]').trigger('click');
    await wrapper.get('[data-testid="confirm-clear"]').trigger('click');
    expect(call).toHaveBeenCalledWith('wxapi.clear');
  });

  it('renders one row when the same rid arrives from the event stream and from the poll', async () => {
    vi.useFakeTimers();
    const record = { rid: 'wx.request-wxone-1758600000000-7', type: 'wx.request', name: 'ledger', status: 'pending', timestamp: '10:00:00' };
    call.mockImplementation((method: string) => {
      if (method === 'wxapi.poll') return Promise.resolve([record]);
      if (method === 'wxapi.stats') return Promise.resolve({ running: true, dropped: 0 });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountWxApiView();
    await wrapper.get('[data-testid="wxapi-toggle"]').trigger('click');
    await flushQueue(wrapper);

    listeners.wxapi_capture?.(record);
    await flushQueue(wrapper);
    expect(wrapper.get('[data-testid="wxapi-count"]').text()).toContain('1 / 1');

    // poll 再投递同一条记录：按 rid 原地替换，不追加第二行
    await pollTick(wrapper);
    expect(wrapper.get('[data-testid="wxapi-count"]').text()).toContain('1 / 1');
    expect(rows(wrapper)).toHaveLength(1);
  });

  it('updates the matching row in place when a wxapi_update frame arrives', async () => {
    vi.useFakeTimers();
    const wrapper = mountWxApiView();
    listeners.wxapi_capture?.([
      { rid: 'r1', type: 'wx.request', name: 'ledger', status: 'pending', timestamp: '10:00:00' },
      { rid: 'r2', type: 'wx.request', name: 'other', status: 'pending', timestamp: '10:00:01' },
    ]);
    await flushQueue(wrapper);

    listeners.wxapi_update?.({ seq: 3, rid: 'r1', status: 'success', result: { statusCode: 200 }, durationMs: 1830 });
    await flushQueue(wrapper);

    expect(rows(wrapper)).toHaveLength(2);
    expect(wrapper.get('[data-testid="wxapi-record-0"]').text()).toContain('成功');
    expect(wrapper.get('[data-testid="wxapi-record-0"]').text()).toContain('1.8s');
    expect(wrapper.get('[data-testid="wxapi-record-1"]').text()).toContain('等待中');

    // 找不到记录的更新帧不是被忽略，而是暂存等记录到达（R3）：这里不该抛错也不该新增行
    listeners.wxapi_update?.({ rid: 'late', status: 'fail', error: 'boom' });
    await flushQueue(wrapper);
    expect(rows(wrapper)).toHaveLength(2);

    // 记录一到，孤儿帧立刻补打，行不会再停在「等待中」
    listeners.wxapi_capture?.({ rid: 'late', type: 'wx.request', name: 'late', status: 'pending' });
    await flushQueue(wrapper);
    expect(rows(wrapper)).toHaveLength(3);
    const late = wrapper.get('[data-testid="wxapi-record-2"]');
    expect(late.text()).toContain('失败');
    expect(late.text()).not.toContain('等待中');
  });

  it('applies every frame of a batched array update payload', async () => {
    vi.useFakeTimers();
    const wrapper = mountWxApiView();
    listeners.wxapi_capture?.([
      { rid: 'r1', type: 'wx.request', name: 'first', status: 'pending', timestamp: '10:00:00' },
      { rid: 'r2', type: 'wx.request', name: 'second', status: 'pending', timestamp: '10:00:01' },
    ]);
    await flushQueue(wrapper);

    // F2 之后更新帧按数组投递（一个 tick 一个数组），数组里可以混着找不到 rid 的帧
    listeners.wxapi_update?.([
      { seq: 1, rid: 'r1', status: 'success', result: { statusCode: 200 }, durationMs: 1200 },
      { seq: 2, rid: 'unknown', status: 'fail', error: 'boom' },
      { seq: 3, rid: 'r2', status: 'fail', error: 'timeout' },
    ]);
    await flushQueue(wrapper);

    // 数组里每一帧都要落定，不能只认第一帧
    expect(rows(wrapper)).toHaveLength(2);
    expect(wrapper.get('[data-testid="wxapi-record-0"]').text()).toContain('成功');
    expect(wrapper.get('[data-testid="wxapi-record-0"]').text()).toContain('1.2s');
    expect(wrapper.get('[data-testid="wxapi-record-1"]').text()).toContain('失败');
    expect(wrapper.get('[data-testid="wxapi-record-1"]').text()).not.toContain('等待中');

    // 数组里找不到 rid 的那一帧按孤儿暂存：不新增行，记录到达时补打
    listeners.wxapi_capture?.({ rid: 'unknown', type: 'wx.request', name: 'late', status: 'pending' });
    await flushQueue(wrapper);
    expect(rows(wrapper)).toHaveLength(3);
    const late = wrapper.get('[data-testid="wxapi-record-2"]');
    expect(late.text()).toContain('失败');
    expect(late.text()).not.toContain('等待中');
  });

  it('settles a slow request from pending to success with its real duration', async () => {
    vi.useFakeTimers();
    const pending = { rid: 'rid-slow', type: 'wx.request', name: 'slowLedger', status: 'pending', timestamp: '10:00:00' };
    call.mockImplementation((method: string) => {
      if (method === 'wxapi.poll') return Promise.resolve([pending]);
      return Promise.resolve({ running: true, dropped: 0 });
    });
    const wrapper = mountWxApiView();
    await wrapper.get('[data-testid="wxapi-toggle"]').trigger('click');
    await pollTick(wrapper);
    expect(wrapper.get('[data-testid="wxapi-record-0"]').text()).toContain('等待中');

    listeners.wxapi_update?.({ rid: 'rid-slow', status: 'success', result: { statusCode: 200 }, durationMs: 1830 });
    await flushQueue(wrapper);

    const row = wrapper.get('[data-testid="wxapi-record-0"]');
    expect(row.text()).toContain('成功');
    expect(row.text()).toContain('1.8s');
    expect(rows(wrapper)).toHaveLength(1);

    // 落定后的返回值能在详情里看到
    await row.trigger('click');
    await wrapper.get('[data-testid="detail-tab-response"]').trigger('click');
    expect(wrapper.get('[data-testid="wxapi-response-view"]').text()).toContain('statusCode');
  });

  it('does not let a stale pending poll record downgrade a settled row', async () => {
    vi.useFakeTimers();
    // 同一条记录的两条投递路径：poll 取走的是 pending 版本，落定帧随后把它刷成终态；
    // 在途的那一轮 poll 响应最后才落地，投递的还是旧 pending 版本。
    const stale = { rid: 'settle-1', type: 'wx.request', name: 'slowLedger', status: 'pending', timestamp: '10:00:00' };
    call.mockImplementation((method: string) => {
      if (method === 'wxapi.poll') return Promise.resolve([stale]);
      if (method === 'wxapi.stats') return Promise.resolve({ running: true, dropped: 0 });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountWxApiView();
    await wrapper.get('[data-testid="wxapi-toggle"]').trigger('click');
    await pollTick(wrapper);
    expect(wrapper.get('[data-testid="wxapi-record-0"]').text()).toContain('等待中');

    listeners.wxapi_update?.({ rid: 'settle-1', status: 'success', result: { statusCode: 200 }, durationMs: 1830 });
    await flushQueue(wrapper);
    expect(wrapper.get('[data-testid="wxapi-record-0"]').text()).toContain('成功');

    // 旧 pending 版本落地：不能覆盖已落定的行，否则它会永久停在「等待中」（落定帧只发一次）
    await pollTick(wrapper);
    const row = wrapper.get('[data-testid="wxapi-record-0"]');
    expect(rows(wrapper)).toHaveLength(1);
    expect(row.text()).toContain('成功');
    expect(row.text()).toContain('1.8s');
    expect(row.text()).not.toContain('等待中');
  });

  it('keeps the capture state and resumes polling when the page is revisited', async () => {
    vi.useFakeTimers();
    const pinia = createTestingPinia({ createSpy: vi.fn, stubActions: false });

    const first = mountWxApiView(pinia);
    await first.get('[data-testid="wxapi-toggle"]').trigger('click');
    await flushPromises();
    expect(first.get('[data-testid="wxapi-toggle"]').attributes('disabled')).toBeUndefined();
    first.unmount();

    const second = mountWxApiView(pinia);
    await flushPromises();
    expect(second.get('[data-testid="wxapi-toggle"]').attributes('disabled')).toBeUndefined();
    await vi.advanceTimersByTimeAsync(2100);
    expect(pollCalls()).toBeGreaterThan(0);
  });

  it('selects a record and replays wx.request as request', async () => {
    vi.useFakeTimers();
    const wrapper = mountWxApiView();
    listeners.wxapi_capture?.({ type: 'wx.request', name: 'wx.request', data: { url: '/x' }, result: { ok: true } });
    await flushQueue(wrapper);
    await wrapper.get('[data-testid="wxapi-record-0"]').trigger('click');
    expect(wrapper.get('[data-testid="wxapi-detail"]').text()).toContain('"url": "/x"');
    await wrapper.get('[data-testid="replay-wxapi"]').trigger('click');
    expect(call).toHaveBeenCalledWith('wxapi.replay', { api_name: 'request', options: { url: '/x' } });
    await flushQueue(wrapper, 20);
    // 重放成功自动切到「重放结果」
    expect(wrapper.get('[data-testid="detail-tab-replay"]').attributes('aria-selected')).toBe('true');
  });

  it('keeps the replay result attached to the record it was run on', async () => {
    vi.useFakeTimers();
    const wrapper = mountWxApiView();
    listeners.wxapi_capture?.([
      { rid: 'r1', type: 'wx.request', name: 'first', data: { id: 1 }, status: 'success' },
      { rid: 'r2', type: 'wx.request', name: 'second', data: { id: 2 }, status: 'success' },
    ]);
    await flushQueue(wrapper);

    await wrapper.get('[data-testid="wxapi-record-0"]').trigger('click');
    await wrapper.get('[data-testid="replay-wxapi"]').trigger('click');
    await flushQueue(wrapper, 20);
    expect(wrapper.get('[data-testid="wxapi-replay-result"]').text()).toContain('ok');

    await wrapper.get('[data-testid="wxapi-record-1"]').trigger('click');
    await wrapper.vm.$nextTick();
    expect(wrapper.find('[data-testid="wxapi-replay-result"]').exists()).toBe(false);
  });

  it('clears a selection that no longer matches the filter', async () => {
    vi.useFakeTimers();
    const wrapper = mountWxApiView();
    listeners.wxapi_capture?.([
      { rid: 'r1', type: 'wx.auth', name: 'login', data: { id: 1 } },
      { rid: 'r2', type: 'wx.request', name: 'ledger', data: { id: 2 } },
    ]);
    await flushQueue(wrapper);
    await wrapper.get('[data-testid="wxapi-record-0"]').trigger('click');
    expect(wrapper.get('[data-testid="wxapi-detail"]').text()).toContain('"id": 1');
    await wrapper.get('[data-testid="category-request"]').trigger('click');
    await wrapper.vm.$nextTick();
    expect(wrapper.get('[data-testid="wxapi-detail"]').text()).toContain('未选择记录');
    expect(rows(wrapper)).toHaveLength(1);
  });

  // 详情栏也是原文：掩码过的报文不是报文。
  it('shows the raw request in the detail view, credentials included', async () => {
    const wrapper = mountWxApiView();
    listeners.wxapi_capture?.({ type: 'request', name: 'wx.request', data: { headers: { Authorization: 'Bearer abcdefghijkl' }, url: '/x' }, result: { ok: true } });
    await vi.waitFor(() => expect(wrapper.find('[data-testid="wxapi-record-0"]').exists()).toBe(true));
    await wrapper.get('[data-testid="wxapi-record-0"]').trigger('click');
    expect(wrapper.get('[data-testid="wxapi-request-view"]').text()).toContain('Bearer abcdefghijkl');
  });

  // 超过展示上限的正文只渲染前缀并明说截断（与历史页同口径）；普通记录不亮提示。
  it('truncates an oversized response in the detail pane and says so', async () => {
    vi.useFakeTimers();
    const wrapper = mountWxApiView();
    listeners.wxapi_capture?.([
      { rid: 'big', type: 'wx.request', name: 'big', status: 'success', data: { id: 1 }, result: { body: 'x'.repeat(DETAIL_TEXT_LIMIT + 16) } },
      { rid: 'small', type: 'wx.request', name: 'small', status: 'success', data: { id: 2 }, result: { ok: true } },
    ]);
    await flushQueue(wrapper);

    await wrapper.get('[data-testid="wxapi-record-0"]').trigger('click');
    await wrapper.get('[data-testid="detail-tab-response"]').trigger('click');
    expect(wrapper.get('[data-testid="wxapi-response-view"]').text()).toHaveLength(DETAIL_TEXT_LIMIT);
    expect(wrapper.get('[data-testid="wxapi-detail-truncated"]').text()).toContain('已截断显示前');

    // 换一条普通记录：明示消失
    await wrapper.get('[data-testid="wxapi-record-1"]').trigger('click');
    await wrapper.vm.$nextTick();
    expect(wrapper.find('[data-testid="wxapi-detail-truncated"]').exists()).toBe(false);
  });

  it('warns instead of silently dropping the oldest captured records at the limit', async () => {
    vi.useFakeTimers();
    const wrapper = mountWxApiView();
    // 单波 1200 条不触顶，三波之后越过 ITEM_LIMIT：裁剪最旧的记录并给出提示
    for (const wave of ['a', 'b', 'c']) {
      listeners.wxapi_capture?.(Array.from({ length: 1200 }, (_, index) => ({ rid: `${wave}-${index}`, type: 'request', name: `${wave}-${index}`, data: {} })));
      await flushQueue(wrapper, 400);
    }
    expect(wrapper.get('[data-testid="wxapi-count"]').text()).toContain('/ 2000');
    expect(wrapper.get('.limit-callout').text()).toContain('最早的记录已被丢弃');
  }, 20000);

  it('filters captures with the category chips and clears the filter from 全部', async () => {
    vi.useFakeTimers();
    const wrapper = mountWxApiView();
    listeners.wxapi_capture?.([
      { rid: 'r1', type: 'wx.request', name: 'ledger', data: {} },
      { rid: 'r2', type: 'wx.storage', name: 'setStorage', data: {} },
    ]);
    await flushQueue(wrapper);

    expect(wrapper.get('[data-testid="category-all"]').attributes('aria-pressed')).toBe('true');
    await wrapper.get('[data-testid="category-cache"]').trigger('click');
    expect(wrapper.get('[data-testid="category-cache"]').attributes('aria-pressed')).toBe('true');
    expect(wrapper.get('[data-testid="category-all"]').attributes('aria-pressed')).toBe('false');
    expect(wrapper.text()).toContain('setStorage');
    expect(wrapper.find('[data-testid="wxapi-record-1"]').exists()).toBe(false);
    expect(wrapper.get('[data-testid="wxapi-count"]').text()).toContain('1 / 2');

    // 多选：再点一个分类就是两个分类的并集
    await wrapper.get('[data-testid="category-request"]').trigger('click');
    expect(wrapper.get('[data-testid="wxapi-count"]').text()).toContain('2 / 2');

    await wrapper.get('[data-testid="category-all"]').trigger('click');
    expect(wrapper.get('[data-testid="category-cache"]').attributes('aria-pressed')).toBe('false');
    expect(wrapper.get('[data-testid="category-all"]').attributes('aria-pressed')).toBe('true');
  });

  it('groups wx.storage and the cache type under 缓存', async () => {
    vi.useFakeTimers();
    const wrapper = mountWxApiView();
    listeners.wxapi_capture?.([
      { rid: 'r1', type: 'wx.storage', name: 'getStorageSync' },
      { rid: 'r2', type: 'cache', name: 'oldCache' },
      { rid: 'r3', type: 'wx.auth', name: 'login' },
    ]);
    await flushQueue(wrapper);

    expect(wrapper.get('[data-testid="category-cache"] .chip-count').text()).toBe('2');
    expect(wrapper.get('[data-testid="category-native"] .chip-count').text()).toBe('1');
    await wrapper.get('[data-testid="category-cache"]').trigger('click');
    expect(rows(wrapper)).toHaveLength(2);
    expect(wrapper.text()).toContain('oldCache');
  });

  it('moves the record selection with the arrow keys', async () => {
    vi.useFakeTimers();
    const wrapper = mountWxApiView();
    listeners.wxapi_capture?.([
      { type: 'request', name: 'first', data: { id: 1 } },
      { type: 'request', name: 'second', data: { id: 2 } },
    ]);
    await flushQueue(wrapper);
    await wrapper.get('[data-testid="wxapi-record-0"]').trigger('keydown', { key: 'ArrowDown' });
    expect(wrapper.get('[data-testid="wxapi-detail"]').text()).toContain('"id": 1');
    await wrapper.get('[data-testid="wxapi-record-0"]').trigger('keydown', { key: 'ArrowDown' });
    expect(wrapper.get('[data-testid="wxapi-detail"]').text()).toContain('"id": 2');
    await wrapper.get('[data-testid="wxapi-record-1"]').trigger('keydown', { key: 'ArrowUp' });
    expect(wrapper.get('[data-testid="wxapi-detail"]').text()).toContain('"id": 1');
  });

  it('scrolls the list to the newest row when 跟随最新 is on', async () => {
    vi.useFakeTimers();
    const wrapper = mountWxApiView();
    const list = wrapper.get('.record-list').element as HTMLUListElement;
    let scrollTop = 0;
    Object.defineProperty(list, 'scrollHeight', { configurable: true, value: 400 });
    Object.defineProperty(list, 'scrollTop', { configurable: true, get: () => scrollTop, set: (value: number) => { scrollTop = value; } });

    listeners.wxapi_capture?.({ rid: 'r1', type: 'wx.request', name: 'first' });
    await flushQueue(wrapper);
    expect(scrollTop).toBe(0);

    await wrapper.get('[data-testid="follow-tail"]').setValue(true);
    listeners.wxapi_capture?.({ rid: 'r2', type: 'wx.request', name: 'second' });
    await flushQueue(wrapper);
    expect(scrollTop).toBe(400);
  });

  it('stops capture and reports backend errors', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'wxapi.start') return Promise.resolve({ ok: true });
      if (method === 'wxapi.stats') return Promise.resolve({ running: true });
      if (method === 'wxapi.stop') return Promise.reject(new Error('not attached'));
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountWxApiView();
    await wrapper.get('[data-testid="wxapi-toggle"]').trigger('click');
    await flushPromises();
    await wrapper.get('[data-testid="wxapi-toggle"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('wxapi.stop');
    expect(wrapper.get('[role="alert"]').text()).toBe('not attached');
  });

  it('resets the capture state when the backend reports it is not capturing', async () => {
    vi.useFakeTimers();
    call.mockImplementation((method: string) => {
      if (method === 'wxapi.start') return Promise.resolve({ ok: true });
      if (method === 'wxapi.stats') return Promise.resolve({ running: false, pending: 0, dropped: 0 });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountWxApiView();
    await wrapper.get('[data-testid="wxapi-toggle"]').trigger('click');
    await flushPromises();

    expect(wrapper.get('[data-testid="wxapi-capture-status"]').classes()).toContain('off');
    expect(wrapper.get('[data-testid="wxapi-toggle"]').attributes('disabled')).toBeUndefined();
    expect(wrapper.get('[role="alert"]').text()).toContain('未进入捕获状态');
    await vi.advanceTimersByTimeAsync(4000);
    expect(pollCalls()).toBe(0);
  });

  it('resets the capture state and shows the reason when starting fails', async () => {
    vi.useFakeTimers();
    call.mockImplementation((method: string) => (
      method === 'wxapi.start' ? Promise.reject(new Error('hook install failed')) : Promise.resolve({ running: false })
    ));
    const wrapper = mountWxApiView();
    await wrapper.get('[data-testid="wxapi-toggle"]').trigger('click');
    await flushPromises();

    expect(wrapper.get('[data-testid="wxapi-toggle"]').attributes('disabled')).toBeUndefined();
    expect(wrapper.get('[data-testid="wxapi-capture-status"]').classes()).toContain('off');
    expect(wrapper.get('[role="alert"]').text()).toContain('hook install failed');
    await vi.advanceTimersByTimeAsync(4000);
    expect(pollCalls()).toBe(0);
  });

  it('shows the shell and the page-side drop counters while capturing', async () => {
    vi.useFakeTimers();
    call.mockImplementation((method: string) => {
      // shell 侧 pending 溢出 12 条（未进实时列表，历史记录里仍有）+ 页面侧两条缓冲各自溢出 3 / 5 条
      // （永久丢失）：两个读数分开展示，不能合成一个「已丢弃」
      if (method === 'wxapi.stats') return Promise.resolve({ running: true, pending: 0, dropped: 12, pageDroppedRecords: 3, pageDroppedUpdates: 5 });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountWxApiView();
    expect(wrapper.find('[data-testid="wxapi-dropped"]').exists()).toBe(false);
    await wrapper.get('[data-testid="wxapi-toggle"]').trigger('click');
    await flushQueue(wrapper);
    expect(wrapper.get('[data-testid="wxapi-dropped"]').text()).toContain('已丢弃 8 条');
    expect(wrapper.get('[data-testid="wxapi-unlisted"]').text()).toContain('未进列表 12 条');
  });

  it('shows the page-side drops even when the shell reports none', async () => {
    vi.useFakeTimers();
    call.mockImplementation((method: string) => {
      if (method === 'wxapi.stats') return Promise.resolve({ running: true, pending: 0, dropped: 0, pageDroppedRecords: 7, pageDroppedUpdates: 5 });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountWxApiView();
    await wrapper.get('[data-testid="wxapi-toggle"]').trigger('click');
    await flushQueue(wrapper);

    expect(wrapper.get('[data-testid="wxapi-dropped"]').text()).toContain('已丢弃 12 条');
    expect(wrapper.find('[data-testid="wxapi-unlisted"]').exists()).toBe(false);
  });

  it('warns about storage only when the backend explicitly reports it unavailable', async () => {
    vi.useFakeTimers();
    const wrapper = mountWxApiView();
    // 缺省（旧后端没有 storageAvailable 字段）按可用处理，不亮警示
    expect(wrapper.find('[data-testid="wxapi-storage-warning"]').exists()).toBe(false);

    call.mockImplementation((method: string) => {
      if (method === 'wxapi.stats') return Promise.resolve({ running: true, pending: 0, dropped: 0, storageAvailable: false });
      return Promise.resolve({ ok: true });
    });
    await wrapper.get('[data-testid="wxapi-toggle"]').trigger('click');
    await flushQueue(wrapper);
    expect(wrapper.get('[data-testid="wxapi-storage-warning"]').text()).toContain('存储不可用');

    // 明确的 true 同样按可用处理：警示消失
    call.mockImplementation((method: string) => {
      if (method === 'wxapi.stats') return Promise.resolve({ running: true, pending: 0, dropped: 0, storageAvailable: true });
      return Promise.resolve({ ok: true });
    });
    await pollTick(wrapper);
    expect(wrapper.find('[data-testid="wxapi-storage-warning"]').exists()).toBe(false);
  });

  it('starts and stops polling together with the store capture flag', async () => {
    vi.useFakeTimers();
    mountWxApiView();
    const store = useEngineStore();

    store.wxapiCapturing = true;
    await vi.advanceTimersByTimeAsync(2100);
    expect(pollCalls()).toBeGreaterThan(0);

    // 引擎掉线会把 wxapiCapturing 置回 false：轮询必须跟着停，不能每 2s 重刷错误
    store.wxapiCapturing = false;
    await vi.advanceTimersByTimeAsync(100);
    const settled = pollCalls();
    await vi.advanceTimersByTimeAsync(6000);
    expect(pollCalls()).toBe(settled);
  });

  it('does not recreate polling after unmount while capture is starting', async () => {
    vi.useFakeTimers();
    let resolveStart: (() => void) | undefined;
    call.mockImplementation((method: string) => {
      if (method === 'wxapi.start') return new Promise<void>((resolve) => { resolveStart = resolve; });
      return Promise.resolve({ ok: true });
    });
    const setIntervalSpy = vi.spyOn(window, 'setInterval');
    const wrapper = mountWxApiView();
    await wrapper.get('[data-testid="wxapi-toggle"]').trigger('click');
    wrapper.unmount();
    resolveStart?.();
    await flushPromises();

    expect(setIntervalSpy).not.toHaveBeenCalled();
  });

  it('does not overlap WxAPI polling while a poll is pending', async () => {
    vi.useFakeTimers();
    let resolvePoll: ((value: unknown) => void) | undefined;
    call.mockImplementation((method: string) => {
      if (method === 'wxapi.start') return Promise.resolve({ ok: true });
      if (method === 'wxapi.poll') return new Promise((resolve) => { resolvePoll = resolve; });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountWxApiView();
    await wrapper.get('[data-testid="wxapi-toggle"]').trigger('click');
    await flushPromises();
    await vi.advanceTimersByTimeAsync(2000);
    await vi.advanceTimersByTimeAsync(4000);
    expect(pollCalls()).toBe(1);

    resolvePoll?.([]);
    await flushPromises();
  });

  it('discards a poll response that was in flight when the records were cleared', async () => {
    vi.useFakeTimers();
    const pollResolvers: Array<(value: unknown) => void> = [];
    let pollRound = 0;
    call.mockImplementation((method: string) => {
      if (method === 'wxapi.start') return Promise.resolve({ ok: true });
      if (method === 'wxapi.stats') return Promise.resolve({ running: true, pending: 0, dropped: 0 });
      if (method === 'wxapi.poll') {
        pollRound += 1;
        // 第一轮故意悬着：用户点清空时它正好在飞
        if (pollRound === 1) return new Promise((resolve) => { pollResolvers.push(resolve); });
        return Promise.resolve([{ rid: 'fresh-1', type: 'wx.request', name: 'fresh-1', status: 'success' }]);
      }
      return Promise.resolve({ ok: true });
    });
    const statsCalls = () => call.mock.calls.filter(([method]) => method === 'wxapi.stats').length;
    const wrapper = mountWxApiView();
    await wrapper.get('[data-testid="wxapi-toggle"]').trigger('click');
    await flushPromises();

    listeners.wxapi_capture?.({ rid: 'kept-1', type: 'wx.request', name: 'kept-1', status: 'success' });
    await flushQueue(wrapper);
    expect(wrapper.get('[data-testid="wxapi-count"]').text()).toContain('1 / 1');

    // 2s 的轮询已经发起，响应也还没回来 —— 它就是在飞的这一页
    await vi.advanceTimersByTimeAsync(2000);
    await flushPromises();
    expect(pollCalls()).toBe(1);
    const statsBeforeClear = statsCalls();

    await wrapper.get('[data-testid="clear-wxapi"]').trigger('click');
    await wrapper.get('[data-testid="confirm-clear"]').trigger('click');
    await flushPromises();
    expect(wrapper.get('[data-testid="wxapi-count"]').text()).toContain('0 / 0');

    // 在飞的响应现在才带着「清空之前」的记录到达：整批丢弃，既不进列表也不刷统计
    pollResolvers.shift()?.([
      { rid: 'stale-1', type: 'wx.request', name: 'stale-1', status: 'success' },
      { rid: 'stale-2', type: 'wx.request', name: 'stale-2', status: 'success' },
    ]);
    await flushPromises();
    await flushQueue(wrapper);
    expect(rows(wrapper)).toHaveLength(0);
    expect(wrapper.get('[data-testid="wxapi-count"]').text()).toContain('0 / 0');
    expect(statsCalls()).toBe(statsBeforeClear);

    // 守卫只作用于「清空之前发起」的那一页：清空之后发起的轮询照常落地
    await pollTick(wrapper);
    expect(wrapper.get('[data-testid="wxapi-count"]').text()).toContain('1 / 1');
    expect(wrapper.get('[data-testid="wxapi-record-0"]').text()).toContain('fresh-1');
  });

  it('starts no new poll while a clear is in flight', async () => {
    vi.useFakeTimers();
    let resolveClear: ((value: unknown) => void) | undefined;
    call.mockImplementation((method: string) => {
      if (method === 'wxapi.start') return Promise.resolve({ ok: true });
      if (method === 'wxapi.stats') return Promise.resolve({ running: true, pending: 0, dropped: 0 });
      if (method === 'wxapi.poll') return Promise.resolve([{ rid: 'after-1', type: 'wx.request', name: 'after-1', status: 'success' }]);
      if (method === 'wxapi.clear') return new Promise((resolve) => { resolveClear = resolve; });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountWxApiView();
    await wrapper.get('[data-testid="wxapi-toggle"]').trigger('click');
    await flushPromises();
    await pollTick(wrapper);
    expect(pollCalls()).toBe(1);
    expect(wrapper.get('[data-testid="wxapi-count"]').text()).toContain('1 / 1');

    // 清空的往返还没回来时定时器到点：这一轮不能发起。否则它带着旧代次却取到清空之后写入
    // 的记录，响应落地时被 epoch 守卫整批丢掉 —— 而那批记录已被 shell 从 pending 取走。
    await wrapper.get('[data-testid="clear-wxapi"]').trigger('click');
    await wrapper.get('[data-testid="confirm-clear"]').trigger('click');
    await vi.advanceTimersByTimeAsync(4000);
    expect(pollCalls()).toBe(1);

    resolveClear?.({ ok: true });
    await flushPromises();
    expect(wrapper.get('[data-testid="wxapi-count"]').text()).toContain('0 / 0');

    // 清空完成后照旧起表：这一轮取到的记录不会再被作废
    await pollTick(wrapper);
    expect(pollCalls()).toBe(2);
    expect(wrapper.get('[data-testid="wxapi-count"]').text()).toContain('1 / 1');
    expect(wrapper.get('[data-testid="wxapi-record-0"]').text()).toContain('after-1');
  });

  it('cancels polling and event subscription when unmounted', async () => {
    vi.useFakeTimers();
    const wrapper = mountWxApiView();
    await wrapper.get('[data-testid="wxapi-toggle"]').trigger('click');
    await flushPromises();
    wrapper.unmount();
    call.mockClear();
    await vi.advanceTimersByTimeAsync(4000);
    expect(pollCalls()).toBe(0);
  });

  it('keeps polling inside the same tick while the backend still has a backlog', async () => {
    vi.useFakeTimers();
    let page = 0;
    call.mockImplementation((method: string) => {
      if (method === 'wxapi.start') return Promise.resolve({ ok: true });
      if (method === 'wxapi.stats') return Promise.resolve({ running: true, dropped: 0 });
      if (method === 'wxapi.poll') {
        page += 1;
        // 前两页取满 500 条说明还有积压，第三页不满 → 补齐结束
        if (page <= 2) {
          return Promise.resolve(Array.from({ length: 500 }, (_, index) => ({ rid: `p${page}-${index}`, type: 'wx.request', name: `p${page}-${index}` })));
        }
        return Promise.resolve([{ rid: 'p3-0', type: 'wx.request', name: 'p3-0' }]);
      }
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountWxApiView();
    await wrapper.get('[data-testid="wxapi-toggle"]').trigger('click');
    await flushPromises();
    await pollTick(wrapper);

    expect(pollCalls()).toBe(3);
    expect(wrapper.get('[data-testid="wxapi-count"]').text()).toContain('1001 / 1001');
  });

  it('caps the backlog top-up at ten polls per tick and shows what the burst dropped', async () => {
    vi.useFakeTimers();
    let page = 0;
    call.mockImplementation((method: string) => {
      if (method === 'wxapi.start') return Promise.resolve({ ok: true });
      if (method === 'wxapi.stats') return Promise.resolve({ running: true, dropped: 0 });
      if (method === 'wxapi.poll') {
        page += 1;
        return Promise.resolve(Array.from({ length: 500 }, (_, index) => ({ rid: `page${page}-${index}`, type: 'wx.request', name: `page${page}-${index}` })));
      }
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountWxApiView();
    await wrapper.get('[data-testid="wxapi-toggle"]').trigger('click');
    await flushPromises();
    await pollTick(wrapper);

    // 一个 tick 最多补 10 轮 = 5000 条，超出的部分不能静默消失
    expect(pollCalls()).toBe(10);
    expect(wrapper.get('[data-testid="wxapi-count"]').text()).toContain('2000 / 2000');
    expect(wrapper.get('.limit-callout').text()).toContain('最早的记录已被丢弃');
  }, 20000);

  it('applies an oversized burst in frames and keeps every record in order', async () => {
    vi.useFakeTimers();
    const wrapper = mountWxApiView();
    listeners.wxapi_capture?.(Array.from({ length: 1000 }, (_, index) => ({ rid: `burst-${index}`, type: 'wx.request', name: `burst-${index}` })));
    await flushQueue(wrapper, 1000);

    expect(wrapper.get('[data-testid="wxapi-count"]').text()).toContain('1000 / 1000');
    expect(wrapper.get('[data-testid="wxapi-record-0"]').text()).toContain('burst-0');
    expect(wrapper.get('[data-testid="wxapi-record-999"]').text()).toContain('burst-999');
  }, 20000);

  it('keeps the newest records of one huge wave and warns about the dropped ones', async () => {
    vi.useFakeTimers();
    const wrapper = mountWxApiView();
    listeners.wxapi_capture?.(Array.from({ length: 2500 }, (_, index) => ({ rid: `bulk-${index}`, type: 'wx.request', name: `bulk-${index}` })));
    await flushQueue(wrapper, 2000);

    expect(wrapper.get('[data-testid="wxapi-count"]').text()).toContain('2000 / 2000');
    expect(wrapper.get('.limit-callout').text()).toContain('最早的记录已被丢弃');
    // 留下的是最新的一批：第一批 500 条被丢掉
    expect(wrapper.get('[data-testid="wxapi-record-0"]').text()).toContain('bulk-500');
    expect(wrapper.get('[data-testid="wxapi-record-1999"]').text()).toContain('bulk-2499');
  }, 20000);

  // 无 rid 记录也拿稳定键（对象身份换来的序号）：裁剪最旧记录时键不再全体移位，存活行的
  // DOM 节点被保留，而不是按 index 键整排重写。
  it('keeps the DOM node of a surviving rid-less row when the oldest record is trimmed', async () => {
    vi.useFakeTimers();
    const wrapper = mountWxApiView();
    listeners.wxapi_capture?.(Array.from({ length: 2000 }, (_, index) => ({ type: 'request', name: `anon-${index}`, data: {} })));
    await flushQueue(wrapper, 2000);
    expect(wrapper.get('[data-testid="wxapi-count"]').text()).toContain('2000 / 2000');
    const survivor = wrapper.get('[data-testid="wxapi-record-1"]').element;

    listeners.wxapi_capture?.({ type: 'request', name: 'anon-new', data: {} });
    await flushQueue(wrapper, 400);

    // 裁掉 anon-0 后 anon-1 顶到下标 0：还是原来那个节点
    expect(wrapper.get('[data-testid="wxapi-record-0"]').element).toBe(survivor);
    expect(wrapper.get('[data-testid="wxapi-record-0"]').text()).toContain('anon-1');
    expect(wrapper.get('[data-testid="wxapi-record-1999"]').text()).toContain('anon-new');
  }, 20000);

  it('holds a whole tick of poll pages plus an event batch without dropping at the queue bound', async () => {
    vi.useFakeTimers();
    let page = 0;
    call.mockImplementation((method: string) => {
      if (method === 'wxapi.start') return Promise.resolve({ ok: true });
      if (method === 'wxapi.stats') return Promise.resolve({ running: true, dropped: 0 });
      if (method === 'wxapi.poll') {
        page += 1;
        return Promise.resolve(Array.from({ length: 500 }, (_, index) => ({ rid: `page${page}-${index}`, type: 'wx.request', name: `page${page}-${index}` })));
      }
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountWxApiView();
    await wrapper.get('[data-testid="wxapi-toggle"]').trigger('click');
    await flushPromises();

    // 同一个冲刷窗口里的两条投递路径叠加：poll 10 轮 × 500 = 5000（响应在 120ms 的冲刷
    // 之前到齐），再叠加事件批 2000 —— 正好是推导出的单 tick 上界 7000。
    await vi.advanceTimersByTimeAsync(2000);
    await flushPromises();
    expect(pollCalls()).toBe(10);
    listeners.wxapi_capture?.(Array.from({ length: 2000 }, (_, index) => ({ rid: `late-${index}`, type: 'wx.request', name: `late-${index}` })));
    await wrapper.vm.$nextTick();

    // 队列上限 8192 覆盖推导出的 7000：丢弃提示只能来自应用阶段的 ITEM_LIMIT 裁剪，不能在
    // 入队阶段就亮（上限取 2000 时这里已经丢掉最旧的 5000 条并亮出提示，而那些记录已被 Go
    // 从 pending 取走，丢了补不回来）。
    expect(wrapper.find('.limit-callout').exists()).toBe(false);

    await flushQueue(wrapper, 2000);
    expect(wrapper.get('[data-testid="wxapi-count"]').text()).toContain('2000 / 2000');
    expect(wrapper.get('.limit-callout').text()).toContain('最早的记录已被丢弃');
    // 留下的是最后到达的 2000 条（事件批），顺序不变
    expect(wrapper.get('[data-testid="wxapi-record-0"]').text()).toContain('late-0');
    expect(wrapper.get('[data-testid="wxapi-record-1999"]').text()).toContain('late-1999');
  }, 20000);

  it('keeps the selection when the selected pending row settles in place', async () => {
    vi.useFakeTimers();
    const wrapper = mountWxApiView();
    listeners.wxapi_capture?.([
      { rid: 'keep-1', type: 'wx.request', name: 'keep', status: 'pending', data: { id: 7 } },
      { rid: 'keep-2', type: 'wx.request', name: 'other', status: 'pending', data: { id: 8 } },
    ]);
    await flushQueue(wrapper);

    // 先选中 pending 行，再投递它的落定帧：原地替换行对象不能让选中被 watch(filteredItems) 清掉
    await wrapper.get('[data-testid="wxapi-record-0"]').trigger('click');
    expect(wrapper.get('[data-testid="wxapi-detail"]').text()).toContain('"id": 7');

    listeners.wxapi_update?.({ seq: 3, rid: 'keep-1', status: 'success', result: { statusCode: 200 }, durationMs: 120 });
    await flushQueue(wrapper);

    expect(rows(wrapper)).toHaveLength(2);
    expect(wrapper.get('[data-testid="wxapi-detail"]').text()).not.toContain('未选择记录');
    expect(wrapper.get('[data-testid="wxapi-detail"]').text()).toContain('"id": 7');
    expect(wrapper.get('[data-testid="wxapi-record-0"]').classes()).toContain('selected');
    expect(wrapper.get('[data-testid="wxapi-record-0"]').text()).toContain('成功');
  });

  it('bounds the orphan update buffer and drops it on clear', async () => {
    vi.useFakeTimers();
    const wrapper = mountWxApiView();
    // 501 条孤儿帧：最旧的一条被挤出去
    for (let index = 0; index < 501; index += 1) {
      listeners.wxapi_update?.({ rid: `orphan-${index}`, status: 'fail', error: 'boom' });
    }
    await flushQueue(wrapper);
    expect(rows(wrapper)).toHaveLength(0);

    listeners.wxapi_capture?.([
      { rid: 'orphan-0', type: 'wx.request', name: 'oldest', status: 'pending' },
      { rid: 'orphan-500', type: 'wx.request', name: 'newest', status: 'pending' },
    ]);
    await flushQueue(wrapper);
    expect(wrapper.get('[data-testid="wxapi-record-0"]').text()).toContain('等待中');
    expect(wrapper.get('[data-testid="wxapi-record-1"]').text()).toContain('失败');

    // 清空连孤儿缓冲一起清掉：清空后到达的同 rid 记录不会被打上早已作废的帧
    await wrapper.get('[data-testid="clear-wxapi"]').trigger('click');
    await wrapper.get('[data-testid="confirm-clear"]').trigger('click');
    await flushQueue(wrapper);
    listeners.wxapi_capture?.({ rid: 'orphan-500', type: 'wx.request', name: 'again', status: 'pending' });
    await flushQueue(wrapper);
    expect(wrapper.get('[data-testid="wxapi-record-0"]').text()).toContain('等待中');
  }, 20000);

  it('reports polling failures separately from action errors', async () => {
    vi.useFakeTimers();
    call.mockImplementation((method: string) => (
      method === 'wxapi.poll' ? Promise.reject(new Error('poll failed')) : Promise.resolve({ running: true, dropped: 0 })
    ));
    const wrapper = mountWxApiView();
    const store = useEngineStore();
    store.wxapiCapturing = true;
    await pollTick(wrapper);

    expect(wrapper.get('[data-testid="wxapi-poll-error"]').text()).toContain('poll failed');
    expect(wrapper.find('[data-testid="wxapi-action-error"]').exists()).toBe(false);
  });

  it('does not let a successful poll wipe an action error', async () => {
    vi.useFakeTimers();
    call.mockImplementation((method: string) => {
      if (method === 'wxapi.start') return Promise.resolve({ ok: true });
      if (method === 'wxapi.stats') return Promise.resolve({ running: true, dropped: 0 });
      if (method === 'wxapi.stop') return Promise.reject(new Error('not attached'));
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountWxApiView();
    await wrapper.get('[data-testid="wxapi-toggle"]').trigger('click');
    await flushPromises();
    await wrapper.get('[data-testid="wxapi-toggle"]').trigger('click');
    await flushPromises();
    expect(wrapper.get('[data-testid="wxapi-action-error"]').text()).toBe('not attached');

    await pollTick(wrapper);
    expect(wrapper.get('[data-testid="wxapi-action-error"]').text()).toBe('not attached');
  });

  it('resets the capture state when a later stats call reports the backend stopped', async () => {
    vi.useFakeTimers();
    let capturing = true;
    call.mockImplementation((method: string) => {
      if (method === 'wxapi.start') return Promise.resolve({ ok: true });
      if (method === 'wxapi.stats') return Promise.resolve({ running: capturing, dropped: 0 });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountWxApiView();
    await wrapper.get('[data-testid="wxapi-toggle"]').trigger('click');
    await flushPromises();
    expect(wrapper.get('[data-testid="wxapi-capture-status"]').text()).toBe('捕获中');

    capturing = false;
    await pollTick(wrapper);
    expect(wrapper.get('[data-testid="wxapi-capture-status"]').text()).toBe('未捕获');
    const settled = pollCalls();
    await vi.advanceTimersByTimeAsync(6000);
    expect(pollCalls()).toBe(settled);
  });

  it('resets the local drop counter after a successful clear', async () => {
    vi.useFakeTimers();
    call.mockImplementation((method: string) => {
      if (method === 'wxapi.stats') return Promise.resolve({ running: true, pending: 0, dropped: 12 });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountWxApiView();
    await wrapper.get('[data-testid="wxapi-toggle"]').trigger('click');
    await flushQueue(wrapper);
    // 只有 shell 侧溢出（页面缓冲没丢）→ 记录仍可从历史记录找回，所以是「未进列表」而不是「已丢弃」
    expect(wrapper.get('[data-testid="wxapi-unlisted"]').text()).toContain('12');
    expect(wrapper.find('[data-testid="wxapi-dropped"]').exists()).toBe(false);

    listeners.wxapi_capture?.({ rid: 'r1', type: 'wx.request', name: 'x' });
    await flushQueue(wrapper);
    await wrapper.get('[data-testid="clear-wxapi"]').trigger('click');
    await wrapper.get('[data-testid="confirm-clear"]').trigger('click');
    await wrapper.vm.$nextTick();
    expect(wrapper.find('[data-testid="wxapi-unlisted"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="wxapi-dropped"]').exists()).toBe(false);
  });

  it('hides the duration when it is zero or unknown', async () => {
    vi.useFakeTimers();
    const wrapper = mountWxApiView();
    listeners.wxapi_capture?.([
      { rid: 'd1', type: 'wx.request', name: 'zero', status: 'success', durationMs: 0 },
      { rid: 'd2', type: 'wx.request', name: 'unknown', status: 'success' },
    ]);
    await flushQueue(wrapper);

    expect(rows(wrapper)).toHaveLength(2);
    expect(wrapper.find('.record-duration').exists()).toBe(false);
  });

  it('scrolls to the selected record by its id, not by the index at call time', async () => {
    vi.useFakeTimers();
    const descriptor = Object.getOwnPropertyDescriptor(Element.prototype, 'scrollIntoView');
    const scroll = vi.fn<(options?: boolean | ScrollIntoViewOptions) => void>();
    Object.defineProperty(Element.prototype, 'scrollIntoView', { configurable: true, writable: true, value: scroll });
    try {
      const wrapper = mountWxApiView();
      listeners.wxapi_capture?.([
        { rid: 'row-1', type: 'wx.request', name: 'first' },
        { rid: 'row-2', type: 'wx.request', name: 'second' },
      ]);
      await flushQueue(wrapper);

      await wrapper.get('[data-testid="wxapi-record-0"]').trigger('keydown', { key: 'ArrowDown' });
      await wrapper.vm.$nextTick();
      await wrapper.vm.$nextTick();

      expect(scroll).toHaveBeenCalled();
      expect(scroll.mock.contexts[0]).toBe(wrapper.get('[data-rid="row-1"]').element);
    } finally {
      if (descriptor) Object.defineProperty(Element.prototype, 'scrollIntoView', descriptor);
      else delete (Element.prototype as Partial<Element>).scrollIntoView;
    }
  });

  it('moves between the detail tabs with the arrow keys and wires the tabpanel roles', async () => {
    vi.useFakeTimers();
    const wrapper = mountWxApiView();
    listeners.wxapi_capture?.({ rid: 'r1', type: 'wx.request', name: 'x', data: { url: '/x' } });
    await flushQueue(wrapper);
    await wrapper.get('[data-testid="wxapi-record-0"]').trigger('click');

    const request = wrapper.get('[data-testid="detail-tab-request"]');
    expect(request.attributes('aria-controls')).toBe('detail-panel-request');
    expect(request.attributes('tabindex')).toBe('0');
    expect(wrapper.get('[data-testid="wxapi-request-view"]').attributes('role')).toBe('tabpanel');
    expect(wrapper.get('[data-testid="wxapi-request-view"]').attributes('aria-labelledby')).toBe('detail-tab-request');

    await request.trigger('keydown', { key: 'ArrowRight' });
    expect(wrapper.get('[data-testid="detail-tab-response"]').attributes('aria-selected')).toBe('true');
    expect(wrapper.get('[data-testid="detail-tab-request"]').attributes('tabindex')).toBe('-1');
    expect(wrapper.get('[data-testid="wxapi-response-view"]').attributes('role')).toBe('tabpanel');

    await wrapper.get('[data-testid="detail-tab-response"]').trigger('keydown', { key: 'ArrowRight' });
    expect(wrapper.get('[data-testid="detail-tab-replay"]').attributes('aria-selected')).toBe('true');

    // 末尾继续按右键回到第一个标签
    await wrapper.get('[data-testid="detail-tab-replay"]').trigger('keydown', { key: 'ArrowRight' });
    expect(wrapper.get('[data-testid="detail-tab-request"]').attributes('aria-selected')).toBe('true');

    await wrapper.get('[data-testid="detail-tab-request"]').trigger('keydown', { key: 'ArrowLeft' });
    expect(wrapper.get('[data-testid="detail-tab-replay"]').attributes('aria-selected')).toBe('true');
  });

  // 详情面板的高度归正文：标签页与重放/复制按钮固定在面板里，只有 .detail-body 滚。
  // jsdom 没有布局，能守的是结构——正文之外的东西不许被塞进滚动区。
  it('keeps the detail tabs and actions outside the scrolling body', async () => {
    vi.useFakeTimers();
    const wrapper = mountWxApiView();
    listeners.wxapi_capture?.({ rid: 'r1', type: 'wx.request', name: 'wx.request', data: { url: '/x' }, status: 'success' });
    await flushQueue(wrapper);
    await wrapper.get('[data-testid="wxapi-record-0"]').trigger('click');

    const body = wrapper.get('[data-testid="wxapi-detail"] .detail-body');
    expect(body.find('[data-testid="wxapi-request-view"]').exists()).toBe(true);
    expect(body.find('.detail-tabs').exists()).toBe(false);
    expect(body.find('.detail-actions').exists()).toBe(false);
    expect(wrapper.find('[data-testid="wxapi-detail"] > .detail-tabs').exists()).toBe(true);
    expect(wrapper.find('[data-testid="wxapi-detail"] > .detail-actions').exists()).toBe(true);
  });

  // 密度开关（数据多的列表共用）：默认紧凑，关掉后列表不再带紧凑类。
  it('defaults to compact rows and drops the class when the toggle is cleared', async () => {
    const wrapper = mountWxApiView();
    await flushPromises();
    const list = () => wrapper.get('.record-list');
    expect(wrapper.get('[data-testid="wxapi-density"]').element).toHaveProperty('checked', true);
    expect(list().classes()).toContain('is-compact');

    await wrapper.get('[data-testid="wxapi-density"]').setValue(false);
    expect(list().classes()).not.toContain('is-compact');
  });
});
