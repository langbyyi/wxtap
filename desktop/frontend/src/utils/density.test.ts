import { beforeEach, describe, expect, it, vi } from 'vitest';
import { useDensity } from './density';

// 密度偏好是这台机器上的观感偏好：默认紧凑（盯列表翻数据的人先要吞吐），用户改过就记住，
// 记不住（隐私模式、localStorage 抛错）不能影响功能本身。
describe('useDensity', () => {
  beforeEach(() => {
    localStorage.clear();
    vi.restoreAllMocks();
  });

  it('defaults to compact and remembers the choice per page', () => {
    const first = useDensity('traffic');
    expect(first.compact.value).toBe(true);

    first.compact.value = false;
    expect(localStorage.getItem('wxtap-density:traffic')).toBe('comfort');

    // 另一个页面各记一份，互不影响
    expect(useDensity('wxapi').compact.value).toBe(true);
    expect(useDensity('traffic').compact.value).toBe(false);
  });

  it('honours a stored preference over the default', () => {
    localStorage.setItem('wxtap-density:cloud', 'comfort');
    expect(useDensity('cloud').compact.value).toBe(false);
    localStorage.setItem('wxtap-density:cloud', 'compact');
    expect(useDensity('cloud').compact.value).toBe(true);
  });

  it('falls back to the default when storage is unreadable or unwritable', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => { throw new Error('blocked'); });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw new Error('blocked'); });

    const { compact } = useDensity('traffic');
    expect(compact.value).toBe(true);
    // 写入失败也必须能翻转：这次会话里的切换照常生效
    expect(() => { compact.value = false; }).not.toThrow();
    expect(compact.value).toBe(false);
  });
});
