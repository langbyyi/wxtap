import { flushPromises, mount } from '@vue/test-utils';
import { afterEach, describe, expect, it, vi } from 'vitest';
import CodeBrowserView from './CodeBrowserView.vue';
import PageHeader from '../components/PageHeader.vue';

const { call, on, copyText } = vi.hoisted(() => ({
  call: vi.fn(),
  on: vi.fn<(event: string, listener: (payload: unknown) => void) => () => void>(() => vi.fn()),
  copyText: vi.fn(),
}));
vi.mock('../api/bridge', () => ({ backend: { call, on } }));
vi.mock('../utils/notify', () => ({ copyText }));

describe('CodeBrowserView', () => {
  it('uses the shared page header contract', () => {
    expect(mount(CodeBrowserView).findComponent(PageHeader).exists()).toBe(true);
  });
  afterEach(() => { call.mockReset(); on.mockReset(); on.mockReturnValue(vi.fn()); copyText.mockReset(); vi.restoreAllMocks(); window.location.hash = ''; });

  it('opens a tree, expands folders, reads files and searches with regex', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'code.projects') return Promise.resolve({ projects: [{ appid: 'wx1', name: 'Demo', path: 'C:/out', mtime: 1 }] });
      if (method === 'code.project') return Promise.resolve({ root: 'C:/out', tree: [{ name: 'src', path: 'C:/out/src', isDir: true }] });
      if (method === 'code.expandDir') return Promise.resolve({ children: [{ name: 'app.js', path: 'C:/out/src/app.js', isDir: false }] });
      if (method === 'code.readFile') return Promise.resolve({ content: 'const token = 1;', size: 16, language: 'javascript' });
      if (method === 'code.search') return Promise.resolve({ results: [{ file: 'C:/out/src/app.js', line: 1, text: 'const token = 1;' }] });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(CodeBrowserView);
    await flushPromises();
    await wrapper.get('[data-testid="tree-C__out_src"]').trigger('click'); await flushPromises();
    await wrapper.get('[data-testid="tree-C__out_src_app_js"]').trigger('click'); await flushPromises();
    expect(wrapper.text()).toContain('const token = 1;');
    // 编辑区显示语言与行数，便于判断打开的是不是同一个文件
    expect(wrapper.get('.editor-meta').text()).toContain('javascript');
    expect(wrapper.get('.editor-meta').text()).toContain('1 行');
    await wrapper.get('[data-testid="code-search"]').setValue('token');
    await wrapper.get('[data-testid="regex-search"]').setValue(true); await flushPromises();
    expect(call).toHaveBeenCalledWith('code.search', { root: 'C:/out', query: 'token', regex: true });
  });

  it('debounces code search so rapid typing does not hit the backend per keystroke', async () => {
    vi.useFakeTimers();
    call.mockImplementation((method: string) => method === 'code.projects'
      ? Promise.resolve({ projects: [{ appid: 'wx1', name: 'Demo', path: 'C:/out', mtime: 1 }] })
      : method === 'code.project' ? Promise.resolve({ root: 'C:/out', tree: [] })
        : method === 'code.search' ? Promise.resolve({ results: [] }) : Promise.resolve({ ok: true }));
    const wrapper = mount(CodeBrowserView);
    await flushPromises();

    const input = wrapper.get('[data-testid="code-search"]');
    for (const value of ['t', 'to', 'tok', 'toke', 'token']) {
      await input.setValue(value);
    }
    expect(call.mock.calls.filter(([method]) => method === 'code.search')).toHaveLength(0);
    await vi.advanceTimersByTimeAsync(400);
    await flushPromises();
    const searches = call.mock.calls.filter(([method]) => method === 'code.search');
    expect(searches).toHaveLength(1);
    expect(searches[0][1]).toEqual({ root: 'C:/out', query: 'token', regex: false });
    wrapper.unmount();
    vi.useRealTimers();
  });
  it('closes every open tab at once', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'code.projects') return Promise.resolve({ projects: [{ appid: 'wx1', name: 'Demo', path: 'C:/out', mtime: 1 }] });
      if (method === 'code.project') return Promise.resolve({ root: 'C:/out', tree: [
        { name: 'app.js', path: 'C:/out/app.js', isDir: false },
        { name: 'util.js', path: 'C:/out/util.js', isDir: false },
      ] });
      if (method === 'code.readFile') return Promise.resolve({ content: 'x', size: 1, language: 'javascript' });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(CodeBrowserView);
    await flushPromises();
    await wrapper.get('[data-testid="tree-C__out_app_js"]').trigger('click'); await flushPromises();
    await wrapper.get('[data-testid="tree-C__out_util_js"]').trigger('click'); await flushPromises();
    expect(wrapper.findAll('.tab')).toHaveLength(2);
    await wrapper.get('.tab-close-all').trigger('click'); await flushPromises();
    expect(wrapper.findAll('.tab')).toHaveLength(0);
    expect(wrapper.text()).toContain('尚未打开文件');
  });
  it('highlights the matching line when a search result is opened', async () => {
    vi.useFakeTimers();
    call.mockImplementation((method: string) => {
      if (method === 'code.projects') return Promise.resolve({ projects: [{ appid: 'wx1', name: 'Demo', path: 'C:/out', mtime: 1 }] });
      if (method === 'code.project') return Promise.resolve({ root: 'C:/out', tree: [] });
      if (method === 'code.search') return Promise.resolve({ results: [{ file: 'C:/out/app.js', line: 3, text: 'token' }] });
      if (method === 'code.readFile') return Promise.resolve({ content: 'a\nb\ntoken here\nd', size: 20, language: 'javascript' });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(CodeBrowserView);
    await flushPromises();
    await wrapper.get('[data-testid="code-search"]').setValue('token');
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();

    expect(wrapper.get('.result-file').text()).toBe('app.js:3');
    await wrapper.get('.search-results button').trigger('click');
    await flushPromises();
    const hit = wrapper.get('.code-line.is-hit');
    expect(hit.attributes('data-line')).toBe('3');
    expect(hit.get('.code-text').text()).toBe('token here');
    expect(wrapper.findAll('.code-line')).toHaveLength(4);
    wrapper.unmount();
    vi.useRealTimers();
  });
  it('keeps the audit surface focused and omits destructive format actions', async () => {
    call.mockImplementation((method: string) => method === 'code.projects'
      ? Promise.resolve({ projects: [{ appid: 'wx1', name: 'Demo', path: 'C:/out', mtime: 1 }] })
      : method === 'code.project' ? Promise.resolve({ root: 'C:/out', tree: [{ name: 'app.js', path: 'C:/out/app.js', isDir: false }] })
        : method === 'code.readFile' ? Promise.resolve({ content: 'let x=1', size: 7, language: 'javascript' }) : Promise.resolve({ ok: true }));
    const wrapper = mount(CodeBrowserView);
    await flushPromises();
    expect(wrapper.find('[data-testid="format-file"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="format-all"]').exists()).toBe(false);
    expect(on).not.toHaveBeenCalled();

    await wrapper.get('[data-testid="tree-C__out_app_js"]').trigger('click');
    await flushPromises();
    expect(wrapper.find('[data-testid="copy-file-path"]').exists()).toBe(true);
  });

  it('lists decompiled mini programs and switches the audited one', async () => {
    call.mockImplementation((method: string, params?: Record<string, unknown>) => {
      if (method === 'code.projects') return Promise.resolve({ projects: [
        { appid: 'wx111', name: 'One', path: 'C:/out/wx111', mtime: 2 },
        { appid: 'wx222', name: 'Two', path: 'C:/out/wx222', mtime: 1 },
      ] });
      if (method === 'code.project') {
        const path = `C:/out/${String(params?.appid)}`;
        return Promise.resolve({ root: path, tree: [{ name: 'app.js', path: `${path}/app.js`, isDir: false }] });
      }
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(CodeBrowserView);
    await flushPromises();

    expect(call.mock.calls.some(([method]) => method === 'code.projects')).toBe(true);
    expect(wrapper.get('[data-testid="code-project"]').findAll('option')).toHaveLength(2);
    expect(wrapper.find('.subtitle').exists()).toBe(false);
    expect(wrapper.text()).toContain('One');

    await wrapper.get('[data-testid="code-project"]').setValue('wx222');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('code.project', { appid: 'wx222' });
    expect(wrapper.find('[data-testid="tree-C__out_wx222_app_js"]').exists()).toBe(true);
  });


  it('reopens the selected project when the decompiled output list is refreshed', async () => {
    let revision = 0;
    call.mockImplementation((method: string) => {
      if (method === 'code.projects') return Promise.resolve({ projects: [{ appid: 'wx1', name: 'Demo', path: 'C:/out/wx1', mtime: 1 }] });
      if (method === 'code.project') {
        revision += 1;
        return Promise.resolve({ root: 'C:/out/wx1', tree: [{ name: `rev${revision}.js`, path: `C:/out/wx1/rev${revision}.js`, isDir: false }] });
      }
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(CodeBrowserView);
    await flushPromises();
    expect(wrapper.find('[data-testid="tree-C__out_wx1_rev1_js"]').exists()).toBe(true);

    await wrapper.get('[data-testid="refresh-projects"]').trigger('click');
    await flushPromises();
    expect(call.mock.calls.filter(([method]) => method === 'code.project')).toHaveLength(2);
    expect(wrapper.find('[data-testid="tree-C__out_wx1_rev2_js"]').exists()).toBe(true);
  });

  it('ignores a stale search response after switching projects', async () => {
    vi.useFakeTimers();
    let resolveFirst!: (value: unknown) => void;
    const firstSearch = new Promise((resolve) => { resolveFirst = resolve; });
    call.mockImplementation((method: string, params?: Record<string, unknown>) => {
      if (method === 'code.projects') return Promise.resolve({ projects: [
        { appid: 'wx111', name: 'One', path: 'C:/out/wx111', mtime: 2 },
        { appid: 'wx222', name: 'Two', path: 'C:/out/wx222', mtime: 1 },
      ] });
      if (method === 'code.project') {
        const path = `C:/out/${String(params?.appid)}`;
        return Promise.resolve({ root: path, tree: [] });
      }
      if (method === 'code.search' && params?.root === 'C:/out/wx111') return firstSearch;
      if (method === 'code.search') return Promise.resolve({ results: [{ file: 'C:/out/wx222/current.js', line: 1, text: 'current' }] });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(CodeBrowserView);
    await flushPromises();

    await wrapper.get('[data-testid="code-search"]').setValue('needle');
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();

    await wrapper.get('[data-testid="code-project"]').setValue('wx222');
    await flushPromises();
    await wrapper.get('[data-testid="code-search"]').setValue('needle');
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();
    expect(wrapper.get('.result-file').text()).toBe('current.js:1');

    resolveFirst({ results: [{ file: 'C:/out/wx111/stale.js', line: 1, text: 'stale' }] });
    await flushPromises();
    expect(wrapper.text()).not.toContain('stale.js');
    expect(wrapper.get('.result-file').text()).toBe('current.js:1');
    wrapper.unmount();
    vi.useRealTimers();
  });

  it('shows the truncated head of an oversized file instead of hiding it', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'code.projects') return Promise.resolve({ projects: [{ appid: 'wx1', name: 'Demo', path: 'C:/out', mtime: 1 }] });
      if (method === 'code.project') return Promise.resolve({ root: 'C:/out', tree: [{ name: 'big.js', path: 'C:/out/big.js', isDir: false }] });
      if (method === 'code.readFile') return Promise.resolve({ content: 'head', size: 2 * 1024 * 1024, language: 'javascript', truncated: true });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(CodeBrowserView);
    await flushPromises();
    await wrapper.get('[data-testid="tree-C__out_big_js"]').trigger('click');
    await flushPromises();

    expect(wrapper.get('.file-warning').text()).toContain('仅显示前 1MB');
    expect(wrapper.get('[data-testid="code-content"]').text()).toContain('head');
  });

  it('renders a line-number gutter and jumps to the requested line', async () => {
    window.location.hash = '#/code?root=C:/out&file=pages/app.js&line=3';
    call.mockImplementation((method: string) => {
      if (method === 'code.projects') return Promise.resolve({ projects: [{ appid: 'wx1', name: 'Demo', path: 'C:/out', mtime: 1 }] });
      if (method === 'code.project') return Promise.resolve({ root: 'C:/out', tree: [{ name: 'pages', path: 'C:/out/pages', isDir: true }] });
      if (method === 'code.readFile') return Promise.resolve({ content: 'a\nb\ntoken here\nd', size: 20, language: 'javascript' });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(CodeBrowserView);
    await flushPromises();

    const lines = wrapper.findAll('.code-line');
    expect(lines).toHaveLength(4);
    expect(lines[0].get('.code-gutter').text()).toBe('1');
    expect(lines[3].get('.code-gutter').text()).toBe('4');

    const hit = wrapper.get('.code-line.is-hit');
    expect(hit.attributes('data-line')).toBe('3');
    expect(hit.get('.code-text').text()).toBe('token here');
    expect(call).toHaveBeenCalledWith('code.readFile', { path: 'C:/out/pages/app.js' });
  });

  it('renders an oversized file as a window that keeps real line numbers and admits what is left out', async () => {
    const content = Array.from({ length: 5000 }, (_, index) => `line ${index + 1}`).join('\n');
    call.mockImplementation((method: string) => {
      if (method === 'code.projects') return Promise.resolve({ projects: [{ appid: 'wx1', name: 'Demo', path: 'C:/out', mtime: 1 }] });
      if (method === 'code.project') return Promise.resolve({ root: 'C:/out', tree: [{ name: 'big.js', path: 'C:/out/big.js', isDir: false }] });
      if (method === 'code.readFile') return Promise.resolve({ content, size: content.length, language: 'javascript' });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(CodeBrowserView);
    await flushPromises();
    await wrapper.get('[data-testid="tree-C__out_big_js"]').trigger('click');
    await flushPromises();

    const lines = wrapper.findAll('.code-line');
    expect(lines).toHaveLength(4000);
    expect(lines[0].get('.code-gutter').text()).toBe('1');
    expect(lines[3999].get('.code-gutter').text()).toBe('4000');
    expect(wrapper.find('[data-testid="load-before"]').exists()).toBe(false);
    expect(wrapper.get('[data-testid="load-more"]').text()).toContain('还有 1000 行未渲染');

    await wrapper.get('[data-testid="load-more"]').trigger('click');
    await flushPromises();
    expect(wrapper.findAll('.code-line')).toHaveLength(5000);
    expect(wrapper.find('[data-testid="load-more"]').exists()).toBe(false);
    // 一次渲染 5000 行 + 再加载一屏：满载时会被拖慢，超时按这个用例的实际成本给
  }, 30000);

  it('opens a huge file around the requested line instead of showing one opaque blob', async () => {
    window.location.hash = '#/code?root=C:/out&file=big.js&line=9000';
    const content = Array.from({ length: 12000 }, (_, index) => (index === 8999 ? 'token here' : `line ${index + 1}`)).join('\n');
    call.mockImplementation((method: string) => {
      if (method === 'code.projects') return Promise.resolve({ projects: [{ appid: 'wx1', name: 'Demo', path: 'C:/out', mtime: 1 }] });
      if (method === 'code.project') return Promise.resolve({ root: 'C:/out', tree: [{ name: 'big.js', path: 'C:/out/big.js', isDir: false }] });
      if (method === 'code.readFile') return Promise.resolve({ content, size: content.length, language: 'javascript' });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(CodeBrowserView);
    await flushPromises();

    const lines = wrapper.findAll('.code-line');
    expect(lines[0].attributes('data-line')).toBe('8800');
    const hit = wrapper.get('.code-line.is-hit');
    expect(hit.attributes('data-line')).toBe('9000');
    expect(hit.get('.code-text').text()).toBe('token here');
    expect(wrapper.get('[data-testid="load-before"]').text()).toContain('还有 8799 行未渲染');
    expect(wrapper.find('[data-testid="load-more"]').exists()).toBe(false);
    // 同一个窗口：3201 行 DOM
  }, 30000);

  it('wraps long lines on request and copies the whole file', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'code.projects') return Promise.resolve({ projects: [{ appid: 'wx1', name: 'Demo', path: 'C:/out', mtime: 1 }] });
      if (method === 'code.project') return Promise.resolve({ root: 'C:/out', tree: [{ name: 'app.js', path: 'C:/out/app.js', isDir: false }] });
      if (method === 'code.readFile') return Promise.resolve({ content: 'const a = 1;', size: 12, language: 'javascript' });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(CodeBrowserView);
    await flushPromises();
    await wrapper.get('[data-testid="tree-C__out_app_js"]').trigger('click');
    await flushPromises();

    const pre = wrapper.get('[data-testid="code-content"]');
    expect(pre.classes()).not.toContain('is-wrap');
    await wrapper.get('[data-testid="wrap-toggle"]').setValue(true);
    expect(pre.classes()).toContain('is-wrap');

    await wrapper.get('[data-testid="copy-file-content"]').trigger('click');
    expect(copyText).toHaveBeenCalledWith('const a = 1;', 'app.js 内容');
  });

  it('falls back to the newest artifact when a deep link points at a missing one', async () => {
    window.location.hash = '#/code?root=C:/gone&file=pages/app.js&line=3';
    call.mockImplementation((method: string) => {
      if (method === 'code.projects') return Promise.resolve({ projects: [{ appid: 'wx1', name: 'Demo', path: 'C:/out/wx1', mtime: 2 }] });
      if (method === 'code.project') return Promise.resolve({ root: 'C:/out/wx1', tree: [{ name: 'app.js', path: 'C:/out/wx1/app.js', isDir: false }] });
      if (method === 'code.readFile') return Promise.resolve({ content: 'a\nb\ntoken\n', size: 10, language: 'javascript' });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(CodeBrowserView);
    await flushPromises();

    expect(wrapper.get('[role="alert"]').text()).toContain('未找到指定的反编译产物');
    expect(wrapper.find('[data-testid="tree-C__out_wx1_app_js"]').exists()).toBe(true);
    // 文件路径属于那个已经找不到的产物，不能拿它去跳另一个产物里的文件
    expect(call.mock.calls.some(([method]) => method === 'code.readFile')).toBe(false);
  });

  it('keeps a stale jump line inside the file instead of rendering nothing', async () => {
    // 行号来自上一次反编译的产物：文件只有 3 行，却要求跳到第 5000 行
    window.location.hash = '#/code?root=C:/out&file=small.js&line=5000';
    call.mockImplementation((method: string) => {
      if (method === 'code.projects') return Promise.resolve({ projects: [{ appid: 'wx1', name: 'Demo', path: 'C:/out', mtime: 1 }] });
      if (method === 'code.project') return Promise.resolve({ root: 'C:/out', tree: [{ name: 'small.js', path: 'C:/out/small.js', isDir: false }] });
      if (method === 'code.readFile') return Promise.resolve({ content: 'a\nb\nc', size: 5, language: 'javascript' });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(CodeBrowserView);
    await flushPromises();

    expect(wrapper.findAll('.code-line')).toHaveLength(3);
    expect(wrapper.get('.code-line .code-gutter').text()).toBe('1');
    expect(wrapper.find('[data-testid="load-before"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="load-more"]').exists()).toBe(false);
    expect(wrapper.get('[role="alert"]').text()).toContain('第 5000 行超出文件范围');
  });

  it('says an empty file is empty instead of showing a blank editor', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'code.projects') return Promise.resolve({ projects: [{ appid: 'wx1', name: 'Demo', path: 'C:/out', mtime: 1 }] });
      if (method === 'code.project') return Promise.resolve({ root: 'C:/out', tree: [{ name: 'empty.css', path: 'C:/out/empty.css', isDir: false }] });
      if (method === 'code.readFile') return Promise.resolve({ content: '', size: 0, language: 'css' });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(CodeBrowserView);
    await flushPromises();
    await wrapper.get('[data-testid="tree-C__out_empty_css"]').trigger('click');
    await flushPromises();

    expect(wrapper.get('.code-empty').text()).toContain('空文件');
    expect(wrapper.find('[data-testid="code-content"]').exists()).toBe(false);
    expect(wrapper.get('[data-testid="copy-file-content"]').attributes('disabled')).toBeDefined();
  });

  it('draws an image file as a picture instead of a pane of mojibake', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'code.projects') return Promise.resolve({ projects: [{ appid: 'wx1', name: 'Demo', path: 'C:/out', mtime: 1 }] });
      if (method === 'code.project') return Promise.resolve({ root: 'C:/out', tree: [{ name: 'logo.png', path: 'C:/out/logo.png', isDir: false }] });
      if (method === 'code.readFile') return Promise.resolve({ content: '', size: 68, language: 'image/png', kind: 'image', dataUrl: 'data:image/png;base64,iVBORw0KGgo=' });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(CodeBrowserView);
    await flushPromises();
    await wrapper.get('[data-testid="tree-C__out_logo_png"]').trigger('click');
    await flushPromises();

    expect(wrapper.get('[data-testid="code-image"]').attributes('src')).toBe('data:image/png;base64,iVBORw0KGgo=');
    // 没有行窗口、没有换行开关，也不该被当成空文件
    expect(wrapper.find('[data-testid="code-content"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="wrap-toggle"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="binary-notice"]').exists()).toBe(false);
    expect(wrapper.get('.editor-meta').text()).toContain('image/png');
  });

  it('explains an uninlined image and a binary file instead of showing garbage', async () => {
    call.mockImplementation((method: string, params?: { path?: string }) => {
      if (method === 'code.projects') return Promise.resolve({ projects: [{ appid: 'wx1', name: 'Demo', path: 'C:/out', mtime: 1 }] });
      if (method === 'code.project') return Promise.resolve({ root: 'C:/out', tree: [
        { name: 'huge.png', path: 'C:/out/huge.png', isDir: false },
        { name: 'app.wasm', path: 'C:/out/app.wasm', isDir: false },
      ] });
      if (method === 'code.readFile' && params?.path?.endsWith('huge.png')) {
        return Promise.resolve({ content: '图片 5.2MB，超过 4.0MB 内联上限，未显示。', size: 5452595, language: 'image/png', kind: 'image', dataUrl: '' });
      }
      if (method === 'code.readFile') {
        return Promise.resolve({ content: '二进制文件（1.2KB），无法按源码显示。', size: 1200, language: 'text', kind: 'binary' });
      }
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(CodeBrowserView);
    await flushPromises();
    await wrapper.get('[data-testid="tree-C__out_huge_png"]').trigger('click');
    await flushPromises();

    expect(wrapper.find('[data-testid="code-image"]').exists()).toBe(false);
    expect(wrapper.get('[data-testid="binary-notice"]').text()).toContain('超过 4.0MB 内联上限');

    await wrapper.get('[data-testid="tree-C__out_app_wasm"]').trigger('click');
    await flushPromises();
    expect(wrapper.get('[data-testid="binary-notice"]').text()).toContain('二进制文件');
    expect(wrapper.find('[data-testid="code-content"]').exists()).toBe(false);
  });

  it('remembers the wrap preference the way the theme is remembered', async () => {
    localStorage.setItem('code-wrap', '1');
    call.mockImplementation((method: string) => {
      if (method === 'code.projects') return Promise.resolve({ projects: [{ appid: 'wx1', name: 'Demo', path: 'C:/out', mtime: 1 }] });
      if (method === 'code.project') return Promise.resolve({ root: 'C:/out', tree: [{ name: 'app.js', path: 'C:/out/app.js', isDir: false }] });
      if (method === 'code.readFile') return Promise.resolve({ content: 'const a = 1;', size: 12, language: 'javascript' });
      return Promise.resolve({ ok: true });
    });
    const first = mount(CodeBrowserView);
    await flushPromises();
    await first.get('[data-testid="tree-C__out_app_js"]').trigger('click');
    await flushPromises();
    expect(first.get('[data-testid="code-content"]').classes()).toContain('is-wrap');

    await first.get('[data-testid="wrap-toggle"]').setValue(false);
    expect(localStorage.getItem('code-wrap')).toBe('0');
    first.unmount();

    const second = mount(CodeBrowserView);
    await flushPromises();
    await second.get('[data-testid="tree-C__out_app_js"]').trigger('click');
    await flushPromises();
    expect(second.get('[data-testid="code-content"]').classes()).not.toContain('is-wrap');
    localStorage.removeItem('code-wrap');
  });

  it('reports a search that hit the backend result cap', async () => {
    vi.useFakeTimers();
    call.mockImplementation((method: string) => {
      if (method === 'code.projects') return Promise.resolve({ projects: [{ appid: 'wx1', name: 'Demo', path: 'C:/out', mtime: 1 }] });
      if (method === 'code.project') return Promise.resolve({ root: 'C:/out', tree: [] });
      if (method === 'code.search') return Promise.resolve({ results: [{ file: 'C:/out/a.js', line: 1, text: 'token' }], truncated: true });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(CodeBrowserView);
    await flushPromises();
    await wrapper.get('[data-testid="code-search"]').setValue('token');
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();

    expect(wrapper.get('.panel-header .status-line').text()).toContain('命中 1+ 处');
    expect(wrapper.get('.result-limit').text()).toContain('已达 1 条上限');
    wrapper.unmount();
    vi.useRealTimers();
  });

  it('renders syntax-highlighted code for supported languages', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'code.projects') return Promise.resolve({ projects: [{ appid: 'wx1', name: 'Demo', path: 'C:/out', mtime: 1 }] });
      if (method === 'code.project') return Promise.resolve({ root: 'C:/out', tree: [{ name: 'app.js', path: 'C:/out/app.js', isDir: false }] });
      if (method === 'code.readFile') return Promise.resolve({ content: 'const token = "wx123";', size: 23, language: 'javascript' });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(CodeBrowserView);
    await flushPromises();
    await wrapper.get('[data-testid="tree-C__out_app_js"]').trigger('click');
    await flushPromises();
    // 关键字与字符串各有各的颜色 span；纯文本断言不受着色影响
    expect(wrapper.get('.code-content .tok-kw').text()).toBe('const');
    expect(wrapper.get('.code-content .tok-str').text()).toBe('"wx123"');
    expect(wrapper.get('.code-text').text()).toBe('const token = "wx123";');
  });

  it('marks the matched value inside the hit line when arriving from scan results', async () => {
    // 压缩成一整行的产物：整行高亮看不出来，行内 mark 才是有效落点
    window.location.hash = '#/code?root=C:/out&file=app.js&line=1&value=wx456secret';
    const content = 'var cfg={appid:"wx456secret",token:1};';
    call.mockImplementation((method: string) => {
      if (method === 'code.projects') return Promise.resolve({ projects: [{ appid: 'wx1', name: 'Demo', path: 'C:/out', mtime: 1 }] });
      if (method === 'code.project') return Promise.resolve({ root: 'C:/out', tree: [] });
      if (method === 'code.readFile') return Promise.resolve({ content, size: content.length, language: 'javascript' });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(CodeBrowserView);
    await flushPromises();

    const hit = wrapper.get('.code-line.is-hit');
    expect(hit.attributes('data-line')).toBe('1');
    expect(hit.get('mark.code-hit-text').text()).toBe('wx456secret');
    // 工具条交代命中行并允许回到/清除
    expect(wrapper.get('[data-testid="hit-line-chip"]').text()).toBe('命中行 1');

    await wrapper.get('[data-testid="clear-hit"]').trigger('click');
    await flushPromises();
    expect(wrapper.find('.code-line.is-hit').exists()).toBe(false);
    expect(wrapper.find('[data-testid="hit-tools"]').exists()).toBe(false);
  });

  it('keeps the hit anchor when switching between tabs and back', async () => {
    // 命中行要超过 200 行的引导窗口，锚点效果（窗口从命中行上方开始）才可观察
    const content = Array.from({ length: 400 }, (_, index) => (index === 299 ? 'hit here' : `line ${index + 1}`)).join('\n');
    call.mockImplementation((method: string) => {
      if (method === 'code.projects') return Promise.resolve({ projects: [{ appid: 'wx1', name: 'Demo', path: 'C:/out', mtime: 1 }] });
      if (method === 'code.project') return Promise.resolve({ root: 'C:/out', tree: [
        { name: 'app.js', path: 'C:/out/app.js', isDir: false },
        { name: 'util.js', path: 'C:/out/util.js', isDir: false },
      ] });
      if (method === 'code.readFile') return Promise.resolve({ content, size: content.length, language: 'javascript' });
      if (method === 'code.search') return Promise.resolve({ results: [{ file: 'C:/out/app.js', line: 300, text: 'hit here' }] });
      return Promise.resolve({ ok: true });
    });
    vi.useFakeTimers();
    const wrapper = mount(CodeBrowserView);
    await flushPromises();
    await wrapper.get('[data-testid="tree-C__out_util_js"]').trigger('click');
    await flushPromises();
    // 从搜索结果跳入，建立第 300 行的命中锚点（搜索有防抖，推进假时钟）
    await wrapper.get('[data-testid="code-search"]').setValue('hit');
    await vi.advanceTimersByTimeAsync(300);
    await flushPromises();
    await wrapper.get('.search-results button').trigger('click');
    await flushPromises();
    expect(wrapper.get('.code-line.is-hit').attributes('data-line')).toBe('300');

    // 切走（无锚点的标签回到文件开头）再切回来：窗口仍以命中行为准
    await wrapper.findAll('[role="tab"]')[0].trigger('click');
    await flushPromises();
    expect(wrapper.find('.code-line.is-hit').exists()).toBe(false);
    await wrapper.findAll('[role="tab"]')[1].trigger('click');
    await flushPromises();
    expect(wrapper.get('.code-line.is-hit').attributes('data-line')).toBe('300');
    expect(wrapper.get('[data-testid="load-before"]').text()).toContain('上方还有 99 行未渲染');
    wrapper.unmount();
    vi.useRealTimers();
  });

});
