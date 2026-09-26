import { describe, expect, it } from 'vitest';
import { boundedPrettyJson, DETAIL_TEXT_LIMIT, isNoMiniappError, messageOf } from './format';

describe('boundedPrettyJson', () => {
  it('returns the full pretty text without a truncation marker when under the limit', () => {
    expect(boundedPrettyJson({ a: 1 })).toEqual({ text: '{\n  "a": 1\n}' });
  });

  it('keeps only the prefix of an oversized payload and reports shown / total', () => {
    const value = { body: 'x'.repeat(120) };
    const view = boundedPrettyJson(value, 50);
    const full = JSON.stringify(value, null, 2);
    expect(view.text).toHaveLength(50);
    expect(view.truncated).toEqual({ shown: 50, total: full.length });
    // 缺省上限即详情展示口径
    expect(boundedPrettyJson({ body: 'x'.repeat(DETAIL_TEXT_LIMIT) }).truncated).toBeDefined();
  });

  it('normalizes null and undefined to an empty object', () => {
    expect(boundedPrettyJson(null).text).toBe('{}');
    expect(boundedPrettyJson(undefined).text).toBe('{}');
  });
});

describe('messageOf', () => {
  it('renders the known Core failures in Chinese', () => {
    expect(messageOf(new Error('core error 1000: no miniapp connected')))
      .toBe('未连接小程序（请在微信里打开目标小程序后重试）');
    expect(messageOf('core error 1002: core call timed out: wxapi.start'))
      .toBe('调试引擎调用超时（wxapi.start）：请重试，或先重新启动引擎');
    expect(messageOf(new Error('core error 1001: core process has exited')))
      .toBe('调试引擎已退出：请重新启动引擎');
    expect(messageOf('core error 1001: core connection lost: read EOF'))
      .toBe('调试引擎连接已断开：请重新启动引擎');
  });

  it('translates the Core fragment inside a message that wraps it', () => {
    // devtools 那条：后端把 Core 的失败拼进自己的文案，前缀与其余部分必须留着。
    expect(messageOf('读取调试目标失败：core error 1000: no miniapp connected'))
      .toBe('读取调试目标失败：未连接小程序（请在微信里打开目标小程序后重试）');
  });

  it('passes an unknown failure through untouched, code and all', () => {
    // 认不出来的错误保留原文：错误码是用户与日志唯一能查的东西。
    const raw = 'core error 2000: engine not started';
    expect(messageOf(new Error(raw))).toBe(raw);
    expect(messageOf('非法参数')).toBe('非法参数');
    expect(messageOf(undefined)).toBe('undefined');
  });

  it('judges the no-miniapp condition from the raw failure, not the rendered text', () => {
    expect(isNoMiniappError(new Error('core error 1000: no miniapp connected'))).toBe(true);
    expect(isNoMiniappError('读取调试目标失败：core error 1000: no miniapp connected')).toBe(true);
    expect(isNoMiniappError(new Error('core error 1002: core call timed out: navigator.pages'))).toBe(false);
    // 已经翻过的文案不再命中：判据只认原文，否则同一次失败判两次两种结果。
    expect(isNoMiniappError(messageOf('core error 1000: no miniapp connected'))).toBe(false);
  });
});
