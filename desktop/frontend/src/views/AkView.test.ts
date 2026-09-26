import { flushPromises, mount } from '@vue/test-utils';
import { createPinia } from 'pinia';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import AkView from './AkView.vue';
import { useCredentialStore } from '../stores/credentials';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import PageHeader from '../components/PageHeader.vue';

const { call } = vi.hoisted(() => ({ call: vi.fn() }));
vi.mock('../api/bridge', () => ({ backend: { call } }));
vi.mock('../utils/notify', () => ({ copyText: vi.fn(), notify: vi.fn() }));

describe('AkView', () => {
  it('pins the type switch and 清空 above the scrolling column', () => {
    const wrapper = mount(AkView, { global: { plugins: [createPinia()] } });
    expect(wrapper.findComponent(PageHeader).exists()).toBe(true);
    expect(wrapper.find('.page-toolbar').exists()).toBe(false);
    // 它们是整页的作用域，钉在左栏顶部，不跟着表单滚走
    expect(wrapper.find('.controls-bar [data-testid="ak-clear"]').exists()).toBe(true);
    expect(wrapper.find('.controls-scroll [data-testid="ak-clear"]').exists()).toBe(false);
    // 面板头里那行「凭据」去掉了
    expect(wrapper.find('.input-panel .panel-header').exists()).toBe(false);
  });

  // 布局：页面分三块——控制 | 请求 | 响应；类型只有一个开关，不再有第二遍类型选择。
  it('keeps one type switch and the three zones: controls, request, response', () => {
    const wrapper = mount(AkView, { global: { plugins: [createPinia()] } });
    expect(wrapper.find('.mode-switch').exists()).toBe(false);
    expect(wrapper.findAll('.controls-bar .chip-row .chip')).toHaveLength(3);
    expect(wrapper.find('select#ak-mode').exists()).toBe(false);

    const columns = wrapper.find('.ak-layout').element.children;
    expect(columns).toHaveLength(3);
    expect(columns[0].classList.contains('controls')).toBe(true);
    expect(columns[1].classList.contains('request-panel')).toBe(true);
    expect(columns[2].classList.contains('response-panel')).toBe(true);
    // The credential verdict is a status line inside the form, not a second
    // result area competing with 响应结果.
    expect(wrapper.find('.input-panel form').exists()).toBe(true);
    expect(wrapper.find('.response-panel .response-empty').exists()).toBe(true);
  });

  // 三栏是默认形态，不是"宽屏才有的待遇"：窗口再窄也得是 控制 | 请求 | 响应，宁可让
  // 控制栏先收窄；而且请求与响应是均分的两栏。jsdom 不做布局，所以这两条直接读样式
  // 块——它们挡的正是"随手加一条 max-width 就叠成一列"和"把某一栏写成别的比例"。
  it('never collapses the three zones, and keeps 请求/响应 equal', () => {
    const source = readFileSync(resolve(process.cwd(), 'src/views/AkView.vue'), 'utf8');
    const style = source.slice(source.indexOf('<style scoped>'));
    const columns = /grid-template-columns:\s*([^;]+);/.exec(style)?.[1].trim() ?? '';
    // 三栏：按顶层空格切（括号里的空格不算）
    const tracks = columns.split(/\s+(?![^(]*\))/);
    expect(tracks).toHaveLength(3);
    expect(tracks[1]).toBe(tracks[2]);
    expect(style).not.toMatch(/@media[^{]*\{[^}]*\.ak-layout/);
  });

  it('offers every endpoint of the type in one grouped dropdown', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'wxopen.endpoints') {
        return Promise.resolve({
          groups: [{ id: 'mini', label: '微信小程序' }],
          endpoints: [
            { id: 'mini-code-unlimited', group: 'mini', category: '小程序码', label: '生成小程序码', summary: '生成码', method: 'POST', path: '/wxa/getwxacodeunlimit', json_body: true, returns: 'image' },
            { id: 'mini-sec-check', group: 'mini', category: '内容安全', label: '内容安全检测', summary: '检测文本', method: 'POST', path: '/wxa/msg_sec_check', json_body: true, returns: 'json' },
          ],
        });
      }
      return Promise.resolve({});
    });
    const wrapper = mount(AkView, { global: { plugins: [createPinia()] } });
    await flushPromises();

    // 一个下拉装全部接口，分类分组。关着的时候选项不在 DOM 里，也就不会被误点。
    const trigger = wrapper.get('[data-testid="wxopen-endpoint"]');
    expect(trigger.attributes('aria-expanded')).toBe('false');
    expect(wrapper.find('[data-testid="wxopen-endpoint-menu"]').exists()).toBe(false);

    await trigger.trigger('click');
    expect(trigger.attributes('aria-expanded')).toBe('true');
    const menu = wrapper.get('[data-testid="wxopen-endpoint-menu"]');
    expect(menu.findAll('.picker-group')).toHaveLength(2);
    expect(menu.findAll('.picker-option').map((option) => option.attributes('data-testid')))
      .toEqual(['wxopen-endpoint-mini-code-unlimited', 'wxopen-endpoint-mini-sec-check']);
    expect(wrapper.find('[data-testid="wxopen-category-小程序码"]').exists()).toBe(true);
    expect(wrapper.find('[data-testid="wxopen-category-内容安全"]').exists()).toBe(true);

    // 原生 select 的弹层由系统渲染，条目一多就顶出屏幕；自己画的这层把高度封死。
    const source = readFileSync(resolve(process.cwd(), 'src/views/AkView.vue'), 'utf8');
    expect(source).toMatch(/\.picker-menu\s*\{[^}]*max-height/);

    await wrapper.get('[data-testid="wxopen-endpoint-mini-sec-check"]').trigger('click');
    expect(wrapper.find('[data-testid="wxopen-endpoint-menu"]').exists()).toBe(false);
    expect(wrapper.get('[data-testid="wxopen-summary"]').text()).toContain('检测文本');
  });

  beforeEach(() => {
    call.mockReset();
    // 默认：没有接口表、没有判定。需要这两样的用例自己设 mock。
    call.mockResolvedValue({});
  });

  // 接口选择器是个下拉：openPicker 只展开，chooseEndpoint 再点目标接口。
  async function openPicker(wrapper: ReturnType<typeof mount>) {
    await wrapper.get('[data-testid="wxopen-endpoint"]').trigger('click');
    await flushPromises();
  }

  async function chooseEndpoint(wrapper: ReturnType<typeof mount>, id: string) {
    await openPicker(wrapper);
    await wrapper.get(`[data-testid="wxopen-endpoint-${id}"]`).trigger('click');
    await flushPromises();
  }

  // 验证失败时不能把上一次的结果留在屏上：旧 token 与「有效」会被读成这一次的结果，
  // 用户会把旧凭据的 token 复制走。与「执行」那边先清 callResult 是同一做法。
  it('drops the previous verdict and token when the next verification fails', async () => {
    call.mockResolvedValue({
      mode: 'mini', valid: true, endpoint: 'https://api.weixin.qq.com/cgi-bin/token', token: 'OLD-TOKEN', expires_in: 7200,
    });
    const wrapper = mount(AkView, { global: { plugins: [createPinia()] } });
    await flushPromises();
    await wrapper.get('[data-testid="ak-access"]').setValue('wx123');
    await wrapper.get('[data-testid="ak-secret"]').setValue('shhh');
    await wrapper.get('[data-testid="ak-verify"]').trigger('submit');
    await flushPromises();
    expect(wrapper.get('[data-testid="ak-valid"]').text()).toBe('有效');
    expect((wrapper.get('[data-testid="ak-token"]').element as HTMLInputElement).value).toBe('OLD-TOKEN');

    // 第二次请求抛出（传输失败、500 等）：屏上不得再剩下旧的判定与 token。
    call.mockRejectedValue(new Error('请求失败: connection reset'));
    await wrapper.get('[data-testid="ak-verify"]').trigger('submit');
    await flushPromises();

    expect(wrapper.get('[data-testid="ak-error"]').text()).toContain('connection reset');
    expect((wrapper.get('[data-testid="ak-token"]').element as HTMLInputElement).value).toBe('');
    expect(wrapper.find('[data-testid="ak-valid"]').exists()).toBe(false);
    expect(wrapper.find('[data-testid="ak-result"]').exists()).toBe(false);
  });

  it('verifies mini-program credentials and shows the verdict and the token', async () => {
    call.mockResolvedValue({
      mode: 'mini', valid: true, endpoint: 'https://api.weixin.qq.com/cgi-bin/token',
      errcode: 0, token_fingerprint: 'abcd1234abcd1234', expires_in: 7200,
    });
    const wrapper = mount(AkView, { global: { plugins: [createPinia()] } });
    await wrapper.get('[data-testid="ak-mode-mini"]').trigger('click');
    await wrapper.get('[data-testid="ak-access"]').setValue('wx123');
    await wrapper.get('[data-testid="ak-secret"]').setValue('secret');
    await wrapper.get('[data-testid="ak-fake-ip"]').setValue('192.0.2.10');
    await wrapper.get('form.input-stack').trigger('submit');
    await flushPromises();

    expect(call).toHaveBeenCalledWith('ak.verify', {
      mode: 'mini', access_key: 'wx123', secret_key: 'secret', fake_ip: '192.0.2.10', include_token: true,
    });
    expect(wrapper.get('[data-testid="ak-valid"]').text()).toBe('有效');
    expect(wrapper.get('[data-testid="ak-result"]').text()).toContain('abcd1234');
  });

  // 页面上不再有任何掩码：token 拿到就是全的，请求行里的凭据也是原文。
  it('shows the whole token and the whole request line, with no masking left', async () => {
    call.mockResolvedValue({
      mode: 'mini', valid: true, endpoint: 'x', request_url: 'https://api.weixin.qq.com/cgi-bin/token?grant_type=client_credential&appid=wx123&secret=shhh',
      token: 'abcdefghijklmnop', token_fingerprint: 'abcd1234',
    });
    const wrapper = mount(AkView, { global: { plugins: [createPinia()] } });
    await wrapper.get('[data-testid="ak-access"]').setValue('wx123');
    await wrapper.get('[data-testid="ak-secret"]').setValue('shhh');
    await wrapper.get('[data-testid="ak-verify"]').trigger('submit');
    await flushPromises();

    expect((wrapper.get('[data-testid="ak-token"]').element as HTMLInputElement).value).toBe('abcdefghijklmnop');
    expect(wrapper.find('[data-testid="ak-toggle-token"]').exists()).toBe(false);
    const requestHead = wrapper.get('[data-testid="packet-request-head"]').text();
    expect(requestHead).toContain('secret=shhh');
    expect(requestHead).not.toContain('***');

    // 两栏各有一个「复制」，响应栏末端不再重复一个「复制结果」。
    expect(wrapper.find('[data-testid="request-copy"]').exists()).toBe(true);
    expect(wrapper.find('[data-testid="response-copy"]').exists()).toBe(true);
    expect(wrapper.find('[data-testid="wxopen-copy-url"]').exists()).toBe(false);
  });

  it('requires both credential fields before calling the backend', async () => {
    const wrapper = mount(AkView, { global: { plugins: [createPinia()] } });
    await wrapper.get('[data-testid="ak-access"]').setValue('only-appid');
    await wrapper.get('form.input-stack').trigger('submit');
    // The page loads the endpoint table on mount, so the assertion is about
    // the verify call, not about the backend being untouched.
    expect(call).not.toHaveBeenCalledWith('ak.verify', expect.anything());
    expect(wrapper.get('[data-testid="ak-error"]').text()).toContain('AppSecret');
  });

  it('switches enterprise field labels and sends work mode', async () => {
    call.mockResolvedValue({ mode: 'work', valid: false, errcode: 40013, errmsg: 'invalid corpid' });
    const wrapper = mount(AkView, { global: { plugins: [createPinia()] } });
    await wrapper.get('[data-testid="ak-mode-work"]').trigger('click');
    expect(wrapper.text()).toContain('CorpID');
    expect(wrapper.text()).toContain('CorpSecret');
    await wrapper.get('[data-testid="ak-access"]').setValue('corp');
    await wrapper.get('[data-testid="ak-secret"]').setValue('corp-secret');
    await wrapper.get('form.input-stack').trigger('submit');
    await flushPromises();

    expect(call).toHaveBeenCalledWith('ak.verify', expect.objectContaining({ mode: 'work' }));
    expect(wrapper.get('[data-testid="ak-valid"]').text()).toBe('无效');
  });

  // The 「利用」 pages share one credential set: typing it here is what makes
  // the official-endpoint console usable, and 清空 must clear it for both.
  it('shares the typed credentials with the sibling 利用 page', async () => {
    const pinia = createPinia();
    const wrapper = mount(AkView, { global: { plugins: [pinia] } });
    const store = useCredentialStore(pinia);

    await wrapper.get('[data-testid="ak-mode-oa"]').trigger('click');
    await wrapper.get('[data-testid="ak-access"]').setValue('wx123');
    await wrapper.get('[data-testid="ak-secret"]').setValue('secret');
    expect(store.mode).toBe('oa');
    expect(store.accessKey).toBe('wx123');
    expect(store.secretKey).toBe('secret');
    expect(store.complete).toBe(true);

    await wrapper.get('[data-testid="ak-clear"]').trigger('click');
    expect(store.complete).toBe(false);
    expect(store.accessKey).toBe('');
  });

  // 「利用记录」整块去掉了：一次执行只活在请求/响应两栏里，右下角不再有列表与保存入口，
  // 也就没有落盘的记录文件。
  it('keeps an exchange in the two packet columns and leaves the corner empty', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'ak.verify') {
        return Promise.resolve({
          mode: 'mini', valid: true, endpoint: 'https://api.weixin.qq.com/cgi-bin/token',
          errcode: 0, expires_in: 7200, token: 'TOKEN-abcdefghijklmnop', token_fingerprint: 'abcd1234',
        });
      }
      return Promise.resolve({});
    });
    const wrapper = mount(AkView, { global: { plugins: [createPinia()] } });
    await flushPromises();
    await wrapper.get('[data-testid="ak-access"]').setValue('wx0123456789abcdef');
    await wrapper.get('[data-testid="ak-secret"]').setValue('secret');
    await wrapper.get('form.input-stack').trigger('submit');
    await flushPromises();

    expect(wrapper.get('[data-testid="packet-request-head"]').text()).toContain('GET /cgi-bin/token');
    expect(wrapper.get('[data-testid="ak-result"]').text()).toContain('TOKEN-abcdefghijklmnop');
    expect(wrapper.find('.history').exists()).toBe(false);

    // 响应区的「清空」清的仍是这一次执行
    await wrapper.get('[data-testid="response-clear"]').trigger('click');
    expect(wrapper.find('[data-testid="packet-request-head"]').exists()).toBe(false);
  });

  // --- 动态参数：一族的公共入参填一次，相关接口共用 ---
  describe('动态参数', () => {
    const table = {
      groups: [{ id: 'oa', label: '公众号' }],
      endpoints: [
        {
          id: 'oa-user-info', group: 'oa', category: '用户', label: '用户资料', summary: '按 openid 读资料',
          method: 'GET', path: '/cgi-bin/user/info', json_body: false, returns: 'json',
          params: [
            { id: 'openid', label: 'openid', kind: 'string', required: true, shared: true },
            { id: 'lang', label: 'lang', kind: 'string', default: 'zh_CN', shared: true },
          ],
        },
        {
          id: 'oa-followers', group: 'oa', category: '用户', label: '关注者列表', summary: '翻页读 openid',
          method: 'GET', path: '/cgi-bin/user/get', json_body: false, returns: 'json',
          params: [{ id: 'next_openid', label: 'next_openid', kind: 'string', shared: true }],
        },
      ],
    };

    async function mountConsole() {
      call.mockImplementation((method: string) => {
        if (method === 'wxopen.endpoints') return Promise.resolve(table);
        if (method === 'wxopen.call') {
          return Promise.resolve({
            endpoint: 'oa-user-info', label: '用户资料', method: 'GET',
            url: 'https://api.weixin.qq.com/cgi-bin/user/info?access_token=TOKEN-a1b2c3d4e5f6&openid=oABC',
            http_status: 200, errcode: 0, errmsg: 'ok', duration_ms: 12, body: { errcode: 0, nickname: 'x' },
          });
        }
        return Promise.resolve({});
      });
      const pinia = createPinia();
      const wrapper = mount(AkView, { global: { plugins: [pinia] } });
      await flushPromises();
      const store = useCredentialStore(pinia);
      store.mode = 'oa';
      store.accessKey = 'wx123';
      store.secretKey = 'secret';
      await flushPromises();
      return wrapper;
    }

    // 一个族的公共参数只出现一次，不是每个接口各问一遍。
    it('shows the family shared parameters once, above the endpoint groups', async () => {
      const wrapper = await mountConsole();
      expect(wrapper.find('[data-testid="wxopen-shared-openid"]').exists()).toBe(true);
      expect(wrapper.find('[data-testid="wxopen-shared-next_openid"]').exists()).toBe(true);
      expect(wrapper.find('[data-testid="wxopen-shared-lang"]').exists()).toBe(true);
      // 它们不在选中接口的详情里重复出现
      expect(wrapper.find('.endpoint-detail [data-testid="wxopen-shared-openid"]').exists()).toBe(false);
      // 默认值来自接口声明（lang=zh_CN）
      expect((wrapper.get('[data-testid="wxopen-shared-lang"]').element as HTMLInputElement).value).toBe('');
    });

    it('fills a required parameter from the shared group and keeps it across endpoints', async () => {
      const wrapper = await mountConsole();
      // 只填动态参数里的 openid，不碰接口自己的表单
      expect(wrapper.get('[data-testid="wxopen-missing"]').text()).toContain('openid');
      await wrapper.get('[data-testid="wxopen-shared-openid"]').setValue('oABC123');
      await flushPromises();
      expect(wrapper.get('[data-testid="wxopen-run"]').attributes('disabled')).toBeUndefined();

      await wrapper.get('[data-testid="wxopen-run"]').trigger('click');
      await flushPromises();
      const payload = call.mock.calls.find(([method]) => method === 'wxopen.call')?.[1] as { params: Record<string, string> };
      expect(payload.params.openid).toBe('oABC123');

      // 换到另一个接口，动态参数仍在（这正是"填一次"的意义）
      await chooseEndpoint(wrapper, 'oa-followers');
      expect((wrapper.get('[data-testid="wxopen-shared-openid"]').element as HTMLInputElement).value).toBe('oABC123');
    });
  });

  // --- 官方接口（同一页内的下一步）---
  // The token call proves the secret is valid; these calls show what it reaches.
  // Same page, same credentials, one request further.
  describe('官方接口', () => {
    const table = {
      groups: [{ id: 'mini', label: '微信小程序' }, { id: 'oa', label: '公众号' }, { id: 'work', label: '企业微信' }],
      endpoints: [
        {
          id: 'mini-sec-check', group: 'mini', label: '内容安全检测', summary: '提交文本做检测',
          method: 'POST', path: '/wxa/msg_sec_check', json_body: true, returns: 'json',
          params: [{ id: 'content', label: 'content', kind: 'string', required: true }],
        },
        {
          id: 'mini-code-unlimited', group: 'mini', label: '生成小程序码（不限量）', summary: '按 path/scene 生成码',
          method: 'POST', path: '/wxa/getwxacodeunlimit', json_body: true, returns: 'image',
          params: [
            { id: 'scene', label: 'scene', kind: 'string', required: true },
            { id: 'check_path', label: 'check_path', kind: 'bool', default: 'false' },
          ],
        },
        {
          id: 'oa-followers', group: 'oa', label: '关注者 openid 列表', summary: '读取关注者',
          method: 'GET', path: '/cgi-bin/user/get', json_body: false, returns: 'json',
          params: [{ id: 'next_openid', label: 'next_openid', kind: 'string' }],
        },
        {
          id: 'work-department-list', group: 'work', label: '部门列表', summary: '读取部门树',
          method: 'GET', path: '/cgi-bin/department/list', json_body: false, returns: 'json',
        },
      ],
    };

    function mockConsole() {
      call.mockImplementation((method: string) => {
        if (method === 'wxopen.endpoints') return Promise.resolve(table);
        return Promise.resolve({});
      });
    }

    function fillCredentials(store: ReturnType<typeof useCredentialStore>) {
      store.mode = 'mini';
      store.accessKey = 'wx123';
      store.secretKey = 'secret';
    }

    it('clears the console inputs too, so 清空 means the whole page', async () => {
      mockConsole();
      const wrapper = await mountConsole();
      await chooseEndpoint(wrapper, 'mini-code-unlimited');
      await wrapper.get('[data-testid="wxopen-param-scene"]').setValue('openid=oABC123');
      expect((wrapper.get('[data-testid="wxopen-param-scene"]').element as HTMLInputElement).value).toBe('openid=oABC123');

      await wrapper.get('[data-testid="ak-clear"]').trigger('click');
      // The value is gone; the parameter's own default is what remains.
      expect((wrapper.get('[data-testid="wxopen-param-scene"]').element as HTMLInputElement).value).toBe('');
      expect((wrapper.get('[data-testid="wxopen-param-check_path"]').element as HTMLInputElement).value).toBe('false');
    });

    it('keeps the table refresh in the endpoint panel header', async () => {
      mockConsole();
      const wrapper = await mountConsole();
      expect(wrapper.find('.console-panel .panel-header [data-testid="wxopen-reload"]').exists()).toBe(true);
    });

    it('shows a loading message while the endpoint table is on the way', async () => {
      let release: (value: unknown) => void = () => {};
      call.mockImplementation((method: string) => {
        if (method === 'wxopen.endpoints') return new Promise((resolve) => { release = resolve; });
        return Promise.resolve({});
      });
      const wrapper = mount(AkView, { global: { plugins: [createPinia()] } });
      await wrapper.vm.$nextTick();
      expect(wrapper.get('[data-testid="wxopen-list-empty"]').text()).toContain('正在读取接口表');
      release(table);
      await flushPromises();
      expect(wrapper.find('[data-testid="wxopen-list-empty"]').exists()).toBe(false);
      await openPicker(wrapper);
      expect(wrapper.find('[data-testid="wxopen-endpoint-mini-sec-check"]').exists()).toBe(true);
    });

    it('lists the endpoint table and opens the first endpoint', async () => {
      mockConsole();
      const wrapper = await mountConsole();
      expect(call).toHaveBeenCalledWith('wxopen.endpoints');
      // 收起时也能看出选中了哪一个：触发按钮上就写着
      expect(wrapper.get('[data-testid="wxopen-endpoint"]').text()).toContain('内容安全检测');
      expect(wrapper.get('[data-testid="wxopen-summary"]').text()).toContain('提交文本做检测');
    });

    // 选中态：屏幕上靠触发按钮的文字，屏幕阅读器靠 aria-expanded / aria-selected。
    it('marks the selected endpoint for assistive tech', async () => {
      mockConsole();
      const wrapper = await mountConsole();
      const trigger = wrapper.get('[data-testid="wxopen-endpoint"]');
      expect(trigger.attributes('aria-haspopup')).toBe('listbox');
      expect(trigger.attributes('aria-expanded')).toBe('false');
      expect(trigger.text()).toContain('内容安全检测');

      await openPicker(wrapper);
      expect(trigger.attributes('aria-expanded')).toBe('true');
      expect(wrapper.get('[data-testid="wxopen-endpoint-mini-sec-check"]').attributes('aria-selected')).toBe('true');
      expect(wrapper.get('[data-testid="wxopen-endpoint-mini-code-unlimited"]').attributes('aria-selected')).toBe('false');

      await wrapper.get('[data-testid="wxopen-endpoint-mini-code-unlimited"]').trigger('click');
      expect(trigger.attributes('aria-expanded')).toBe('false');
      expect(trigger.text()).toContain('生成小程序码');
      expect(wrapper.get('[data-testid="wxopen-summary"]').text()).toContain('生成码');
    });

    // 类型只在第 1 步选一次：第 2 步的列表跟着它走，重复的筛选片与"类型不匹配"
    // 状态都不存在了。
    it('scopes the endpoint list to the type chosen in step 1', async () => {
      mockConsole();
      const wrapper = await mountConsole();
      await openPicker(wrapper);
      expect(wrapper.find('[data-testid="wxopen-endpoint-mini-sec-check"]').exists()).toBe(true);
      expect(wrapper.find('[data-testid="wxopen-endpoint-oa-followers"]').exists()).toBe(false);
      expect(wrapper.find('[data-testid="wxopen-groups"]').exists()).toBe(false);
      expect(wrapper.text()).toContain('微信小程序');

      // 换类型连菜单一起收起：里面的条目已经不属于新范围了
      await wrapper.get('[data-testid="ak-mode-oa"]').trigger('click');
      await flushPromises();
      expect(wrapper.find('[data-testid="wxopen-endpoint-menu"]').exists()).toBe(false);
      await openPicker(wrapper);
      expect(wrapper.find('[data-testid="wxopen-endpoint-oa-followers"]').exists()).toBe(true);
      expect(wrapper.find('[data-testid="wxopen-endpoint-mini-sec-check"]').exists()).toBe(false);
      // The stale selection is replaced, not left pointing at a call the type
      // cannot make.
      expect(wrapper.get('[data-testid="wxopen-summary"]').text()).toContain('读取关注者');
      expect(wrapper.text()).toContain('公众号');
    });

    it('keeps 执行 disabled until credentials and required parameters are present', async () => {
      mockConsole();
      const pinia = createPinia();
      const wrapper = mount(AkView, { global: { plugins: [pinia] } });
      await flushPromises();
      await chooseEndpoint(wrapper, 'mini-sec-check');

      expect(wrapper.get('[data-testid="wxopen-run"]').attributes('disabled')).toBeDefined();
      expect(wrapper.get('[data-testid="wxopen-missing"]').text()).toContain('content');

      fillCredentials(useCredentialStore(pinia));
      await wrapper.get('[data-testid="wxopen-param-content"]').setValue('hello');
      await flushPromises();
      expect(wrapper.get('[data-testid="wxopen-run"]').attributes('disabled')).toBeUndefined();
    });

    it('sends the credential family the endpoint belongs to and renders the JSON body', async () => {
      mockConsole();
      const pinia = createPinia();
      const wrapper = mount(AkView, { global: { plugins: [pinia] } });
      await flushPromises();
      fillCredentials(useCredentialStore(pinia));
      await wrapper.get('[data-testid="wxopen-param-content"]').setValue('hello');
      call.mockResolvedValue({
        endpoint: 'mini-sec-check', label: '内容安全检测', method: 'POST',
        url: 'https://api.weixin.qq.com/wxa/msg_sec_check?access_token=TOKEN-a1b2c3d4e5f6',
        http_status: 200, errcode: 0, errmsg: 'ok', duration_ms: 42, body: { errcode: 0, result: { suggest: 'pass' } },
      });
      await wrapper.get('[data-testid="wxopen-run"]').trigger('click');
      await flushPromises();

      expect(call).toHaveBeenCalledWith('wxopen.call', {
        mode: 'mini', access_key: 'wx123', secret_key: 'secret',
        endpoint: 'mini-sec-check', params: { content: 'hello' },
      });
      expect(wrapper.get('[data-testid="wxopen-body"]').text()).toContain('pass');
      // 响应栏只有报文：状态行 + 头 + 体。结论小字（成功/耗时那一行）不再另起一块。
      expect(wrapper.get('[data-testid="packet-response-head"]').text()).toContain('HTTP/1.1 200');
      expect(wrapper.find('[data-testid="wxopen-meta"]').exists()).toBe(false);
    });

    // 旧的"选出别的类型的接口 → 警告并禁用执行"这个状态在结构上已经不可能出现
    // （列表只装当前类型的接口），所以换成这条更强的不变量。
    it('never lists an endpoint the chosen type cannot call', async () => {
      mockConsole();
      const wrapper = await mountConsole();
      for (const mode of ['mini', 'oa', 'work'] as const) {
        await wrapper.get(`[data-testid="ak-mode-${mode}"]`).trigger('click');
        await flushPromises();
        await openPicker(wrapper);
        const options = wrapper.get('[data-testid="wxopen-endpoint-menu"]').findAll('.picker-option');
        expect(options.length).toBeGreaterThan(0);
        for (const option of options) {
          expect(option.attributes('data-testid')).toContain(mode);
        }
      }
    });

    // 右侧是整段 HTTP 报文：请求（起始行 + 头 + 体）与响应（状态行 + 头 + 体），
    // 不是只给一个结果体。起始行只放 path?query，host 归到头上。
    it('shows the whole exchange as an HTTP packet, not just the response body', async () => {
      mockConsole();
      const pinia = createPinia();
      const wrapper = mount(AkView, { global: { plugins: [pinia] } });
      await flushPromises();
      fillCredentials(useCredentialStore(pinia));
      await wrapper.get('[data-testid="wxopen-param-content"]').setValue('hello');
      call.mockResolvedValue({
        endpoint: 'mini-sec-check', label: '内容安全检测', method: 'POST',
        url: 'https://api.weixin.qq.com/wxa/msg_sec_check?access_token=TOKEN-a1b2c3d4e5f6',
        http_status: 200, errcode: 0, errmsg: 'ok', duration_ms: 42,
        body: { errcode: 0, result: { suggest: 'pass' } },
        request_headers: { 'Content-Type': 'application/json' },
        request_body: '{"content":"hello"}',
        response_headers: { 'Content-Type': 'application/json; encoding=utf-8' },
      });
      await wrapper.get('[data-testid="wxopen-run"]').trigger('click');
      await flushPromises();

      const requestHead = wrapper.get('[data-testid="packet-request-head"]').text();
      expect(requestHead).toContain('POST /wxa/msg_sec_check?access_token=TOKEN-a1b2c3d4e5f6 HTTP/1.1');
      expect(requestHead).toContain('Host: api.weixin.qq.com');
      expect(requestHead).toContain('Content-Type: application/json');
      expect(wrapper.get('[data-testid="packet-request-body"]').text()).toBe('{"content":"hello"}');

      const responseHead = wrapper.get('[data-testid="packet-response-head"]').text();
      expect(responseHead).toContain('HTTP/1.1 200');
      expect(responseHead).toContain('encoding=utf-8');
      expect(wrapper.get('[data-testid="wxopen-body"]').text()).toContain('pass');
    });

    it('renders an image result instead of a JSON body', async () => {
      mockConsole();
      const pinia = createPinia();
      const wrapper = mount(AkView, { global: { plugins: [pinia] } });
      await flushPromises();
      fillCredentials(useCredentialStore(pinia));
      await chooseEndpoint(wrapper, 'mini-code-unlimited');
      await wrapper.get('[data-testid="wxopen-param-scene"]').setValue('s=1');
      call.mockResolvedValue({
        endpoint: 'mini-code-unlimited', label: '生成小程序码（不限量）', method: 'POST',
        url: 'https://api.weixin.qq.com/wxa/getwxacodeunlimit?access_token=TOKEN-a1b2c3d4e5f6',
        http_status: 200, errcode: 0, errmsg: '', duration_ms: 88,
        image_data_url: 'data:image/png;base64,AAAA',
      });
      await wrapper.get('[data-testid="wxopen-run"]').trigger('click');
      await flushPromises();

      expect(wrapper.get('[data-testid="wxopen-image"]').attributes('src')).toBe('data:image/png;base64,AAAA');
      expect(wrapper.find('[data-testid="wxopen-body"]').exists()).toBe(false);
      expect(wrapper.find('[data-testid="response-copy"]').exists()).toBe(true);
      // A default parameter still travels with the request.
      const payload = call.mock.calls.find(([method]) => method === 'wxopen.call')?.[1] as { params: Record<string, string> };
      expect(payload.params).toEqual({ scene: 's=1', check_path: 'false' });
    });

    it('reports a failed call without clearing the form', async () => {
      mockConsole();
      const pinia = createPinia();
      const wrapper = mount(AkView, { global: { plugins: [pinia] } });
      await flushPromises();
      fillCredentials(useCredentialStore(pinia));
      await wrapper.get('[data-testid="wxopen-param-content"]').setValue('hello');
      call.mockRejectedValue(new Error('凭据无效: invalid appid'));
      await wrapper.get('[data-testid="wxopen-run"]').trigger('click');
      await flushPromises();

      expect(wrapper.get('[data-testid="ak-error"]').text()).toContain('凭据无效');
      expect((wrapper.get('[data-testid="wxopen-param-content"]').element as HTMLInputElement).value).toBe('hello');
    });

    async function mountConsole() {
      const wrapper = mount(AkView, { global: { plugins: [createPinia()] } });
      await flushPromises();
      return wrapper;
    }
  });
});
