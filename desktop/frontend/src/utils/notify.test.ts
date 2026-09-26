import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { copyText, dismissNotice, notify, useNotices } from './notify';

describe('notify', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    Object.defineProperty(window.navigator, 'clipboard', {
      configurable: true,
      value: { writeText: vi.fn().mockResolvedValue(undefined) },
    });
  });

  afterEach(() => {
    for (const notice of [...useNotices().notices.value]) dismissNotice(notice.id);
    window.runtime = undefined;
    vi.useRealTimers();
  });

  it('adds and auto-dismisses action feedback', () => {
    notify('保存成功', 'success');
    expect(useNotices().notices.value.at(-1)).toMatchObject({ message: '保存成功', tone: 'success' });

    vi.advanceTimersByTime(3200);
    expect(useNotices().notices.value).toHaveLength(0);
  });

  it('reports clipboard success and failure through the shared channel', async () => {
    await copyText('wx-demo', 'AppID');
    expect(window.navigator.clipboard.writeText).toHaveBeenCalledWith('wx-demo');
    expect(useNotices().notices.value.at(-1)?.message).toBe('AppID已复制');

    vi.mocked(window.navigator.clipboard.writeText).mockRejectedValueOnce(new Error('denied'));
    await copyText('value', '配置');
    expect(useNotices().notices.value.at(-1)).toMatchObject({ tone: 'error' });
    expect(useNotices().notices.value.at(-1)?.message).toContain('denied');
  });

  // The web Clipboard API is secure-context only, so on a platform where the
  // webview does not qualify the runtime's native clipboard has to carry it.
  it('falls back to the runtime clipboard when the web API is missing', async () => {
    Object.defineProperty(window.navigator, 'clipboard', { configurable: true, value: undefined });
    const setText = vi.fn().mockResolvedValue(true);
    window.runtime = { ClipboardSetText: setText };

    await copyText('wx-fallback', 'AppID');
    expect(setText).toHaveBeenCalledWith('wx-fallback');
    expect(useNotices().notices.value.at(-1)?.message).toBe('AppID已复制');
  });

  it('falls back to the runtime clipboard when writeText rejects', async () => {
    vi.mocked(window.navigator.clipboard.writeText).mockRejectedValueOnce(new Error('not allowed'));
    const setText = vi.fn().mockResolvedValue(true);
    window.runtime = { ClipboardSetText: setText };

    await copyText('wx-fallback', 'AppID');
    expect(setText).toHaveBeenCalledWith('wx-fallback');
    expect(useNotices().notices.value.at(-1)?.message).toBe('AppID已复制');
  });

  it('reports honestly when neither clipboard is available', async () => {
    Object.defineProperty(window.navigator, 'clipboard', { configurable: true, value: undefined });

    await copyText('wx-fallback', 'AppID');
    expect(useNotices().notices.value.at(-1)).toMatchObject({ tone: 'error' });
    expect(useNotices().notices.value.at(-1)?.message).toContain('没有可用的剪贴板');
  });
});
