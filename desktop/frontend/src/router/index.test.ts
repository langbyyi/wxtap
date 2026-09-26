import { describe, expect, it } from 'vitest';
import { router } from './index';
import AssetsView from '../views/AssetsView.vue';
import CodeBrowserView from '../views/CodeBrowserView.vue';
import ConsoleView from '../views/ConsoleView.vue';
import DevtoolsView from '../views/DevtoolsView.vue';
import ExtractView from '../views/ExtractView.vue';
import FaqView from '../views/FaqView.vue';
import FeedbackView from '../views/FeedbackView.vue';
import McpView from '../views/McpView.vue';
import AkView from '../views/AkView.vue';
import SettingsView from '../views/SettingsView.vue';

describe('router', () => {
  it('redirects the root route to the engine control page', async () => {
    await router.push('/');
    await router.isReady();

    expect(router.currentRoute.value.path).toBe('/control');
  });

  it('routes every known path to its view', () => {
    const appRoutes = [
      ['/devtools', DevtoolsView, 'DevTools'],
      ['/console', ConsoleView, 'Console 日志'],
      ['/code', CodeBrowserView, '代码浏览'],
      ['/assets', AssetsView, '资产清单'],
      ['/extract', ExtractView, '反编译'],
      ['/faq', FaqView, '使用帮助'],
      ['/mcp', McpView, 'MCP 服务'],
      ['/ak', AkView, '微信 AK'],
      ['/settings', SettingsView, '设置'],
      ['/feedback', FeedbackView, '交流反馈'],
    ] as const;

    for (const [path, component, title] of appRoutes) {
      const route = router.resolve(path);
      expect(route.matched).toHaveLength(1);
      expect(route.matched[0]?.components?.default).toBe(component);
      expect(route.matched[0]?.meta.title).toBe(title);
    }
  });

  // 新增的两个页面：路由与侧边栏条目（App.vue groups）各认一份，这里钉住路由侧。
  it('exposes the assets page under its group', () => {
    expect(router.resolve('/assets').matched[0]?.meta.title).toBe('资产清单');
  });
});
