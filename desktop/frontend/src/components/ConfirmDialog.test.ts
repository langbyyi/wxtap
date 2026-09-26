import { flushPromises, mount } from '@vue/test-utils';
import { afterEach, describe, expect, it } from 'vitest';
import ConfirmDialog from './ConfirmDialog.vue';

describe('ConfirmDialog', () => {
  afterEach(() => { document.body.innerHTML = ''; });

  it('renders accessible content and emits the selected action', async () => {
    const wrapper = mount(ConfirmDialog, {
      attachTo: document.body,
      props: {
        open: true,
        title: '删除记录',
        message: '确认删除？',
        confirmText: '删除',
        testId: 'confirm-delete',
      },
    });
    await flushPromises();

    const dialog = wrapper.get('[role="dialog"]');
    expect(dialog.attributes('aria-modal')).toBe('true');
    expect(dialog.attributes('aria-label')).toBe('删除记录');
    expect(wrapper.text()).toContain('确认删除？');
    expect(wrapper.get('[data-testid="confirm-delete"]').element).toBe(document.activeElement);

    await wrapper.get('[data-testid="confirm-delete"]').trigger('click');
    await wrapper.get('.modal-actions .secondary').trigger('click');
    expect(wrapper.emitted('confirm')).toHaveLength(1);
    expect(wrapper.emitted('cancel')).toHaveLength(1);
    wrapper.unmount();
  });

  it('supports Escape and restores focus to the trigger', async () => {
    const trigger = document.createElement('button');
    document.body.append(trigger);
    trigger.focus();

    const wrapper = mount(ConfirmDialog, {
      attachTo: document.body,
      props: {
        open: false,
        title: '清空记录',
        message: '确认清空？',
        testId: 'confirm-records',
      },
    });
    await wrapper.setProps({ open: true });
    await flushPromises();

    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
    expect(wrapper.emitted('cancel')).toHaveLength(1);

    await wrapper.setProps({ open: false });
    await flushPromises();
    expect(document.activeElement).toBe(trigger);
    wrapper.unmount();
  });
});
