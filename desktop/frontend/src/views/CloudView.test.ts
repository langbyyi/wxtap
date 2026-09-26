import { createTestingPinia } from '@pinia/testing';
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import CloudView from './CloudView.vue';
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

function mountCloudView(pinia: Pinia = createTestingPinia({ createSpy: vi.fn, stubActions: false })) {
  return mount(CloudView, { global: { plugins: [pinia] } });
}

const pollCalls = () => call.mock.calls.filter(([method]) => method === 'cloud.poll').length;
const statsCalls = () => call.mock.calls.filter(([method]) => method === 'cloud.stats').length;
const rows = (wrapper: ReturnType<typeof mountCloudView>) => wrapper.findAll('[data-testid^="cloud-record-"]');

/** 批量队列 120ms 冲刷一次；fake timers 下用它同时推进微任务。 */
const flushQueue = (wrapper: ReturnType<typeof mountCloudView>, extraMs = 200) =>
  vi.advanceTimersByTimeAsync(extraMs).then(() => wrapper.vm.$nextTick());

/** 轮询在 2000ms 触发，再等一次队列冲刷才能看到记录进列表。 */
const pollTick = (wrapper: ReturnType<typeof mountCloudView>) =>
  flushQueue(wrapper, 2000).then(() => flushQueue(wrapper, 200));

