import { mount } from '@vue/test-utils';
import { describe, expect, it } from 'vitest';
import ProjectTree from './ProjectTree.vue';

describe('ProjectTree', () => {
  it('emits search, clear and node selection actions', async () => {
    const wrapper = mount(ProjectTree, {
      props: {
        sidebarTitle: 'Demo',
        active: '',
        query: '',
        regex: false,
        searchResults: [],
        searching: false,
        searchTruncated: false,
        openingProject: false,
        projectsCount: 1,
        visibleNodes: [{ node: { name: 'app.js', path: 'app.js', isDir: false }, depth: 0 }],
        resultPath: (file: string) => file,
      },
    });

    await wrapper.get('[data-testid="code-search"]').setValue('app');
    await wrapper.get('[data-testid="tree-app_js"]').trigger('click');

    expect(wrapper.emitted('update:query')?.at(-1)).toEqual(['app']);
    expect(wrapper.emitted('search')).toHaveLength(1);
    expect(wrapper.emitted('select')?.[0]).toEqual([{ name: 'app.js', path: 'app.js', isDir: false }]);
  });
});
