import { mount } from '@vue/test-utils';
import { describe, expect, it } from 'vitest';
import TaskProgress from './TaskProgress.vue';

describe('TaskProgress', () => {
  it('renders progress and errors for active tasks', () => {
    const wrapper = mount(TaskProgress, { props: { tasks: [{ id: 'x', kind: 'extract', phase: 'running', current: 2, total: 4, message: '扫描' }] } });
    expect(wrapper.get('[data-testid="task-x"]')).toBeTruthy();
    expect(wrapper.get('progress').attributes('max')).toBe('4');
  });
});
