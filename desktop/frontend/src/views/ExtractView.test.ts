import { flushPromises, mount } from '@vue/test-utils';
import { afterEach, describe, expect, it, vi } from 'vitest';
import ExtractView from './ExtractView.vue';
import PageHeader from '../components/PageHeader.vue';

const { call, on, copyText } = vi.hoisted(() => ({ call: vi.fn(), on: vi.fn((_event: string, _listener: (payload: unknown) => void) => vi.fn()), copyText: vi.fn() }));
vi.mock('../api/bridge', () => ({ backend: { call, on } }));
vi.mock('../utils/notify', () => ({ copyText, notify: vi.fn() }));

describe('ExtractView', () => {
  it('places account selection with the package-directory controls', () => {
    const wrapper = mount(ExtractView);
    expect(wrapper.findComponent(PageHeader).exists()).toBe(true);
    expect(wrapper.get('#extract-title').text()).toBe('反编译');
    expect(wrapper.get('#extract-title').classes()).toContain('sr-only');
    expect(wrapper.find('.page-toolbar').exists()).toBe(false);
    expect(wrapper.find('[data-testid="directory-actions"] [data-testid="inventory-summary"]').exists()).toBe(true);
  });

  afterEach(() => { call.mockReset(); on.mockReset(); on.mockReturnValue(vi.fn()); copyText.mockReset(); vi.restoreAllMocks(); });

  it('loads configured directory, groups packages, decompiles and shows scan results', async () => {
    let scanDone: ((payload: unknown) => void) | undefined;
    on.mockImplementation((event: string, listener: (payload: unknown) => void) => { if (event === 'extract_scan_done') scanDone = listener; return vi.fn(); });
    call.mockImplementation((method: string) => {
      if (method === 'config.load') return Promise.resolve({ extract_packages_dir: 'C:/packages' });
      if (method === 'extract.packages') return Promise.resolve({ packages: [{ appid: 'wx1', name: 'Demo', path: 'C:/packages/wx1/app.wxapkg', mtime: 2, decompiled: true, scanned: true }] });
      if (method === 'extract.decompile') return Promise.resolve({ files_count: 3 });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(ExtractView);
    await flushPromises();
    expect(wrapper.find('.subtitle').exists()).toBe(false);
    expect(wrapper.text()).toContain('wx1');

    await wrapper.get('[data-testid="primary-wx1"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('extract.scan', { dir: 'C:/packages', appid: 'wx1' });

    await wrapper.get('[data-testid="primary-wx1"]').trigger('click');
    await flushPromises();
    scanDone?.({
      appid: 'wx1',
      results: { secret: ['token'], 密码: ['abc123'] },
      findings: [{ id: 'secret-1', category: 'secret', title: '密码', severity: 'high', confidence: 'high', value: 'token', masked: '*****', file: 'app.js', line: 1 }]
    });
    await wrapper.vm.$nextTick();

    await wrapper.get('[data-testid="primary-wx1"]').trigger('click');
    await flushPromises();
    expect(wrapper.get('[data-testid="category-secret"]').text()).toContain('账号密码');
    expect(wrapper.get('[data-testid="finding-secret-1"]').text()).toContain('token');
    expect(wrapper.text()).not.toContain('abc123');
  });

  it('shows only packages that can be decompiled for the selected user directory', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'config.load') return Promise.resolve({ extract_packages_dir: 'C:/packages' });
      if (method === 'extract.inventory') return Promise.resolve({
        items: [
          { appid: 'wxindexed', package_paths: [], decompiled: false, output_dir: '' },
          { appid: 'wxpackage', name: 'Ready app', package_paths: ['C:/packages/wxpackage/main.wxapkg'], decompiled: false, output_dir: '' },
          { appid: 'wxoutput', name: 'Saved app', mtime: 1700000000, package_paths: [], decompiled: true, output_dir: 'C:/out/wxoutput' },
        ],
        summary: { indexed_count: 2, package_count: 1, output_count: 1 }
      });
      return Promise.resolve({ ok: true });
    });

    const wrapper = mount(ExtractView);
    await flushPromises();

    expect(wrapper.get('[data-testid="inventory-summary"]').text()).toContain('1 个小程序');
    expect(wrapper.get('[data-testid="inventory-summary"]').text()).not.toContain('账号索引');
    expect(wrapper.get('[data-testid="inventory-summary"]').text()).not.toContain('已有产物');
    expect(wrapper.get('[data-testid="inventory-title"]').text()).toContain('小程序 1');
    expect(wrapper.find('[data-testid="app-wxindexed"]').exists()).toBe(false);
    expect(wrapper.get('[data-testid="app-wxpackage"]').text()).toContain('wxpackage');
    expect(wrapper.find('[data-testid="app-wxoutput"]').exists()).toBe(false);
  });

  // 还原不了的小程序（后端标 unsupported，例如页面模板由微信新版编译模板运行时生成）
  // 不进列表、不计进数量：列表就是总闸门，不在列表里就不会走后续流程。
  it('leaves a program whose templates cannot be restored out of the list and the count', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'config.load') return Promise.resolve({ extract_packages_dir: 'C:/packages' });
      if (method === 'extract.inventory') return Promise.resolve({
        items: [
          { appid: 'wxcompiled0000001', name: 'Compiled app', package_paths: ['C:/packages/wxcompiled/main.wxapkg'], decompiled: false, output_dir: '', unsupported: true },
          { appid: 'wxpackage', name: 'Ready app', package_paths: ['C:/packages/wxpackage/main.wxapkg'], decompiled: false, output_dir: '' },
        ],
        summary: { indexed_count: 0, package_count: 1, output_count: 0 }
      });
      return Promise.resolve({ ok: true });
    });

    const wrapper = mount(ExtractView);
    await flushPromises();

    expect(wrapper.get('[data-testid="inventory-summary"]').text()).toContain('1 个小程序');
    expect(wrapper.find('[data-testid="app-wxcompiled0000001"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="app-wxpackage"]').exists()).toBe(true);
  });

  it('disables the batch decompile when every program cannot be restored', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'config.load') return Promise.resolve({ extract_packages_dir: 'C:/packages' });
      if (method === 'extract.inventory') return Promise.resolve({
        items: [
          { appid: 'wxcompiled0000001', name: 'Compiled app', package_paths: ['C:/packages/wxcompiled/main.wxapkg'], decompiled: false, output_dir: '', unsupported: true },
        ],
        summary: { indexed_count: 0, package_count: 0, output_count: 0 }
      });
      return Promise.resolve({ ok: true });
    });

    const wrapper = mount(ExtractView);
    await flushPromises();

    expect(wrapper.find('[data-testid="app-wxcompiled0000001"]').exists()).toBe(false);
    expect(wrapper.get('[data-testid="decompile-all"]').attributes('disabled')).toBeDefined();
  });

  it('counts only restorable programs when the inventory is unavailable', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'config.load') return Promise.resolve({ extract_packages_dir: 'C:/packages' });
      if (method === 'extract.inventory') return Promise.resolve({});
      if (method === 'extract.packages') return Promise.resolve({
        packages: [
          { appid: 'wxcompiled0000001', name: 'Compiled app', path: 'C:/packages/wxcompiled/main.wxapkg', unsupported: true },
          { appid: 'wxpackage', name: 'Ready app', path: 'C:/packages/wxpackage/main.wxapkg' },
        ]
      });
      return Promise.resolve({ ok: true });
    });

    const wrapper = mount(ExtractView);
    await flushPromises();

    expect(wrapper.get('[data-testid="inventory-summary"]').text()).toContain('1 个小程序');
    expect(wrapper.find('[data-testid="app-wxcompiled0000001"]').exists()).toBe(false);
  });

  it('shows a declared local icon without extra source labels', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'config.load') return Promise.resolve({ extract_packages_dir: 'C:/packages' });
      if (method === 'extract.inventory') return Promise.resolve({
        items: [{ appid: 'wxicon00000000001', name: '图标小程序', package_paths: ['C:/packages/wxicon/main.wxapkg'], decompiled: true, output_dir: 'C:/out/wxicon', icon_data_url: 'data:image/png;base64,abc' }],
        summary: { indexed_count: 0, package_count: 1, output_count: 1 }
      });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(ExtractView);
    await flushPromises();
    expect(wrapper.get('[data-testid="app-icon-wxicon00000000001"]').attributes('src')).toBe('data:image/png;base64,abc');
    expect(wrapper.find('[data-testid="icon-source-wxicon00000000001"]').exists()).toBe(false);
  });

  it('uses a generic AppID icon when no local icon is available', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'config.load') return Promise.resolve({ extract_packages_dir: 'C:/packages' });
      if (method === 'extract.inventory') return Promise.resolve({
        items: [{ appid: 'wxicon00000000002', package_paths: ['C:/packages/wxicon/main.wxapkg'], decompiled: true, output_dir: 'C:/out/wxicon' }],
        summary: { indexed_count: 0, package_count: 1, output_count: 1 }
      });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(ExtractView);
    await flushPromises();
    expect(wrapper.find('[data-testid="app-icon-fallback-wxicon00000000002"]').exists()).toBe(true);
  });

  it('falls back when a local icon cannot be rendered', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'config.load') return Promise.resolve({ extract_packages_dir: 'C:/packages' });
      if (method === 'extract.inventory') return Promise.resolve({
        items: [{ appid: 'wxicon00000000003', package_paths: ['C:/packages/wxicon/main.wxapkg'], decompiled: true, output_dir: 'C:/out/wxicon', icon_data_url: 'data:image/png;base64,broken' }],
        summary: { indexed_count: 0, package_count: 1, output_count: 1 }
      });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(ExtractView);
    await flushPromises();
    await wrapper.get('[data-testid="app-icon-wxicon00000000003"]').trigger('error');
    expect(wrapper.find('[data-testid="app-icon-fallback-wxicon00000000003"]').exists()).toBe(true);
  });

  it('renders complete findings and filters by information category only', async () => {
    let scanDone: ((payload: unknown) => void) | undefined;
    on.mockImplementation((event: string, listener: (payload: unknown) => void) => { if (event === 'extract_scan_done') scanDone = listener; return vi.fn(); });
    call.mockImplementation((method: string) => {
      if (method === 'config.load') return Promise.resolve({ extract_packages_dir: 'C:/packages' });
      if (method === 'extract.packages') return Promise.resolve({ packages: [{ appid: 'wx1', name: 'Demo', path: 'C:/packages/wx1/app.wxapkg', decompiled: true, scanned: true }] });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(ExtractView);
    await flushPromises();

    scanDone?.({
      appid: 'wx1',
      output_dir: 'C:/out/wx1',
      results: { secret: ['a1b2c3d4e5f6g7h8'] },
      findings: [
        { id: 'f1', rule_id: 'secret:app_secret', category: 'secret', title: '应用或支付密钥', severity: 'critical', confidence: 'high', value: '"appsecret" = "a1b2c3d4e5f6g7h8"', masked: 'a1b2...g7h8', file: 'pages/login.js', line: 12, snippet: 'var appsecret = "a1b2c3d4e5f6g7h8"', privilege: '可能导致账号接管' },
        { id: 'f2', rule_id: 'builtin:url', category: 'url', title: '业务接口地址', severity: 'low', confidence: 'high', value: 'https://api.example.com/health', masked: 'http...alth', file: 'services/health.js', line: 3, snippet: 'https://api.example.com/health' },
      ]
    });
    await wrapper.vm.$nextTick();

    await wrapper.get('[data-testid="primary-wx1"]').trigger('click');
    await flushPromises();

    expect(wrapper.get('[data-testid="finding-f1"]').text()).toContain('a1b2c3d4e5f6g7h8');
    expect(wrapper.text()).toContain('a1b2c3d4e5f6g7h8');
    expect(wrapper.find('[data-testid="severity-critical"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="summary-critical"]').exists()).toBe(false);
    expect(wrapper.get('[data-testid="findings-table"]').text()).not.toContain('风险');

    await wrapper.get('[data-testid="category-url"]').trigger('click');
    expect(wrapper.find('[data-testid="finding-f1"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="finding-f2"]').exists()).toBe(true);
  });

  it('uses backend scan events and requires confirmation before destructive cleanup', async () => {
    let progress: ((payload: unknown) => void) | undefined;
    on.mockImplementation((event: string, listener: (payload: unknown) => void) => { if (event === 'extract_progress') progress = listener; return vi.fn(); });
    call.mockImplementation((method: string) => {
      if (method === 'config.load') return Promise.resolve({ extract_packages_dir: 'C:/packages' });
      if (method === 'extract.packages') return Promise.resolve({ packages: [{ appid: 'wx1', path: 'C:/packages/wx1/app.wxapkg', decompiled: true }] });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(ExtractView);
    await flushPromises();
    await wrapper.get('[data-testid="primary-wx1"]').trigger('click');
    progress?.({ percent: 50, text: '扫描中' });
    await wrapper.vm.$nextTick();
    expect(wrapper.text()).toContain('50%');
    await wrapper.get('[data-testid="clear-decompiled"]').trigger('click');
    expect(wrapper.get('.modal-card').text()).toContain('所有解包和扫描数据');
    expect(call).not.toHaveBeenCalledWith('extract.clearOutput', { type: 'decompiled', dir: 'C:/packages' });
    await wrapper.get('[data-testid="confirm-extract"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('extract.clearOutput', { type: 'decompiled', dir: 'C:/packages' });

    await wrapper.get('[data-testid="delete-wx1"]').trigger('click');
    expect(wrapper.get('.modal-card').text()).toContain('wx1');
    expect(call).not.toHaveBeenCalledWith('extract.delete', { dir: 'C:/packages', appid: 'wx1' });
    await wrapper.get('[data-testid="confirm-extract"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('extract.delete', { dir: 'C:/packages', appid: 'wx1' });
  });

  it('reports scan failures instead of marking the app scanned', async () => {
    let scanDone: ((payload: unknown) => void) | undefined;
    on.mockImplementation((event: string, listener: (payload: unknown) => void) => { if (event === 'extract_scan_done') scanDone = listener; return vi.fn(); });
    call.mockImplementation((method: string) => {
      if (method === 'config.load') return Promise.resolve({ extract_packages_dir: 'C:/packages' });
      if (method === 'extract.packages') return Promise.resolve({ packages: [{ appid: 'wx1', path: 'C:/packages/wx1/app.wxapkg', decompiled: true }] });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(ExtractView);
    await flushPromises();
    await wrapper.get('[data-testid="primary-wx1"]').trigger('click');
    await flushPromises();

    scanDone?.({ appid: 'wx1', error: '读取产物目录失败' });
    await wrapper.vm.$nextTick();

    expect(wrapper.get('p.error[role="alert"]').text()).toContain('扫描失败: 读取产物目录失败');
    expect(wrapper.get('[data-testid="primary-wx1"]').text()).toBe('扫描');
    expect(wrapper.text()).not.toContain('已扫描');
    expect(wrapper.find('.progress-track').exists()).toBe(false);
  });

  it('picks a WeChat account in the header and points the package directory at it', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'config.load') return Promise.resolve({ extract_packages_dir: 'C:/packages' });
      if (method === 'extract.packages') return Promise.resolve({ packages: [] });
      if (method === 'sessionkey.detect') return Promise.resolve({ dir: 'C:/users', exists: true });
      if (method === 'sessionkey.users') {
        return Promise.resolve({
          users: [
            { id: '2ff6ef464aec84bdf6c3f39f0506ee68', dir: 'C:/users/2ff6ef46', appids: ['wx1'], packages_dir: 'C:/users/2ff6ef46/Applet/packages' },
            { id: 'aabbccddeeff00112233445566778899', dir: 'C:/users/aabbccdd', appids: [] },
          ]
        });
      }
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(ExtractView);
    await flushPromises();

    const select = wrapper.get('[data-testid="user-directory"]');
    expect(select.findAll('option')).toHaveLength(3);
    expect(select.findAll('option')[1].text()).toBe('2ff6ef464aec84bdf6c3f39f0506ee68');
    expect(select.findAll('option')[1].text()).not.toContain('…');
    expect(select.findAll('option')[2].attributes('disabled')).toBeDefined();

    await select.setValue('2ff6ef464aec84bdf6c3f39f0506ee68');
    await flushPromises();
    expect(wrapper.get<HTMLInputElement>('[data-testid="extract-directory"]').element.value).toBe('C:/users/2ff6ef46/Applet/packages');
    expect(call).toHaveBeenCalledWith('config.save', expect.objectContaining({ extract_packages_dir: 'C:/users/2ff6ef46/Applet/packages' }));
  });

  it('clears the active result when switching to another user directory', async () => {
    let scanDone: ((payload: unknown) => void) | undefined;
    on.mockImplementation((event: string, listener: (payload: unknown) => void) => { if (event === 'extract_scan_done') scanDone = listener; return vi.fn(); });
    call.mockImplementation((method: string) => {
      if (method === 'config.load') return Promise.resolve({ extract_packages_dir: 'C:/users/u1/Applet/packages' });
      if (method === 'extract.inventory') return Promise.resolve({
        items: [{ appid: 'wx1', name: 'One', package_paths: ['C:/users/u1/Applet/packages/wx1/main.wxapkg'], decompiled: true, output_dir: 'C:/out/wx1' }],
        summary: { indexed_count: 1, package_count: 1, output_count: 1 }
      });
      if (method === 'sessionkey.detect') return Promise.resolve({ dir: 'C:/users', exists: true });
      if (method === 'sessionkey.users') return Promise.resolve({ users: [
        { id: 'u1', dir: 'C:/users/u1', appids: ['wx1'], packages_dir: 'C:/users/u1/Applet/packages' },
        { id: 'u2', dir: 'C:/users/u2', appids: ['wx2'], packages_dir: 'C:/users/u2/Applet/packages' },
      ] });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(ExtractView);
    await flushPromises();

    scanDone?.({ appid: 'wx1', results: {}, findings: [{ id: 'f1', category: 'secret', title: '密钥', severity: 'high', confidence: 'high', value: 'raw-secret', masked: 'raw...cret', file: 'app.js', line: 1 }] });
    await wrapper.vm.$nextTick();
    expect(wrapper.find('[data-testid="finding-f1"]').exists()).toBe(true);

    await wrapper.get('[data-testid="user-directory"]').setValue('u2');
    await flushPromises();
    expect(wrapper.find('[data-testid="finding-f1"]').exists()).toBe(false);
    expect(wrapper.text()).toContain('等待选择小程序');
  });

  it('streams unpack logs into the log strip', async () => {
    let logListener: ((payload: unknown) => void) | undefined;
    on.mockImplementation((event: string, listener: (payload: unknown) => void) => { if (event === 'extract_log') logListener = listener; return vi.fn(); });
    call.mockImplementation((method: string) => {
      if (method === 'config.load') return Promise.resolve({ extract_packages_dir: 'C:/packages' });
      if (method === 'extract.packages') return Promise.resolve({ packages: [] });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(ExtractView);
    await flushPromises();
    expect(logListener).toBeTypeOf('function');

    logListener?.({ message: '解包完成 42 个文件' });
    await wrapper.vm.$nextTick();
    expect(wrapper.text()).toContain('解包完成 42 个文件');
  });

  it('filters the app table by name or appid', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'config.load') return Promise.resolve({ extract_packages_dir: 'C:/packages' });
      if (method === 'extract.packages') return Promise.resolve({ packages: [
        { appid: 'wx111', name: 'Demo', path: 'C:/packages/wx111/app.wxapkg' },
        { appid: 'wx222', name: 'Other', path: 'C:/packages/wx222/app.wxapkg' },
      ] });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(ExtractView);
    await flushPromises();
    expect(wrapper.find('[data-testid="primary-wx111"]').exists()).toBe(true);
    await wrapper.get('[data-testid="extract-search"]').setValue('other');
    expect(wrapper.find('[data-testid="primary-wx111"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="primary-wx222"]').exists()).toBe(true);
    await wrapper.get('[data-testid="extract-search"]').setValue('wx111');
    expect(wrapper.find('[data-testid="primary-wx111"]').exists()).toBe(true);
    expect(wrapper.find('[data-testid="primary-wx222"]').exists()).toBe(false);
  });
  it('picks a candidate directory from the help list and can decompile every app', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'config.load') return Promise.resolve({ extract_packages_dir: 'C:/packages' });
      if (method === 'extract.candidateDirs') return Promise.resolve({
        default: 'C:/users/demo/Applet/packages',
        dirs: [
          { os: 'windows', version: 'Windows v4 最新版', path: 'C:/users/demo/Applet/packages', exists: true, selected: true, description: '按登录用户自动发现' },
          { os: 'windows', version: 'Windows v3', path: 'C:/Documents/WeChat Files/Applet', exists: false, selected: false, description: '微信 3.x 文件管理目录' },
        ]
      });
      if (method === 'extract.packages') return Promise.resolve({ packages: [
        { appid: 'wx111', name: 'One', path: 'C:/packages/wx111/app.wxapkg' },
        { appid: 'wx222', name: 'Two', path: 'C:/packages/wx222/app.wxapkg' },
      ] });
      if (method === 'extract.decompileAll') return Promise.resolve({ succeeded: 2, failed: 0, results: [] });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(ExtractView);
    await flushPromises();

    expect(wrapper.text()).toContain('Windows v4 最新版');
    expect(wrapper.text()).toContain('按登录用户自动发现');
    // 自动识别已移除，候选列表是唯一的手动选择入口
    await wrapper.get('.candidate-list li button').trigger('click');
    await flushPromises();
    expect(wrapper.get<HTMLInputElement>('[data-testid="extract-directory"]').element.value).toBe('C:/users/demo/Applet/packages');

    await wrapper.get('[data-testid="decompile-all"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('extract.decompileAll', { dir: 'C:/users/demo/Applet/packages' });
    expect(wrapper.text()).toContain('成功 2 个');
  });



  it('reveals the Applet directory help from the ? affordance', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'config.load') return Promise.resolve({ extract_packages_dir: 'C:/packages' });
      if (method === 'extract.packages') return Promise.resolve({ packages: [] });
      if (method === 'extract.candidateDirs') return Promise.resolve({ default: '', dirs: [] });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(ExtractView);
    await flushPromises();

    const dot = wrapper.get('[data-testid="applet-dir-help"]');
    expect(dot.element.tagName).toBe('BUTTON');
    expect(wrapper.find('.path-help-pop details').exists()).toBe(false);
    const pop = wrapper.get('.path-help-pop');
    expect(pop.text()).toContain('Windows v3');
    expect(pop.text()).toContain('com.tencent.xinWeChat');
  });


  it('filters findings by category and hands the source over to the code browser', async () => {
    window.location.hash = '';
    let scanDone: ((payload: unknown) => void) | undefined;
    on.mockImplementation((event: string, listener: (payload: unknown) => void) => { if (event === 'extract_scan_done') scanDone = listener; return vi.fn(); });
    call.mockImplementation((method: string) => {
      if (method === 'config.load') return Promise.resolve({ extract_packages_dir: 'C:/packages' });
      if (method === 'extract.packages') return Promise.resolve({ packages: [{ appid: 'wx1', name: 'Demo', path: 'C:/packages/wx1/app.wxapkg', decompiled: true, scanned: true }] });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(ExtractView);
    await flushPromises();

    scanDone?.({
      appid: 'wx1',
      output_dir: 'C:/out/wx1',
      results: {},
      findings: [
        { id: 'f1', category: 'secret', title: '应用密钥', severity: 'critical', confidence: 'high', value: 'raw-secret', masked: 'raw...cret', file: 'pages/login.js', line: 12 },
        { id: 'f2', category: 'url', title: '接口地址', severity: 'low', confidence: 'high', value: 'https://api.example.com/health', masked: 'http...alth', file: 'services/health.js', line: 3 },
      ]
    });
    await wrapper.vm.$nextTick();
    await wrapper.get('[data-testid="primary-wx1"]').trigger('click');
    await flushPromises();

    expect(wrapper.get('[data-testid="category-secret"]').text()).toContain('账号密码');
    await wrapper.get('[data-testid="category-url"]').trigger('click');
    expect(wrapper.find('[data-testid="finding-f2"]').exists()).toBe(true);
    expect(wrapper.find('[data-testid="finding-f1"]').exists()).toBe(false);

    await wrapper.get('[data-testid="category-secret"]').trigger('click');
    const buttons = wrapper.get('[data-testid="finding-f1"]').findAll('button');
    await buttons[buttons.length - 1].trigger('click');
    expect(window.location.hash).toContain('#/code?');
    expect(decodeURIComponent(window.location.hash)).toContain('root=C:/out/wx1');
    // 匹配值一并带过去：代码浏览器用它做行内精确标记
    expect(decodeURIComponent(window.location.hash)).toContain('value=raw-secret');
  });

  it('finds a finding by the matched value the table shows', async () => {
    let scanDone: ((payload: unknown) => void) | undefined;
    on.mockImplementation((event: string, listener: (payload: unknown) => void) => { if (event === 'extract_scan_done') scanDone = listener; return vi.fn(); });
    call.mockImplementation((method: string) => {
      if (method === 'config.load') return Promise.resolve({ extract_packages_dir: 'C:/packages' });
      if (method === 'extract.inventory') return Promise.resolve({
        items: [{ appid: 'wx1', name: 'Demo', package_paths: ['C:/packages/wx1/main.wxapkg'], decompiled: true, output_dir: 'C:/out/wx1' }],
        summary: { indexed_count: 0, package_count: 1, output_count: 1 }
      });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(ExtractView);
    await flushPromises();
    scanDone?.({
      appid: 'wx1', results: {}, output_dir: 'C:/out/wx1',
      findings: [
        { id: 'f1', category: 'path', title: '路径', severity: 'low', confidence: 'high', value: 'config/config.js', masked: 'conf...g.js', file: 'game.js', line: 29 },
        { id: 'f2', category: 'secret', title: '密码', severity: 'high', confidence: 'high', value: 'setDisplayAsPassword=function', masked: 'setD...tion', file: 'game.js', line: 45 },
      ]
    });
    await wrapper.vm.$nextTick();

    // 「匹配结果」列显示的是 value：照着这一列搜就能搜到
    await wrapper.get('[data-testid="finding-search"]').setValue('setDisplayAsPassword');
    expect(wrapper.find('[data-testid="finding-f2"]').exists()).toBe(true);
    expect(wrapper.find('[data-testid="finding-f1"]').exists()).toBe(false);

    await wrapper.get('[data-testid="finding-search"]').setValue('game.js');
    expect(wrapper.find('[data-testid="finding-f1"]').exists()).toBe(true);
    expect(wrapper.find('[data-testid="finding-f2"]').exists()).toBe(true);
  });

  it('copies only the currently filtered findings', async () => {
    let scanDone: ((payload: unknown) => void) | undefined;
    on.mockImplementation((event: string, listener: (payload: unknown) => void) => { if (event === 'extract_scan_done') scanDone = listener; return vi.fn(); });
    call.mockImplementation((method: string) => {
      if (method === 'config.load') return Promise.resolve({ extract_packages_dir: 'C:/packages' });
      if (method === 'extract.packages') return Promise.resolve({ packages: [{ appid: 'wx1', name: 'Demo', path: 'C:/packages/wx1/main.wxapkg', decompiled: true }] });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(ExtractView);
    await flushPromises();
    scanDone?.({
      appid: 'wx1', results: { secret: ['raw-secret'], url: ['raw-url'] },
      findings: [
        { id: 'f1', category: 'secret', title: '密钥', severity: 'high', confidence: 'high', value: 'raw-secret', masked: 'raw...cret', file: 'app.js', line: 1 },
        { id: 'f2', category: 'url', title: '接口', severity: 'low', confidence: 'high', value: 'raw-url', masked: 'raw...url', file: 'api.js', line: 2 },
      ]
    });
    await wrapper.vm.$nextTick();
    await wrapper.get('[data-testid="category-url"]').trigger('click');
    await wrapper.get('[data-testid="copy-extract-results"]').trigger('click');
    expect(copyText).toHaveBeenCalledTimes(1);
    const [copied] = copyText.mock.calls[0] as [string];
    expect(copied).toContain('raw-url');
    expect(copied).not.toContain('raw-secret');
  });

  it('opens the per-app output directory from the app list', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'config.load') return Promise.resolve({ extract_packages_dir: 'C:/packages' });
      if (method === 'extract.packages') return Promise.resolve({ packages: [
        { appid: 'wx1', name: 'Demo', path: 'C:/packages/wx1/app.wxapkg', decompiled: true, output_dir: 'C:/out/wx1' },
        { appid: 'wx2', name: 'Fresh', path: 'C:/packages/wx2/app.wxapkg', decompiled: false, output_dir: 'C:/out/wx2' },
      ] });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(ExtractView);
    await flushPromises();

    // 未反编译的小程序没有产物可打开
    expect(wrapper.get('[data-testid="open-wx2"]').attributes('disabled')).toBeDefined();

    await wrapper.get('[data-testid="open-wx1"]').trigger('click');
    expect(call).toHaveBeenCalledWith('extract.openDir', { path: 'C:/out/wx1' });
  });

  it('loads results on scan and switches between scanned mini programs', async () => {
    let scanDone: ((payload: unknown) => void) | undefined;
    on.mockImplementation((event: string, listener: (payload: unknown) => void) => { if (event === 'extract_scan_done') scanDone = listener; return vi.fn(); });
    call.mockImplementation((method: string) => {
      if (method === 'config.load') return Promise.resolve({ extract_packages_dir: 'C:/packages' });
      if (method === 'extract.packages') return Promise.resolve({ packages: [
        { appid: 'wx1', name: 'One', path: 'C:/packages/wx1/app.wxapkg', decompiled: true },
        { appid: 'wx2', name: 'Two', path: 'C:/packages/wx2/app.wxapkg', decompiled: true },
      ] });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(ExtractView);
    await flushPromises();

    // 没扫过之前，切换入口不可用
    expect(wrapper.get('[data-testid="primary-wx1"]').text()).toContain('扫描');

    scanDone?.({
      appid: 'wx1',
      results: {},
      findings: [{ id: 'a1', category: 'secret', title: '一号密钥', severity: 'high', confidence: 'high', value: 'raw-1', masked: 'r...1', file: 'a.js', line: 1 }]
    });
    await wrapper.vm.$nextTick();
    // 扫描完成即自动加载明细
    expect(wrapper.find('[data-testid="finding-a1"]').exists()).toBe(true);

    scanDone?.({
      appid: 'wx2',
      results: {},
      findings: [{ id: 'b1', category: 'url', title: '二号接口', severity: 'low', confidence: 'high', value: 'https://x/y', masked: 'ht.../y', file: 'b.js', line: 2 }]
    });
    await wrapper.vm.$nextTick();
    expect(wrapper.find('[data-testid="finding-b1"]').exists()).toBe(true);
    expect(wrapper.find('[data-testid="finding-a1"]').exists()).toBe(false);

    // 切回已扫描的小程序，不触发重新扫描
    await wrapper.get('[data-testid="primary-wx1"]').trigger('click');
    await flushPromises();
    expect(wrapper.find('[data-testid="finding-a1"]').exists()).toBe(true);
    expect(wrapper.find('[data-testid="finding-b1"]').exists()).toBe(false);
    expect(call.mock.calls.filter(([method]) => method === 'extract.scan')).toHaveLength(0);
  });


  // 命中上千条时 50/页要翻几十页：每页大小可调；换大小必须回第一页，否则当前页会越界
  // 渲染成一张空表，看着像「没有命中」。
  it('changes the findings page size and returns to the first page', async () => {
    let scanDone: ((payload: unknown) => void) | undefined;
    on.mockImplementation((event: string, listener: (payload: unknown) => void) => { if (event === 'extract_scan_done') scanDone = listener; return vi.fn(); });
    call.mockImplementation((method: string) => {
      if (method === 'config.load') return Promise.resolve({ extract_packages_dir: 'C:/packages' });
      if (method === 'extract.packages') return Promise.resolve({ packages: [{ appid: 'wx1', name: 'Demo', path: 'C:/packages/wx1/app.wxapkg', mtime: 2, decompiled: true }] });
      return Promise.resolve({ ok: true });
    });
    const wrapper = mount(ExtractView);
    await flushPromises();
    await wrapper.get('[data-testid="primary-wx1"]').trigger('click');
    await flushPromises();
    const findings = Array.from({ length: 120 }, (_, i) => ({ id: `f${i}`, category: 'secret', title: '密码', severity: 'high', confidence: 'high', value: `v${i}`, masked: 'v…', file: 'app.js', line: i + 1 }));
    scanDone?.({ appid: 'wx1', findings });
    await wrapper.vm.$nextTick();

    expect(wrapper.get('.finding-pagination').text()).toContain('第 1 / 3 页');
    await wrapper.get('.finding-pagination button:not([disabled])').trigger('click');
    expect(wrapper.get('.finding-pagination').text()).toContain('第 2 / 3 页');
    expect(wrapper.get('[data-testid="finding-f50"] td').text()).toBe('51');

    await wrapper.get('[data-testid="findings-page-size"]').setValue('200');
    await flushPromises();
    // 120 条 ÷ 200 = 1 页：回到第一页，且 120 条全在表里
    expect(wrapper.get('.finding-pagination').text()).toContain('共 120 条');
    expect(wrapper.get('[data-testid="finding-f0"] td').text()).toBe('1');
    expect(wrapper.findAll('[data-testid^="finding-f"]')).toHaveLength(120);
  });
});
