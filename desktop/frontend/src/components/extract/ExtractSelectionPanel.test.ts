import { mount } from '@vue/test-utils';
import { describe, expect, it } from 'vitest';
import ExtractSelectionPanel from './ExtractSelectionPanel.vue';
import type { ExtractApp } from './types';

const app: ExtractApp = {
  appid: 'wx1', name: 'Demo', packages: [{ appid: 'wx1', path: 'a.wxapkg' }], decompiled: false,
  scanned: false, mtime: 0, outputDir: '', iconDataURL: '', iconBroken: false,
};

describe('ExtractSelectionPanel', () => {
  it('emits search and app actions without owning extraction state', async () => {
    const wrapper = mount(ExtractSelectionPanel, {
      props: {
        apps: [app], resultApp: '', query: '', running: '', actionableCount: 1,
        formatTime: () => '', primaryLabel: () => '反编译', appStatusLabel: () => '待反编译',
      },
    });

    await wrapper.get('[data-testid="extract-search"]').setValue('Demo');
    await wrapper.get('[data-testid="primary-wx1"]').trigger('click');
    await wrapper.get('[data-testid="decompile-all"]').trigger('click');

    expect(wrapper.emitted('update:query')).toEqual([['Demo']]);
    expect(wrapper.emitted('runPrimary')).toEqual([[app]]);
    expect(wrapper.emitted('decompileAll')).toHaveLength(1);
  });

  it('shows the AppID when no verified package name is available', () => {
    const wrapper = mount(ExtractSelectionPanel, {
      props: {
        apps: [{ ...app, appid: 'wxdeadbeefdeadbeef', name: 'wxdeadbeefdeadbeef' }], resultApp: '', query: '', running: '', actionableCount: 1,
        formatTime: () => '', primaryLabel: () => '反编译', appStatusLabel: () => '待反编译',
      },
    });

    expect(wrapper.get('.app-card-name').text()).toBe('wxdeadbeefdeadbeef');
    expect(wrapper.text()).not.toContain('本地包未声明');
  });
});
