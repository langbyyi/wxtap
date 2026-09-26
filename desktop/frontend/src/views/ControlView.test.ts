import { createPinia, setActivePinia } from 'pinia';
import { createTestingPinia } from '@pinia/testing';
import { flushPromises, mount } from '@vue/test-utils';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { useEngineStore } from '../stores/engine';
import { router } from '../router';
import ControlView from './ControlView.vue';
import PageHeader from '../components/PageHeader.vue';
import * as notifyModule from '../utils/notify';

const { call, on } = vi.hoisted(() => ({ call: vi.fn(), on: vi.fn(() => vi.fn()) }));
vi.mock('../api/bridge', () => ({ backend: { call, on } }));

function mountWithRealStore() {
  const pinia = createPinia();
  setActivePinia(pinia);
  call.mockResolvedValue({});
  return mount(ControlView, { global: { plugins: [pinia, router] } });
}

describe('ControlView', () => {
  let wrapper: ReturnType<typeof mount>;

  beforeEach(() => {
    wrapper = mount(ControlView, {
      global: { plugins: [createTestingPinia({ createSpy: vi.fn }), router] },
    });
  });

  it('loads the engine state when mounted', () => {
    expect(useEngineStore().load).toHaveBeenCalledOnce();
  });

  it('uses the shared page header contract', () => {
    expect(wrapper.findComponent(PageHeader).exists()).toBe(true);
  });

  it('fills the window so the log list scrolls inside its panel', () => {
    expect(wrapper.classes()).toContain('page-workbench');
  });

  it('binds the CDP port input to the engine store', async () => {
    await wrapper.get('[data-testid="cdp-port"]').setValue('62001');

    expect(useEngineStore().cdpPort).toBe(62001);
  });

  it('renders the engine component states and invokes start and stop', async () => {
    expect(wrapper.text()).toContain('Frida');
    // 小程序与 DevTools 的实时状态只在各自的功能页里显示，状态页不再重复
    expect(wrapper.find('[data-testid="miniapp-runtime-status"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="devtools-status"]').exists()).toBe(false);

    // 一个开关：关着时点击 = 启动引擎，开着时点击 = 停止引擎
    await wrapper.get('[data-testid="engine-toggle"]').trigger('click');
    expect(useEngineStore().start).toHaveBeenCalledOnce();
    expect(useEngineStore().stop).not.toHaveBeenCalled();

    // 引擎连上（Frida 已附加）之后，同一个按钮换成停止方向
    useEngineStore().status.frida = true;
    await wrapper.vm.$nextTick();
    expect(wrapper.get('[data-testid="engine-toggle"]').text()).toBe('停止引擎');
    await wrapper.get('[data-testid="engine-toggle"]').trigger('click');
    expect(useEngineStore().stop).toHaveBeenCalledOnce();
  });

  it('disables controls while starting or stopping', async () => {
    const store = useEngineStore();
    store.starting = true;
    await wrapper.vm.$nextTick();

    expect(wrapper.get('[data-testid="engine-toggle"]').attributes('disabled')).toBeDefined();
    expect(wrapper.get('[data-testid="engine-toggle"]').attributes('disabled')).toBeDefined();

    store.starting = false;
    store.stopping = true;
    await wrapper.vm.$nextTick();

    expect(wrapper.get('[data-testid="engine-toggle"]').attributes('disabled')).toBeDefined();
    expect(wrapper.get('[data-testid="engine-toggle"]').attributes('disabled')).toBeDefined();
  });

  it('renders the Frida channel state', async () => {
    const store = useEngineStore();
    store.status = { frida: true, miniapp: false, devtools: true };
    await wrapper.vm.$nextTick();

    expect(wrapper.get('[data-testid="frida-status"]').classes()).toContain('connected');
  });

  it('shows WeChat as running independently from the debug engine', async () => {
    const store = useEngineStore();
    store.status = { frida: false, miniapp: false, devtools: false, wechatRunning: true };
    await wrapper.vm.$nextTick();

    expect(wrapper.get('[data-testid="wechat-status"]').text()).toContain('运行中');
  });

  it('shows whether the engine supports the running WeChat build', async () => {
    const store = useEngineStore();
    store.cdpPort = 31415;
    store.status = { frida: false, miniapp: false, devtools: false, wechatRunning: true };
    store.wechatHost = { pid: 10, version: 14161, path: 'C:\\WeChat\\WeChatAppEx.exe', addressTable: false, note: '没有这份构建的静态地址表，启动时会自动检测' };
    await wrapper.vm.$nextTick();

    const labels = wrapper.findAll('.status-card dt').map((item) => item.text());
    expect(labels).toEqual(['版本支持', '微信', 'Frida 注入']);
    const host = wrapper.get('[data-testid="wechat-host"]');
    expect(host.text()).toContain('不支持');
    expect(host.get('.status-pill').attributes('title')).toContain('addresses.14161.json');
  });

  it('shows a process-check failure instead of misreporting WeChat as stopped', async () => {
    const store = useEngineStore();
    store.wechatStatusError = 'Frida local device unavailable';
    await wrapper.vm.$nextTick();

    expect(wrapper.get('[data-testid="wechat-status"]').text()).toContain('检测失败');
  });

  it('guides the user to the next step once every channel is ready', async () => {
    const store = useEngineStore();
    store.status = { frida: true, miniapp: true, devtools: true };
    await wrapper.vm.$nextTick();
    const note = wrapper.get('[data-testid="ready-note"]');
    expect(note.text()).toContain('三条通道已就绪');
    expect(note.find('a[href="#/navigator"]').exists()).toBe(true);
    expect(note.find('a[href="#/hook"]').exists()).toBe(true);
  });
  it('renders an engine error', async () => {
    useEngineStore().error = 'Frida unavailable';
    await wrapper.vm.$nextTick();

    expect(wrapper.get('[data-testid="engine-error"]').text()).toBe('Frida unavailable');
  });

  it('does not offer a DevTools action on the status page', () => {
    expect(wrapper.find('[data-testid="open-devtools-window"]').exists()).toBe(false);
    expect(wrapper.text()).not.toContain('复制 DevTools 地址');
  });

  it('rejects an out-of-range port and disables starting', async () => {
    const w = mountWithRealStore();
    await flushPromises();

    await w.get('[data-testid="cdp-port"]').setValue('70000');
    await w.vm.$nextTick();

    expect(w.text()).toContain('端口需为 1-65535 的整数');
    expect(w.get('[data-testid="engine-toggle"]').attributes('disabled')).toBeDefined();
    w.unmount();
  });

  it('filters the log list by keyword and by level', async () => {
    const w = mountWithRealStore();
    await flushPromises();
    const store = useEngineStore();
    store.logs = [
      { time: '10:00:00', level: 'info', message: '引擎已启动', seq: 1 },
      { time: '10:00:01', level: 'error', message: '注入失败', seq: 2 },
    ];
    await w.vm.$nextTick();
    expect(w.findAll('[data-testid="log-list"] li')).toHaveLength(2);

    await w.get('[aria-label="筛选运行日志"]').setValue('注入');
    expect(w.findAll('[data-testid="log-list"] li')).toHaveLength(1);
    expect(w.get('[data-testid="log-list"]').text()).toContain('注入失败');

    await w.get('[aria-label="筛选运行日志"]').setValue('');
    await w.get('[aria-label="日志级别"]').setValue('info');
    expect(w.get('[data-testid="log-list"]').text()).toContain('引擎已启动');
    expect(w.get('[data-testid="log-list"]').text()).not.toContain('注入失败');
    w.unmount();
  });

  it('copies the filtered log lines', async () => {
    const copySpy = vi.spyOn(notifyModule, 'copyText').mockResolvedValue(true);
    const w = mountWithRealStore();
    await flushPromises();
    const store = useEngineStore();
    store.logs = [
      { time: '10:00:00', level: 'info', message: '引擎已启动', seq: 1 },
      { time: '10:00:01', level: 'error', message: '注入失败', seq: 2 },
    ];
    await w.vm.$nextTick();

    await w.get('[aria-label="日志级别"]').setValue('error');
    await w.get('[data-testid="copy-log"]').trigger('click');
    await flushPromises();

    expect(copySpy).toHaveBeenCalledWith('10:00:01 error 注入失败', '运行日志');
    copySpy.mockRestore();
    w.unmount();
  });

  it('shows the retention cap in the counter and a notice for dropped rows', async () => {
    const w = mountWithRealStore();
    await flushPromises();
    const store = useEngineStore();
    store.logs = [{ time: '10:00:00', level: 'info', message: '一条', seq: 1 }];
    store.logsDropped = 2;
    store.logsTrimmed = 3;
    await w.vm.$nextTick();

    expect(w.get('[data-testid="log-count"]').text()).toBe('1 / 500');
    expect(w.get('[data-testid="log-dropped"]').text()).toContain('已丢弃 5 条');
    w.unmount();
  });

  it('polls the WeChat host once a second while mounted and stops when unmounted', async () => {
    vi.useFakeTimers();
    const w = mountWithRealStore();
    await flushPromises();
    const pollCount = () => call.mock.calls.filter(([method]) => method === 'wechat.status').length;
    const afterMount = pollCount();
    expect(afterMount).toBeGreaterThan(0);

    await vi.advanceTimersByTimeAsync(3000);
    const afterTicks = pollCount();
    expect(afterTicks).toBeGreaterThan(afterMount);

    w.unmount();
    await vi.advanceTimersByTimeAsync(5000);
    expect(pollCount()).toBe(afterTicks);
    vi.useRealTimers();
  });

  it('offers a retry for an error the backend flagged retryable', async () => {
    const w = mountWithRealStore();
    await flushPromises();
    const store = useEngineStore();
    store.error = '端口已被占用（CDP 31415 / 调试 9421）：请关闭占用该端口的程序，或在「状态」页换一个 CDP 端口';
    store.errorRetryable = true;
    store.lastFailed = 'start';
    await w.vm.$nextTick();

    const button = w.get('[data-testid="engine-error"] button');
    expect(button.text()).toBe('重试');
    await button.trigger('click');
    await flushPromises();

    expect(call).toHaveBeenCalledWith('engine.start', { cdp_port: 31415 });
    w.unmount();
  });

  it('renders logs kept by the engine store and clears them from the page', async () => {
    const pinia = createPinia();
    setActivePinia(pinia);
    call.mockResolvedValue({});
    const wrapper = mount(ControlView, { global: { plugins: [pinia, router] } });
    await flushPromises();
    expect(wrapper.text()).toContain('暂无日志');

    const store = useEngineStore();
    store.logs = [
      { time: '10:00:00', level: 'info', message: '引擎已启动' },
      { time: '10:00:01', level: 'error', message: '注入失败' },
    ];
    await wrapper.vm.$nextTick();
    expect(wrapper.get('[data-testid="log-list"]').text()).toContain('10:00:00');
    expect(wrapper.get('[data-testid="log-list"]').text()).toContain('注入失败');

    await wrapper.get('[data-testid="clear-log"]').trigger('click');
    await wrapper.vm.$nextTick();
    expect(store.logs).toEqual([]);
    expect(wrapper.text()).toContain('暂无日志');
    wrapper.unmount();
  });

  it('freezes the log view on pause and reports the new arrivals by seq', async () => {
    const pinia = createPinia();
    setActivePinia(pinia);
    call.mockResolvedValue({});
    const wrapper = mount(ControlView, { global: { plugins: [pinia, router] } });
    await flushPromises();

    const store = useEngineStore();
    store.logs = [
      { time: '10:00:00', level: 'info', message: '第一条', seq: 1 },
      { time: '10:00:01', level: 'info', message: '第二条', seq: 2 },
    ];
    await wrapper.vm.$nextTick();
    expect(wrapper.find('[data-testid="jump-log"]').exists()).toBe(false);

    await wrapper.get('[data-testid="pause-log"]').trigger('click');
    await wrapper.vm.$nextTick();
    expect(wrapper.text()).toContain('已暂停');
    expect(wrapper.get('[data-testid="pause-log"]').text()).toBe('恢复');

    // 暂停后新日志进 store，但视图保持冻结
    store.logs = [
      ...store.logs,
      { time: '10:00:02', level: 'error', message: '第三条', seq: 3 },
      { time: '10:00:03', level: 'error', message: '第四条', seq: 4 },
    ];
    await wrapper.vm.$nextTick();
    expect(wrapper.get('[data-testid="log-list"]').text()).not.toContain('第三条');
    expect(wrapper.get('[data-testid="jump-log"]').text()).toBe('2 条新日志');

    // 恢复：标题计数回到实时总量，角标消失
    await wrapper.get('[data-testid="jump-log"]').trigger('click');
    await wrapper.vm.$nextTick();
    expect(wrapper.get('[data-testid="log-list"]').text()).toContain('第三条');
    expect(wrapper.find('[data-testid="jump-log"]').exists()).toBe(false);
    expect(wrapper.text()).not.toContain('已暂停');

    // 暂停期间日志超限被从头裁剪时，长度不升反降，按 seq 计数才不会少数
    await wrapper.get('[data-testid="pause-log"]').trigger('click');
    await wrapper.vm.$nextTick();
    store.logs = [{ time: '10:00:09', level: 'info', message: '裁剪后', seq: 9 }];
    await wrapper.vm.$nextTick();
    expect(wrapper.get('[data-testid="jump-log"]').text()).toBe('1 条新日志');
    wrapper.unmount();
  });

  it('leaves the paused view when the log is cleared', async () => {
    const pinia = createPinia();
    setActivePinia(pinia);
    call.mockResolvedValue({});
    const wrapper = mount(ControlView, { global: { plugins: [pinia, router] } });
    await flushPromises();

    const store = useEngineStore();
    store.logs = [{ time: '10:00:00', level: 'info', message: '第一条', seq: 1 }];
    await wrapper.get('[data-testid="pause-log"]').trigger('click');
    await wrapper.vm.$nextTick();
    expect(wrapper.text()).toContain('已暂停');

    await wrapper.get('[data-testid="clear-log"]').trigger('click');
    await wrapper.vm.$nextTick();
    expect(wrapper.text()).not.toContain('已暂停');
    expect(wrapper.get('[data-testid="pause-log"]').text()).toBe('暂停');
    wrapper.unmount();
  });
});
