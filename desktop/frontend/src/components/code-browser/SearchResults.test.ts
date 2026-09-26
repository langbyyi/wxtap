import { mount } from '@vue/test-utils';
import { describe, expect, it } from 'vitest';
import SearchResults from './SearchResults.vue';

describe('SearchResults', () => {
  it('emits the selected result and renders an empty search state', async () => {
    const wrapper = mount(SearchResults, {
      props: { results: [{ file: 'src/app.ts', line: 4, text: 'needle' }], searching: false, resultPath: (file: string) => file },
    });
    await wrapper.get('button').trigger('click');
    expect(wrapper.emitted('open')).toEqual([[{ file: 'src/app.ts', line: 4, text: 'needle' }]]);

    await wrapper.setProps({ results: [] });
    expect(wrapper.text()).toContain('没有匹配结果');
  });
});
