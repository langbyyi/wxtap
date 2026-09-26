import { mount } from '@vue/test-utils';
import { describe, expect, it } from 'vitest';
import ExtractFindingsPanel from './ExtractFindingsPanel.vue';

const finding = { id: 'f1', category: 'secret', title: 'Token', severity: 'high' as const, value: 'raw', masked: '***', file: 'app.js', line: 3 };

describe('ExtractFindingsPanel', () => {
  it('emits filters and finding actions while rendering the result table', async () => {
    const wrapper = mount(ExtractFindingsPanel, {
      props: {
        resultApp: 'wx1', findingQuery: '', findingCategories: [{ key: 'secret', label: '账号密码', count: 1 }],
        categoryFilter: 'all', visibleFindings: [finding], pagedFindings: [finding], findingPage: 2, findingPageCount: 2, pageOffset: 50, resultOutputDir: 'C:/out',
      },
    });

    // 序号接着上一页往下排：分页大小由页面给，面板不自己算
    expect(wrapper.get('[data-testid="finding-f1"] td').text()).toBe('51');

    await wrapper.get('[data-testid="finding-search"]').setValue('token');
    await wrapper.get('[data-testid="finding-f1"] button').trigger('click');
    await wrapper.get('[data-testid="category-secret"]').trigger('click');

    expect(wrapper.emitted('update:findingQuery')).toEqual([['token']]);
    expect(wrapper.emitted('copyFinding')).toEqual([[finding]]);
    expect(wrapper.emitted('update:categoryFilter')).toEqual([['secret']]);
    expect(wrapper.find('[data-testid="severity-high"]').exists()).toBe(false);
  });

  // 表格是唯一滚动的东西，表头粘在它的顶端；翻页控件必须在滚动区之外，否则表格
  // 滚到下面时翻页也跟着滚走（jsdom 没有布局，能守的就是这个结构）。
  it('keeps the pagination outside the scrolling table area', () => {
    const wrapper = mount(ExtractFindingsPanel, {
      props: {
        resultApp: 'wx1', findingQuery: '', findingCategories: [{ key: 'secret', label: '账号密码', count: 120 }],
        categoryFilter: 'all', visibleFindings: [finding], pagedFindings: [finding], findingPage: 1, findingPageCount: 3, pageOffset: 0, resultOutputDir: 'C:/out',
      },
    });

    const wrap = wrapper.get('.table-wrap');
    expect(wrap.find('table').exists()).toBe(true);
    expect(wrap.find('.finding-pagination').exists()).toBe(false);
    expect(wrapper.get('.findings-panel .finding-pagination').text()).toContain('第 1 / 3 页');
  });
});
