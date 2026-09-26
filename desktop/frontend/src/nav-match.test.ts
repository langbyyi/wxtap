import { describe, expect, it } from 'vitest';
import { navItemMatches } from './nav-match';

describe('navItemMatches', () => {
  it('matches the item itself and its child paths only', () => {
    expect(navItemMatches('/code', '/code')).toBe(true);
    expect(navItemMatches('/code/file', '/code')).toBe(true);
    expect(navItemMatches('/code-browser', '/code')).toBe(false);
    expect(navItemMatches('/control', '/code')).toBe(false);
    expect(navItemMatches('/ak', '/a')).toBe(false);
  });
});
