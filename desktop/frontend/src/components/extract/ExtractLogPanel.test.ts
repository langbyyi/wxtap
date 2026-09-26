import { mount } from '@vue/test-utils';
import { describe, expect, it } from 'vitest';
import ExtractLogPanel from './ExtractLogPanel.vue';

describe('ExtractLogPanel', () => {
  it('renders logs and emits copy/clear actions', async () => {
    const wrapper = mount(ExtractLogPanel, { props: { logs: ['开始', '完成'] } });
    expect(wrapper.get('[role="log"]').text()).toContain('开始');
    await wrapper.get('[data-testid="copy-extract-logs"]').trigger('click');
    await wrapper.get('[data-testid="clear-extract-logs"]').trigger('click');
    expect(wrapper.emitted('copy')).toHaveLength(1);
    expect(wrapper.emitted('clear')).toHaveLength(1);
  });
});
