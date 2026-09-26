import { createTestingPinia } from '@pinia/testing';
import { flushPromises, mount } from '@vue/test-utils';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import SettingsView from './SettingsView.vue';
import PageHeader from '../components/PageHeader.vue';
import { useEngineStore } from '../stores/engine';
import { router } from '../router';

const { call } = vi.hoisted(() => ({ call: vi.fn() }));
vi.mock('../api/bridge', () => ({ backend: { call } }));

// 按方法名派发，而不是按调用顺序：load() 一次并发三个请求，顺序化的
// mockResolvedValueOnce 会在新增一个请求时静默错位。
function route(handlers: Record<string, unknown>) {
  call.mockImplementation((method: string) => Promise.resolve(handlers[method] ?? {}));
}

function mountView() {
  return mount(SettingsView, { global: { plugins: [createTestingPinia({ createSpy: vi.fn }), router] } });
}

describe('SettingsView', () => {
  beforeEach(() => {
    call.mockReset();
    route({});
  });

  it('keeps reload with version information', async () => {
    const wrapper = mountView();
    expect(wrapper.findComponent(PageHeader).exists()).toBe(true);
    await flushPromises();
    expect(wrapper.find('.page-toolbar').exists()).toBe(false);
    expect(wrapper.get('.panel .panel-header button').text()).toContain('重新加载');
  });

  it('opens configured folders and persists the Electron path without losing config', async () => {
    route({
      'settings.getPaths': {
        hook_scripts: 'C:/hooks',
        frida_config: 'C:/frida',
        skills: 'C:/skills',
        outputDir: 'C:/out',
        dbPath: 'C:/data/traffic.db',
        logDir: 'C:/logs',
      },
      'config.load': { electron_path: 'old' },
      'config.save': { ok: true },
    });
    const wrapper = mountView();
    await flushPromises();
    // 后端返回的全部 6 个路径都要有中文标签，不能漏出英文键名。
    const listed = wrapper.get('ul').text();
    expect(listed).toContain('反编译输出目录');
    expect(listed).toContain('流量数据库');
    expect(listed).toContain('日志目录');
    expect(listed).not.toContain('packagesDir');
    await wrapper.get('[data-testid="open-hook_scripts"]').trigger('click');
    expect(call).toHaveBeenCalledWith('shell.openFolder', { path: 'C:/hooks' });
    await wrapper.get('[data-testid="electron-path"]').setValue('C:/electron.exe');
    await wrapper.get('[data-testid="save-electron"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('config.save', { electron_path: 'C:/electron.exe' });
  });

  it('checks versions and starts a requested sync', async () => {
    route({
      'update.checkVersion': { current: 'v1', latest: 'v2', has_update: true, total_size: 1048576 },
      'update.syncWMPF': { updated: 2 },
    });
    const wrapper = mountView();
    await flushPromises();
    await wrapper.get('[data-testid="check-version"]').trigger('click');
    await wrapper.get('[data-testid="sync-wmpf"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('update.checkVersion');
    expect(call).toHaveBeenCalledWith('update.syncWMPF');
    expect(wrapper.text()).toContain('v2');
    expect(wrapper.text()).toContain('1.0 MB');
  });

  it('surfaces a check failure inline instead of throwing', async () => {
    route({ 'update.checkVersion': { current: 'v1', has_update: false, error: '更新源全部失败' } });
    const wrapper = mountView();
    await flushPromises();
    await wrapper.get('[data-testid="check-version"]').trigger('click');
    await flushPromises();
    expect(wrapper.text()).toContain('检查更新失败：更新源全部失败');
  });

  it('offers a restart for a version that is already staged', async () => {
    route({ 'update.status': { staged: true, version: 'v2.1.0' }, 'update.restart': { restarting: true } });
    const wrapper = mountView();
    await flushPromises();
    expect(wrapper.text()).toContain('待重启生效 v2.1.0');
    await wrapper.get('[data-testid="restart-update"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('update.restart');
  });

  it('tracks the background download and offers a restart once it lands', async () => {
    // 后端在下载完成时写入待生效标记，所以 update.status 在任务落定后才翻转。
    let staged = false;
    call.mockImplementation((method: string) => {
      if (method === 'update.downloadRelease') return Promise.resolve({ staged: false, version: 'v2.1.0', async: true, task_id: 't1' });
      if (method === 'update.status') return Promise.resolve(staged ? { staged: true, version: 'v2.1.0' } : { staged: false });
      return Promise.resolve({});
    });
    const wrapper = mountView();
    await flushPromises();
    const store = useEngineStore();

    await wrapper.get('[data-testid="download-release"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('update.downloadRelease');
    expect(wrapper.find('[data-testid="restart-update"]').exists()).toBe(false);

    store.tasks['t1'] = { id: 't1', kind: 'update.download', phase: 'running', current: 5, total: 10, message: '正在下载更新' };
    await flushPromises();
    expect(wrapper.text()).toContain('正在下载更新');

    staged = true;
    store.tasks['t1'] = { id: 't1', kind: 'update.download', phase: 'done', message: '更新已就绪，重启后生效' };
    await flushPromises();
    expect(wrapper.find('[data-testid="restart-update"]').exists()).toBe(true);
    expect(wrapper.text()).toContain('待重启生效 v2.1.0');
  });

  it('replaces the in-progress line when the download fails', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'update.downloadRelease') return Promise.resolve({ staged: false, version: 'v2.1.0', async: true, task_id: 't1' });
      return Promise.resolve({});
    });
    const wrapper = mountView();
    await flushPromises();
    const store = useEngineStore();

    await wrapper.get('[data-testid="download-release"]').trigger('click');
    await flushPromises();
    store.tasks['t1'] = { id: 't1', kind: 'update.download', phase: 'failed', error: 'sha256 不符' };
    await flushPromises();
    expect(wrapper.text()).toContain('下载更新失败：sha256 不符');
    expect(wrapper.find('[data-testid="restart-update"]').exists()).toBe(false);
  });

  it('shows which Node runs Core and where it was found', async () => {
    route({ 'node.status': { path: 'D:/nodejs/node.exe', version: '24.19.0', source: 'path' } });
    const wrapper = mountView();
    await flushPromises();
    expect(wrapper.text()).toContain('D:/nodejs/node.exe');
    expect(wrapper.text()).toContain('系统 PATH');
    expect(wrapper.text()).toContain('v24.19.0');
  });

  it('surfaces a missing Node together with the candidates it had to skip', async () => {
    route({
      'node.status': {
        error: '未找到可用的 Node.js：需要在「设置 → Node 运行时」里指定路径',
        skipped: ['D:/old/node.exe 的 Node.js 版本过低（16.20.2），需要 22 或更高'],
      },
    });
    const wrapper = mountView();
    await flushPromises();
    expect(wrapper.text()).toContain('未找到可用的 Node');
    // 换用另一个 Node 必须看得见，否则用户不知道自己装的 Node 被跳过了
    expect(wrapper.text()).toContain('版本过低');
  });

  it('auto-detects a Node and fills the field with it', async () => {
    route({ 'node.detect': { candidates: [{ path: 'C:/Program Files/nodejs/node.exe', version: '24.19.0' }] } });
    const wrapper = mountView();
    await flushPromises();
    await wrapper.get('[data-testid="detect-node"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('node.detect');
    expect((wrapper.get('[data-testid="node-path"]').element as HTMLInputElement).value).toBe('C:/Program Files/nodejs/node.exe');
  });

  it('auto-detects an Electron and fills the field with it', async () => {
    route({ 'electron.detect': { candidates: [{ path: 'D:/nodejs/node_global/node_modules/electron/dist/electron.exe', source: 'path' }] } });
    const wrapper = mountView();
    await flushPromises();
    await wrapper.get('[data-testid="detect-electron"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('electron.detect');
    expect((wrapper.get('[data-testid="electron-path"]').element as HTMLInputElement).value).toBe('D:/nodejs/node_global/node_modules/electron/dist/electron.exe');
    // 候选要列出来，用户才知道这个路径是从哪来的（PATH / 常见安装位置）。
    expect(wrapper.get('.path-list').text()).toContain('系统 PATH');
  });

  it('refuses to persist a path the backend cannot use', async () => {
    route({ 'node.detect': { candidates: [{ path: 'C:/nope/node.exe', error: '无法运行 C:/nope/node.exe：请确认它是可用的 node 可执行文件' }] } });
    const wrapper = mountView();
    await flushPromises();
    await wrapper.get('[data-testid="node-path"]').setValue('C:/nope/node.exe');
    await wrapper.get('[data-testid="save-node"]').trigger('click');
    await flushPromises();
    expect(wrapper.text()).toContain('无法使用该路径');
    // 校验不过就不落盘：把一条能用的路径覆盖成坏的不是用户能自己走出来的状态
    expect(call).not.toHaveBeenCalledWith('config.save', expect.anything());
  });

  it('saves a working path and re-reads the resolved Node', async () => {
    let saved: Record<string, unknown> = {};
    call.mockImplementation((method: string, params?: unknown) => {
      if (method === 'node.detect') return Promise.resolve({ candidates: [{ path: 'C:/nodejs/node.exe', version: '24.19.0' }] });
      if (method === 'config.save') {
        saved = params as Record<string, unknown>;
        return Promise.resolve({ ok: true });
      }
      if (method === 'node.status') {
        return Promise.resolve(saved.node_path ? { path: saved.node_path, version: '24.19.0', source: 'config' } : {});
      }
      return Promise.resolve({});
    });
    const wrapper = mountView();
    await flushPromises();
    await wrapper.get('[data-testid="node-path"]').setValue('C:/nodejs/node.exe');
    await wrapper.get('[data-testid="save-node"]').trigger('click');
    await flushPromises();
    expect(saved.node_path).toBe('C:/nodejs/node.exe');
    expect(wrapper.text()).toContain('本页指定');
  });
});