describe('CloudView', () => {
  beforeEach(() => {
    call.mockImplementation((method: string) => {
      if (method === 'cloudapi.status') return Promise.resolve({ running: false });
      if (method === 'cloud.stats') return Promise.resolve({ running: true, pending: 0, dropped: 0, ack: 0, updateAck: 0 });
      return Promise.resolve({ ok: true });
    });
  });

  afterEach(() => {
    vi.useRealTimers();
    call.mockReset();
  });

  it('uses the shared page header contract', () => {
    expect(mountCloudView().findComponent(PageHeader).exists()).toBe(true);
  });

  it('keeps every capture action and the API service controls inside the panel', async () => {
    vi.useFakeTimers();
    const wrapper = mountCloudView();

    // 页头不再承载操作，操作全部并入内容区的状态行
    expect(wrapper.find('.page-toolbar').exists()).toBe(false);
    const panel = wrapper.get('.panel');
    for (const testId of ['cloud-capture-toggle', 'cloud-capture-toggle', 'scan-cloud', 'export-cloud', 'clear-cloud', 'cloudapi-toggle', 'cloudapi-toggle', 'cloudapi-port']) {
      expect(panel.find(`[data-testid="${testId}"]`).exists()).toBe(true);
    }
    expect(wrapper.get('[data-testid="cloud-capture-status"]').classes()).toContain('off');
    expect(wrapper.get('[data-testid="cloudapi-status"]').text()).toContain('已停止');

    listeners.cloud_capture?.([
      { rid: 'r1', type: 'function', name: 'hello' },
      { rid: 'r2', type: 'storage', name: 'uploadFile' },
      { rid: 'r3', type: 'db.find', name: 'find' },
    ]);
    await flushQueue(wrapper);

    expect(wrapper.get('[data-testid="cloud-count"]').text()).toContain('3 / 3');
    expect(wrapper.get('[data-testid="cloud-category-all"] .chip-count').text()).toBe('3');
    expect(wrapper.get('[data-testid="cloud-category-function"] .chip-count').text()).toBe('1');
    expect(wrapper.get('[data-testid="cloud-category-storage"] .chip-count').text()).toBe('1');
    // db.<op> 与静态扫描的 database 归同一类
    expect(wrapper.get('[data-testid="cloud-category-database"] .chip-count').text()).toBe('1');
  });

  it('scans, filters, selects and clears cloud records', async () => {
    call.mockImplementationOnce(() => Promise.resolve({ running: false })).mockImplementationOnce(() => Promise.resolve({
      items: [{ app_id: 'wx1', type: 'function', name: 'hello', data: { id: 1 }, status: 'success', timestamp: '10:00' }],
    }));
    const wrapper = mountCloudView();
    await flushPromises();

    await wrapper.get('[data-testid="scan-cloud"]').trigger('click');
    await flushPromises();
    expect(wrapper.text()).toContain('hello');
    await wrapper.get('[data-testid="cloud-search"]').setValue('hello');
    await wrapper.get('[data-testid="cloud-record-0"]').trigger('click');
    expect(wrapper.get('[data-testid="cloud-detail"]').text()).toContain('"id": 1');

    await wrapper.get('[data-testid="clear-cloud"]').trigger('click');
    await wrapper.get('[data-testid="confirm-clear"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('cloud.clear');
    expect(wrapper.text()).not.toContain('hello');
  });

  it('moves the record selection with the arrow keys', async () => {
    vi.useFakeTimers();
    const wrapper = mountCloudView();
    listeners.cloud_capture?.([
      { rid: 'r1', type: 'function', name: 'first', data: { id: 1 } },
      { rid: 'r2', type: 'function', name: 'second', data: { id: 2 } },
      { rid: 'r3', type: 'function', name: 'third', data: { id: 3 } },
    ]);
    await flushQueue(wrapper);

    await wrapper.get('[data-testid="cloud-record-0"]').trigger('keydown', { key: 'ArrowDown' });
    expect(wrapper.get('[data-testid="cloud-detail"]').text()).toContain('"id": 1');
    await wrapper.get('[data-testid="cloud-record-0"]').trigger('keydown', { key: 'ArrowDown' });
    expect(wrapper.get('[data-testid="cloud-detail"]').text()).toContain('"id": 2');
    await wrapper.get('[data-testid="cloud-record-1"]').trigger('keydown', { key: 'ArrowUp' });
    expect(wrapper.get('[data-testid="cloud-detail"]').text()).toContain('"id": 1');
    await wrapper.get('[data-testid="cloud-record-0"]').trigger('keydown', { key: 'ArrowUp' });
    expect(wrapper.get('[data-testid="cloud-detail"]').text()).toContain('"id": 1');
  });

  it('keeps the capture state and resumes polling when the page is revisited', async () => {
    vi.useFakeTimers();
    const pinia = createTestingPinia({ createSpy: vi.fn, stubActions: false });

    const first = mountCloudView(pinia);
    await flushPromises();
    await first.get('[data-testid="cloud-capture-toggle"]').trigger('click');
    await flushPromises();
    expect(first.get('[data-testid="cloud-capture-toggle"]').attributes('disabled')).toBeUndefined();
    first.unmount();

    // 切换页面期间后端仍在记录，返回后应恢复轮询并把期间的数据补齐
    const second = mountCloudView(pinia);
    await flushPromises();
    expect(second.get('[data-testid="cloud-capture-toggle"]').attributes('disabled')).toBeUndefined();
    await vi.advanceTimersByTimeAsync(2100);
    expect(pollCalls()).toBeGreaterThan(0);
    second.unmount();
  });

  it('starts capture polling, receives events, and stops polling on unmount', async () => {
    vi.useFakeTimers();
    const wrapper = mountCloudView();
    await flushPromises();
    expect(typeof listeners.cloud_capture).toBe('function');
    expect(typeof listeners.cloud_update).toBe('function');

    await wrapper.get('[data-testid="cloud-capture-toggle"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('cloud.start');
    listeners.cloud_capture?.({ type: 'function', name: 'eventFn', data: {} });
    await flushQueue(wrapper);
    expect(wrapper.text()).toContain('eventFn');
    await vi.advanceTimersByTimeAsync(2000);
    expect(call).toHaveBeenCalledWith('cloud.poll', { limit: 500 });
    expect(statsCalls()).toBeGreaterThan(0);

    wrapper.unmount();
    call.mockClear();
    await vi.advanceTimersByTimeAsync(4000);
    expect(call).not.toHaveBeenCalledWith('cloud.poll');
  });

  it('stops capture through the backend and reports the failure in place', async () => {
    vi.useFakeTimers();
    call.mockImplementation((method: string) => {
      if (method === 'cloudapi.status') return Promise.resolve({ running: false });
      if (method === 'cloud.stats') return Promise.resolve({ running: true, dropped: 0 });
      if (method === 'cloud.stop') return Promise.reject(new Error('not attached'));
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountCloudView();
    await flushPromises();
    await wrapper.get('[data-testid="cloud-capture-toggle"]').trigger('click');
    await flushPromises();
    call.mockClear();
    await wrapper.get('[data-testid="cloud-capture-toggle"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('cloud.stop');
    expect(wrapper.get('[data-testid="cloud-action-error"]').text()).toBe('not attached');
  });

  it('renders one row when the same rid arrives from the event stream and from the poll', async () => {
    vi.useFakeTimers();
    const record = { rid: 'function-wxone-1758600000000-7', type: 'function', name: 'hello', status: 'pending', timestamp: '10:00:00' };
    call.mockImplementation((method: string) => {
      if (method === 'cloudapi.status') return Promise.resolve({ running: false });
      if (method === 'cloud.poll') return Promise.resolve([record]);
      if (method === 'cloud.stats') return Promise.resolve({ running: true, dropped: 0 });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountCloudView();
    await wrapper.get('[data-testid="cloud-capture-toggle"]').trigger('click');
    await flushQueue(wrapper);

    listeners.cloud_capture?.(record);
    await flushQueue(wrapper);
    expect(wrapper.get('[data-testid="cloud-count"]').text()).toContain('1 / 1');

    // poll 再投递同一条记录：按 rid 原地替换，不追加第二行
    await pollTick(wrapper);
    expect(wrapper.get('[data-testid="cloud-count"]').text()).toContain('1 / 1');
    expect(rows(wrapper)).toHaveLength(1);
  });

  it('appends records without a rid instead of throwing', async () => {
    vi.useFakeTimers();
    const wrapper = mountCloudView();
    // 静态扫描的记录没有 rid（也没有落定流）：按追加处理
    listeners.cloud_capture?.({ type: 'function', name: 'scanned' });
    await flushQueue(wrapper);
    listeners.cloud_capture?.(['nonsense', null, { rid: 'r1', type: 'function', name: 'live' }]);
    await flushQueue(wrapper);

    expect(rows(wrapper)).toHaveLength(2);
    expect(wrapper.get('[data-testid="cloud-record-0"]').text()).toContain('scanned');
    expect(wrapper.get('[data-testid="cloud-record-1"]').text()).toContain('live');
  });

  it('settles a pending row in place when a cloud_update frame arrives', async () => {
    vi.useFakeTimers();
    const wrapper = mountCloudView();
    listeners.cloud_capture?.([
      { rid: 'r1', type: 'function', name: 'ledger', status: 'pending', timestamp: '10:00:00' },
      { rid: 'r2', type: 'function', name: 'other', status: 'pending', timestamp: '10:00:01' },
    ]);
    await flushQueue(wrapper);

    listeners.cloud_update?.({ rid: 'r1', status: 'success', result: { errMsg: 'ok' }, durationMs: 1830 });
    await flushQueue(wrapper);

    expect(rows(wrapper)).toHaveLength(2);
    expect(wrapper.get('[data-testid="cloud-record-0"]').text()).toContain('成功');
    expect(wrapper.get('[data-testid="cloud-record-0"]').text()).toContain('1.8s');
    expect(wrapper.get('[data-testid="cloud-record-1"]').text()).toContain('等待中');

    // 落定的返回值在详情里可见（否则「落定」只剩一个状态徽标）
    await wrapper.get('[data-testid="cloud-record-0"]').trigger('click');
    expect(wrapper.get('[data-testid="cloud-result-view"]').text()).toContain('errMsg');

    // 找不到记录的更新帧不抛错也不新增行，而是暂存等记录到达（孤儿缓冲）
    listeners.cloud_update?.({ rid: 'late', status: 'fail', error: 'boom' });
    await flushQueue(wrapper);
    expect(rows(wrapper)).toHaveLength(2);

    // 记录一到，孤儿帧立刻补打，行不会再停在「等待中」
    listeners.cloud_capture?.({ rid: 'late', type: 'function', name: 'late', status: 'pending' });
    await flushQueue(wrapper);
    expect(rows(wrapper)).toHaveLength(3);
    const late = wrapper.get('[data-testid="cloud-record-2"]');
    expect(late.text()).toContain('失败');
    expect(late.text()).not.toContain('等待中');
  });

  // 超过展示上限的正文只渲染前缀并明说截断（与历史页同口径）；普通记录不亮提示。
  it('truncates an oversized result in the detail pane and says so', async () => {
    vi.useFakeTimers();
    const wrapper = mountCloudView();
    listeners.cloud_capture?.([
      { rid: 'big', type: 'function', name: 'big', status: 'success', data: { id: 1 }, result: { body: 'x'.repeat(DETAIL_TEXT_LIMIT + 16) } },
      { rid: 'small', type: 'function', name: 'small', status: 'success', data: { id: 2 }, result: { ok: true } },
    ]);
    await flushQueue(wrapper);

    await wrapper.get('[data-testid="cloud-record-0"]').trigger('click');
    expect(wrapper.get('[data-testid="cloud-result-view"]').text()).toHaveLength(DETAIL_TEXT_LIMIT);
    expect(wrapper.get('[data-testid="cloud-detail-truncated"]').text()).toContain('已截断显示前');

    // 换一条普通记录：明示消失
    await wrapper.get('[data-testid="cloud-record-1"]').trigger('click');
    await wrapper.vm.$nextTick();
    expect(wrapper.find('[data-testid="cloud-detail-truncated"]').exists()).toBe(false);
  });

  it('does not let a stale pending poll record downgrade a settled row', async () => {
    vi.useFakeTimers();
    // 同一条记录的两条投递路径：poll 取走的是 pending 版本，落定帧随后把它刷成终态；
    // 在途的那一轮 poll 响应最后才落地，投递的还是旧 pending 版本。
    const stale = { rid: 'settle-1', type: 'function', name: 'slowCall', status: 'pending', timestamp: '10:00:00' };
    call.mockImplementation((method: string) => {
      if (method === 'cloudapi.status') return Promise.resolve({ running: false });
      if (method === 'cloud.stats') return Promise.resolve({ running: true, pending: 0, dropped: 0 });
      if (method === 'cloud.poll') return Promise.resolve([stale]);
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountCloudView();
    await wrapper.get('[data-testid="cloud-capture-toggle"]').trigger('click');
    await pollTick(wrapper);
    expect(wrapper.get('[data-testid="cloud-record-0"]').text()).toContain('等待中');

    listeners.cloud_update?.({ rid: 'settle-1', status: 'success', result: { errMsg: 'ok' }, durationMs: 1830 });
    await flushQueue(wrapper);
    expect(wrapper.get('[data-testid="cloud-record-0"]').text()).toContain('成功');

    // 旧 pending 版本落地：不能覆盖已落定的行，否则它会永久停在「等待中」（落定帧只发一次）
    await pollTick(wrapper);
    const row = wrapper.get('[data-testid="cloud-record-0"]');
    expect(rows(wrapper)).toHaveLength(1);
    expect(row.text()).toContain('成功');
    expect(row.text()).toContain('1.8s');
    expect(row.text()).not.toContain('等待中');
  });

  it('applies every frame of a batched array update payload', async () => {
    vi.useFakeTimers();
    const wrapper = mountCloudView();
    listeners.cloud_capture?.([
      { rid: 'r1', type: 'function', name: 'first', status: 'pending', timestamp: '10:00:00' },
      { rid: 'r2', type: 'function', name: 'second', status: 'pending', timestamp: '10:00:01' },
    ]);
    await flushQueue(wrapper);

    // 落定帧按数组投递（一个 tick 一个数组），数组里可以混着找不到 rid 的帧
    listeners.cloud_update?.([
      { rid: 'r1', status: 'success', result: { errMsg: 'ok' }, durationMs: 1200 },
      { rid: 'unknown', status: 'fail', error: 'boom' },
      { rid: 'r2', status: 'fail', error: 'timeout' },
    ]);
    await flushQueue(wrapper);

    // 数组里每一帧都要落定，不能只认第一帧
    expect(rows(wrapper)).toHaveLength(2);
    expect(wrapper.get('[data-testid="cloud-record-0"]').text()).toContain('成功');
    expect(wrapper.get('[data-testid="cloud-record-0"]').text()).toContain('1.2s');
    expect(wrapper.get('[data-testid="cloud-record-1"]').text()).toContain('失败');
    expect(wrapper.get('[data-testid="cloud-record-1"]').text()).not.toContain('等待中');

    // 数组里找不到 rid 的那一帧按孤儿暂存：记录到达时补打
    listeners.cloud_capture?.({ rid: 'unknown', type: 'function', name: 'late', status: 'pending' });
    await flushQueue(wrapper);
    expect(wrapper.get('[data-testid="cloud-record-2"]').text()).toContain('失败');
  });

  it('hides the duration when it is zero or unknown and shows it in the detail', async () => {
    vi.useFakeTimers();
    const wrapper = mountCloudView();
    listeners.cloud_capture?.([
      { rid: 'd1', type: 'function', name: 'zero', status: 'success', durationMs: 0 },
      { rid: 'd2', type: 'function', name: 'unknown', status: 'success' },
    ]);
    await flushQueue(wrapper);

    expect(rows(wrapper)).toHaveLength(2);
    expect(wrapper.find('.record-duration').exists()).toBe(false);

    // 落定帧带来真实耗时：列表行与详情都显示，详情同时给出状态
    listeners.cloud_update?.({ rid: 'd1', status: 'fail', error: 'timeout', durationMs: 1830 });
    await flushQueue(wrapper);
    expect(wrapper.get('[data-testid="cloud-record-0"]').text()).toContain('1.8s');
    await wrapper.get('[data-testid="cloud-record-0"]').trigger('click');
    expect(wrapper.get('[data-testid="cloud-detail-duration"]').text()).toContain('1.8s');
    expect(wrapper.get('[data-testid="cloud-detail-status"]').text()).toBe('失败');
  });

  it('keeps the selection when the selected pending row settles in place', async () => {
    vi.useFakeTimers();
    const wrapper = mountCloudView();
    listeners.cloud_capture?.([
      { rid: 'keep-1', type: 'function', name: 'keep', status: 'pending', data: { id: 7 } },
      { rid: 'keep-2', type: 'function', name: 'other', status: 'pending', data: { id: 8 } },
    ]);
    await flushQueue(wrapper);
    await wrapper.get('[data-testid="cloud-record-0"]').trigger('click');

    listeners.cloud_update?.({ rid: 'keep-1', status: 'success', result: { errMsg: 'ok' }, durationMs: 120 });
    await flushQueue(wrapper);

    expect(rows(wrapper)).toHaveLength(2);
    expect(wrapper.get('[data-testid="cloud-detail"]').text()).not.toContain('未选择记录');
    expect(wrapper.get('[data-testid="cloud-record-0"]').classes()).toContain('selected');
    expect(wrapper.get('[data-testid="cloud-record-0"]').text()).toContain('成功');
  });

  it('bounds the orphan update buffer and drops it on clear', async () => {
    vi.useFakeTimers();
    const wrapper = mountCloudView();
    // 501 条孤儿帧：最旧的一条被挤出去
    for (let index = 0; index < 501; index += 1) {
      listeners.cloud_update?.({ rid: `orphan-${index}`, status: 'fail', error: 'boom' });
    }
    await flushQueue(wrapper);
    expect(rows(wrapper)).toHaveLength(0);

    listeners.cloud_capture?.([
      { rid: 'orphan-0', type: 'function', name: 'oldest', status: 'pending' },
      { rid: 'orphan-500', type: 'function', name: 'newest', status: 'pending' },
    ]);
    await flushQueue(wrapper);
    expect(wrapper.get('[data-testid="cloud-record-0"]').text()).toContain('等待中');
    expect(wrapper.get('[data-testid="cloud-record-1"]').text()).toContain('失败');

    // 清空连孤儿缓冲一起清掉：清空后到达的同 rid 记录不会被打上早已作废的帧
    await wrapper.get('[data-testid="clear-cloud"]').trigger('click');
    await wrapper.get('[data-testid="confirm-clear"]').trigger('click');
    await flushQueue(wrapper);
    listeners.cloud_capture?.({ rid: 'orphan-500', type: 'function', name: 'again', status: 'pending' });
    await flushQueue(wrapper);
    expect(wrapper.get('[data-testid="cloud-record-0"]').text()).toContain('等待中');
  }, 20000);

  it('discards a poll response that was in flight when the records were cleared', async () => {
    vi.useFakeTimers();
    const pollResolvers: Array<(value: unknown) => void> = [];
    let pollRound = 0;
    call.mockImplementation((method: string) => {
      if (method === 'cloudapi.status') return Promise.resolve({ running: false });
      if (method === 'cloud.stats') return Promise.resolve({ running: true, dropped: 0 });
      if (method === 'cloud.poll') {
        pollRound += 1;
        // 第一轮故意悬着：用户点清空时它正好在飞
        if (pollRound === 1) return new Promise((resolve) => { pollResolvers.push(resolve); });
        return Promise.resolve([{ rid: 'fresh-1', type: 'function', name: 'fresh-1', status: 'success' }]);
      }
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountCloudView();
    await wrapper.get('[data-testid="cloud-capture-toggle"]').trigger('click');
    await flushPromises();

    listeners.cloud_capture?.({ rid: 'kept-1', type: 'function', name: 'kept-1', status: 'success' });
    await flushQueue(wrapper);
    expect(wrapper.get('[data-testid="cloud-count"]').text()).toContain('1 / 1');

    await vi.advanceTimersByTimeAsync(2000);
    await flushPromises();
    expect(pollCalls()).toBe(1);
    const statsBeforeClear = statsCalls();

    await wrapper.get('[data-testid="clear-cloud"]').trigger('click');
    await wrapper.get('[data-testid="confirm-clear"]').trigger('click');
    await flushPromises();
    expect(wrapper.get('[data-testid="cloud-count"]').text()).toContain('0 / 0');

    // 在飞的响应现在才带着「清空之前」的记录到达：整批丢弃，既不进列表也不刷统计
    pollResolvers.shift()?.([
      { rid: 'stale-1', type: 'function', name: 'stale-1', status: 'success' },
      { rid: 'stale-2', type: 'function', name: 'stale-2', status: 'success' },
    ]);
    await flushPromises();
    await flushQueue(wrapper);
    expect(rows(wrapper)).toHaveLength(0);
    expect(wrapper.get('[data-testid="cloud-count"]').text()).toContain('0 / 0');
    expect(statsCalls()).toBe(statsBeforeClear);

    // 守卫只作用于「清空之前发起」的那一页：清空之后发起的轮询照常落地
    await pollTick(wrapper);
    expect(wrapper.get('[data-testid="cloud-count"]').text()).toContain('1 / 1');
    expect(wrapper.get('[data-testid="cloud-record-0"]').text()).toContain('fresh-1');
  });

  it('starts no new poll while a clear is in flight', async () => {
    vi.useFakeTimers();
    let resolveClear: ((value: unknown) => void) | undefined;
    call.mockImplementation((method: string) => {
      if (method === 'cloudapi.status') return Promise.resolve({ running: false });
      if (method === 'cloud.stats') return Promise.resolve({ running: true, pending: 0, dropped: 0 });
      if (method === 'cloud.poll') return Promise.resolve([{ rid: 'after-1', type: 'function', name: 'after-1', status: 'success' }]);
      if (method === 'cloud.clear') return new Promise((resolve) => { resolveClear = resolve; });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountCloudView();
    await wrapper.get('[data-testid="cloud-capture-toggle"]').trigger('click');
    await flushPromises();
    await pollTick(wrapper);
    expect(pollCalls()).toBe(1);
    expect(wrapper.get('[data-testid="cloud-count"]').text()).toContain('1 / 1');

    // 清空的往返还没回来时定时器到点：这一轮不能发起。否则它带着旧代次却取到清空之后写入
    // 的记录，响应落地时被 epoch 守卫整批丢掉 —— 而那批记录已被 shell 从 pending 取走。
    await wrapper.get('[data-testid="clear-cloud"]').trigger('click');
    await wrapper.get('[data-testid="confirm-clear"]').trigger('click');
    await vi.advanceTimersByTimeAsync(4000);
    expect(pollCalls()).toBe(1);

    resolveClear?.({ ok: true });
    await flushPromises();
    expect(wrapper.get('[data-testid="cloud-count"]').text()).toContain('0 / 0');

    // 清空完成后照旧起表：这一轮取到的记录不会再被作废
    await pollTick(wrapper);
    expect(pollCalls()).toBe(2);
    expect(wrapper.get('[data-testid="cloud-count"]').text()).toContain('1 / 1');
    expect(wrapper.get('[data-testid="cloud-record-0"]').text()).toContain('after-1');
  });

  it('keeps polling inside the same tick while the backend still has a backlog', async () => {
    vi.useFakeTimers();
    let page = 0;
    call.mockImplementation((method: string) => {
      if (method === 'cloudapi.status') return Promise.resolve({ running: false });
      if (method === 'cloud.stats') return Promise.resolve({ running: true, dropped: 0 });
      if (method === 'cloud.poll') {
        page += 1;
        // 前两页取满 500 条说明还有积压，第三页不满 → 补齐结束
        if (page <= 2) {
          return Promise.resolve(Array.from({ length: 500 }, (_, index) => ({ rid: `p${page}-${index}`, type: 'function', name: `p${page}-${index}` })));
        }
        return Promise.resolve([{ rid: 'p3-0', type: 'function', name: 'p3-0' }]);
      }
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountCloudView();
    await wrapper.get('[data-testid="cloud-capture-toggle"]').trigger('click');
    await flushPromises();
    await pollTick(wrapper);

    expect(pollCalls()).toBe(3);
    expect(wrapper.get('[data-testid="cloud-count"]').text()).toContain('1001 / 1001');
  }, 20000);

  it('caps the backlog top-up at ten polls per tick and shows what the burst dropped', async () => {
    vi.useFakeTimers();
    let page = 0;
    call.mockImplementation((method: string) => {
      if (method === 'cloudapi.status') return Promise.resolve({ running: false });
      if (method === 'cloud.stats') return Promise.resolve({ running: true, dropped: 0 });
      if (method === 'cloud.poll') {
        page += 1;
        return Promise.resolve(Array.from({ length: 500 }, (_, index) => ({ rid: `page${page}-${index}`, type: 'function', name: `page${page}-${index}` })));
      }
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountCloudView();
    await wrapper.get('[data-testid="cloud-capture-toggle"]').trigger('click');
    await flushPromises();
    await pollTick(wrapper);

    // 一个 tick 最多补 10 轮 = 5000 条，超出的部分不能静默消失
    expect(pollCalls()).toBe(10);
    expect(wrapper.get('[data-testid="cloud-count"]').text()).toContain('2000 / 2000');
    expect(wrapper.get('.limit-callout').text()).toContain('最早的记录已被丢弃');
  }, 20000);

  it('bounds the DOM work of a single frame and keeps a huge burst in order', async () => {
    vi.useFakeTimers();
    const wrapper = mountCloudView();
    const list = wrapper.get('.record-list').element;
    const perBatch: number[] = [];
    const observer = new MutationObserver((mutations) => {
      let added = 0;
      for (const mutation of mutations) {
        for (const node of mutation.addedNodes) {
          // 只数记录行（按 rid 认）：空态那一行不算 DOM 工作量
          if (node.nodeType === 1 && (node as Element).querySelector('[data-rid]')) added += 1;
        }
      }
      perBatch.push(added);
    });
    observer.observe(list, { childList: true });

    listeners.cloud_capture?.(Array.from({ length: 1000 }, (_, index) => ({ rid: `frame-${index}`, type: 'function', name: `frame-${index}` })));
    await flushQueue(wrapper, 2000);
    observer.disconnect();

    expect(wrapper.get('[data-testid="cloud-count"]').text()).toContain('1000 / 1000');
    expect(perBatch.length).toBeGreaterThan(0);
    // 单帧最多应用 400 条：任何一帧插入的行数都不能超过它
    expect(Math.max(...perBatch)).toBeLessThanOrEqual(400);
    // 分帧不改变顺序
    expect(wrapper.get('[data-testid="cloud-record-0"]').text()).toContain('frame-0');
    expect(wrapper.get('[data-testid="cloud-record-999"]').text()).toContain('frame-999');
  }, 20000);

  it('keeps the DOM node of a surviving row when the oldest records are trimmed', async () => {
    vi.useFakeTimers();
    const wrapper = mountCloudView();
    listeners.cloud_capture?.(Array.from({ length: 2400 }, (_, index) => ({ rid: `bulk-${index}`, type: 'function', name: `bulk-${index}` })));
    await flushQueue(wrapper, 2000);

    expect(wrapper.get('[data-testid="cloud-count"]').text()).toContain('2000 / 2000');
    expect(wrapper.get('.limit-callout').text()).toContain('最早的记录已被丢弃');
    // 留下的是最新的一批：最早的 400 条被裁掉
    expect(wrapper.get('[data-testid="cloud-record-0"]').text()).toContain('bulk-400');

    const survivor = wrapper.get('[data-rid="bulk-1500"]').element;

    // 再推一波：最早的 600 条被裁掉。稳定键（rid）让存活行跟着记录走，而不是按下标重写。
    listeners.cloud_capture?.(Array.from({ length: 600 }, (_, index) => ({ rid: `late-${index}`, type: 'function', name: `late-${index}` })));
    await flushQueue(wrapper, 1000);

    expect(wrapper.get('[data-rid="bulk-1500"]').element).toBe(survivor);
    expect(wrapper.find('[data-rid="bulk-999"]').exists()).toBe(false);
    expect(wrapper.get('[data-testid="cloud-record-0"]').text()).toContain('bulk-1000');
    expect(wrapper.get('[data-testid="cloud-record-1999"]').text()).toContain('late-599');
  }, 20000);

  it('holds a whole tick of poll pages plus an event batch without dropping at the queue bound', async () => {
    vi.useFakeTimers();
    let page = 0;
    call.mockImplementation((method: string) => {
      if (method === 'cloudapi.status') return Promise.resolve({ running: false });
      if (method === 'cloud.stats') return Promise.resolve({ running: true, dropped: 0 });
      if (method === 'cloud.poll') {
        page += 1;
        return Promise.resolve(Array.from({ length: 500 }, (_, index) => ({ rid: `page${page}-${index}`, type: 'function', name: `page${page}-${index}` })));
      }
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountCloudView();
    await wrapper.get('[data-testid="cloud-capture-toggle"]').trigger('click');
    await flushPromises();

    // 同一个冲刷窗口里的两条投递路径叠加：poll 10 轮 × 500 = 5000（响应在 120ms 的冲刷
    // 之前到齐），再叠加事件批 2000 —— 正好是推导出的单 tick 上界 7000。
    await vi.advanceTimersByTimeAsync(2000);
    await flushPromises();
    expect(pollCalls()).toBe(10);
    listeners.cloud_capture?.(Array.from({ length: 2000 }, (_, index) => ({ rid: `late-${index}`, type: 'function', name: `late-${index}` })));
    await wrapper.vm.$nextTick();

    // 队列上限 8192 覆盖推导出的 7000：丢弃提示只能来自应用阶段的 ITEM_LIMIT 裁剪，不能在
    // 入队阶段就亮（上限取小了会在这里就丢掉最旧的记录，而那些记录已被 Go 从 pending 取走）。
    expect(wrapper.find('.limit-callout').exists()).toBe(false);

    await flushQueue(wrapper, 3000);
    expect(wrapper.get('[data-testid="cloud-count"]').text()).toContain('2000 / 2000');
    expect(wrapper.get('.limit-callout').text()).toContain('最早的记录已被丢弃');
  }, 20000);

  it('warns as soon as one burst is larger than the queue bound', async () => {
    vi.useFakeTimers();
    const wrapper = mountCloudView();
    listeners.cloud_capture?.(Array.from({ length: 9000 }, (_, index) => ({ rid: `queue-${index}`, type: 'function', name: `queue-${index}` })));
    await wrapper.vm.$nextTick();

    // 超出队列上限的记录在入队阶段就被丢掉，提示必须当场亮，不能等应用阶段
    expect(wrapper.get('.limit-callout').text()).toContain('最早的记录已被丢弃');

    await flushQueue(wrapper, 3000);
    expect(wrapper.get('[data-testid="cloud-count"]').text()).toContain('2000 / 2000');
  }, 20000);

  it('shows the shell and the page-side drop counters and resets them after a successful clear', async () => {
    vi.useFakeTimers();
    call.mockImplementation((method: string) => {
      if (method === 'cloudapi.status') return Promise.resolve({ running: false });
      // shell 侧 pending 溢出 12 条（未进实时列表，历史记录里仍有）+ 页面侧两条缓冲各自溢出 3 / 5 条
      // （永久丢失）：两个读数分开展示，不能合成一个「已丢弃」
      if (method === 'cloud.stats') return Promise.resolve({ running: true, pending: 0, dropped: 12, pageDroppedRecords: 3, pageDroppedUpdates: 5 });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountCloudView();
    expect(wrapper.find('[data-testid="cloud-dropped"]').exists()).toBe(false);
    await wrapper.get('[data-testid="cloud-capture-toggle"]').trigger('click');
    await flushQueue(wrapper);
    expect(wrapper.get('[data-testid="cloud-dropped"]').text()).toContain('已丢弃 8 条');
    expect(wrapper.get('[data-testid="cloud-unlisted"]').text()).toContain('未进列表 12 条');

    listeners.cloud_capture?.({ rid: 'r1', type: 'function', name: 'x' });
    await flushQueue(wrapper);
    await wrapper.get('[data-testid="clear-cloud"]').trigger('click');
    await wrapper.get('[data-testid="confirm-clear"]').trigger('click');
    await wrapper.vm.$nextTick();
    expect(wrapper.find('[data-testid="cloud-dropped"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="cloud-unlisted"]').exists()).toBe(false);
  });

  it('shows the page-side drops even when the shell reports none', async () => {
    vi.useFakeTimers();
    call.mockImplementation((method: string) => {
      if (method === 'cloudapi.status') return Promise.resolve({ running: false });
      if (method === 'cloud.stats') return Promise.resolve({ running: true, pending: 0, dropped: 0, pageDroppedRecords: 7, pageDroppedUpdates: 5 });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountCloudView();
    await wrapper.get('[data-testid="cloud-capture-toggle"]').trigger('click');
    await flushQueue(wrapper);

    expect(wrapper.get('[data-testid="cloud-dropped"]').text()).toContain('已丢弃 12 条');
    expect(wrapper.find('[data-testid="cloud-unlisted"]').exists()).toBe(false);
  });

  it('warns about storage only when the backend explicitly reports it unavailable', async () => {
    vi.useFakeTimers();
    const wrapper = mountCloudView();
    await flushPromises();
    // 缺省（旧后端没有 storageAvailable 字段）按可用处理，不亮警示
    expect(wrapper.find('[data-testid="cloud-storage-warning"]').exists()).toBe(false);

    call.mockImplementation((method: string) => {
      if (method === 'cloudapi.status') return Promise.resolve({ running: false });
      if (method === 'cloud.stats') return Promise.resolve({ running: true, pending: 0, dropped: 0, storageAvailable: false });
      return Promise.resolve({ ok: true });
    });
    await wrapper.get('[data-testid="cloud-capture-toggle"]').trigger('click');
    await flushQueue(wrapper);
    expect(wrapper.get('[data-testid="cloud-storage-warning"]').text()).toContain('存储不可用');

    // 明确的 true 同样按可用处理：警示消失
    call.mockImplementation((method: string) => {
      if (method === 'cloudapi.status') return Promise.resolve({ running: false });
      if (method === 'cloud.stats') return Promise.resolve({ running: true, pending: 0, dropped: 0, storageAvailable: true });
      return Promise.resolve({ ok: true });
    });
    await pollTick(wrapper);
    expect(wrapper.find('[data-testid="cloud-storage-warning"]').exists()).toBe(false);
  });

  it('resets the capture state when the backend reports it is not capturing', async () => {
    vi.useFakeTimers();
    call.mockImplementation((method: string) => {
      if (method === 'cloudapi.status') return Promise.resolve({ running: false });
      if (method === 'cloud.stats') return Promise.resolve({ running: false, pending: 0, dropped: 0 });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountCloudView();
    await wrapper.get('[data-testid="cloud-capture-toggle"]').trigger('click');
    await flushPromises();

    expect(wrapper.get('[data-testid="cloud-capture-status"]').classes()).toContain('off');
    expect(wrapper.get('[data-testid="cloud-capture-toggle"]').attributes('disabled')).toBeUndefined();
    expect(wrapper.get('[role="alert"]').text()).toContain('未进入捕获状态');
    await vi.advanceTimersByTimeAsync(4000);
    expect(pollCalls()).toBe(0);
  });

  it('reports polling failures separately from action errors', async () => {
    vi.useFakeTimers();
    call.mockImplementation((method: string) => {
      if (method === 'cloudapi.status') return Promise.resolve({ running: false });
      if (method === 'cloud.poll') return Promise.reject(new Error('poll failed'));
      return Promise.resolve({ running: true, dropped: 0 });
    });
    const wrapper = mountCloudView();
    const store = useEngineStore();
    store.cloudCapturing = true;
    await pollTick(wrapper);

    expect(wrapper.get('[data-testid="cloud-poll-error"]').text()).toContain('poll failed');
    expect(wrapper.find('[data-testid="cloud-action-error"]').exists()).toBe(false);
  });

  it('starts and stops polling together with the store capture flag', async () => {
    vi.useFakeTimers();
    mountCloudView();
    const store = useEngineStore();

    store.cloudCapturing = true;
    await vi.advanceTimersByTimeAsync(2100);
    expect(pollCalls()).toBeGreaterThan(0);

    // 引擎掉线会把 cloudCapturing 置回 false：轮询必须跟着停，不能每 2s 重刷错误
    store.cloudCapturing = false;
    await vi.advanceTimersByTimeAsync(100);
    const settled = pollCalls();
    await vi.advanceTimersByTimeAsync(6000);
    expect(pollCalls()).toBe(settled);
  });

  it('calls functions and replays container requests with parsed JSON', async () => {
    vi.useFakeTimers();
    const wrapper = mountCloudView();
    await flushPromises();
    await wrapper.get('[data-testid="call-name"]').setValue('sum');
    await wrapper.get('[data-testid="call-data"]').setValue('{"a": 1}');
    await wrapper.get('[data-testid="call-cloud"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('cloud.call', { name: 'sum', data: { a: 1 } });

    listeners.cloud_capture?.({ type: 'container', name: '/api', data: { method: 'GET', header: { a: 'b' }, data: { q: 1 } } });
    await flushQueue(wrapper);
    await wrapper.get('[data-testid="cloud-record-0"]').trigger('click');
    await wrapper.get('[data-testid="replay-cloud"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('cloud.call_container', { path: '/api', method: 'GET', header: { a: 'b' }, data: { q: 1 } });
    // 重放结束后按钮必须恢复可用，否则只能刷新页面才能再次重放
    expect(wrapper.get('[data-testid="replay-cloud"]').attributes('disabled')).toBeUndefined();
    expect(wrapper.get('[data-testid="replay-cloud"]').text()).toBe('重放请求');
  });

  it('controls the cloud API service with port validation and renders backend errors', async () => {
    vi.useFakeTimers();
    const wrapper = mountCloudView();
    await flushPromises();

    // 端口越界：启动按钮保持禁用（不发调用），并在输入旁给出原因
    await wrapper.get('[data-testid="cloudapi-port"]').setValue('10');
    expect(wrapper.get('[data-testid="cloudapi-toggle"]').attributes('disabled')).toBeDefined();
    expect(wrapper.get('.api-port ~ .error').text()).toContain('1024-65535');
    await wrapper.get('[data-testid="cloudapi-toggle"]').trigger('click');
    await flushPromises();
    expect(call).not.toHaveBeenCalledWith('cloudapi.start', expect.anything());

    call.mockImplementationOnce(() => Promise.reject(new Error('port busy')));
    await wrapper.get('[data-testid="cloudapi-port"]').setValue('19000');
    await wrapper.get('[data-testid="cloudapi-toggle"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('cloudapi.start', { port: 19000 });
    expect(wrapper.get('[data-testid="cloud-action-error"]').text()).toContain('port busy');
  });

  it('exports captured records with progress updates and copies details', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(window.navigator, 'clipboard', { value: { writeText }, configurable: true });
    const wrapper = mountCloudView();
    await flushPromises();
    const progressListener = listeners.export_progress as ((payload: { status?: string; current?: number; total?: number; path?: string; message?: string }) => void) | undefined;
    expect(progressListener).toBeTypeOf('function');

    listeners.cloud_capture?.({ rid: 'r1', type: 'function', name: 'hello', data: { id: 1 } });
    await vi.waitFor(() => expect(wrapper.find('[data-testid="cloud-record-0"]').exists()).toBe(true));
    await wrapper.get('[data-testid="cloud-record-0"]').trigger('click');
    await wrapper.get('[data-testid="export-cloud"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('cloud.export', { items: [expect.objectContaining({ name: 'hello' })] });
    expect(wrapper.get('[data-testid="copy-detail"]').isVisible()).toBe(true);

    progressListener?.({ status: 'working', current: 1, total: 2 });
    await wrapper.vm.$nextTick();
    expect(wrapper.text()).toContain('导出中 1/2');
    progressListener?.({ status: 'done', path: 'C:/out/report.xlsx' });
    await wrapper.vm.$nextTick();
    expect(wrapper.text()).toContain('C:/out/report.xlsx');

    // 导出完成后可以直接打开所在目录
    await wrapper.get('[data-testid="open-export-folder"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('shell.openFolder', { path: 'C:/out' });

    await wrapper.get('[data-testid="copy-detail"]').trigger('click');
    await flushPromises();
    expect(writeText).toHaveBeenCalledWith(expect.stringContaining('"hello"'));
  });

  it('shows export failures surfaced through export_progress', async () => {
    const wrapper = mountCloudView();
    await flushPromises();
    const progressListener = listeners.export_progress as ((payload: { status?: string; message?: string }) => void) | undefined;
    progressListener?.({ status: 'error', message: 'disk full' });
    await wrapper.vm.$nextTick();
    expect(wrapper.get('[data-testid="cloud-action-error"]').text()).toContain('disk full');
  });

  it('re-enables the export button when the save dialog is cancelled', async () => {
    const wrapper = mountCloudView();
    await flushPromises();
    listeners.cloud_capture?.({ rid: 'r1', type: 'function', name: 'hello', data: { id: 1 } });
    await vi.waitFor(() => expect(wrapper.find('[data-testid="cloud-record-0"]').exists()).toBe(true));

    // 用户取消保存对话框时 Go 返回 {ok:false, reason:'用户取消'} 且不发 export_progress：
    // 只有被受理（async:true）的导出才由进度事件收尾，其余的要当场解锁。
    call.mockImplementationOnce(() => Promise.resolve({ ok: false, reason: '用户取消' }));
    await wrapper.get('[data-testid="export-cloud"]').trigger('click');
    await flushPromises();

    expect(call).toHaveBeenCalledWith('cloud.export', { items: [expect.objectContaining({ name: 'hello' })] });
    expect(wrapper.get('[data-testid="export-cloud"]').attributes('disabled')).toBeUndefined();
    expect(wrapper.text()).not.toContain('准备导出');

    // 再点一次（这次被受理）：按钮重新禁用，直到 export_progress 收尾
    call.mockImplementationOnce(() => Promise.resolve({ ok: true, async: true }));
    await wrapper.get('[data-testid="export-cloud"]').trigger('click');
    await flushPromises();
    expect(wrapper.get('[data-testid="export-cloud"]').attributes('disabled')).toBeDefined();
    listeners.export_progress?.({ status: 'done', path: 'C:/out/report.xlsx' });
    await wrapper.vm.$nextTick();
    expect(wrapper.get('[data-testid="export-cloud"]').attributes('disabled')).toBeUndefined();
  });

  it('does not recreate polling after unmount while capture is starting', async () => {
    vi.useFakeTimers();
    let resolveStart: (() => void) | undefined;
    call.mockImplementation((method: string) => {
      if (method === 'cloudapi.status') return Promise.resolve({ running: false });
      if (method === 'cloud.start') return new Promise<void>((resolve) => { resolveStart = resolve; });
      return Promise.resolve({ ok: true });
    });
    const setIntervalSpy = vi.spyOn(window, 'setInterval');
    const wrapper = mountCloudView();
    await flushPromises();

    await wrapper.get('[data-testid="cloud-capture-toggle"]').trigger('click');
    wrapper.unmount();
    resolveStart?.();
    await flushPromises();

    expect(setIntervalSpy).not.toHaveBeenCalled();
  });

  it('does not overlap cloud polling while a poll is pending', async () => {
    vi.useFakeTimers();
    let resolvePoll: ((value: unknown) => void) | undefined;
    call.mockImplementation((method: string) => {
      if (method === 'cloudapi.status') return Promise.resolve({ running: false });
      if (method === 'cloud.poll') return new Promise((resolve) => { resolvePoll = resolve; });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountCloudView();
    await flushPromises();

    await wrapper.get('[data-testid="cloud-capture-toggle"]').trigger('click');
    await flushPromises();
    await vi.advanceTimersByTimeAsync(2000);
    await vi.advanceTimersByTimeAsync(4000);
    expect(pollCalls()).toBe(1);

    resolvePoll?.([]);
    await flushPromises();
  });

  // 与 WxApi 同一套高度模型：记录身份、底部重放/复制按钮固定，只有 .detail-body 滚。
  it('keeps the detail meta and actions outside the scrolling body', async () => {
    call.mockImplementationOnce(() => Promise.resolve({ running: false })).mockImplementationOnce(() => Promise.resolve({
      items: [{ app_id: 'wx1', type: 'function', name: 'hello', data: { id: 1 }, status: 'success', timestamp: '10:00' }],
    }));
    const wrapper = mountCloudView();
    await flushPromises();
    await wrapper.get('[data-testid="scan-cloud"]').trigger('click');
    await flushPromises();
    await wrapper.get('[data-testid="cloud-record-0"]').trigger('click');

    const body = wrapper.get('[data-testid="cloud-detail"] .detail-body');
    expect(body.text()).toContain('"id": 1');
    expect(body.find('.detail-meta').exists()).toBe(false);
    expect(body.find('.detail-actions').exists()).toBe(false);
    expect(wrapper.find('[data-testid="cloud-detail"] > .detail-meta').exists()).toBe(true);
    expect(wrapper.find('[data-testid="cloud-detail"] > .detail-actions').exists()).toBe(true);
  });

  // 密度开关（数据多的列表共用）：默认紧凑，关掉后列表不再带紧凑类。
  it('defaults to compact rows and drops the class when the toggle is cleared', async () => {
    const wrapper = mountCloudView();
    await flushPromises();
    const list = () => wrapper.get('.record-list');
    expect(wrapper.get('[data-testid="cloud-density"]').element).toHaveProperty('checked', true);
    expect(list().classes()).toContain('is-compact');

    await wrapper.get('[data-testid="cloud-density"]').setValue(false);
    expect(list().classes()).not.toContain('is-compact');
  });
});
