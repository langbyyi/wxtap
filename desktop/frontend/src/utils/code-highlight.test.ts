import { describe, expect, it } from 'vitest';
import { highlightLines, markHitText, normalizeHighlightLanguage } from './code-highlight';

describe('code-highlight', () => {
  it('maps backend language names onto the supported scanners', () => {
    expect(normalizeHighlightLanguage('javascript')).toBe('javascript');
    expect(normalizeHighlightLanguage('typescript')).toBe('javascript');
    expect(normalizeHighlightLanguage('json')).toBe('json');
    expect(normalizeHighlightLanguage('xml')).toBe('xml');
    expect(normalizeHighlightLanguage('html')).toBe('xml');
    expect(normalizeHighlightLanguage('css')).toBe('css');
    expect(normalizeHighlightLanguage('markdown')).toBe('text');
    expect(normalizeHighlightLanguage('text')).toBe('text');
    expect(normalizeHighlightLanguage(undefined)).toBe('text');
  });

  it('keeps one output line per source line, even across multi-line tokens', () => {
    const content = 'let a = 1;\n/* spans\nthree lines */\nconst b = `template\nstring`;\n';
    const lines = highlightLines(content, 'javascript');
    expect(lines).toHaveLength(6);
    expect(lines[5]).toBe('');
    // 多行注释与模板串逐行都有 span，不会整段缩进第一行
    expect(lines[1]).toContain('tok-comment');
    expect(lines[2]).toContain('tok-comment');
    expect(lines[4]).toContain('tok-str');
  });

  it('escapes raw text so only the tokenizer emits markup', () => {
    const lines = highlightLines('const s = "<script>&amp;\'";', 'javascript');
    expect(lines[0]).toContain('&lt;script&gt;&amp;amp;&#39;');
    expect(lines[0]).not.toContain('<script>');
  });

  it('colors javascript keywords, strings, comments, numbers and calls', () => {
    const [line] = highlightLines('function load(url) { // fetch it\n', 'javascript');
    expect(line).toContain('<span class="tok-kw">function</span>');
    expect(line).toContain('<span class="tok-fn">load</span>');
    expect(line).toContain('url');
    expect(line).toContain('<span class="tok-comment">// fetch it</span>');
    const [num] = highlightLines('return 42;\n', 'javascript');
    expect(num).toContain('<span class="tok-num">42</span>');
    const [lit] = highlightLines('if (a === true)\n', 'javascript');
    expect(lit).toContain('<span class="tok-lit">true</span>');
  });

  it('treats a slash as a regex literal only where one can start', () => {
    const [regexLine] = highlightLines('return /ab+c/g.test(x);\n', 'javascript');
    expect(regexLine).toContain('<span class="tok-str">/ab+c/g</span>');
    const [divisionLine] = highlightLines('total = a / b;\n', 'javascript');
    expect(divisionLine).not.toContain('tok-str');
  });

  it('distinguishes json keys from string values', () => {
    const [line] = highlightLines('{"appid": "wx123", "ok": true}\n', 'json');
    expect(line).toContain('<span class="tok-attr">&quot;appid&quot;</span>');
    expect(line).toContain('<span class="tok-str">&quot;wx123&quot;</span>');
    expect(line).toContain('<span class="tok-lit">true</span>');
  });

  it('colors xml tags, attributes and wxml mustaches', () => {
    const [line] = highlightLines('<view class="box">{{ title }}</view>\n', 'xml');
    expect(line).toContain('<span class="tok-tag">view</span>');
    expect(line).toContain('<span class="tok-attr">class</span>');
    expect(line).toContain('<span class="tok-str">&quot;box&quot;</span>');
    expect(line).toContain('<span class="tok-expr">{{ title }}</span>');
    const [comment] = highlightLines('<!-- note -->\n', 'xml');
    expect(comment).toContain('tok-comment');
  });

  it('splits wxss properties from selectors', () => {
    const css = '.card { color: #ffffff; margin: 4px; }\n';
    const [line] = highlightLines(css, 'css');
    expect(line).toContain('<span class="tok-tag">card</span>');
    expect(line).toContain('<span class="tok-attr">color</span>');
    expect(line).toContain('<span class="tok-num">#ffffff</span>');
    expect(line).toContain('<span class="tok-num">4px</span>');
  });

  it('leaves unknown languages as escaped plain text', () => {
    const [line] = highlightLines('a < b & c\n', 'markdown');
    expect(line).toBe('a &lt; b &amp; c');
  });

  it('returns empty output for empty content', () => {
    expect(highlightLines('', 'javascript')).toEqual([]);
  });

  it('marks the hit text inside rendered html without touching tag markup', () => {
    // 命中值落在字符串 token 里：包裹 mark 但不能破坏 span 结构
    const [line] = highlightLines('const token = "secret-value";\n', 'javascript');
    const marked = markHitText(line, 'secret-value');
    expect(marked).toContain('<mark class="code-hit-text">secret-value</mark>');
    expect(marked.startsWith('<span')).toBe(true);
    // 标签属性里出现的同名文本（class="tok-…"）绝不能被替换
    const tricky = markHitText('<span class="tok-str">class</span>', 'class');
    expect(tricky).toBe('<span class="tok-str"><mark class="code-hit-text">class</mark></span>');
    // 命中值不在行里时原样返回
    expect(markHitText(line, 'missing')).toBe(line);
  });
});
