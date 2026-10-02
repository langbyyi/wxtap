import { createTestingPinia } from '@pinia/testing';
import { flushPromises, mount } from '@vue/test-utils';
import { describe, expect, it, vi } from 'vitest';
import { useEngineStore } from '../stores/engine';
import DevtoolsView from './DevtoolsView.vue';
import PageHeader from '../components/PageHeader.vue';
import { router } from '../router';

const { call, copyText } = vi.hoisted(() => ({ call: vi.fn(), copyText: vi.fn() }));
vi.mock('../api/bridge', () => ({ backend: { call } }));
vi.mock('../utils/notify', () => ({ copyText, notify: vi.fn() }));

function mountView(callImpl?: (method: string) => Promise<unknown>) {
  if (callImpl) call.mockImplementation((method: string) => callImpl(method));
  return mount(DevtoolsView, { global: { plugins: [createTestingPinia({ createSpy: vi.fn, stubActions: false }), router] } });
}

describe('DevtoolsView', () => {
  it('uses live H5 navigation in the list, search and copied details and keeps disconnect available on a container', async () => {
    vi.useFakeTimers();
    let url = 'https://example.com/pay'; let title = '支付页'; let active = false;
    const wrapper = mountView((method) => {
      if (method === 'targets.list') return Promise.resolve({ clientId: 7, locked: true, targets: [{ targetId: 'h5-pay', type: 'page', url: 'https://example.com/pay', title: '支付页' }] });
      if (method === 'electron.status') return Promise.resolve({ path: 'C:/electron.exe' });
      if (method === 'targets.sessions') return Promise.resolve({ sessions: [{ clientId: 7, targetId: 'h5-pay', state: active ? 'connected' : 'closed', active, url, title }] });
      if (method === 'targets.probe') return Promise.resolve({ clientId: 7, targetId: 'h5-pay', verified: true, released: true, url, title, readyState: 'complete' });
      if (method === 'shell.openDevtoolsWindow') active = true;
      if (method === 'targets.close') active = false;
      return Promise.resolve({});
    });
    try {
      useEngineStore().status.miniapp = true; await flushPromises();
      await wrapper.get('[data-testid="target-probe-h5-pay"]').trigger('click'); await flushPromises();
      await wrapper.get('[data-testid="target-debug-h5-pay"]').trigger('click'); await flushPromises();
      url = 'https://example.com/receipt'; title = '支付结果';
      await vi.advanceTimersByTimeAsync(1000); await flushPromises();
      expect(wrapper.get('[data-testid="target-row"]').text()).toContain(url);
      expect(wrapper.get('[data-testid="target-row"]').text()).toContain(title);
      await wrapper.get('[data-testid="target-search"]').setValue('receipt');
      expect(wrapper.findAll('[data-testid="target-row"]')).toHaveLength(1);
      await wrapper.get('[data-testid="target-select-h5-pay"]').trigger('click');
      await wrapper.get('[data-testid="target-copy"]').trigger('click'); await flushPromises();
      const copied = JSON.parse(copyText.mock.calls.at(-1)![0]);
      expect(copied.target).toMatchObject({ url, title });
      expect(copied.verification).toBeNull();
      await wrapper.get('[data-testid="target-search"]').setValue('');
      url = 'https://liteapp.weixin.qq.com/'; title = '';
      await vi.advanceTimersByTimeAsync(1000); await flushPromises();
      expect(wrapper.get('[data-testid="target-row"]').text()).toContain('微信容器');
      expect(wrapper.get('[data-testid="target-summary"]').text()).toContain('0 个 H5 候选');
      expect(wrapper.get('[data-testid="target-debug-h5-pay"]').text()).toBe('断开');
      expect(wrapper.find('[data-testid="target-probe-h5-pay"]').exists()).toBe(false);
      await wrapper.get('[data-testid="target-debug-h5-pay"]').trigger('click'); await flushPromises();
      expect(wrapper.find('[data-testid="target-debug-h5-pay"]').exists()).toBe(false);
      expect(wrapper.get('[data-testid="target-row"]').text()).toContain(url);
    } finally { wrapper.unmount(); vi.useRealTimers(); }
  });

  it('lists the WeChat container without offering H5 actions or counting it as a candidate', async () => {
    const wrapper = mountView((method) => method === 'targets.list' ? Promise.resolve({ clientId: 7, locked: true, targets: [
      { targetId: 'container', type: 'page', title: '微信页面', url: 'https://liteapp.weixin.qq.com/' },
      { targetId: 'business', type: 'page', title: '业务页面', url: 'https://example.com/pay' },
    ] }) : Promise.resolve({}));
    try {
      useEngineStore().status.miniapp = true; await flushPromises();
      expect(wrapper.findAll('[data-testid="target-row"]')).toHaveLength(2);
      expect(wrapper.get('[data-testid="target-summary"]').text()).toContain('1 个 H5 候选');
      expect(wrapper.find('[data-testid="target-debug-container"]').exists()).toBe(false);
      expect(wrapper.find('[data-testid="target-probe-container"]').exists()).toBe(false);
      expect(wrapper.get('[data-testid="target-debug-business"]').text()).toBe('调试');
      expect(wrapper.get('[data-testid="target-row"]').text()).toContain('微信容器');
    } finally { wrapper.unmount(); }
  });

  it('keeps an existing container session disconnectable without allowing it to be probed', async () => {
    let active = true;
    const wrapper = mountView((method) => {
      if (method === 'targets.list') return Promise.resolve({ clientId: 7, locked: true, targets: [
        { targetId: 'container', type: 'page', title: '微信页面', url: 'https://liteapp.weixin.qq.com/' },
      ] });
      if (method === 'targets.sessions') return Promise.resolve({ sessions: [{ clientId: 7, targetId: 'container', state: active ? 'connected' : 'closed', active }] });
      if (method === 'targets.close') active = false;
      return Promise.resolve({});
    });
    try {
      useEngineStore().status.miniapp = true; await flushPromises();
      expect(wrapper.get('[data-testid="target-debug-container"]').text()).toBe('断开');
      expect(wrapper.find('[data-testid="target-probe-container"]').exists()).toBe(false);
      await wrapper.get('[data-testid="target-debug-container"]').trigger('click'); await flushPromises();
      expect(call).toHaveBeenCalledWith('targets.close', { clientId: 7, targetId: 'container' });
      expect(wrapper.find('[data-testid="target-debug-container"]').exists()).toBe(false);
    } finally { wrapper.unmount(); }
  });

  it('opens the discovered H5 source and shows connection only after Core confirms it', async () => {
    vi.useFakeTimers();
    let sessions: unknown[] = [];
    call.mockImplementation((method: string) => {
      if (method === 'targets.list') return Promise.resolve({ clientId: 7, locked: true, targets: [{ targetId: 'h5-pay', type: 'page', url: 'https://example.com/pay' }] });
      if (method === 'electron.status') return Promise.resolve({ path: 'C:\\electron.exe' });
      if (method === 'targets.sessions') return Promise.resolve({ sessions });
      if (method === 'debugger.pausePolicy') return Promise.resolve({ clientId: 7, known: true });
      if (method === 'targets.close') { sessions = [{ clientId: 7, targetId: 'h5-pay', state: 'closed', active: false }]; }
      return Promise.resolve({});
    });
    const wrapper = mountView();
    try {
      useEngineStore().status.miniapp = true; await flushPromises();
      await wrapper.get('[data-testid="target-debug-h5-pay"]').trigger('click'); await flushPromises();
      expect(call).toHaveBeenCalledWith('shell.openDevtoolsWindow', { cdp_port: Number(useEngineStore().cdpPort), electron_path: 'C:\\electron.exe', client_id: 7, target_id: 'h5-pay' });
      expect(wrapper.get('[data-testid="target-debug-h5-pay"]').text()).toBe('调试');
      sessions = [{ clientId: 7, targetId: 'h5-pay', state: 'connected', active: true, url: 'https://example.com/pay', title: '支付页', error: '' }];
      await vi.advanceTimersByTimeAsync(1000); await flushPromises();
      expect(wrapper.get('[data-testid="target-debug-h5-pay"]').text()).toBe('断开');
      expect(wrapper.get('[data-testid="target-probe-h5-pay"]').attributes('disabled')).toBeDefined();
      await wrapper.get('[data-testid="target-select-h5-pay"]').trigger('click');
      expect(wrapper.get('[data-testid="target-detail"]').text()).toContain('已连接');
      await wrapper.get('[data-testid="target-debug-h5-pay"]').trigger('click'); await flushPromises();
      expect(call).toHaveBeenCalledWith('targets.close', { clientId: 7, targetId: 'h5-pay' });
      expect(wrapper.get('[data-testid="target-debug-h5-pay"]').text()).toBe('调试');
    } finally { wrapper.unmount(); vi.useRealTimers(); call.mockClear(); }
  });

  it('keeps failed release retryable and ignores a session belonging to another source', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'targets.list') return Promise.resolve({ clientId: 7, locked: true, targets: [{ targetId: 'h5-pay', type: 'page', url: 'https://example.com/pay' }] });
      if (method === 'targets.sessions') return Promise.resolve({ sessions: [
        { clientId: 7, targetId: 'h5-pay', state: 'error', active: true, error: 'detach failed' },
        { clientId: 8, targetId: 'h5-pay', state: 'connected', active: true },
      ] });
      if (method === 'targets.close') return Promise.reject(new Error('仍未确认释放'));
      return Promise.resolve({});
    });
    const wrapper = mountView(); useEngineStore().status.miniapp = true; await flushPromises();
    expect(wrapper.get('[data-testid="target-debug-h5-pay"]').text()).toBe('重试释放');
    await wrapper.get('[data-testid="target-debug-h5-pay"]').trigger('click'); await flushPromises();
    expect(wrapper.get('[data-testid="target-debug-h5-pay"]').text()).toBe('重试释放');
    await wrapper.get('[data-testid="target-select-h5-pay"]').trigger('click');
    expect(wrapper.get('[data-testid="target-detail"]').text()).toContain('仍未确认释放');
    wrapper.unmount();
  });

  it('rediscovers when the lock changes while names are still loading and discards the old list', async () => {
    vi.useFakeTimers();
    let clientId = 7;
    let holdNames = false;
    let releaseNames!: (value: unknown) => void;
    const wrapper = mountView((method) => {
      if (method === 'targets.list') return Promise.resolve({ clientId, locked: true, targets: [{ targetId: `h5-${clientId}`, type: 'page', url: 'https://example.com' }] });
      if (method === 'miniapp.list' && holdNames) {
        holdNames = false;
        return new Promise((resolve) => { releaseNames = resolve; });
      }
      if (method === 'debugger.pausePolicy') return Promise.resolve({ clientId, known: true });
      if (method === 'engine.status') return Promise.resolve({ frida: true, miniapp: true });
      return Promise.resolve({});
    });
    try {
      useEngineStore().status.miniapp = true;
      await flushPromises();
      holdNames = true;
      await wrapper.get('.status-row button').trigger('click');
      await flushPromises();
      clientId = 8;
      await vi.advanceTimersByTimeAsync(1000);
      await flushPromises();
      releaseNames({ list: [{ id: 7, locked: true }] });
      await flushPromises();
      expect(wrapper.get('[data-testid="target-row"]').text()).toContain('h5-8');
      expect(wrapper.get('[data-testid="target-row"]').text()).not.toContain('h5-7');
      await wrapper.get('[data-testid="target-probe-h5-8"]').trigger('click');
      expect(call).toHaveBeenLastCalledWith('targets.probe', { clientId: 8, targetId: 'h5-8' });
    } finally {
      wrapper.unmount();
      vi.useRealTimers();
    }
  });

  it('filters targets locally by role and case-insensitive title, URL or ID', async () => {
    const wrapper = mountView((method) => method === 'targets.list' ? Promise.resolve({ clientId: 7, locked: true, targets: [
      { targetId: 'h5-a', type: 'page', title: 'Checkout', url: 'https://example.com/pay' },
      { targetId: 'worker-a', type: 'worker', title: 'worker', url: 'https://example.com/task.js' },
    ] }) : Promise.resolve({}));
    useEngineStore().status.miniapp = true;
    await flushPromises();
    const requests = call.mock.calls.length;
    await wrapper.get('[data-testid="target-role-filter"]').setValue('H5 候选');
    expect(wrapper.findAll('[data-testid="target-row"]')).toHaveLength(1);
    await wrapper.get('[data-testid="target-search"]').setValue('CHECKOUT');
    expect(wrapper.get('[data-testid="target-row"]').text()).toContain('h5-a');
    await wrapper.get('[data-testid="target-search"]').setValue('example.com/pay');
    expect(wrapper.findAll('[data-testid="target-row"]')).toHaveLength(1);
    await wrapper.get('[data-testid="target-search"]').setValue('H5-A');
    expect(wrapper.findAll('[data-testid="target-row"]')).toHaveLength(1);
    await wrapper.get('[data-testid="target-search"]').setValue('missing');
    expect(wrapper.findAll('[data-testid="target-row"]')).toHaveLength(0);
    expect(wrapper.get('[data-testid="target-summary"]').text()).toContain('0 / 2');
    expect(call.mock.calls).toHaveLength(requests);
    wrapper.unmount();
  });

  it('uses the discovery source rather than a separate miniapp-list lock for H5 verification', async () => {
    const wrapper = mountView((method) => {
      if (method === 'targets.list') return Promise.resolve({ clientId: 7, locked: true, targets: [{ targetId: 'h5-a', type: 'page', url: 'https://example.com' }] });
      if (method === 'miniapp.list') return Promise.resolve({ list: [{ id: 8, locked: true }] });
      return Promise.resolve({});
    });
    useEngineStore().status.miniapp = true;
    await flushPromises();
    await wrapper.get('[data-testid="target-probe-h5-a"]').trigger('click');
    expect(call).toHaveBeenLastCalledWith('targets.probe', { clientId: 7, targetId: 'h5-a' });
    wrapper.unmount();
  });

  it('keeps verification disabled for an unlocked discovery source despite a stale miniapp-list lock', async () => {
    const wrapper = mountView((method) => {
      if (method === 'targets.list') return Promise.resolve({ clientId: 7, locked: false, targets: [{ targetId: 'h5-a', type: 'page', url: 'https://example.com' }] });
      if (method === 'miniapp.list') return Promise.resolve({ list: [{ id: 7, locked: true }] });
      return Promise.resolve({});
    });
    useEngineStore().status.miniapp = true;
    await flushPromises();
    expect(wrapper.get('[data-testid="target-probe-h5-a"]').attributes('disabled')).toBeDefined();
    wrapper.unmount();
  });

  it('shows and copies full target data with actual verified page information, clearing it on refresh', async () => {
    const wrapper = mountView((method) => {
      if (method === 'targets.list') return Promise.resolve({ clientId: 7, locked: true, targets: [{ targetId: 'h5-a', type: 'page', title: '旧标题', url: 'https://example.com/old' }] });
      if (method === 'engine.status') return Promise.resolve({ frida: true, miniapp: true });
      if (method === 'targets.probe') return Promise.resolve({ clientId: 7, targetId: 'h5-a', verified: true, released: true, title: '支付页', url: 'https://example.com/pay', readyState: 'complete' });
      return Promise.resolve({});
    });
    useEngineStore().status.miniapp = true;
    await flushPromises();
    await wrapper.get('[data-testid="target-select-h5-a"]').trigger('click');
    await wrapper.get('[data-testid="target-probe-h5-a"]').trigger('click');
    await flushPromises();
    const detail = wrapper.get('[data-testid="target-detail"]');
    expect(detail.text()).toContain('https://example.com/pay');
    expect(detail.text()).toContain('支付页');
    expect(detail.text()).toContain('complete');
    await wrapper.get('[data-testid="target-copy"]').trigger('click');
    const copied = JSON.parse(copyText.mock.lastCall![0]);
    expect(copied).toMatchObject({ clientId: 7, target: { targetId: 'h5-a' }, verification: { ok: true, url: 'https://example.com/pay', released: true } });
    await wrapper.get('.status-row button').trigger('click');
    await flushPromises();
    expect(wrapper.find('[data-testid="target-detail"]').exists()).toBe(false);
    wrapper.unmount();
  });

  it('does not apply a probe result after the discovery list is refreshed', async () => {
    let resolve!: (value: unknown) => void;
    call.mockImplementation((method: string) => {
      if (method === 'targets.list') return Promise.resolve({ clientId: 7, locked: true, targets: [{ targetId: 'h5-1', type: 'page', url: 'https://example.com/test' }] });
      if (method === 'miniapp.list') return Promise.resolve({ list: [{ id: 7, locked: true }] });
      if (method === 'targets.probe') return new Promise((done) => { resolve = done; });
      if (method === 'engine.status') return Promise.resolve({ frida: true, miniapp: true });
      return Promise.resolve({});
    });
    const wrapper = mountView();
    useEngineStore().status.frida = true;
    await flushPromises();
    await wrapper.get('[data-testid="target-probe-h5-1"]').trigger('click');
    await wrapper.get('.status-row button').trigger('click');
    await flushPromises();
    resolve({ clientId: 7, targetId: 'h5-1', verified: true, released: true, title: '旧验证', readyState: 'complete' });
    await flushPromises();
    expect(wrapper.get('[data-testid="target-row"]').text()).not.toContain('验证成功');
    wrapper.unmount();
  });
  it('verifies one H5 through its locked source and waits for confirmed cleanup', async () => {
    const wrapper = mountView((method) => {
      if (method === 'targets.list') return Promise.resolve({ clientId: 7, locked: true, targets: [{ targetId: 'h5-1', type: 'page', url: 'https://example.com/test' }] });
      if (method === 'miniapp.list') return Promise.resolve({ list: [{ id: 7, locked: true, appid: 'wxaaa111' }] });
      return Promise.resolve({});
    });
    useEngineStore().status.frida = true;
    await flushPromises();
    let resolve!: (value: unknown) => void;
    call.mockImplementation(() => new Promise((done) => { resolve = done; }));
    await wrapper.get('[data-testid="target-probe-h5-1"]').trigger('click');
    expect(call).toHaveBeenLastCalledWith('targets.probe', { clientId: 7, targetId: 'h5-1' });
    expect(wrapper.get('[data-testid="target-probe-h5-1"]').attributes('disabled')).toBeDefined();
    expect(wrapper.get('[data-testid="target-row"]').text()).not.toContain('验证成功');
    resolve({ clientId: 7, targetId: 'h5-1', verified: true, released: true, url: 'https://example.com/test', title: '自有页面', readyState: 'complete' });
    await flushPromises();
    expect(wrapper.get('.probe-result').text()).toBe('通过');
    expect(wrapper.get('.probe-result').attributes('title')).toContain('临时会话已释放');
    wrapper.unmount();
  });

  it('reports unconfirmed cleanup as failure and keeps retry available', async () => {
    const wrapper = mountView((method) => {
      if (method === 'targets.list') return Promise.resolve({ clientId: 7, locked: true, targets: [{ targetId: 'h5-1', type: 'page', url: 'https://example.com/test' }] });
      if (method === 'miniapp.list') return Promise.resolve({ list: [{ id: 7, locked: true }] });
      return Promise.resolve({});
    });
    useEngineStore().status.frida = true;
    await flushPromises();
    call.mockRejectedValue(new Error('临时会话释放失败：detach failed'));
    await wrapper.get('[data-testid="target-probe-h5-1"]').trigger('click');
    await flushPromises();
    expect(wrapper.get('.probe-result').attributes('title')).toContain('临时会话释放失败');
    expect(wrapper.get('[data-testid="target-row"]').text()).not.toContain('验证成功');
    expect(wrapper.get('[data-testid="target-probe-h5-1"]').attributes('disabled')).toBeUndefined();
    wrapper.unmount();
  });

  it('shows every discovered target and distinguishes H5 candidates without offering unverified attachment', async () => {
    const wrapper = mountView((method) => {
      if (method === 'targets.list') return Promise.resolve({ targets: [
        { targetId: 'mini', type: 'page', title: 'AppIndex', url: 'https://servicewechat.com/wxabc1234567890a/8/page-frame.html' },
        { targetId: 'h5', type: 'page', title: '自有 H5', url: 'https://example.com/test' },
        { targetId: 'worker', type: 'worker', title: 'Worker', url: 'https://example.com/worker.js' },
      ] });
      return Promise.resolve({});
    });
    useEngineStore().status.frida = true;
    await flushPromises();
    expect(wrapper.get('[data-testid="target-summary"]').text()).toContain('3 个目标');
    expect(wrapper.get('[data-testid="target-summary"]').text()).toContain('1 个 H5 候选');
    const rows = wrapper.findAll('[data-testid="target-row"]');
    expect(rows).toHaveLength(3);
    expect(rows[1]!.text()).toContain('自有 H5');
    expect(rows[1]!.text()).toContain('https://example.com/test');
    expect(rows[2]!.text()).toContain('逻辑层');
    expect(wrapper.get('th:last-child').attributes('title')).toContain('不表示常驻调试已连接');
    expect(call).not.toHaveBeenCalledWith('targets.attach', expect.anything());
    wrapper.unmount();
  });

  it('clears stale discovery results on failure and does not report H5 absence as a successful query', async () => {
    let fail = false;
    const wrapper = mountView((method) => {
      if (method === 'targets.list') return Promise.resolve(fail
        ? { targets: [], error: 'Target.getTargets unsupported' }
        : { targets: [{ targetId: 'h5', type: 'page', url: 'https://example.com/test' }] });
      if (method === 'engine.status') return Promise.resolve({ frida: true, miniapp: true });
      return Promise.resolve({});
    });
    useEngineStore().status.frida = true;
    await flushPromises();
    expect(wrapper.findAll('[data-testid="target-row"]')).toHaveLength(1);
    fail = true;
    await wrapper.get('.status-row button').trigger('click');
    await flushPromises();
    const panel = wrapper.get('[data-testid="target-diagnostics"]');
    expect(panel.text()).toContain('Target.getTargets unsupported');
    expect(panel.text()).not.toContain('暂未暴露 H5');
    expect(wrapper.findAll('[data-testid="target-row"]')).toHaveLength(0);
    wrapper.unmount();
  });

  it.each(['engine', 'miniapp'])('discards discovery replies after %s disconnects', async (source) => {
    let resolve!: (value: unknown) => void;
    const wrapper = mountView((method) => method === 'targets.list'
      ? new Promise((done) => { resolve = done; }) : Promise.resolve({}));
    const store = useEngineStore();
    store.status.frida = true;
    store.status.miniapp = true;
    await flushPromises();
    if (source === 'engine') store.status.frida = false;
    store.status.miniapp = false;
    await flushPromises();
    resolve({ targets: [{ targetId: 'old', type: 'page', url: 'https://example.com/test' }] });
    await flushPromises();
    expect(wrapper.findAll('[data-testid="target-row"]')).toHaveLength(0);
    expect(wrapper.get('[data-testid="target-diagnostics"]').text()).toContain(source === 'engine' ? '未连接' : '小程序连接已断开');
    wrapper.unmount();
  });

  it('clears a disconnected target list and rediscovers when the miniapp reconnects while Frida stays online', async () => {
    let targetId = 'old';
    const wrapper = mountView((method) => method === 'targets.list'
      ? Promise.resolve({ targets: [{ targetId, type: 'page', url: 'https://example.com/test' }] }) : Promise.resolve({}));
    const store = useEngineStore();
    store.status.frida = true; store.status.miniapp = true;
    await flushPromises();
    expect(wrapper.get('[data-testid="target-row"]').text()).toContain('old');
    store.status.miniapp = false;
    await flushPromises();
    expect(wrapper.findAll('[data-testid="target-row"]')).toHaveLength(0);
    targetId = 'new'; store.status.miniapp = true;
    await flushPromises();
    expect(wrapper.get('[data-testid="target-row"]').text()).toContain('new');
    wrapper.unmount();
  });

  it('shows the actual locked target and only confirms skip mode after backend success', async () => {
    const wrapper = mountView((method) => {
      if (method === 'debugger.pausePolicy') return Promise.resolve({ clientId: 7, appid: 'wxaaa111', name: '锁定目标', enabled: false, known: true, busy: false, error: '' });
      return Promise.resolve({});
    });
    useEngineStore().status.miniapp = true;
    await flushPromises();
    expect(wrapper.get('[data-testid="pause-target"]').text()).toContain('锁定目标');
    expect(wrapper.get('[data-testid="pause-toggle"]').attributes('title')).toContain('正常断点');
    let resolve!: (value: unknown) => void;
    call.mockImplementation(() => new Promise((done) => { resolve = done; }));
    await wrapper.get('[data-testid="pause-toggle"]').trigger('click');
    expect(call).toHaveBeenLastCalledWith('debugger.pausePolicy', { clientId: 7, enabled: true });
    expect(wrapper.get('[data-testid="pause-toggle"]').attributes('disabled')).toBeDefined();
    expect(wrapper.get('[data-testid="pause-state"]').text()).not.toBe('已跳过暂停');
    resolve({ clientId: 7, appid: 'wxaaa111', name: '锁定目标', enabled: true, known: true, busy: false, error: '' });
    await flushPromises();
    expect(wrapper.get('[data-testid="pause-state"]').text()).toBe('已跳过暂停');
    expect(wrapper.get('[data-testid="pause-toggle"]').text()).toBe('恢复正常暂停');
    wrapper.unmount();
  });

  it('shows failures as unknown rather than claiming skip mode is enabled', async () => {
    const wrapper = mountView((method) => {
      if (method === 'debugger.pausePolicy') return Promise.resolve({ clientId: 7, appid: 'wxaaa111', name: '', enabled: false, known: true, busy: false, error: '' });
      return Promise.resolve({});
    });
    useEngineStore().status.miniapp = true;
    await flushPromises();
    call.mockRejectedValue(new Error('Debugger unsupported'));
    await wrapper.get('[data-testid="pause-toggle"]').trigger('click');
    await flushPromises();
    expect(wrapper.get('[data-testid="pause-error"]').text()).toContain('Debugger unsupported');
    expect(wrapper.get('[data-testid="pause-state"]').text()).toBe('状态未确认');
    wrapper.unmount();
  });

  it('offers explicit restoration when the actual setting is unknown', async () => {
    const wrapper = mountView((method) => {
      if (method === 'debugger.pausePolicy') return Promise.resolve({ clientId: 7, enabled: false, known: false, busy: false, error: '' });
      return Promise.resolve({});
    });
    useEngineStore().status.miniapp = true;
    await flushPromises();
    expect(wrapper.find('[data-testid="pause-state"]').exists()).toBe(false);
    const button = wrapper.get('[data-testid="pause-toggle"]');
    expect(button.text()).toBe('恢复正常暂停');
    expect(button.attributes('aria-pressed')).toBe('mixed');
    await button.trigger('click');
    expect(call).toHaveBeenLastCalledWith('debugger.pausePolicy', { clientId: 7, enabled: false });
    wrapper.unmount();
  });

  it('offers no browser-open path: Electron is the only way to open a DevTools window', async () => {
    const wrapper = mountView((method) => {
      if (method === 'config.load') return Promise.resolve({});
      if (method === 'targets.list') return Promise.resolve({ targets: [] });
      return Promise.resolve({ ok: true });
    });
    await flushPromises();

    // 浏览器会丢弃以启动参数传入的 devtools:// 地址，外部打开路径已删除。
    expect(wrapper.find('[data-testid="open-default-browser"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="open-electron"]').exists()).toBe(false);
    expect(call).not.toHaveBeenCalledWith('shell.openDevtools', expect.anything());
    wrapper.unmount();
  });

  it('reports why the target list is empty instead of blaming the connection', async () => {
    const wrapper = mountView((method) => {
      if (method === 'targets.list') {
        return Promise.resolve({ targets: [], error: '读取调试目标失败：core error 1000: no miniapp connected' });
      }
      return Promise.resolve({});
    });
    const store = useEngineStore();
    store.status.frida = true;
    await flushPromises();

    expect(wrapper.get('[role="alert"]').text()).toContain('读取调试目标失败');
    // 后端文案里嵌着 Core 的英文原文：显示层要翻掉它，但不能连前端自己的前缀一起丢。
    expect(wrapper.get('[role="alert"]').text()).toContain('未连接小程序');
    expect(wrapper.text()).not.toContain('尚未检测到小程序');
    wrapper.unmount();
  });

  it('uses the shared page header contract', () => {
    expect(mount(DevtoolsView, { global: { plugins: [createTestingPinia({ createSpy: vi.fn }), router] } }).findComponent(PageHeader).exists()).toBe(true);
  });
  it('reports the DevTools window connection separately from the running engine', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'config.load') return Promise.resolve({ cdp_port: 62000 });
      if (method === 'engine.status') return Promise.resolve({ frida: true, miniapp: true, devtools: false });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(DevtoolsView, { global: { plugins: [createTestingPinia({ createSpy: vi.fn, stubActions: false }), router] } });
    expect(wrapper.get('[data-testid="cdp-state"]').text()).toBe('DevTools：未启动');
    await wrapper.get('.status-row button').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('engine.status');
    expect(wrapper.get('[data-testid="cdp-state"]').text()).toBe('DevTools：未连接');
    useEngineStore().status.devtools = true;
    await flushPromises();
    expect(wrapper.get('[data-testid="cdp-state"]').text()).toBe('DevTools：已连接');
    wrapper.unmount();
  });
  it('copies the DevTools address for the configured CDP port', async () => {
    const wrapper = mount(DevtoolsView, { global: { plugins: [createTestingPinia({ createSpy: vi.fn, stubActions: false }), router] } });
    useEngineStore().cdpPort = 62001;
    await wrapper.get('[data-testid="open-devtools"]').trigger('click');
    await flushPromises();
    expect(copyText).toHaveBeenCalledWith('devtools://devtools/bundled/inspector.html?ws=127.0.0.1:62001', 'DevTools 地址');
    expect(call).not.toHaveBeenCalledWith('shell.openDevtoolsWindow', expect.anything());
    expect(wrapper.find('iframe').exists()).toBe(false);
  });

  it('asks which mini-program to open when several are connected', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'targets.list') {
        return Promise.resolve({ targets: [
          { targetId: 'page-a', type: 'page', title: '甲小程序', url: 'https://servicewechat.com/wxaaa111111111111a/8/page-frame.html' },
          { targetId: 'page-b', type: 'page', title: '乙小程序', url: 'https://servicewechat.com/wxbbb222222222222b/3/page-frame.html' },
        ] });
      }
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(DevtoolsView, { global: { plugins: [createTestingPinia({ createSpy: vi.fn, stubActions: false }), router] } });
    const store = useEngineStore();
    store.cdpPort = 31415;
    store.status.devtools = true;
    await flushPromises();

    expect(wrapper.get('[data-testid="mini-choices"]').text()).toContain('2 个小程序');
    await wrapper.get('[data-testid="mini-wxbbb222222222222b"]').trigger('click');
    await wrapper.get('[data-testid="open-devtools"]').trigger('click');
    await flushPromises();
    expect(copyText).toHaveBeenCalledWith('devtools://devtools/bundled/inspector.html?ws=127.0.0.1:31415/devtools/page/page-b', 'DevTools 地址');
  });

  it('opens Electron when a path is configured', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'electron.status') return Promise.resolve({ path: 'C:/electron/electron.exe', source: 'config' });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(DevtoolsView, { global: { plugins: [createTestingPinia({ createSpy: vi.fn, stubActions: false }), router] } });
    useEngineStore().cdpPort = 62001;
    await flushPromises();
    await wrapper.get('[data-testid="open-electron"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('shell.openDevtoolsWindow', {
      cdp_port: 62001,
      electron_path: 'C:/electron/electron.exe',
      target_id: '',
    });
  });

  it('takes the name from the resolved identity, not from the CDP target title', async () => {
    // 真机实测：appid 那个目标的 CDP 标题就是 AppIndex（小游戏是 GameIndex），
    // 它是 WMPF 给 appservice 帧起的壳名，不能当小程序名。
    const wrapper = mountView((method) => {
      if (method === 'targets.list') {
        return Promise.resolve({ targets: [
          { targetId: 'page-a', type: 'page', title: 'AppIndex', url: 'https://servicewechat.com/wxabc1234567890a/103/page-frame.html' },
        ] });
      }
      if (method === 'miniapp.list') {
        return Promise.resolve({ list: [{ id: 1, appid: 'wxabc1234567890a', name: '电量管家' }] });
      }
      return Promise.resolve({});
    });
    useEngineStore().status.devtools = true;
    await flushPromises();

    const line = wrapper.get('[data-testid="current-mini"]').text();
    expect(line).toContain('电量管家');
    expect(wrapper.get('[data-testid="current-mini"]').attributes('title')).toBe('wxabc1234567890a');
    expect(line).not.toContain('AppIndex');
    wrapper.unmount();
  });

  it('falls back to the appid when no name has been resolved yet', async () => {
    const wrapper = mountView((method) => {
      if (method === 'targets.list') {
        return Promise.resolve({ targets: [
          { targetId: 'page-a', type: 'page', title: 'AppIndex', url: 'https://servicewechat.com/wxabc1234567890a/103/page-frame.html' },
        ] });
      }
      if (method === 'miniapp.list') return Promise.resolve({ list: [] });
      return Promise.resolve({});
    });
    useEngineStore().status.devtools = true;
    await flushPromises();

    const line = wrapper.get('[data-testid="current-mini"]').text();
    expect(wrapper.get('[data-testid="current-mini"]').attributes('title')).toBe('wxabc1234567890a');
    expect(line).not.toContain('AppIndex');
    wrapper.unmount();
  });

  it('opens the DevTools window with an auto-detected Electron, no settings needed', async () => {
    // 设置里留空是常态：后端自己按 PATH → npm 全局 → 常见位置解析，
    // 前端只是把解析结果显示出来，不再要求先去配置。
    const wrapper = mountView((method) => {
      if (method === 'electron.status') return Promise.resolve({ path: 'D:/nodejs/node_global/node_modules/electron/dist/electron.exe', source: 'path' });
      return Promise.resolve({});
    });
    useEngineStore().cdpPort = 62001;
    await flushPromises();

    const button = wrapper.get('[data-testid="open-electron"]');
    expect(button.attributes('disabled')).toBeUndefined();
    await button.trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('shell.openDevtoolsWindow', {
      cdp_port: 62001,
      electron_path: 'D:/nodejs/node_global/node_modules/electron/dist/electron.exe',
      target_id: '',
    });
    wrapper.unmount();
  });

  it('explains a missing Electron instead of demanding a configured path', async () => {
    const wrapper = mountView((method) => {
      if (method === 'electron.status') return Promise.resolve({ error: '未找到可用的 Electron：WxTap 不自带 Electron' });
      return Promise.resolve({});
    });
    // 引擎起来了、调试器还没连上，才是那条提示出现的位置。
    useEngineStore().status.frida = true;
    await flushPromises();

    expect(wrapper.find('[data-testid="open-electron"]').exists()).toBe(false);
    expect(wrapper.text()).toContain('未找到可用的 Electron');
    wrapper.unmount();
  });
});
