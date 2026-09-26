import { mount } from '@vue/test-utils';
import { describe, expect, it } from 'vitest';
import FaqView from './FaqView.vue';
import PageHeader from '../components/PageHeader.vue';
import { router } from '../router';

function mountFaq() {
  return mount(FaqView, { global: { plugins: [router] } });
}

describe('FaqView', () => {
  it('renders every question as its own collapsed row', () => {
    const wrapper = mountFaq();
    expect(wrapper.findComponent(PageHeader).exists()).toBe(true);
    expect(wrapper.find('.page-toolbar').exists()).toBe(false);
    expect(wrapper.findAll('.help-question')).toHaveLength(21);
    const expanded = wrapper.findAll('.help-question').map((node) => node.attributes('aria-expanded'));
    expect(expanded.every((value) => value === 'false')).toBe(true);
    expect(wrapper.get('.help-search button').text()).toContain('展开全部');
  });

  it('shows the quick-start order with links into the app and can collapse it', async () => {
    const wrapper = mountFaq();
    expect(wrapper.text()).toContain('快速上手');
    expect(wrapper.get('.help-steps').text()).toContain('先在微信里打开一个小程序');
    const firstLink = wrapper.get('.help-quickstart .help-link');
    expect(firstLink.attributes('href')).toContain('/control');
    expect(firstLink.text()).toContain('状态');

    await wrapper.get('.help-quickstart .panel-header button').trigger('click');
    expect(wrapper.find('.help-steps').isVisible()).toBe(false);
  });

  it('reaches any entry from the search box', async () => {
    const wrapper = mountFaq();
    await wrapper.get('[aria-label="搜索帮助"]').setValue('WeChatAppEx');
    expect(wrapper.text()).toContain('Frida 一直显示「未连接」');
    expect(wrapper.text()).toContain('未找到微信 WMPF 宿主进程');
  });

  it('auto-expands matches while searching so answers are visible without extra clicks', async () => {
    const wrapper = mountFaq();
    await wrapper.get('[aria-label="搜索帮助"]').setValue('VACUUM');
    expect(wrapper.get('.help-question').attributes('aria-expanded')).toBe('true');
    expect(wrapper.get('.help-question').element.closest('.help-item')?.textContent).toContain('VACUUM');

    await wrapper.get('[aria-label="搜索帮助"]').setValue('');
    const expanded = wrapper.findAll('.help-question').map((node) => node.attributes('aria-expanded'));
    expect(expanded.filter((value) => value === 'true')).toHaveLength(0);
  });

  it('filters by keyword across answers and shows the empty state when nothing matches', async () => {
    const wrapper = mountFaq();
    await wrapper.get('[aria-label="搜索帮助"]').setValue('session_key');
    expect(wrapper.findAll('.help-item')).toHaveLength(1);
    expect(wrapper.text()).toContain('如何获取 session_key 与 iv？');

    await wrapper.get('[aria-label="搜索帮助"]').setValue('zzz-没有这条内容');
    expect(wrapper.findAll('.help-item')).toHaveLength(0);
    expect(wrapper.get('.empty-state').text()).toContain('没有匹配的内容');
    // 空结果要给出路：指向「交流反馈」的 Issue 入口
    expect(wrapper.get('.empty-state').text()).toContain('交流反馈');
  });

  it('marks search hits inside titles and answers', async () => {
    const wrapper = mountFaq();
    await wrapper.get('[aria-label="搜索帮助"]').setValue('VACUUM');
    const marks = wrapper.findAll('mark');
    expect(marks.length).toBeGreaterThan(0);
    expect(marks[0].text().toLowerCase()).toContain('vacuum');
  });

  it('exposes accessible expand state', async () => {
    const wrapper = mountFaq();
    const question = wrapper.get('.help-question');
    await question.trigger('click');
    expect(question.attributes('aria-expanded')).toBe('true');
    await question.trigger('click');
    expect(question.attributes('aria-expanded')).toBe('false');
  });
});
