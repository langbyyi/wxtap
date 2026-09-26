import { createTestingPinia } from '@pinia/testing';
import { flushPromises, mount } from '@vue/test-utils';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { useEngineStore } from '../stores/engine';
import ConsoleView from './ConsoleView.vue';
import PageHeader from '../components/PageHeader.vue';

const { call, copyText, notify } = vi.hoisted(() => ({ call: vi.fn(), copyText: vi.fn(), notify: vi.fn() }));
vi.mock('../api/bridge', () => ({ backend: { call } }));
vi.mock('../utils/notify', () => ({ copyText, notify }));

function mountView() {
  return mount(ConsoleView, { global: { plugins: [createTestingPinia({ createSpy: vi.fn, stubActions: false })] } });
}

afterEach(() => {
  vi.useRealTimers();
  call.mockReset();
  copyText.mockReset();
  notify.mockReset();
});

describe('ConsoleView', () => {
  it('uses the shared page header contract', () => {
    const wrapper = mountView();
    expect(wrapper.findComponent(PageHeader).exists()).toBe(true);
    expect(wrapper.find('.page-toolbar').exists()).toBe(false);
    wrapper.unmount();
  });

  it('renders the records the shell drained and their levels', async () => {
    call.mockResolvedValue({
      records: [
        { seq: 1, record: { level: 'log', text: 'hello', timestamp: '10:00:00' } },
        { seq: 2, record: { level: 'error', text: 'boom', timestamp: '10:00:01', extra: { source: 'app.js', line: '12' } } },
      ],
      nextSeq: 2,
    });
    const wrapper = mountView();
    await flushPromises();

    const list = wrapper.get('[data-testid="console-list"]');
    expect(list.text()).toContain('hello');
    expect(list.text()).toContain('boom');
    expect(list.text()).toContain('source: app.js');
    expect(wrapper.get('[data-testid="console-count"]').text()).toContain('2 / 500');
    wrapper.unmount();
  });

  // 环形缓冲跨小程序共享：切换目标后旧应用的记录还在列表里，appId 与当前
  // 应用不同时要标出来，免得两段输出被误读成同一个页面打的。
  it('annotates records that came from another miniapp', async () => {
    call.mockResolvedValue({
      records: [
        { seq: 1, record: { level: 'log', text: 'previous app', appId: 'wxold' } },
        { seq: 2, record: { level: 'log', text: 'current app', appId: 'wxcurrent' } },
        { seq: 3, record: { level: 'log', text: 'no appid yet' } },
      ],
      nextSeq: 3,
    });
    const wrapper = mountView();
    // mount 之后才能拿到组件同一个 pinia 实例里的 store。
    const store = useEngineStore();
    store.status = { ...store.status, appInfo: { appid: 'wxcurrent', name: '当前应用' } };
    await flushPromises();

    const list = wrapper.get('[data-testid="console-list"]');
    expect(list.text()).toContain('appId: wxold');
    expect(list.text()).not.toContain('appId: wxcurrent');
    wrapper.unmount();
  });

  it('filters by level and by keyword', async () => {
    call.mockResolvedValue({
      records: [
        { seq: 1, record: { level: 'log', text: 'request ok' } },
        { seq: 2, record: { level: 'error', text: 'request failed' } },
      ],
      nextSeq: 2,
    });
    const wrapper = mountView();
    await flushPromises();

    await wrapper.get('[aria-label="日志级别"]').setValue('error');
    expect(wrapper.get('[data-testid="console-list"]').text()).not.toContain('request ok');

    await wrapper.get('[aria-label="日志级别"]').setValue('all');
    await wrapper.get('[aria-label="搜索 Console 日志"]').setValue('failed');
    expect(wrapper.get('[data-testid="console-list"]').text()).toContain('request failed');
    expect(wrapper.get('[data-testid="console-list"]').text()).not.toContain('request ok');
    wrapper.unmount();
  });

  it('copies only the currently filtered records', async () => {
    call.mockResolvedValue({
      records: [
        { seq: 1, record: { level: 'log', text: 'kept', timestamp: '10:00:00' } },
        { seq: 2, record: { level: 'debug', text: 'dropped', timestamp: '10:00:01' } },
      ],
      nextSeq: 2,
    });
    const wrapper = mountView();
    await flushPromises();

    await wrapper.get('[aria-label="日志级别"]').setValue('log');
    await wrapper.get('[data-testid="copy-console"]').trigger('click');
    await flushPromises();

    expect(copyText).toHaveBeenCalledWith('10:00:00 [log] kept', 'Console 日志');
    wrapper.unmount();
  });

  it('clears through the store so the page-side buffer and marker follow', async () => {
    call.mockImplementation((method: string) => Promise.resolve(
      method === 'console.clear' ? { ok: true } : { records: [{ seq: 1, record: { level: 'log', text: 'old' } }], nextSeq: 1 },
    ));
    const wrapper = mountView();
    await flushPromises();
    expect(wrapper.text()).toContain('old');

    await wrapper.get('[data-testid="clear-console"]').trigger('click');
    await flushPromises();

    expect(call).toHaveBeenCalledWith('console.clear');
    expect(useEngineStore().consoleRecords).toEqual([]);
    expect(wrapper.text()).toContain('暂无 Console 输出');
    wrapper.unmount();
  });

  // 暂停只是停止轮询：拉取按序号进行，后端照常缓冲，恢复时不会丢行。
  it('pauses polling without dropping the buffered records', async () => {
    vi.useFakeTimers();
    call.mockResolvedValue({ records: [{ seq: 1, record: { level: 'log', text: 'first' } }], nextSeq: 1 });
    const wrapper = mountView();
    await flushPromises();
    const pollsAfterMount = call.mock.calls.filter(([method]) => method === 'console.list').length;
    expect(pollsAfterMount).toBeGreaterThan(0);

    await wrapper.get('[data-testid="pause-console"]').trigger('click');
    call.mockClear();
    await vi.advanceTimersByTimeAsync(3000);
    expect(call.mock.calls.filter(([method]) => method === 'console.list')).toHaveLength(0);

    await wrapper.get('[data-testid="pause-console"]').trigger('click');
    call.mockResolvedValue({ records: [], nextSeq: 1 });
    await vi.advanceTimersByTimeAsync(1000);
    expect(call.mock.calls.filter(([method]) => method === 'console.list')).toHaveLength(1);
    wrapper.unmount();
  });
});
