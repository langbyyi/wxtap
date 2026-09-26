import { mount } from '@vue/test-utils';
import { describe, expect, it } from 'vitest';
import PageHeader from './PageHeader.vue';
import EmptyState from './EmptyState.vue';
import ErrorState from './ErrorState.vue';
import SplitPane from './SplitPane.vue';

describe('page primitives', () => {
  // 页头只提供无障碍标题与工具条：页面标题已经由顶部分组子导航给出，
  // 再渲染一遍可见标题/描述就是重复。
  it('keeps the page title for assistive tech and shows only the actions', () => {
    const header = mount(PageHeader, {
      props: { title: '标题', titleId: 'page-title' },
      slots: { actions: '<button>刷新</button>' },
    });

    expect(header.get('h1').attributes('id')).toBe('page-title');
    expect(header.get('h1').text()).toBe('标题');
    expect(header.get('h1').classes()).toContain('sr-only');
    expect(header.get('.page-toolbar').text()).toContain('刷新');
  });

  it('renders no toolbar when the page passes no actions', () => {
    const header = mount(PageHeader, { props: { title: '标题', titleId: 'page-title' } });

    expect(header.find('.page-toolbar').exists()).toBe(false);
  });

  it('renders an empty state with its action slot and lets an error state retry', async () => {
    const empty = mount(EmptyState, { props: { title: '暂无数据', description: '先连接' }, slots: { default: '<button>重试</button>' } });
    expect(empty.get('.empty-state').text()).toContain('暂无数据');
    expect(empty.get('.empty-state').text()).toContain('先连接');
    expect(empty.get('.empty-state button').text()).toBe('重试');

    const error = mount(ErrorState, { props: { message: '失败', retryable: true } });
    await error.get('button').trigger('click');
    expect(error.emitted('retry')).toHaveLength(1);
  });

  it('keeps both panes present at narrow viewport widths', () => {
    const previous = window.innerWidth;
    Object.defineProperty(window, 'innerWidth', { configurable: true, value: 640 });
    const split = mount(SplitPane, { slots: { main: '主区', side: '侧区' } });
    expect(split.find('.split-pane-main').exists()).toBe(true);
    expect(split.find('.split-pane-side').exists()).toBe(true);
    Object.defineProperty(window, 'innerWidth', { configurable: true, value: previous });
  });
});
