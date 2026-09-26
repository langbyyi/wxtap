import { flushPromises, mount } from '@vue/test-utils';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import FeedbackView from './FeedbackView.vue';
import PageHeader from '../components/PageHeader.vue';

const { call, notify } = vi.hoisted(() => ({ call: vi.fn(), notify: vi.fn() }));
vi.mock('../api/bridge', () => ({ backend: { call } }));
vi.mock('../utils/notify', () => ({ copyText: vi.fn(), notify }));

describe('FeedbackView', () => {
  beforeEach(() => {
    localStorage.clear();
    call.mockReset();
    notify.mockClear();
  });
  it('keeps document actions with feedback content', async () => {
    call.mockResolvedValue({ text: '# 联系我们' });
    notify.mockClear();
    const wrapper = mount(FeedbackView);
    expect(wrapper.findComponent(PageHeader).exists()).toBe(true);
    await flushPromises();
    expect(wrapper.find('.page-toolbar').exists()).toBe(false);
    expect(wrapper.get('.markdown .panel-header').text()).toContain('打开原文档');
  });
  it('does not toast on the initial load, only on an explicit reload', async () => {
    call.mockResolvedValue({ text: '# 联系我们' });
    const wrapper = mount(FeedbackView);
    await flushPromises();
    expect(notify).not.toHaveBeenCalled();

    const reload = wrapper.findAll('button').find((button) => button.text() === '重新加载');
    expect(reload).toBeDefined();
    await reload!.trigger('click');
    await flushPromises();
    expect(notify).toHaveBeenCalledWith('反馈内容已更新', 'success');
  });
  it('shows the error panel with a retry when the first load fails', async () => {
    call.mockRejectedValue(new Error('HTTP 404'));
    const wrapper = mount(FeedbackView);
    await flushPromises();
    expect(wrapper.get('[role="alert"]').text()).toContain('HTTP 404');
    const actions = wrapper.findAll('button').map((button) => button.text());
    expect(actions).toContain('重试');
    // 文档拉不到时也要有独立于文档的反馈入口
    expect(actions).toContain('提交 Issue');
  });
  it('keeps the loaded content visible when a reload fails', async () => {
    call.mockResolvedValueOnce({ text: '# 联系我们\n\n[群](https://example.com)' });
    const wrapper = mount(FeedbackView);
    await flushPromises();

    call.mockRejectedValue(new Error('网络超时'));
    await wrapper.findAll('button').find((button) => button.text() === '重新加载')!.trigger('click');
    await flushPromises();
    // 内容还在，错误以横幅提示而不是整页替换
    expect(wrapper.get('a').attributes('href')).toBe('https://example.com');
    expect(wrapper.get('[role="alert"]').text()).toContain('网络超时');
    expect(wrapper.get('[role="alert"]').text()).toContain('上次内容');
  });
  it('renders untrusted HTML as text but keeps the published markdown structure', async () => {
    call.mockResolvedValue({
      text: '# 联系我们\n\n<img src=x onerror=alert(1)>\n\n[官网](https://example.com)\n\n> 提示一\n> 提示二\n\n- 条目甲\n- 条目乙\n\n1. 第一\n2. 第二\n\n**加粗**与`代码`',
    });
    const wrapper = mount(FeedbackView);
    await flushPromises();
    expect(call).toHaveBeenCalledWith('fetch.md', { url: expect.any(String) });
    // 不可信内容保持纯文本，不产生任何注入元素
    expect(wrapper.find('img').exists()).toBe(false);
    expect(wrapper.text()).toContain('<img src=x onerror=alert(1)>');
    expect(wrapper.get('a').attributes('href')).toBe('https://example.com');
    // 发布文档用的 markdown 结构要真的渲染出来，而不是露出 -、>、** 记号
    expect(wrapper.get('blockquote').text()).toContain('提示一');
    expect(wrapper.get('blockquote').text()).toContain('提示二');
    expect(wrapper.findAll('.md-list')).toHaveLength(2);
    expect(wrapper.findAll('li')).toHaveLength(4);
    expect(wrapper.get('strong').text()).toBe('加粗');
    expect(wrapper.get('code').text()).toBe('代码');
    // 无壳（浏览器调试）时不拦截：链接保持默认跳转，不走 shell.openUrl。
    await wrapper.get('a').trigger('click');
    expect(call).not.toHaveBeenCalledWith('shell.openUrl', expect.anything());
  });
  it('opens rendered links via shell.openUrl inside the Wails shell', async () => {
    call.mockResolvedValue({ text: '# 联系我们\n\n[官网](https://example.com)' });
    (window as { go?: unknown }).go = { main: { App: { Call: vi.fn() } } };
    try {
      const wrapper = mount(FeedbackView);
      await flushPromises();
      await wrapper.get('a').trigger('click');
      expect(call).toHaveBeenCalledWith('shell.openUrl', { url: 'https://example.com' });
    } finally {
      delete (window as { go?: unknown }).go;
    }
  });
  it('opens the release repository issue tracker for submitting feedback', async () => {
    call.mockResolvedValue({ text: '# 联系我们' });
    (window as { go?: unknown }).go = { main: { App: { Call: vi.fn() } } };
    try {
      const wrapper = mount(FeedbackView);
      await flushPromises();
      const issues = wrapper.findAll('button').find((button) => button.text() === '提交 Issue');
      expect(issues).toBeDefined();
      await issues!.trigger('click');
      expect(call).toHaveBeenCalledWith('shell.openUrl', { url: 'https://github.com/langbyyi/wxtap/issues' });
    } finally {
      delete (window as { go?: unknown }).go;
    }
  });
  it('paints cached content immediately and refreshes it in the background', async () => {
    localStorage.setItem('feedback-doc-cache', JSON.stringify({ text: '# 缓存标题', at: '09/24 03:00' }));
    call.mockResolvedValue({ text: '# 新标题' });
    const wrapper = mount(FeedbackView);
    // 缓存先上屏：不等网络返回
    expect(wrapper.text()).toContain('缓存标题');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('fetch.md', { url: expect.any(String) });
    expect(wrapper.text()).toContain('新标题');
    expect(localStorage.getItem('feedback-doc-cache')).toContain('新标题');
  });
});
