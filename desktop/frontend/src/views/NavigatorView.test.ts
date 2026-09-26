import { createTestingPinia } from '@pinia/testing';
import { flushPromises, mount } from '@vue/test-utils';
import { useEngineStore } from '../stores/engine';
import { afterEach, describe, expect, it, vi } from 'vitest';
import NavigatorView from './NavigatorView.vue';
import PageHeader from '../components/PageHeader.vue';

const { call, on, copyText, notify } = vi.hoisted(() => ({
  call: vi.fn(),
  on: vi.fn((_event: string, _listener: (payload: unknown) => void) => vi.fn()),
  copyText: vi.fn(),
  notify: vi.fn(),
}));
vi.mock('../api/bridge', () => ({ backend: { call, on } }));
vi.mock('../utils/notify', () => ({ copyText, notify }));

function mockNavigator(options: {
  pages?: string[];
  tabBar?: string[];
  current?: string;
  runtime?: string;
  guard?: boolean;
  redirects?: Array<Record<string, unknown>>;
} = {}) {
  call.mockImplementation((method: string) => {
    switch (method) {
      case 'navigator.pages':
        return Promise.resolve({
          pages: options.pages ?? [],
          tab_bar_pages: options.tabBar ?? [],
          current_route: options.current ?? '',
        });
      case 'navigator.getCurrentRoute':
        return Promise.resolve({ route: options.runtime ?? options.current ?? '' });
      case 'navigator.guardState':
        return Promise.resolve({ enabled: options.guard ?? false, redirects: options.redirects ?? [] });
      default:
        return Promise.resolve({ ok: true });
    }
  });
}

function mountNavigatorView(connected = true) {
  const pinia = createTestingPinia({
    createSpy: vi.fn,
    stubActions: false,
    initialState: { engine: { status: { frida: connected, miniapp: connected, devtools: false } } },
  });
  return mount(NavigatorView, { global: { plugins: [pinia] } });
}

