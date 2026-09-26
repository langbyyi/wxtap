import { flushPromises, mount } from '@vue/test-utils';
import { createPinia } from 'pinia';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import App from './App.vue';
import { router } from './router';
import ExtractView from './views/ExtractView.vue';

describe('App', () => {
  let wrapper: ReturnType<typeof mount>;

  beforeEach(async () => {
    localStorage.clear();
    await router.push('/control');
    await router.isReady();
    wrapper = mount(App, { attachTo: document.body, global: { plugins: [createPinia(), router] } });
  });

  afterEach(() => wrapper.unmount());

  it('navigates between functional groups and pages', async () => {
    await wrapper.get('[data-testid="group-runtime"]').trigger('click');
    await flushPromises();
    await wrapper.get('[data-testid="nav-navigator"]').trigger('click');
    await flushPromises();

    expect(router.currentRoute.value.path).toBe('/navigator');
    expect(wrapper.get('[data-testid="nav-navigator"]').classes()).toContain('is-active');
    expect(wrapper.get('[data-testid="group-runtime"]').classes()).toContain('is-active');
  });

  it('keeps extract results in memory when returning from another page', async () => {
    await router.push('/extract');
    await flushPromises();
    const extractUid = wrapper.findComponent(ExtractView).vm.$.uid;

    await router.push('/code');
    await flushPromises();
    await router.push('/extract');
    await flushPromises();

    expect(wrapper.findComponent(ExtractView).vm.$.uid).toBe(extractUid);
  });

  it('focuses the page search box with the slash shortcut and ignores it while typing', async () => {
    await flushPromises();
    const search = wrapper.get('input[placeholder="筛选日志"]').element as HTMLInputElement;
    expect(document.activeElement).not.toBe(search);

    const portInput = wrapper.get('[data-testid="cdp-port"]').element as HTMLInputElement;
    portInput.focus();
    portInput.dispatchEvent(new KeyboardEvent('keydown', { key: '/', bubbles: true }));
    await flushPromises();
    expect(document.activeElement).toBe(portInput);

    document.body.dispatchEvent(new KeyboardEvent('keydown', { key: '/', bubbles: true }));
    await flushPromises();
    expect(document.activeElement).toBe(search);
  });
  it('exposes every app page through the grouped navigation', async () => {
    const groupPaths: Array<[string, string[]]> = [
      ['session', ['control']],
      ['runtime', ['navigator', 'devtools', 'vconsole', 'hook']],
      ['calls', ['wxapi', 'cloud', 'traffic']],
      ['source', ['extract', 'code']],
      ['keys', ['sessionkey', 'ak']],
      ['local', ['settings', 'mcp']],
      ['help', ['faq', 'feedback']],
    ];

    for (const [key, paths] of groupPaths) {
      await wrapper.get(`[data-testid="group-${key}"]`).trigger('click');
      await flushPromises();
      for (const path of paths) {
        expect(wrapper.get(`[data-testid="nav-${path}"]`).attributes('href')).toBe(`#/${path}`);
      }
    }

    expect(wrapper.text()).not.toContain('迁移中');
  });

  it('explains a menu item in a hover card instead of on the page', async () => {
    const link = wrapper.get('[data-testid="nav-control"]');
    expect(link.text()).toBe('状态');
    await link.trigger('mouseenter');
    await flushPromises();
    const intro = wrapper.get('[data-testid="nav-intro"]');
    expect(intro.text()).toContain('状态');
    expect(intro.text()).toContain('引擎');
    expect(link.attributes('aria-describedby')).toBe('nav-intro');
    await link.trigger('mouseleave');
    expect(wrapper.find('[data-testid="nav-intro"]').exists()).toBe(false);
    expect(wrapper.find('.subtitle').exists()).toBe(false);
  });

  it('shows live backend status and current mini-program information', async () => {
    window.__onBackendEvent?.('status', { frida: true, miniapp: false, devtools: true });
    window.__onBackendEvent?.('app_info', { name: '测试小程序', appid: 'wx123' });
    await wrapper.vm.$nextTick();

    expect(wrapper.find('[data-testid="status-frida"]').exists()).toBe(false);
    expect(wrapper.get('[data-testid="github-link"]').attributes('aria-label')).toBe('GitHub');
    expect(wrapper.get('[data-testid="app-version"]').text()).toMatch(/^v\d+\.\d+\.\d+/);
    expect(wrapper.get('[data-testid="current-miniapp"]').text()).toBe('测试小程序');
  });

  it('shows the miniapp from engine.status when the push was not replayed', async () => {
    const previous = window.go;
    window.go = {
      main: {
        App: {
          Call: async (method: string) => {
            if (method === 'engine.status') {
              return JSON.stringify({ result: { frida: true, miniapp: true, devtools: true, appInfo: { appid: 'wx999', name: '拉到的小程序' } } });
            }
            return JSON.stringify({ result: {} });
          },
        },
      },
    };
    wrapper.unmount();
    wrapper = mount(App, { attachTo: document.body, global: { plugins: [createPinia(), router] } });
    await flushPromises();

    expect(wrapper.get('[data-testid="current-miniapp"]').text()).toBe('拉到的小程序');
    window.go = previous;
  });

  it('falls back to the appid when the core has not resolved a name', async () => {
    window.__onBackendEvent?.('app_info', { name: '', appid: 'wxabc' });
    await wrapper.vm.$nextTick();

    expect(wrapper.get('[data-testid="current-miniapp"]').text()).toBe('wxabc');
  });

  it('does not pull focus to the palette trigger when a closed palette stays closed', async () => {
    const trigger = wrapper.get('.palette-trigger').element;
    const search = wrapper.get('input[placeholder="筛选日志"]').element as HTMLInputElement;
    search.focus();
    await wrapper.get('[data-testid="group-runtime"]').trigger('click');
    await flushPromises();

    expect(document.activeElement).not.toBe(trigger);
    expect(wrapper.get('.palette-trigger').attributes('aria-expanded')).toBe('false');
  });

  it('opens the command palette from the keyboard and focuses its search field', async () => {
    const trigger = wrapper.get('.palette-trigger');
    (trigger.element as HTMLElement).focus();
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'k', ctrlKey: true }));
    await flushPromises();

    expect(trigger.attributes('aria-expanded')).toBe('true');
    const input = wrapper.get('.palette-input');
    expect(input.element).toBe(document.activeElement);

    await input.trigger('keydown', { key: 'Escape' });
    await flushPromises();
    expect(wrapper.find('.palette-card').exists()).toBe(false);
    expect(trigger.attributes('aria-expanded')).toBe('false');
    expect(document.activeElement).toBe(trigger.element);
  });

  it('moves through palette results with arrow keys and opens the selected page', async () => {
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'k', ctrlKey: true }));
    await flushPromises();
    const input = wrapper.get('.palette-input');
    await input.trigger('keydown', { key: 'ArrowDown' });
    expect(wrapper.get('[data-palette-index="1"]').classes()).toContain('is-active');

    await input.trigger('keydown', { key: 'Enter' });
    await flushPromises();
    expect(router.currentRoute.value.path).toBe('/navigator');
  });

  it('toggles and persists the color theme', async () => {
    await wrapper.get('[data-testid="theme-toggle"]').trigger('click');

    expect(document.documentElement.dataset.theme).toBe('light');
    expect(localStorage.getItem('theme')).toBe('light');
  });
});
