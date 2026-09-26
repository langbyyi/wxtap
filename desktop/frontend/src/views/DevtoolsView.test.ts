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
  it('surfaces the CDP channel state and refreshes it on demand', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'config.load') return Promise.resolve({ cdp_port: 62000 });
      if (method === 'engine.status') return Promise.resolve({ frida: true, miniapp: true, devtools: false });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(DevtoolsView, { global: { plugins: [createTestingPinia({ createSpy: vi.fn, stubActions: false }), router] } });
    expect(wrapper.get('[data-testid="cdp-state"]').text()).toContain('未启动');
    expect(wrapper.text()).toContain('引擎未启动');
    await wrapper.get('.status-row button').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('engine.status');
    expect(wrapper.get('[data-testid="cdp-state"]').text()).toContain('等待调试器');
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
    expect(line).toContain('wxabc1234567890a');
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
    expect(line).toContain('wxabc1234567890a');
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