describe('NavigatorView', () => {
  it('keeps route status with the route list controls', () => {
    const wrapper = mount(NavigatorView, { global: { plugins: [createTestingPinia({ createSpy: vi.fn })] } });
    expect(wrapper.findComponent(PageHeader).exists()).toBe(true);
    expect(wrapper.find('.page-toolbar').exists()).toBe(false);
    expect(wrapper.find('.routes-panel .status-pill').exists()).toBe(true);
    wrapper.unmount();
  });

  // call 是模块级共享 mock，不清就会把上一个用例的 mockRejectedValueOnce/mockImplementation
  // 带进来：残留的 once 拒绝会让下一个用例的 navigator.pages 意外走进 catch，从而级联变红。
  afterEach(() => {
    vi.useRealTimers();
    on.mockReset();
    on.mockReturnValue(vi.fn());
    call.mockReset();
    copyText.mockReset();
    notify.mockReset();
  });

  it('loads routes, jumps from the list, and polls the current route instead of the whole config', async () => {
    vi.useFakeTimers();
    mockNavigator({ pages: ['pages/home', 'pages/profile'], tabBar: ['pages/home'], runtime: 'pages/home' });
    const wrapper = mountNavigatorView();
    await flushPromises();
    expect(wrapper.text()).toContain('pages/home');

    // 跳转入口只剩路由列表：选中一行，再「跳转到选中」。
    await wrapper.findAll('.route-list > button')[1].trigger('click');
    await wrapper.get('[data-testid="navigate-selected"]').trigger('click');
    expect(call).toHaveBeenCalledWith('navigator.navigate', { route: 'pages/profile', method: 'navigateTo' });

    // 轮询只回读当前路由：页面自己跳转（含自动重定向）后，配置里的 current_route 已经过期。
    mockNavigator({ pages: ['pages/home', 'pages/profile'], runtime: 'pages/profile' });
    await vi.advanceTimersByTimeAsync(2000);
    await flushPromises();
    expect(wrapper.text()).toContain('当前路由：/pages/profile');

    wrapper.unmount();
    const routeCalls = call.mock.calls.filter(([method]) => method === 'navigator.getCurrentRoute').length;
    await vi.advanceTimersByTimeAsync(4000);
    expect(call.mock.calls.filter(([method]) => method === 'navigator.getCurrentRoute')).toHaveLength(routeCalls);
  });

  it('moves the route selection with the arrow keys and trims the filter query', async () => {
    mockNavigator({ pages: ['pages/home', 'pages/profile', 'pages/settings'], current: 'pages/home' });
    const wrapper = mountNavigatorView();
    await flushPromises();

    const buttons = () => wrapper.findAll('.route-list > button');
    expect(buttons()[0].classes()).toContain('selected');
    await buttons()[0].trigger('keydown', { key: 'ArrowDown' });
    expect(buttons()[1].classes()).toContain('selected');
    await buttons()[1].trigger('keydown', { key: 'ArrowDown' });
    expect(buttons()[2].classes()).toContain('selected');
    await buttons()[2].trigger('keydown', { key: 'ArrowUp' });
    expect(buttons()[1].classes()).toContain('selected');

    await wrapper.get('[aria-label="搜索路由"]').setValue('  settings  ');
    expect(wrapper.findAll('.route-list > button')).toHaveLength(1);
    expect(wrapper.text()).toContain('pages/settings');
    wrapper.unmount();
  });

  // 运行时事实与用户选中是两件事：当前页必须在列表里单独标出来，否则高亮会被
  // 读成「这就是当前页」。
  it('marks the runtime current page separately from the user selection', async () => {
    mockNavigator({ pages: ['pages/home', 'pages/profile'], current: 'pages/home', runtime: 'pages/profile' });
    const wrapper = mountNavigatorView();
    await flushPromises();

    // 选中项落在列表初值（navigator.pages 的 current_route），而运行时当前页
    // 来自页面回读（这里故意让两者不同）。
    const rows = wrapper.findAll('.route-list > button');
    expect(rows[0].classes()).toContain('selected');
    expect(rows[0].find('.current-badge').exists()).toBe(false);
    expect(rows[1].find('.current-badge').exists()).toBe(true);
    wrapper.unmount();
  });

  it('shows whether a miniapp is connected, since routing needs a live target', async () => {
    mockNavigator({ pages: ['pages/home'], current: 'pages/home' });
    const wrapper = mountNavigatorView(false);
    await flushPromises();
    expect(wrapper.text()).toContain('未连接小程序');

    useEngineStore().status = { frida: true, miniapp: true, devtools: true };
    await wrapper.vm.$nextTick();
    expect(wrapper.text()).toContain('小程序已连接');
    wrapper.unmount();
  });

  it('copies the selected route to the clipboard', async () => {
    mockNavigator({ pages: ['pages/home', 'pages/profile'] });
    const wrapper = mountNavigatorView();
    await flushPromises();
    await wrapper.get('[data-testid="copy-route"]').trigger('click');
    expect(copyText).toHaveBeenCalledWith('pages/home', '路由');
    wrapper.unmount();
  });

  it('shows backend failures and toggles redirect guard against the backend state', async () => {
    call.mockRejectedValueOnce(new Error('offline')).mockResolvedValue({});
    const wrapper = mountNavigatorView();
    await flushPromises();
    expect(wrapper.get('[role="alert"]').text()).toContain('offline');

    call.mockImplementation((method: string) => Promise.resolve(
      method === 'navigator.guardState' ? { enabled: true, redirects: [] } : { ok: true },
    ));
    await wrapper.get('[data-testid="redirect-guard"]').setValue(true);
    await flushPromises();
    expect(call).toHaveBeenCalledWith('navigator.enableRedirectGuard');
    expect(wrapper.get('[data-testid="guard-state"]').text()).toContain('拦截中');
    wrapper.unmount();
  });

  it('reads the guard state back from the page and says so when the page reloaded', async () => {
    vi.useFakeTimers();
    mockNavigator({ pages: ['pages/home'], guard: true });
    const wrapper = mountNavigatorView();
    await flushPromises();
    expect((wrapper.get('[data-testid="redirect-guard"]').element as HTMLInputElement).checked).toBe(true);

    // 页面重新加载：拦截器随 realm 一起消失，后端回读为关闭 —— 前端必须跟着改口。
    mockNavigator({ pages: ['pages/home'], guard: false });
    await vi.advanceTimersByTimeAsync(2000);
    await flushPromises();
    expect((wrapper.get('[data-testid="redirect-guard"]').element as HTMLInputElement).checked).toBe(false);
    expect(notify).toHaveBeenCalledWith('页面已重新加载，防跳转拦截已失效', 'info');
    wrapper.unmount();
  });

  // 开启失败必须让勾选框真实回弹：后端拒绝了，UI 不能停在"已拦截"。
  it('reverts the guard checkbox when the backend rejects the toggle', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'navigator.pages') return Promise.resolve({ pages: ['pages/home'], tab_bar_pages: [], current_route: '' });
      if (method === 'navigator.guardState') return Promise.resolve({ enabled: false, redirects: [] });
      if (method === 'navigator.enableRedirectGuard') return Promise.reject(new Error('page realm gone'));
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountNavigatorView();
    await flushPromises();

    await wrapper.get('[data-testid="redirect-guard"]').setValue(true);
    await flushPromises();

    expect(call).toHaveBeenCalledWith('navigator.enableRedirectGuard');
    expect((wrapper.get('[data-testid="redirect-guard"]').element as HTMLInputElement).checked).toBe(false);
    expect(wrapper.get('[data-testid="guard-state"]').text()).toContain('未开启');
    wrapper.unmount();
  });

  it('refreshes the displayed connection state when the live route request finds no miniapp', async () => {
    call.mockRejectedValueOnce(new Error('core error 1000: no miniapp connected'));
    // 要测的是「路由请求真的发出后才撞上没有小程序」，所以必须已连接；
    // 断开状态下 load() 会被 connected 守卫提前返回，这条路根本走不到。
    const wrapper = mountNavigatorView();
    const refreshStatus = vi.spyOn(useEngineStore(), 'refreshStatus');

    await flushPromises();

    expect(refreshStatus).toHaveBeenCalledOnce();
    expect(wrapper.get('[role="alert"]').text()).toContain('未连接小程序');
    wrapper.unmount();
  });

  it('does not request routes before the engine has started', async () => {
    call.mockClear();
    call.mockRejectedValue(new Error('core error 2000: engine not started'));
    const wrapper = mountNavigatorView(false);

    await flushPromises();

    expect(call).not.toHaveBeenCalledWith('navigator.pages');
    expect(wrapper.find('[role="alert"]').exists()).toBe(false);
    wrapper.unmount();
  });

  it('shows the blocked redirects the guard state reports', async () => {
    mockNavigator({
      pages: ['pages/home'],
      guard: true,
      redirects: [{ type: 'redirectTo', url: '/pages/blocked', time: '10:00:00' }],
    });
    const wrapper = mountNavigatorView();
    await flushPromises();

    const list = wrapper.get('[data-testid="blocked-list"]');
    expect(list.text()).toContain('/pages/blocked');
    expect(list.text()).toContain('redirectTo');
    expect(list.text()).toContain('10:00:00');
    wrapper.unmount();
  });

  it('does not overlap route polling while the current-route request is pending', async () => {
    vi.useFakeTimers();
    call.mockClear();
    let resolveRoute: ((value: unknown) => void) | undefined;
    call.mockImplementation((method: string) => {
      if (method === 'navigator.pages') {
        return Promise.resolve({ pages: ['pages/home'], tab_bar_pages: [], current_route: 'pages/home' });
      }
      if (method === 'navigator.getCurrentRoute') {
        return new Promise((resolve) => { resolveRoute = resolve; });
      }
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountNavigatorView();
    await flushPromises();

    await vi.advanceTimersByTimeAsync(2000);
    await vi.advanceTimersByTimeAsync(4000);
    expect(call.mock.calls.filter(([method]) => method === 'navigator.getCurrentRoute')).toHaveLength(1);

    resolveRoute?.({ route: 'pages/home' });
    await flushPromises();
    wrapper.unmount();
  });

  it('starts auto visit, tracks navigate_progress with failures, and stops it', async () => {
    mockNavigator({ pages: ['pages/home', 'pages/detail'] });
    const wrapper = mountNavigatorView();
    await flushPromises();
    const progressListener = on.mock.calls.find(([event]) => event === 'navigate_progress')?.[1] as
      | ((payload: { progress?: number; current?: string; done?: boolean; failed?: number; total?: number }) => void)
      | undefined;
    expect(progressListener).toBeTypeOf('function');

    await wrapper.get('[data-testid="auto-visit-toggle"]').trigger('click');
    expect(call).toHaveBeenCalledWith('navigator.autoVisit');
    expect(wrapper.get('[data-testid="auto-visit-toggle"]').isVisible()).toBe(true);

    progressListener?.({ progress: 50, current: 'pages/home', done: false, failed: 1, total: 2 });
    await wrapper.vm.$nextTick();
    expect(wrapper.text()).toContain('50%');
    expect(wrapper.text()).toContain('pages/home');

    // 遍历结束：失败数要留下来，不能让全失败的一轮看起来像成功。
    progressListener?.({ progress: 100, current: 'pages/detail', done: true, failed: 1, total: 2 });
    await wrapper.vm.$nextTick();
    expect(wrapper.get('[data-testid="auto-visit-toggle"]').isVisible()).toBe(true);
    expect(wrapper.text()).toContain('1 页失败');
    expect(notify).toHaveBeenCalledWith('页面遍历完成，1 页失败', 'info');

    await wrapper.get('[data-testid="auto-visit-toggle"]').trigger('click');
    await wrapper.get('[data-testid="auto-visit-toggle"]').trigger('click');
    expect(call).toHaveBeenCalledWith('navigator.stopAutoVisit');
    expect(wrapper.get('[data-testid="auto-visit-toggle"]').isVisible()).toBe(true);
    wrapper.unmount();
  });

  it('enters visiting state immediately on click, before the backend call resolves', async () => {
    let resolveVisit: (() => void) | undefined;
    call.mockImplementation((method: string) => {
      if (method === 'navigator.pages') return Promise.resolve({ pages: ['pages/home'], tab_bar_pages: [], current_route: '' });
      if (method === 'navigator.autoVisit') return new Promise<void>((resolve) => { resolveVisit = resolve; });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountNavigatorView();
    await flushPromises();
    await wrapper.get('[data-testid="auto-visit-toggle"]').trigger('click');
    expect(wrapper.get('[data-testid="auto-visit-toggle"]').isVisible()).toBe(true);
    resolveVisit?.();
    await flushPromises();
    wrapper.unmount();
  });

  it('does not leak the route polling timer when unmounted before pages resolve', async () => {
    vi.useFakeTimers();
    let resolvePages: ((value: unknown) => void) | undefined;
    call.mockImplementation((method: string) => {
      if (method === 'navigator.pages') return new Promise((resolve) => { resolvePages = resolve; });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountNavigatorView();
    await wrapper.vm.$nextTick();
    wrapper.unmount();
    resolvePages?.({ pages: [], tab_bar_pages: [], current_route: '' });
    await flushPromises();
    call.mockClear();
    vi.advanceTimersByTime(10000);
    await flushPromises();
    expect(call).not.toHaveBeenCalled();
  });
});
