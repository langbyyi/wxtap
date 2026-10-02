import { describe, expect, it } from 'vitest';
import { describeTarget, inspectorUrlForTarget } from './target-role';

describe('describeTarget', () => {
  it('keeps the WeChat LiteApp root container out of business H5 candidates', () => {
    for (const url of ['https://liteapp.weixin.qq.com/', 'https://LITEAPP.WEIXIN.QQ.COM?debug=1']) {
      expect(describeTarget({ type: 'page', url }).role).toBe('微信容器');
    }
    for (const url of ['https://liteapp.weixin.qq.com/article', 'https://liteapp.weixin.qq.com.example.com/', 'https://mp.weixin.qq.com/s/article']) {
      expect(describeTarget({ type: 'page', url }).role).toBe('H5 候选');
    }
  });

  it('reads the mini-program appid and build from a page-frame target', () => {
    expect(describeTarget({
      type: 'page',
      url: 'https://servicewechat.com/wxabc1234567890a/8/page-frame.html',
    })).toEqual({ role: '小程序页面', appid: 'wxabc1234567890a', version: '8', miniappPage: true });
  });

  it('keeps other servicewechat targets out of the page role', () => {
    expect(describeTarget({ type: 'page', url: 'https://servicewechat.com/wxabc/8/appservice.js' }).role).toBe('小程序资源');
  });

  it('points the inspector websocket at the selected target', () => {
    expect(inspectorUrlForTarget(31415, 'page-1'))
      .toBe('devtools://devtools/bundled/inspector.html?ws=127.0.0.1:31415/devtools/page/page-1');
  });

  it('falls back to the default port and drops an unusable target id', () => {
    // 端口非法时退回默认端口；目标 id 不合形状时宁可不带路径，也不把它拼进 URL。
    expect(inspectorUrlForTarget(0, '')).toBe('devtools://devtools/bundled/inspector.html?ws=127.0.0.1:31415');
    expect(inspectorUrlForTarget(62001, '../../etc')).toBe('devtools://devtools/bundled/inspector.html?ws=127.0.0.1:62001');
    expect(inspectorUrlForTarget(31415, 'a'.repeat(129))).toBe('devtools://devtools/bundled/inspector.html?ws=127.0.0.1:31415');
  });

  it('labels workers and ordinary pages without inventing an appid', () => {
    expect(describeTarget({ type: 'worker', url: 'wss://worker' })).toMatchObject({ role: '逻辑层', appid: '', miniappPage: false });
    expect(describeTarget({ type: 'page', url: 'https://example' })).toMatchObject({ role: 'H5 候选', miniappPage: false });
  });

  it('uses the actual hostname and pathname when distinguishing H5 from miniapps', () => {
    for (const url of [
      'https://servicewechat.com.evil.test/wxabc1234567890a/8/page-frame.html',
      'https://example.com/?next=https://servicewechat.com/wxabc1234567890a/8/page-frame.html',
    ]) expect(describeTarget({ type: 'page', url })).toMatchObject({ role: 'H5 候选', appid: '', miniappPage: false });
    for (const url of ['about:blank', 'devtools://devtools/bundled/inspector.html', 'invalid']) {
      expect(describeTarget({ type: 'page', url }).role).toBe('页面');
    }
    expect(describeTarget({ type: 'iframe', url: 'https://example.com/frame' }).role).toBe('H5 候选');
    expect(describeTarget({ type: 'worker', url: 'https://example.com/worker.js' }).role).toBe('逻辑层');
  });
});
