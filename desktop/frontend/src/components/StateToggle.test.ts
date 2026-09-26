import { enableAutoUnmount, mount } from '@vue/test-utils';
import { afterEach, describe, expect, it } from 'vitest';
import StateToggle from './StateToggle.vue';

enableAutoUnmount(afterEach);

function mountToggle(props: Record<string, unknown> = {}) {
  return mount(StateToggle, {
    props: { active: false, testId: 'sample-toggle', startLabel: '开启捕获', stopLabel: '停止捕获', ...props },
  });
}

describe('StateToggle', () => {
  it('says what pressing it will do, in both states', async () => {
    const wrapper = mountToggle();
    const button = wrapper.get('[data-testid="sample-toggle"]');
    expect(button.text()).toBe('开启捕获');
    expect(button.attributes('aria-pressed')).toBe('false');
    expect(button.classes()).not.toContain('secondary');

    await wrapper.setProps({ active: true });
    expect(button.text()).toBe('停止捕获');
    expect(button.attributes('aria-pressed')).toBe('true');
    // 开着时不是主操作：降成次要样式，主色留给「去做点什么」
    expect(button.classes()).toContain('secondary');
  });

  it('emits the direction the state calls for', async () => {
    const wrapper = mountToggle();
    await wrapper.get('[data-testid="sample-toggle"]').trigger('click');
    expect(wrapper.emitted('start')).toHaveLength(1);
    expect(wrapper.emitted('stop')).toBeUndefined();

    await wrapper.setProps({ active: true });
    await wrapper.get('[data-testid="sample-toggle"]').trigger('click');
    expect(wrapper.emitted('stop')).toHaveLength(1);
  });

  it('shows the in-flight direction and refuses clicks while it is in flight', async () => {
    const wrapper = mountToggle({ pending: 'start' });
    const button = wrapper.get('[data-testid="sample-toggle"]');
    expect(button.text()).toBe('启动中…');
    expect(button.attributes('disabled')).toBeDefined();
    await button.trigger('click');
    expect(wrapper.emitted('start')).toBeUndefined();

    await wrapper.setProps({ active: true, pending: 'stop', stoppingLabel: '关闭中…' });
    expect(button.text()).toBe('关闭中…');
    expect(button.attributes('disabled')).toBeDefined();
  });

  it('takes the disabled condition from the caller, per direction', async () => {
    const wrapper = mountToggle({ disabled: true });
    const button = wrapper.get('[data-testid="sample-toggle"]');
    expect(button.attributes('disabled')).toBeDefined();

    // 开着时「停止」不该因为「启动条件不满足」被一起禁掉 —— 调用方按方向算好传进来
    await wrapper.setProps({ active: true, disabled: false });
    expect(button.attributes('disabled')).toBeUndefined();
  });

  it('keeps the hover hint on the direction it belongs to', async () => {
    const wrapper = mountToggle({ startTitle: '有封号风险' });
    expect(wrapper.get('[data-testid="sample-toggle"]').attributes('title')).toBe('有封号风险');
    await wrapper.setProps({ active: true, stopTitle: '直接关闭' });
    expect(wrapper.get('[data-testid="sample-toggle"]').attributes('title')).toBe('直接关闭');
  });
});
