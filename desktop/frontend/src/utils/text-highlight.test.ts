import { describe, expect, it } from 'vitest';
import { splitHighlight } from './text-highlight';

describe('splitHighlight', () => {
  it('returns a single miss for an empty or blank term', () => {
    expect(splitHighlight('api.example.com', '')).toEqual([{ text: 'api.example.com', hit: false }]);
    expect(splitHighlight('api.example.com', '   ')).toEqual([{ text: 'api.example.com', hit: false }]);
  });

  it('splits case-insensitively around every hit', () => {
    expect(splitHighlight('https://api.example.com/v2/User/profile', 'user')).toEqual([
      { text: 'https://api.example.com/v2/', hit: false },
      { text: 'User', hit: true },
      { text: '/profile', hit: false },
    ]);
    expect(splitHighlight('login-login', 'LOGIN')).toEqual([
      { text: 'login', hit: true },
      { text: '-', hit: false },
      { text: 'login', hit: true },
    ]);
  });

  it('keeps the original casing of hit segments', () => {
    expect(splitHighlight('AppConfig.json', 'config')).toEqual([
      { text: 'App', hit: false },
      { text: 'Config', hit: true },
      { text: '.json', hit: false },
    ]);
  });

  it('returns one miss when the term never matches', () => {
    expect(splitHighlight('/v2/order/create', 'zzz')).toEqual([{ text: '/v2/order/create', hit: false }]);
  });
});
