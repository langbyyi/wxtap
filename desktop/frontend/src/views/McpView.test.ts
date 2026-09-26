import { createTestingPinia } from '@pinia/testing';
import { flushPromises, mount } from '@vue/test-utils';
import { nextTick } from 'vue';
import { describe, expect, it, vi } from 'vitest';
import McpView from './McpView.vue';
import PageHeader from '../components/PageHeader.vue';

const { call } = vi.hoisted(() => ({ call: vi.fn() }));
vi.mock('../api/bridge', () => ({ backend: { call } }));

// 浮标要用 Range.getBoundingClientRect 定位，jsdom 没实现这个方法（调用即抛，
// 异常被事件分发吞掉，表现成「浮标不出现」）。补一个零矩形就够：这些用例断言
// 的是浮标出现、复制的是选区，不是像素落点——落点在浏览器里另外核过。
if (!Range.prototype.getBoundingClientRect) {
  Range.prototype.getBoundingClientRect = () => ({ x: 0, y: 0, top: 0, left: 0, right: 0, bottom: 0, width: 0, height: 0, toJSON: () => ({}) }) as DOMRect;
}

describe('McpView', () => {
  it('keeps service status and self-check with service controls', () => {
    const wrapper = mount(McpView, { global: { plugins: [createTestingPinia({ createSpy: vi.fn })] } });
    expect(wrapper.findComponent(PageHeader).exists()).toBe(true);
    expect(wrapper.find('.page-toolbar').exists()).toBe(false);
    expect(wrapper.find('.panel-header [data-testid="refresh-mcp"]').exists()).toBe(true);
  });
  it('loads status then starts MCP with the selected port', async () => {
    call.mockResolvedValueOnce({ running: false }).mockResolvedValueOnce({ ok: true, url: 'http://127.0.0.1:4555/mcp' });
    const wrapper = mount(McpView, { global: { plugins: [createTestingPinia({ createSpy: vi.fn, stubActions: false })] } });
    await flushPromises();
    await wrapper.get('[data-testid="mcp-port"]').setValue('4555');
    await wrapper.get('[data-testid="mcp-toggle"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('mcp.start', { port: 4555 });
    expect(wrapper.text()).toContain('http://127.0.0.1:4555/mcp');
    expect(wrapper.text()).toContain('http://127.0.0.1:4555/sse');
  });

  it('stops a running MCP service', async () => {
    call.mockResolvedValueOnce({ running: true }).mockResolvedValueOnce({ ok: true });
    const wrapper = mount(McpView, { global: { plugins: [createTestingPinia({ createSpy: vi.fn, stubActions: false })] } });
    await flushPromises();
    await wrapper.get('[data-testid="mcp-toggle"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('mcp.stop');
  });

  it('copies the MCP service URL on right click instead of a button', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(window.navigator, 'clipboard', { value: { writeText }, configurable: true });
    call.mockResolvedValueOnce({ running: false }).mockResolvedValueOnce({ ok: true, url: 'http://127.0.0.1:4555/mcp' });
    const wrapper = mount(McpView, { global: { plugins: [createTestingPinia({ createSpy: vi.fn, stubActions: false })] } });
    await flushPromises();
    await wrapper.get('[data-testid="mcp-port"]').setValue('4555');
    await wrapper.get('[data-testid="mcp-toggle"]').trigger('click');
    await flushPromises();

    expect(wrapper.find('[data-testid="copy-url"]').exists()).toBe(false);
    await wrapper.get('[data-testid="mcp-url"]').trigger('contextmenu');
    expect(writeText).toHaveBeenCalledWith('http://127.0.0.1:4555/mcp');
  });

  it('shows the stdio command from status and copies it on right click', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(window.navigator, 'clipboard', { value: { writeText }, configurable: true });
    call.mockResolvedValue({ running: false, command: 'WxTap.exe -mcp' });
    const wrapper = mount(McpView, { global: { plugins: [createTestingPinia({ createSpy: vi.fn, stubActions: false })] } });
    await flushPromises();
    expect(wrapper.get('[data-testid="mcp-command"]').text()).toBe('WxTap.exe -mcp');

    expect(wrapper.find('[data-testid="copy-mcp-command"]').exists()).toBe(false);
    await wrapper.get('[data-testid="mcp-command"]').trigger('contextmenu');
    expect(writeText).toHaveBeenCalledWith('WxTap.exe -mcp');
  });

  it('right-clicks the stdio config and the CLI command', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(window.navigator, 'clipboard', { value: { writeText }, configurable: true });
    call.mockResolvedValue({ running: false, command: 'WxTap.exe -mcp' });
    const wrapper = mount(McpView, { global: { plugins: [createTestingPinia({ createSpy: vi.fn, stubActions: false })] } });
    await flushPromises();

    await wrapper.get('[data-testid="mcp-stdio-config"]').trigger('contextmenu');
    expect(writeText).toHaveBeenCalledWith(JSON.stringify({ mcpServers: { wxtap: { command: 'WxTap.exe -mcp', args: ['-mcp'] } } }, null, 2));

    await wrapper.get('[data-testid="mcp-cli-command"]').trigger('contextmenu');
    expect(writeText).toHaveBeenCalledWith('claude mcp add --transport http wxtap http://127.0.0.1:9527/mcp');
  });

  it('copies an mcpServers client config pinned to the selected port', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(window.navigator, 'clipboard', { value: { writeText }, configurable: true });
    call.mockResolvedValue({ running: true, port: 9600 });
    const wrapper = mount(McpView, { global: { plugins: [createTestingPinia({ createSpy: vi.fn, stubActions: false })] } });
    await flushPromises();
    expect(wrapper.get('[data-testid="mcp-config"]').text()).toContain('"mcpServers"');

    await wrapper.get('[data-testid="mcp-config"]').trigger('contextmenu');
    expect(writeText).toHaveBeenCalledWith(JSON.stringify({ mcpServers: { wxtap: { url: 'http://127.0.0.1:9600/mcp' } } }, null, 2));
  });

  it('copies a whole value with Enter once the value has focus', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(window.navigator, 'clipboard', { value: { writeText }, configurable: true });
    call.mockResolvedValue({ running: true, port: 9600 });
    const wrapper = mount(McpView, { global: { plugins: [createTestingPinia({ createSpy: vi.fn, stubActions: false })] } });
    await flushPromises();

    expect(wrapper.get('[data-testid="mcp-url"]').attributes('tabindex')).toBe('0');
    await wrapper.get('[data-testid="mcp-url"]').trigger('keydown.enter');
    expect(writeText).toHaveBeenCalledWith('http://127.0.0.1:9600/mcp');

    await wrapper.get('[data-testid="mcp-sse-url"]').trigger('keydown.enter');
    expect(writeText).toHaveBeenCalledWith('http://127.0.0.1:9600/sse');
  });

  it('copies only the selected part of a value through the floating button', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(window.navigator, 'clipboard', { value: { writeText }, configurable: true });
    call.mockResolvedValue({ running: true, port: 9600 });
    // 必须挂进文档：jsdom 的 selection.addRange 会丢掉「节点不在文档里」的选区
    // （rangeCount 变 0、anchorNode 变 undefined），浮标就永远不会出现。
    const host = document.createElement('div');
    document.body.appendChild(host);
    const wrapper = mount(McpView, { attachTo: host, global: { plugins: [createTestingPinia({ createSpy: vi.fn, stubActions: false })] } });
    await flushPromises();

    const selection = window.getSelection();
    if (!selection) throw new Error('jsdom 没提供 Selection');
    // 注意用 range.toString() 取文本：jsdom 的 selection.toString() 恒为空串，
    // 断言它会把「实现正确」误判成失败。浮标是 Vue 状态驱动的，改完选区要等
    // 一次渲染才在 DOM 里出现。
    const select = async (node: Node, from: number, to: number) => {
      const range = document.createRange();
      range.setStart(node, from);
      range.setEnd(node, to);
      selection.removeAllRanges();
      selection.addRange(range);
      document.dispatchEvent(new Event('selectionchange'));
      await nextTick();
      return range.toString();
    };

    // 没选中任何东西时不该有浮标
    expect(wrapper.find('[data-testid="copy-selection"]').exists()).toBe(false);

    // 选区落在值之外（这里选中面板标题）时也不该有
    await select(wrapper.get('.panel-header h2').element.firstChild as Text, 0, 2);
    expect(wrapper.find('[data-testid="copy-selection"]').exists()).toBe(false);

    // 选中配置里的一段：浮标出现，按它只复制这一段。
    // 复制挂在 mousedown 上（见 copySelectionDown 的注释），所以这里按的不是 click。
    const selected = await select(wrapper.get('[data-testid="mcp-config"]').element.firstChild as Text, 4, 16);
    expect(selected).toBe('"mcpServers"');
    await wrapper.get('[data-testid="copy-selection"]').trigger('mousedown', { button: 0 });
    expect(writeText).toHaveBeenCalledWith(selected);
    expect(wrapper.find('[data-testid="copy-selection"]').exists()).toBe(false);

    // 离开页面后监听要摘干净：卸载后再改选区不该再报错或再算一次
    selection.removeAllRanges();
    wrapper.unmount();
    document.dispatchEvent(new Event('selectionchange'));
    host.remove();
  });

  it('lists the three connection types, each with its own copyable value and config', async () => {
    call.mockResolvedValue({ running: false, port: 9600, command: 'WxTap.exe -mcp' });
    const wrapper = mount(McpView, { global: { plugins: [createTestingPinia({ createSpy: vi.fn, stubActions: false })] } });
    await flushPromises();

    expect(wrapper.findAll('.panel-header h2').map((head) => head.text())).toEqual(['服务控制', 'stdio', 'Streamable HTTP', 'SSE']);

    // 端点地址常驻，不等「启动服务」：它只由端口决定，配置片段本来就是这么算的
    expect(wrapper.get('[data-testid="mcp-url"]').text()).toBe('http://127.0.0.1:9600/mcp');
    expect(wrapper.get('[data-testid="mcp-sse-url"]').text()).toBe('http://127.0.0.1:9600/sse');
    expect(wrapper.get('[data-testid="mcp-command"]').text()).toBe('WxTap.exe -mcp');

    expect(wrapper.get('[data-testid="mcp-config"]').text()).toContain('"url": "http://127.0.0.1:9600/mcp"');
    expect(wrapper.get('[data-testid="mcp-sse-config"]').text()).toContain('"url": "http://127.0.0.1:9600/sse"');
    expect(wrapper.get('[data-testid="mcp-stdio-config"]').text()).toContain('"command": "WxTap.exe -mcp"');
    expect(wrapper.get('[data-testid="mcp-cli-command"]').text()).toBe('claude mcp add --transport http wxtap http://127.0.0.1:9600/mcp');

    // 「服务控制」里不再重复挂地址：那两行只在启动后才出现，且与下方配置重复
    const controlBody = wrapper.get('.mcp-view .stack .panel').element.querySelector('.panel-body');
    expect(controlBody?.querySelectorAll('code').length).toBe(0);

    // 三种类型平级：没有「兼容旧客户端的…」「以 stdio 方式接入时…」这类块内说明句
    expect(wrapper.text()).not.toContain('兼容旧客户端');
    expect(wrapper.text()).not.toContain('以 stdio 方式接入时');
  });
});
