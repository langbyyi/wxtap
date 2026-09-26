import { mount } from '@vue/test-utils';
import { describe, expect, it, vi } from 'vitest';
import AssetDetail from './AssetDetail.vue';

const { push, copyText } = vi.hoisted(() => ({ push: vi.fn(), copyText: vi.fn() }));
vi.mock('vue-router', () => ({ useRouter: () => ({ push }) }));
vi.mock('../utils/notify', () => ({ copyText }));

describe('AssetDetail', () => {
  it('shows the full URL, tags and sources', () => {
    const wrapper = mount(AssetDetail, {
      props: {
        asset: {
          url: 'https://api.example.com/v2/user/profile',
          host: 'api.example.com',
          path: '/v2/user/profile',
          tags: ['需要登录'],
          sources: [{ type: 'traffic', ref: 'seed-1' }, { type: 'code', ref: 'pages/index.js' }],
        },
      },
    });
    expect(wrapper.text()).toContain('https://api.example.com/v2/user/profile');
    expect(wrapper.text()).toContain('需要登录');
    expect(wrapper.text()).toContain('traffic → seed-1');
    expect(wrapper.text()).toContain('code → pages/index.js');
  });

  it('says when an asset has no recorded sources', () => {
    const wrapper = mount(AssetDetail, { props: { asset: { url: 'cloud://env-1.login', host: 'env-1', path: 'login', sources: [] } } });
    expect(wrapper.text()).toContain('无来源记录');
  });

  it('offers the traffic jump for traffic-seen assets and pushes host+path as keyword', async () => {
    push.mockClear();
    const wrapper = mount(AssetDetail, {
      props: {
        asset: {
          url: 'https://api.example.com/v2/user/profile',
          host: 'api.example.com',
          path: '/v2/user/profile',
          sources: [{ type: 'traffic', ref: 'seed-1' }],
        },
      },
    });
    expect(wrapper.find('[data-testid="asset-open-traffic"]').exists()).toBe(true);

    await wrapper.get('[data-testid="asset-open-traffic"]').trigger('click');
    expect(push).toHaveBeenCalledWith({ path: '/traffic', query: { q: 'api.example.com/v2/user/profile' } });
  });

  it('hides the traffic jump for code-only assets', () => {
    push.mockClear();
    const wrapper = mount(AssetDetail, {
      props: {
        asset: {
          url: 'https://cdn.example.com/logo.png',
          host: 'cdn.example.com',
          path: '/logo.png',
          sources: [{ type: 'code', ref: 'pages/index.js' }],
        },
      },
    });
    expect(wrapper.find('[data-testid="asset-open-traffic"]').exists()).toBe(false);
    expect(push).not.toHaveBeenCalled();
  });

  // trafficSeen 才是该字段的立身之本：来源列表超 8 条会被截断，traffic 来源
  // 可能不在可见 sources 里，此时只有 trafficSeen 能说明端点被流量验证过。
  it('still offers the traffic jump when sources were capped but trafficSeen is set', () => {
    const wrapper = mount(AssetDetail, {
      props: {
        asset: {
          url: 'https://api.example.com/v1/login',
          host: 'api.example.com',
          path: '/v1/login',
          sources: [{ type: 'code', ref: 'a.js' }, { type: 'more', ref: '+9' }],
          trafficSeen: true,
        },
      },
    });
    expect(wrapper.find('[data-testid="asset-open-traffic"]').exists()).toBe(true);
  });

  it('copies the full URL to the clipboard', async () => {
    copyText.mockClear();
    const wrapper = mount(AssetDetail, {
      props: {
        asset: {
          url: 'https://api.example.com/v1/login',
          host: 'api.example.com',
          path: '/v1/login',
          sources: [],
        },
      },
    });
    await wrapper.get('[data-testid="asset-copy-url"]').trigger('click');
    expect(copyText).toHaveBeenCalledWith('https://api.example.com/v1/login', 'URL');
  });
});
