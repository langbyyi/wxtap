import { mount } from '@vue/test-utils';
import { nextTick } from 'vue';
import { describe, expect, it } from 'vitest';
import CodeTabs from './CodeTabs.vue';

const tab = { name: 'app.js', path: 'app.js', content: 'console.log(1)', language: 'javascript', kind: 'text', dataUrl: '', truncated: false, lines: 1 };

describe('CodeTabs', () => {
  it('emits tab actions without owning file loading', async () => {
    const wrapper = mount(CodeTabs, {
      props: {
        tabs: [{ name: 'app.js', path: 'app.js' }],
        active: 'app.js',
        activeTab: { ...tab },
        rows: [{ number: 1, html: 'console.log(1)' }],
      },
    });

    await wrapper.get('[role="tab"]').trigger('click');
    await wrapper.get('[aria-label="关闭 app.js"]').trigger('click');
    await wrapper.get('[data-testid="close-all-tabs"]').trigger('click');
    await wrapper.get('[data-testid="copy-file-path"]').trigger('click');
    await wrapper.get('[data-testid="wrap-toggle"]').setValue(true);

    expect(wrapper.emitted('select')).toEqual([['app.js']]);
    expect(wrapper.emitted('close')).toEqual([['app.js']]);
    expect(wrapper.emitted('closeAll')).toHaveLength(1);
    expect(wrapper.emitted('copyPath')).toEqual([['app.js']]);
    expect(wrapper.emitted('update:wrap')).toEqual([[true]]);
  });

  it('renders the highlighted html as-is and keeps raw rows out of escaping', () => {
    const wrapper = mount(CodeTabs, {
      props: {
        tabs: [{ name: 'app.js', path: 'app.js' }],
        active: 'app.js',
        activeTab: { ...tab },
        rows: [{ number: 1, html: '<span class="tok-kw">const</span> x' }],
      },
    });
    expect(wrapper.get('.code-text .tok-kw').text()).toBe('const');
    expect(wrapper.get('.code-text').text()).toBe('const x');
  });

  it('disambiguates two tabs that share a file name', async () => {
    const wrapper = mount(CodeTabs, {
      props: {
        tabs: [
          { name: 'page-frame.js', path: 'C:/out/app/page-frame.js' },
          { name: 'page-frame.js', path: 'C:/out/app/subHome/page-frame.js' },
          { name: 'app.js', path: 'C:/out/app/app.js' },
        ],
        active: 'C:/out/app/app.js',
        activeTab: { ...tab, name: 'app.js', path: 'C:/out/app/app.js' },
        rows: [{ number: 1, html: 'x' }],
      },
    });

    // 只有重名的标签才带父目录，其余保持短名
    expect(wrapper.findAll('.tab-select').map((button) => button.text())).toEqual(['app/page-frame.js', 'subHome/page-frame.js', 'app.js']);
  });

  it('numbers the gutter from the window start and offers the rows that are not rendered', async () => {
    const wrapper = mount(CodeTabs, {
      props: {
        tabs: [{ name: 'big.js', path: 'big.js' }],
        active: 'big.js',
        activeTab: { ...tab, name: 'big.js', path: 'big.js', lines: 9000 },
        rows: [{ number: 8000, html: 'a' }, { number: 8001, html: 'b' }],
        totalLines: 9000,
      },
    });

    expect(wrapper.findAll('.code-gutter').map((cell) => cell.text())).toEqual(['8000', '8001']);
    expect(wrapper.get('[data-testid="load-before"]').text()).toContain('上方还有 7999 行未渲染');
    expect(wrapper.get('[data-testid="load-more"]').text()).toContain('下方还有 999 行未渲染');

    await wrapper.get('[data-testid="load-before"]').trigger('click');
    await wrapper.get('[data-testid="load-more"]').trigger('click');
    expect(wrapper.emitted('loadBefore')).toHaveLength(1);
    expect(wrapper.emitted('loadMore')).toHaveLength(1);
  });

  it('exposes hit tools only while a hit line is marked and emits goto/clear', async () => {
    const wrapper = mount(CodeTabs, {
      props: {
        tabs: [{ name: 'app.js', path: 'app.js' }],
        active: 'app.js',
        activeTab: { ...tab, hitLine: 3 },
        rows: [{ number: 3, html: 'hit' }],
      },
    });
    expect(wrapper.get('[data-testid="hit-line-chip"]').text()).toBe('命中行 3');
    expect(wrapper.get('.code-line.is-hit').attributes('data-line')).toBe('3');

    await wrapper.get('[data-testid="goto-hit"]').trigger('click');
    await wrapper.get('[data-testid="clear-hit"]').trigger('click');
    expect(wrapper.emitted('gotoHit')).toHaveLength(1);
    expect(wrapper.emitted('clearHit')).toHaveLength(1);

    await wrapper.setProps({ activeTab: { ...tab } });
    expect(wrapper.find('[data-testid="hit-tools"]').exists()).toBe(false);
  });

  it('draws images as pictures and describes binaries, with no line window for either', async () => {
    // 带着命中行（从扫描结果跳进来的一张图）：图片没有行可跳，行工具同样不该出现
    const base = { ...tab, content: '', lines: 0, hitLine: 3 };
    const wrapper = mount(CodeTabs, {
      props: {
        tabs: [{ name: 'logo.png', path: 'logo.png' }],
        active: 'logo.png',
        activeTab: { ...base, name: 'logo.png', path: 'logo.png', language: 'image/png', kind: 'image', dataUrl: 'data:image/png;base64,AAA' },
        rows: [],
        totalLines: 0,
      },
    });
    expect(wrapper.get('[data-testid="code-image"]').attributes('src')).toBe('data:image/png;base64,AAA');
    expect(wrapper.find('[data-testid="code-content"]').exists()).toBe(false);
    // 换行开关管不着图片，行窗口的两端入口与命中行工具同样不该出现
    expect(wrapper.find('[data-testid="wrap-toggle"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="hit-tools"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="load-before"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="load-more"]').exists()).toBe(false);

    // 图片没内联（超上限）和二进制文件都退到说明分支，而不是画出一堆乱码
    await wrapper.setProps({ activeTab: { ...base, name: 'huge.png', path: 'huge.png', language: 'image/png', kind: 'image', content: '图片超过内联上限，未显示。' } });
    expect(wrapper.find('[data-testid="code-image"]').exists()).toBe(false);
    expect(wrapper.get('[data-testid="binary-notice"]').text()).toContain('图片超过内联上限');

    await wrapper.setProps({ activeTab: { ...base, name: 'app.wasm', path: 'app.wasm', kind: 'binary', content: '二进制文件（1.2KB），无法按源码显示。' } });
    expect(wrapper.get('[data-testid="binary-notice"]').text()).toContain('二进制文件');
  });

  it('centers the hit line through its own offsets and returns to the window top otherwise', async () => {
    const wrapper = mount(CodeTabs, {
      props: {
        tabs: [{ name: 'big.js', path: 'big.js' }],
        active: 'big.js',
        activeTab: { ...tab, name: 'big.js', path: 'big.js', lines: 9000, hitLine: 8001 },
        rows: [{ number: 8000, html: 'a' }, { number: 8001, html: 'hit' }],
        totalLines: 9000,
      },
    });
    const pre = wrapper.get('[data-testid="code-content"]').element as HTMLElement;
    Object.defineProperty(pre, 'clientHeight', { value: 400, configurable: true });
    // 代码区不是定位元素：偏移必须靠 rect 差值量，否则会算到页面顶部去
    pre.getBoundingClientRect = () => ({ top: 100 }) as DOMRect;
    const hit = wrapper.get('.code-line.is-hit').element as HTMLElement;
    hit.getBoundingClientRect = () => ({ top: 900 }) as DOMRect;

    const expose = wrapper.vm as unknown as { revealHit: () => void; scrollToTop: () => void };
    expose.revealHit();
    await nextTick();
    expect(pre.scrollTop).toBe(600);

    expose.scrollToTop();
    await nextTick();
    expect(pre.scrollTop).toBe(0);
  });
});
