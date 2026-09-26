import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import HighlightText from './HighlightText.vue';

describe('HighlightText', () => {
  it('wraps case-insensitive hits in mark.hit-term and keeps other text plain', () => {
    const wrapper = mount(HighlightText, { props: { text: '/v2/User/profile', term: 'user' } });
    const marks = wrapper.findAll('mark.hit-term');
    expect(marks).toHaveLength(1);
    expect(marks[0].text()).toBe('User');
    expect(wrapper.text()).toBe('/v2/User/profile');
  });

  it('renders plain text only when the term is empty or absent', () => {
    const empty = mount(HighlightText, { props: { text: 'api.example.com', term: '' } });
    expect(empty.findAll('mark')).toHaveLength(0);
    expect(empty.text()).toBe('api.example.com');
  });

  it('marks every occurrence', () => {
    const wrapper = mount(HighlightText, { props: { text: 'login-login', term: 'LOGIN' } });
    expect(wrapper.findAll('mark.hit-term').map((m) => m.text())).toEqual(['login', 'login']);
  });
});
