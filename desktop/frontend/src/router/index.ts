import { createRouter, createWebHashHistory } from 'vue-router';
import CodeBrowserView from '../views/CodeBrowserView.vue';
import AkView from '../views/AkView.vue';
import AssetsView from '../views/AssetsView.vue';
import CloudView from '../views/CloudView.vue';
import ConsoleView from '../views/ConsoleView.vue';
import ControlView from '../views/ControlView.vue';
import DevtoolsView from '../views/DevtoolsView.vue';
import ExtractView from '../views/ExtractView.vue';
import FaqView from '../views/FaqView.vue';
import FeedbackView from '../views/FeedbackView.vue';
import HookView from '../views/HookView.vue';
import McpView from '../views/McpView.vue';
import NavigatorView from '../views/NavigatorView.vue';
import SessionKeyView from '../views/SessionKeyView.vue';
import SettingsView from '../views/SettingsView.vue';
import TrafficView from '../views/TrafficView.vue';
import VConsoleView from '../views/VConsoleView.vue';
import WxApiView from '../views/WxApiView.vue';

const migratedRoutes = [
  { path: '/control', component: ControlView, meta: { title: '状态' } },
  { path: '/navigator', component: NavigatorView, meta: { title: '页面路由' } },
  { path: '/console', component: ConsoleView, meta: { title: 'Console 日志' } },
  { path: '/hook', component: HookView, meta: { title: '注入脚本' } },
  { path: '/targets', redirect: '/devtools' },
  { path: '/traffic', component: TrafficView, meta: { title: '历史记录' } },
  { path: '/cloud', component: CloudView, meta: { title: '云函数' } },
  { path: '/wxapi', component: WxApiView, meta: { title: 'WxAPI' } },
  { path: '/vconsole', component: VConsoleView, meta: { title: 'vConsole' } },
  { path: '/devtools', component: DevtoolsView, meta: { title: 'DevTools' } },
  { path: '/code', component: CodeBrowserView, meta: { title: '代码浏览' } },
  { path: '/assets', component: AssetsView, meta: { title: '资产清单' } },
  { path: '/extract', component: ExtractView, meta: { title: '反编译' } },
  { path: '/sessionkey', component: SessionKeyView, meta: { title: 'SessionKey' } },
  { path: '/ak', component: AkView, meta: { title: '微信 AK' } },
  { path: '/faq', component: FaqView, meta: { title: '使用帮助' } },
  { path: '/mcp', component: McpView, meta: { title: 'MCP 服务' } },
  { path: '/settings', component: SettingsView, meta: { title: '设置' } },
  { path: '/feedback', component: FeedbackView, meta: { title: '交流反馈' } },
];

export const router = createRouter({
  history: createWebHashHistory(),
  routes: [
    { path: '/', redirect: '/control' },
    ...migratedRoutes,
  ],
});
