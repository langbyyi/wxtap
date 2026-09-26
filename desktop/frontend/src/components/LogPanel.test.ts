import { mount } from '@vue/test-utils';
import { nextTick } from 'vue';
import { describe, expect, it, vi } from 'vitest';
import LogPanel, { type LogRow } from './LogPanel.vue';

const ROWS: LogRow[] = [
  { key: 1, time: '10:00:00.120', level: 'info', text: '引擎已启动' },
  { key: 2, time: '10:00:01.000', level: 'warn', text: '构建没有地址表' },
  { key: 3, time: '10:00:02.500', level: 'error', text: '注入失败' },
];

type PanelProps = {
  title: string;
  entries: LogRow[];
  levels: string[];
  testId: string;
  searchLabel: string;
  searchPlaceholder: string;
  emptyTitle: string;
  emptyHint: string;
  cap?: number;
  paused?: boolean;
};

function mountPanel(overrides: Partial<PanelProps> = {}) {
  return mount(LogPanel, {
    props: {
      title: '运行日志',
      entries: ROWS,
      levels: ['info', 'warn', 'error'],
      testId: 'log',
      searchLabel: '筛选运行日志',
      searchPlaceholder: '筛选日志',
      emptyTitle: '暂无日志',
      emptyHint: '空提示',
      cap: 500,
      ...overrides,
    },
  });
}

// jsdom 没有布局：把滚动几何钉在元素上才能驱动自动滚动的判定。
function stubScrollGeometry(wrapper: ReturnType<typeof mountPanel>) {
  const el = wrapper.get('[data-testid="log-list"]').element as HTMLElement;
  const geometry = { scrollHeight: 900, clientHeight: 300, scrollTop: 0 };
  Object.defineProperty(el, 'scrollHeight', { value: geometry.scrollHeight, configurable: true });
  Object.defineProperty(el, 'clientHeight', { value: geometry.clientHeight, configurable: true });
  Object.defineProperty(el, 'scrollTop', {
    get: () => geometry.scrollTop,
    set: (value: number) => { geometry.scrollTop = value; },
    configurable: true,
  });
  const scrollTo = vi.fn(function scrollTo(options?: { top?: number }) {
    if (options && typeof options.top === 'number') geometry.scrollTop = options.top;
  });
  el.scrollTo = scrollTo as unknown as typeof el.scrollTo;
  return { el, scrollTo, geometry };
}

describe('LogPanel', () => {
  it('renders rows with level marks and shows the count against the cap', () => {
    const wrapper = mountPanel();

    expect(wrapper.get('[data-testid="log-count"]').text()).toBe('3 / 500');
    const levels = wrapper.findAll('.log-level').map((node) => node.classes());
    expect(levels).toContainEqual(['log-level', 'warn']);
    expect(levels).toContainEqual(['log-level', 'error']);
    expect(wrapper.get('[data-testid="log-list"]').text()).toContain('注入失败');
  });

  it('counts each level into the level dropdown', async () => {
    const wrapper = mountPanel();

    await wrapper.get('[aria-label="日志级别"]').setValue('error');
    expect(wrapper.findAll('[data-testid="log-list"] li')).toHaveLength(1);
    expect(wrapper.get('[data-testid="log-list"]').text()).toContain('注入失败');

    expect(wrapper.find('option[value="error"]').text()).toContain('1');
  });

  it('filters rows by keyword across time, level and text', async () => {
    const wrapper = mountPanel();

    await wrapper.get('[aria-label="筛选运行日志"]').setValue('10:00:02');
    expect(wrapper.findAll('[data-testid="log-list"] li')).toHaveLength(1);
  });

  it('pins to the bottom while new rows arrive and offers a way back after scrolling up', async () => {
    const wrapper = mountPanel();
    const { scrollTo, geometry } = stubScrollGeometry(wrapper);
    // 挂载即回放补历史：初轮渲染就应贴底。
    await nextTick();
    await nextTick();
    expect(scrollTo).toHaveBeenCalledWith({ top: 900 });
    expect(wrapper.findAll('[data-testid="bottom-log"]')).toHaveLength(0);

    // 上滑阅读：解除贴底，浮出「回到底部」。
    geometry.scrollTop = 100;
    await wrapper.get('[data-testid="log-list"]').trigger('scroll');
    expect(wrapper.findAll('[data-testid="bottom-log"]')).toHaveLength(1);

    // 未贴底时新行到达不再强行拉底。
    await wrapper.setProps({ entries: [...ROWS, { key: 4, time: '10:00:03', level: 'info', text: '新行' }] });
    await nextTick();
    await nextTick();
    const afterBurst = scrollTo.mock.calls.length;
    expect(wrapper.get('[data-testid="log-count"]').text()).toBe('4 / 500');

    await wrapper.get('[data-testid="bottom-log"]').trigger('click');
    expect(scrollTo).toHaveBeenCalledTimes(afterBurst + 1);
    expect(wrapper.findAll('[data-testid="bottom-log"]')).toHaveLength(0);
  });

  it('stops auto-scrolling while paused', async () => {
    const wrapper = mountPanel({ paused: true });
    const { scrollTo } = stubScrollGeometry(wrapper);
    await nextTick();
    await nextTick();
    const base = scrollTo.mock.calls.length;

    await wrapper.setProps({ entries: [...ROWS, { key: 4, time: '10:00:03', level: 'info', text: '暂停中的新行' }] });
    await nextTick();
    await nextTick();

    expect(scrollTo).toHaveBeenCalledTimes(base);
  });
});
