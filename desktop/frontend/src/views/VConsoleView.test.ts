import { createTestingPinia } from '@pinia/testing';
import { mount } from '@vue/test-utils';
import { describe, expect, it, vi } from 'vitest';
import { useEngineStore } from '../stores/engine';
import { router } from '../router';
import VConsoleView from './VConsoleView.vue';
import PageHeader from '../components/PageHeader.vue';
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

function mountView() {
  const wrapper = mount(VConsoleView, {
    global: { plugins: [createTestingPinia({ createSpy: vi.fn, stubActions: false }), router] },
  });
  // vConsole 的结果由 store 的监听器落定（切页不丢状态），所以先订阅事件。
  useEngineStore().listen();
  // listen() 自带一次 log.list 回放，与这里的 vConsole 断言无关。
  call.mockClear();
  return wrapper;
}

describe('VConsoleView', () => {
  it('uses the shared page header contract', () => {
    const wrapper = mount(VConsoleView, {
      global: { plugins: [createTestingPinia({ createSpy: vi.fn }), router] },
    });
    expect(wrapper.findComponent(PageHeader).exists()).toBe(true);
  });

  // 这个开关的产出在小程序窗口里，本程序读不到：页面必须说清楚，并指向真正能看
  // 日志/请求/断点的那几页 —— 否则用户开完面板在本程序里找不到任何变化。
  it('explains that the panel lives in the miniapp and links to the pages that do show data', async () => {
    const wrapper = mountView();
    const text = wrapper.text();
    expect(text).toContain('微信小程序窗口里');
    expect(text).toContain('无法读取其内容');
    expect(wrapper.get('[data-testid="link-console"]').attributes('href')).toBe('#/console');
    expect(wrapper.get('[data-testid="link-traffic"]').attributes('href')).toBe('#/traffic');
    expect(wrapper.get('[data-testid="link-wxapi"]').attributes('href')).toBe('#/wxapi');
    expect(wrapper.get('[data-testid="link-cloud"]').attributes('href')).toBe('#/cloud');
    expect(wrapper.get('[data-testid="link-devtools"]').attributes('href')).toBe('#/devtools');
    expect(wrapper.get('[data-testid="link-hook"]').attributes('href')).toBe('#/hook');
  });

  it('requires confirmation before enabling and sends the enable IPC', async () => {
    const wrapper = mountView();
    useEngineStore().status.miniapp = true; await wrapper.vm.$nextTick();
    await wrapper.get('[data-testid="vconsole-toggle"]').trigger('click');
    expect(call).not.toHaveBeenCalled();
    await wrapper.get('[data-testid="confirm-vconsole"]').trigger('click');
    expect(call).toHaveBeenCalledWith('engine.vconsole', { enable: true });
  });

  it('disables controls without a miniapp connection and reports IPC failures', async () => {
    call.mockRejectedValue(new Error('denied'));
    const wrapper = mountView();
    expect(wrapper.get('[data-testid="vconsole-toggle"]').attributes('disabled')).toBeDefined();
    useEngineStore().status.miniapp = true; await wrapper.vm.$nextTick();
    // 开启方向要过确认框：确认之后的那次 IPC 失败必须显示出来
    await wrapper.get('[data-testid="vconsole-toggle"]').trigger('click');
    await wrapper.get('[data-testid="confirm-vconsole"]').trigger('click');
    await wrapper.vm.$nextTick();
    expect(wrapper.get('[role="alert"]').text()).toContain('denied');
  });

  it('shows the last known vconsole state instead of only the latest action', async () => {
    call.mockResolvedValue(undefined);
    const wrapper = mountView();
    useEngineStore().status.miniapp = true; await wrapper.vm.$nextTick();
    expect(wrapper.get('[data-testid="vconsole-state"]').text()).toContain('状态未知');
    await wrapper.get('[data-testid="vconsole-toggle"]').trigger('click');
    await wrapper.get('[data-testid="confirm-vconsole"]').trigger('click');
    listeners.vconsole_result?.({ ok: true, enable: true });
    await wrapper.vm.$nextTick();
    expect(wrapper.get('[data-testid="vconsole-state"]').text()).toContain('已开启');
    listeners.vconsole_result?.({ ok: true, enable: false });
    await wrapper.vm.$nextTick();
    expect(wrapper.get('[data-testid="vconsole-state"]').text()).toContain('已关闭');
  });

  // 状态从 store 读，所以切走再回来不会退回「状态未知」；按钮标签按状态与在飞方向给，
  // 不再靠状态字符串里找『开启』两个字来判断方向。
  it('keeps the state and the working direction across page switches', async () => {
    call.mockResolvedValue(undefined);
    const wrapper = mountView();
    const store = useEngineStore();
    store.status.miniapp = true; await wrapper.vm.$nextTick();
    await wrapper.get('[data-testid="vconsole-toggle"]').trigger('click');
    await wrapper.get('[data-testid="confirm-vconsole"]').trigger('click');
    // 在飞：一个按钮同时表达「正在开启」与它此刻的方向
    expect(wrapper.get('[data-testid="vconsole-toggle"]').text()).toContain('开启中');

    listeners.vconsole_result?.({ ok: true, enable: true });
    await wrapper.vm.$nextTick();
    expect(store.vconsoleEnabled).toBe(true);
    expect(wrapper.get('[data-testid="vconsole-toggle"]').attributes('disabled')).toBeUndefined();
    // 落定后同一个按钮换成关闭方向
    expect(wrapper.get('[data-testid="vconsole-toggle"]').text()).toBe('关闭调试');
  });

  it('drops a stale vconsole state when the miniapp disconnects', async () => {
    call.mockResolvedValue(undefined);
    const wrapper = mountView();
    const store = useEngineStore();
    store.status = { frida: true, miniapp: true, devtools: false };
    await wrapper.vm.$nextTick();
    listeners.vconsole_result?.({ ok: true, enable: true });
    await wrapper.vm.$nextTick();
    expect(store.vconsoleEnabled).toBe(true);

    // 小程序断开 ⇒ 页面 realm 消失，之前那次操作结果不再代表现状。
    listeners.status?.({ frida: true, miniapp: false, devtools: false });
    expect(store.vconsoleEnabled).toBeNull();
    await wrapper.vm.$nextTick();
    expect(wrapper.get('[data-testid="vconsole-state"]').text()).toContain('状态未知');
  });
});
