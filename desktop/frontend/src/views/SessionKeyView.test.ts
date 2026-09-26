import { flushPromises, mount } from '@vue/test-utils';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import SessionKeyView from './SessionKeyView.vue';
import PageHeader from '../components/PageHeader.vue';
import { wxEncrypt } from '../utils/wx-crypto';

const { call, copyText } = vi.hoisted(() => ({
  call: vi.fn(),
  copyText: vi.fn().mockResolvedValue(undefined),
}));
vi.mock('../api/bridge', () => ({ backend: { call } }));
vi.mock('../utils/notify', () => ({
  copyText,
  notify: vi.fn(),
}));

const captureFinding = {
  appid: 'wx1234567890abcdef',
  source: 'traffic#42 POST https://api.example.com/login (response)',
  value: 'Xsdni3/wBgoPUlvmCMljyA==',
  kind: 'iv',
  context: '',
  masked: 'Xsdni3...ljyA==',
  foundAt: '2023-11-14T22:13:20Z',
};

describe('SessionKeyView', () => {
  it('uses the shared page header contract', () => {
    expect(mount(SessionKeyView).findComponent(PageHeader).exists()).toBe(true);
  });
  beforeEach(() => {
    call.mockReset();
    call.mockResolvedValue({});
  });

  it('renders decrypt mode and the packet capture source', async () => {
    const wrapper = mount(SessionKeyView);
    await flushPromises();
    expect(wrapper.find('[data-testid="mode-decrypt"]').classes()).toContain('active');
    expect(wrapper.text()).toContain('sessionKey');
    expect(wrapper.text()).toContain('抓包记录');
    expect(wrapper.text()).toContain('从抓包记录提取');
  });

  it('extracts crypto fields from captured packets and fills the matching input', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'sessionkey.scanTraffic') return Promise.resolve({ findings: [captureFinding], scanned: 12 });
      return Promise.resolve({});
    });
    const wrapper = mount(SessionKeyView);
    await flushPromises();
    await wrapper.get('[data-testid="scan-traffic"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('sessionkey.scanTraffic', {});
    expect(wrapper.get('[data-testid="traffic-finding-0"]').text()).toContain('Xsdni3');
    await wrapper.get('[data-testid="traffic-finding-0"]').trigger('click');
    expect((wrapper.get('[data-testid="input-iv"]').element as HTMLTextAreaElement).value).toBe('Xsdni3/wBgoPUlvmCMljyA==');
  });

  it('reports an empty capture scan', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'sessionkey.scanTraffic') return Promise.resolve({ findings: [], scanned: 12 });
      return Promise.resolve({});
    });
    const wrapper = mount(SessionKeyView);
    await flushPromises();
    await wrapper.get('[data-testid="scan-traffic"]').trigger('click');
    await flushPromises();
    expect(wrapper.text()).toContain('已扫描 12 条报文');
  });

  it('switches to encrypt mode', async () => {
    const wrapper = mount(SessionKeyView);
    await flushPromises();
    await wrapper.get('[data-testid="mode-encrypt"]').trigger('click');
    expect(wrapper.get('[data-testid="mode-encrypt"]').classes()).toContain('active');
    expect(wrapper.text()).toContain('明文 JSON');
  });

  it('shows error when inputs are empty', async () => {
    const wrapper = mount(SessionKeyView);
    await flushPromises();
    await wrapper.get('[data-testid="run-crypto"]').trigger('click');
    await flushPromises();
    expect(wrapper.find('[data-testid="sessionkey-error"]').exists()).toBe(true);
  });

  it('fills sample data on loadSample click', async () => {
    const wrapper = mount(SessionKeyView);
    await flushPromises();
    await wrapper.get('[data-testid="load-sample"]').trigger('click');
    expect((wrapper.get('[data-testid="input-sessionkey"]').element as HTMLTextAreaElement).value).not.toBe('');
  });

  it('recognizes combined JSON after URL decoding', async () => {
    const wrapper = mount(SessionKeyView);
    await flushPromises();
    const payload = encodeURIComponent(JSON.stringify({
      session_key: 'tiihtNczf5v6AKRyjwEUhQ==',
      iv: 'Xsdni3/wBgoPUlvmCMljyA==',
      encryptedData: 'kOM74Dk6JNXG6Dc7dwyrpmdalmoEyVhCqNGPmQf2n1yQL/z6bwHQ81eUtW',
    }));
    await wrapper.get('[data-testid="combined-input"]').setValue(payload);
    await wrapper.get('[data-testid="url-decode"]').trigger('click');
    await wrapper.get('[data-testid="recognize-fill"]').trigger('click');
    expect((wrapper.get('[data-testid="input-sessionkey"]').element as HTMLTextAreaElement).value).toBe('tiihtNczf5v6AKRyjwEUhQ==');
    expect((wrapper.get('[data-testid="input-iv"]').element as HTMLTextAreaElement).value).toBe('Xsdni3/wBgoPUlvmCMljyA==');
    expect((wrapper.get('[data-testid="input-payload"]').element as HTMLTextAreaElement).value).toContain('kOM74Dk6');
  });

  it('scans decompiled results and fills the matching field', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'sessionkey.scanDecompiled') return Promise.resolve({ findings: [
        { appid: 'wx123', source: 'pages/login.js', value: 'Xsdni3/wBgoPUlvmCMljyA==', kind: 'iv', context: '', masked: 'Xsdni3...ljyA==', foundAt: '' },
      ] });
      return Promise.resolve({});
    });
    const wrapper = mount(SessionKeyView);
    await flushPromises();
    await wrapper.get('[data-testid="scan-decompiled"]').trigger('click');
    await flushPromises();
    expect(call).toHaveBeenCalledWith('sessionkey.scanDecompiled', {});
    expect(wrapper.get('[data-testid="decompiled-finding-0"]').text()).toContain('Xsdni3');
    await wrapper.get('[data-testid="decompiled-finding-0"]').trigger('click');
    expect((wrapper.get('[data-testid="input-iv"]').element as HTMLTextAreaElement).value).toBe('Xsdni3/wBgoPUlvmCMljyA==');
  });

  it('keeps the mode switch in the input panel header and feeds a decrypted result back into encrypt', async () => {
    const wrapper = mount(SessionKeyView);
    await flushPromises();
    // 模式切换不再独占一整行，它随输入面板走
    expect(wrapper.find('.mode-tabs').exists()).toBe(false);
    expect(wrapper.find('.panel-header .chip-row [data-testid="mode-decrypt"]').exists()).toBe(true);

    // 解密出来的是明文 JSON，改完再加密回去不必手工搬运
    const cipher = await wxEncrypt('{"phoneNumber":"13888888888"}', 'Xsdni3/wBgoPUlvmCMljyA==', 'tiihtNczf5v6AKRyjwEUhQ==');
    await wrapper.get('[data-testid="input-sessionkey"]').setValue('tiihtNczf5v6AKRyjwEUhQ==');
    await wrapper.get('[data-testid="input-iv"]').setValue('Xsdni3/wBgoPUlvmCMljyA==');
    await wrapper.get('[data-testid="input-payload"]').setValue(cipher);
    await wrapper.get('[data-testid="run-crypto"]').trigger('click');
    // 加解密走 WebCrypto，结果比一轮微任务晚到
    await vi.waitFor(() => expect(wrapper.find('[data-testid="crypto-result"]').exists()).toBe(true));
    expect(wrapper.get('[data-testid="crypto-result"]').text()).toContain('13888888888');

    await wrapper.get('[data-testid="use-as-plaintext"]').trigger('click');
    await flushPromises();
    expect(wrapper.get('[data-testid="mode-encrypt"]').classes()).toContain('active');
    expect((wrapper.get('[data-testid="input-payload"]').element as HTMLTextAreaElement).value).toContain('13888888888');
  });

  it('copies only the active input and clears all fields', async () => {
    call.mockImplementation((method: string) => {
      if (method === 'sessionkey.scanTraffic') return Promise.resolve({ findings: [captureFinding], scanned: 12 });
      return Promise.resolve({});
    });
    const wrapper = mount(SessionKeyView);
    await flushPromises();
    await wrapper.get('[data-testid="input-payload"]').setValue('ciphertext-value');
    await wrapper.get('[data-testid="copy-ciphertext"]').trigger('click');
    expect(copyText).toHaveBeenCalledWith('ciphertext-value', '密文');

    await wrapper.get('[data-testid="mode-encrypt"]').trigger('click');
    await wrapper.get('[data-testid="input-payload"]').setValue('{"phoneNumber":"13888888888"}');
    await wrapper.get('[data-testid="copy-plaintext"]').trigger('click');
    expect(copyText).toHaveBeenCalledWith('{"phoneNumber":"13888888888"}', '明文');

    // 清空承诺「输入、结果与扫描列表」一起清：扫描列表也要真的消失。
    await wrapper.get('[data-testid="scan-traffic"]').trigger('click');
    await flushPromises();
    expect(wrapper.find('[data-testid="traffic-finding-0"]').exists()).toBe(true);

    await wrapper.get('[data-testid="clear-sessionkey"]').trigger('click');
    expect((wrapper.get('[data-testid="input-iv"]').element as HTMLTextAreaElement).value).toBe('');
    expect((wrapper.get('[data-testid="input-payload"]').element as HTMLTextAreaElement).value).toBe('');
    expect(wrapper.find('[data-testid="traffic-finding-0"]').exists()).toBe(false);
  });
});
