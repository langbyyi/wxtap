import { createTestingPinia } from '@pinia/testing';
import { flushPromises, mount } from '@vue/test-utils';
import { describe, expect, it, vi } from 'vitest';
import { useEngineStore } from '../stores/engine';
import { router } from '../router';
import HookView from './HookView.vue';
import PageHeader from '../components/PageHeader.vue';

const { call, copyText, notify } = vi.hoisted(() => ({ call: vi.fn(), copyText: vi.fn(), notify: vi.fn() }));
vi.mock('../api/bridge', () => ({ backend: { call } }));
vi.mock('../utils/notify', () => ({ notify, copyText }));

function mountView() {
  return mount(HookView, {
    global: { plugins: [createTestingPinia({ createSpy: vi.fn, stubActions: false }), router] },
  });
}

/** A hook.list payload shaped like the backend's: 壳侧登记 lastRun / mtime / stale。 */
function listPayload(scripts: unknown[]) {
  return Promise.resolve({ scripts });
}

describe('HookView', () => {
  it('keeps injection status with script list controls', () => {
    const wrapper = mountView();
    expect(wrapper.findComponent(PageHeader).exists()).toBe(true);
    expect(wrapper.find('.page-toolbar').exists()).toBe(false);
    expect(wrapper.find('.script-filter-bar [data-testid="refresh-hooks"]').exists()).toBe(true);
  });

  // 「已注入」现在是后端登记的事实（页面 realm 重建时复位），前端不再自己
  // 记一份本地标记 —— 本地标记在小程序重载后必然说错话。
  it('loads scripts and reflects the backend injection state', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'hook.list') {
        return listPayload([
          { filename: 'trace.js', global: false, injected: true, mtime: 100, stale: false },
          { filename: 'test_hello.js', global: true, injected: false, mtime: 100 },
        ]);
      }
      if (method === 'settings.getPaths') return Promise.resolve({ hook_scripts: 'C:/wxtap/hook_scripts' });
      return Promise.resolve({});
    });
    const wrapper = mountView();
    useEngineStore().status.miniapp = true;
    await flushPromises();

    expect(wrapper.get('[data-testid="injected-count"]').text()).toContain('1 / 2 已注入');
    expect(wrapper.get('[data-testid="global-count"]').text()).toContain('1 个全局');
  });

  // 脚本调试就是「改文件 → 重注 → 看结果」的循环：注入了还必须能再点一次，
  // 否则用户改完脚本只能重载小程序才能重跑。这条曾经被 disabled 堵死。
  it('keeps an injected script re-injectable and labels the loop', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'hook.list') return listPayload([{ filename: 'trace.js', global: false, injected: true, mtime: 100 }]);
      return Promise.resolve(method === 'settings.getPaths' ? { hook_scripts: 'C:/h' } : {});
    });
    const wrapper = mountView();
    useEngineStore().status.miniapp = true;
    await flushPromises();

    const button = wrapper.get('[data-testid="inject-trace.js"]');
    expect(button.attributes('disabled')).toBeUndefined();
    expect(button.text()).toBe('重新注入');
    await button.trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('hook.inject', { filename: 'trace.js' });
  });

  // 注入结局必须留在行里：只弹一个 toast，用户切页回来就再也看不到返回值/报错。
  it('shows the last run outcome, including failures', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'hook.list') {
        return listPayload([
          {
            filename: 'trace.js',
            global: false,
            injected: true,
            mtime: 100,
            lastRun: { at: 1700000000, ok: true, summary: 'wx.setStorageSync 已替换', durationMs: 12 },
          },
          {
            filename: 'broken.js',
            global: false,
            injected: false,
            mtime: 100,
            // 负数时长 = 脚本没跑起来（文件读不出来）：不能显示成「0ms」。
            lastRun: { at: 1700000000, ok: false, summary: 'ReferenceError: wx is not defined', durationMs: -1 },
          },
        ]);
      }
      return Promise.resolve(method === 'settings.getPaths' ? { hook_scripts: 'C:/h' } : {});
    });
    const wrapper = mountView();
    await flushPromises();

    expect(wrapper.get('[data-testid="run-trace.js"]').text()).toContain('上次注入成功');
    expect(wrapper.get('[data-testid="run-trace.js"]').text()).toContain('返回：wx.setStorageSync 已替换');
    expect(wrapper.get('[data-testid="run-trace.js"]').text()).toContain('12ms');

    const failed = wrapper.get('[data-testid="run-broken.js"]').text();
    expect(failed).toContain('上次注入失败');
    expect(failed).toContain('错误：ReferenceError: wx is not defined');
    expect(failed).not.toContain('ms');
  });

  // 文件在注入之后被改过：界面必须提示重注，否则用户会以为改动生效了。
  it('flags a script whose file changed after the last run', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'hook.list') {
        return listPayload([
          {
            filename: 'trace.js',
            global: false,
            injected: true,
            mtime: 200,
            stale: true,
            lastRun: { at: 1700000000, ok: true, summary: '无返回值' },
          },
        ]);
      }
      return Promise.resolve(method === 'settings.getPaths' ? { hook_scripts: 'C:/h' } : {});
    });
    const wrapper = mountView();
    await flushPromises();

    expect(wrapper.get('[data-testid="stale-trace.js"]').text()).toContain('文件已更新');
  });

  // 注入的成功/失败结局由后端登记，前端注入完必须回读一次，而不是自己推一份。
  it('re-reads the backend registry after an injection attempt', async () => {
    let listCalls = 0;
    call.mockImplementation((method: string) => {
      if (method === 'hook.list') {
        listCalls += 1;
        return listPayload([
          {
            filename: 'trace.js',
            global: false,
            injected: listCalls > 1,
            mtime: 100,
            lastRun: listCalls > 1 ? { at: 1700000000, ok: true, summary: '42', durationMs: 5 } : undefined,
          },
        ]);
      }
      if (method === 'hook.inject') return Promise.resolve({ ok: true, summary: '42', durationMs: 5 });
      return Promise.resolve(method === 'settings.getPaths' ? { hook_scripts: 'C:/h' } : {});
    });
    const wrapper = mountView();
    useEngineStore().status.miniapp = true;
    await flushPromises();

    await wrapper.get('[data-testid="inject-trace.js"]').trigger('click');
    await flushPromises();

    expect(listCalls).toBe(2);
    expect(notify).toHaveBeenCalledWith('trace.js 已注入：42', 'success');
    expect(wrapper.get('[data-testid="run-trace.js"]').text()).toContain('返回：42');
  });

  // 注入失败不能只留一条一闪而过的 toast：alert 要在回读列表之后仍然挂着，
  // 否则用户看到的是「什么都没发生」。（load() 开头会清错误，所以要重新挂。）
  it('keeps the failure alert visible after the list is re-read', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'hook.list') return listPayload([{ filename: 'trace.js', global: false, injected: false, mtime: 1 }]);
      if (method === 'hook.inject') return Promise.reject(new Error('engine not running'));
      return Promise.resolve(method === 'settings.getPaths' ? { hook_scripts: 'C:/h' } : {});
    });
    const wrapper = mountView();
    useEngineStore().status.miniapp = true;
    await flushPromises();

    await wrapper.get('[data-testid="inject-trace.js"]').trigger('click');
    await flushPromises();

    expect(wrapper.get('[role="alert"]').text()).toContain('engine not running');
    expect(notify).toHaveBeenCalledWith('注入失败：engine not running', 'error');
  });

  // 回读期间按钮必须仍是「注入中…」：不然这半秒里连点两次就会注入两遍。
  it('keeps the button busy until the refreshed state is in', async () => {
    let release: (() => void) | undefined;
    let listCalls = 0;
    call.mockImplementation((method: string) => {
      if (method === 'hook.list') {
        listCalls += 1;
        if (listCalls === 2) {
          return new Promise((resolve) => {
            release = () => resolve({ scripts: [{ filename: 'trace.js', global: false, injected: true, mtime: 1 }] });
          });
        }
        return listPayload([{ filename: 'trace.js', global: false, injected: false, mtime: 1 }]);
      }
      if (method === 'hook.inject') return Promise.resolve({ ok: true, summary: '无返回值' });
      return Promise.resolve(method === 'settings.getPaths' ? { hook_scripts: 'C:/h' } : {});
    });
    const wrapper = mountView();
    useEngineStore().status.miniapp = true;
    await flushPromises();

    await wrapper.get('[data-testid="inject-trace.js"]').trigger('click');
    await flushPromises();
    expect(wrapper.get('[data-testid="inject-trace.js"]').text()).toBe('注入中…');
    expect(wrapper.get('[data-testid="inject-trace.js"]').attributes('disabled')).toBeDefined();

    release?.();
    await flushPromises();
    expect(wrapper.get('[data-testid="inject-trace.js"]').text()).toBe('重新注入');
  });

  // 「不知道怎么用」是这一页原来最大的问题：脚本在哪里跑、日志去哪看、为什么
  // var 不生效，都必须写在页面上，且由测试钉住，免得改版时被删掉。
  it('explains where the script runs and where its output goes', () => {
    const wrapper = mountView();
    const text = wrapper.text();
    expect(text).toContain('小程序当前页面的 JS 上下文');
    expect(text).toContain('window.x = ...');
    expect(text).toContain('[文件名]');
    expect(wrapper.get('[data-testid="open-console"]').attributes('href')).toBe('#/console');
  });

  it('injects a script and keeps the global choice on the backend', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'hook.list') return listPayload([{ filename: 'trace.js', global: false, injected: false, mtime: 1 }]);
      if (method === 'settings.getPaths') return Promise.resolve({ hook_scripts: 'C:/wxtap/hook_scripts' });
      return Promise.resolve({ ok: true, injected: true, summary: '无返回值' });
    });
    const wrapper = mountView();
    useEngineStore().status.miniapp = true;
    await flushPromises();

    await wrapper.get('[data-testid="inject-trace.js"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('hook.inject', { filename: 'trace.js' });

    await wrapper.get('[data-testid="global-trace.js"]').setValue(true);
    await flushPromises();
    expect(call).toHaveBeenCalledWith('hook.setGlobal', { filename: 'trace.js', global: true });
  });

  it('opens the writable script directory instead of asking the user to find it', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'hook.list') return listPayload([]);
      if (method === 'settings.getPaths') return Promise.resolve({ hook_scripts: 'C:/wxtap/hook_scripts' });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mountView();
    await flushPromises();

    expect(wrapper.text()).toContain('C:/wxtap/hook_scripts');
    await wrapper.get('[data-testid="open-script-dir"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('shell.openFolder', { path: 'C:/wxtap/hook_scripts' });
  });

  it('filters scripts by filename', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'hook.list') {
        return listPayload([
          { filename: 'trace.js', global: false, injected: false, mtime: 1 },
          { filename: 'inspect-network.js', global: false, injected: false, mtime: 1 },
        ]);
      }
      return Promise.resolve(method === 'settings.getPaths' ? { hook_scripts: 'C:/h' } : {});
    });
    const wrapper = mountView();
    await flushPromises();
    await wrapper.get('[aria-label="搜索 Hook 脚本"]').setValue('network');
    expect(wrapper.text()).toContain('inspect-network.js');
    expect(wrapper.text()).not.toContain('trace.js');
  });

  it('blocks injection until a miniapp is connected', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'hook.list') return listPayload([{ filename: 'trace.js', global: false, injected: false, mtime: 1 }]);
      return Promise.resolve(method === 'settings.getPaths' ? { hook_scripts: 'C:/h' } : {});
    });
    const wrapper = mountView();
    await flushPromises();
    expect(wrapper.get('[data-testid="inject-trace.js"]').attributes('disabled')).toBeDefined();
    expect(wrapper.text()).toContain('未连接小程序');
    useEngineStore().status.miniapp = true;
    await wrapper.vm.$nextTick();
    expect(wrapper.get('[data-testid="inject-trace.js"]').attributes('disabled')).toBeUndefined();
  });

  it('reports list errors and reverts a global choice the backend rejected', async () => {
    call.mockRejectedValueOnce(new Error('unavailable'));
    const wrapper = mountView();
    await flushPromises();
    expect(wrapper.get('[role="alert"]').text()).toContain('unavailable');

    call.mockImplementation((method: string) => {
      if (method === 'hook.list') return listPayload([{ filename: 'trace.js', global: false, injected: false, mtime: 1 }]);
      if (method === 'hook.setGlobal') return Promise.reject(new Error('invalid hook script name'));
      return Promise.resolve(method === 'settings.getPaths' ? { hook_scripts: 'C:/h' } : {});
    });
    await wrapper.get('[data-testid="refresh-hooks"]').trigger('click');
    await flushPromises();
    await wrapper.get('[data-testid="global-trace.js"]').setValue(true);
    await flushPromises();
    // 保存失败时复选框要回到原状，不能显示一个后端并未接受的设置。
    expect((wrapper.get('[data-testid="global-trace.js"]').element as HTMLInputElement).checked).toBe(false);
  });
});
