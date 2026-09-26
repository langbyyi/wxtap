// 反编译产物的轻量语法高亮。刻意不引第三方高亮库：产物里常见的就是
// js/ts、json、wxml/html、wxss/css 这几类，一个几百行的分词器足够把它们
// 读起来从「一坨白字」变成「编辑器」，又不给嵌入产物加体积。
//
// 输出契约：highlightLines 返回与 content.split('\n') 等长的 HTML 数组，
// 每项只含转义后的文本和本模块生成的 <span class="tok-*">，消费方可以放心
// v-html。所有原始文本一律先转义，这是注入安全边界。

export type HighlightLanguage = 'javascript' | 'json' | 'xml' | 'css' | 'text';

// 后端 codeservice.go 的 codeLanguages 决定了这里收得到哪些名字。
export function normalizeHighlightLanguage(language?: string): HighlightLanguage {
  switch (language) {
    case 'javascript':
    case 'typescript':
      return 'javascript';
    case 'json':
      return 'json';
    case 'xml':
    case 'html':
      return 'xml';
    case 'css':
      return 'css';
    default:
      return 'text';
  }
}

const ESCAPES: Record<string, string> = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };

function escapeHtml(text: string): string {
  return text.replace(/[&<>"']/g, (ch) => ESCAPES[ch]);
}

type Token = { text: string; cls: string };

function renderRow(row: Token[]): string {
  let html = '';
  for (const token of row) {
    html += token.cls ? `<span class="${token.cls}">${escapeHtml(token.text)}</span>` : escapeHtml(token.text);
  }
  return html;
}

// 高亮主入口：整个文件跑一遍分词，按行切成 HTML。多行注释/模板串/标签都
// 会在 emit 处按行断开，所以每一行的着色不依赖渲染窗口从哪一行开始。
export function highlightLines(content: string, language: string): string[] {
  const lang = normalizeHighlightLanguage(language);
  if (!content) return [];
  if (lang === 'text') return content.split('\n').map(escapeHtml);
  const lines: string[] = [];
  const row: Token[] = [];
  const emit = (text: string, cls: string) => {
    const parts = text.split('\n');
    for (let i = 0; i < parts.length; i += 1) {
      if (i > 0) {
        lines.push(renderRow(row));
        row.length = 0;
      }
      if (parts[i]) row.push({ text: parts[i], cls });
    }
  };
  if (lang === 'javascript') scanJs(content, emit);
  else if (lang === 'json') scanJson(content, emit);
  else if (lang === 'xml') scanXml(content, emit);
  else scanCss(content, emit);
  lines.push(renderRow(row));
  return lines;
}

const JS_KEYWORDS = new Set([
  'abstract', 'as', 'asserts', 'async', 'await', 'break', 'case', 'catch', 'class', 'const', 'continue',
  'debugger', 'declare', 'default', 'delete', 'do', 'else', 'enum', 'export', 'extends', 'finally', 'for',
  'from', 'function', 'get', 'if', 'implements', 'import', 'in', 'infer', 'instanceof', 'interface', 'is',
  'keyof', 'let', 'namespace', 'new', 'of', 'package', 'private', 'protected', 'public', 'readonly',
  'return', 'satisfies', 'set', 'static', 'super', 'switch', 'throw', 'try', 'type', 'typeof', 'var',
  'void', 'while', 'with', 'yield',
]);
const JS_LITERALS = new Set(['true', 'false', 'null', 'undefined', 'NaN', 'Infinity']);
// 出现在这些位置（或文件开头）的 / 是正则字面量而不是除号。
const REGEX_PRECEDING_KEYWORDS = new Set(['return', 'typeof', 'instanceof', 'in', 'of', 'new', 'delete', 'void', 'case', 'do', 'else', 'yield', 'await', 'throw']);
const REGEX_PRECEDING_PUNCT = new Set(['(', ',', '=', ':', '[', '!', '&', '|', '?', '{', '}', ';', '+', '-', '*', '%', '~', '^', '<', '>']);

const IDENT_SOURCE = /[A-Za-z_$][\w$]*/y;
const NUMBER_SOURCE = /(?:0[xXbBoO][0-9a-fA-F]+|\d[\d_]*(?:\.[\d_]+)?(?:[eE][+-]?\d+)?)n?/y;

function scanIdentifier(content: string, start: number): string {
  IDENT_SOURCE.lastIndex = start;
  const match = IDENT_SOURCE.exec(content);
  return match ? match[0] : content[start];
}

function scanString(content: string, start: number, quote: string): string {
  let i = start + 1;
  while (i < content.length) {
    const ch = content[i];
    if (ch === '\\') { i += 2; continue; }
    if (ch === quote) return content.slice(start, i + 1);
    // 未闭合的字符串到换行为止：反编译产物里残缺代码很常见，不能吞掉整个文件。
    if (ch === '\n') break;
    i += 1;
  }
  return content.slice(start, i);
}

function scanTemplate(content: string, start: number): string {
  let i = start + 1;
  while (i < content.length) {
    const ch = content[i];
    if (ch === '\\') { i += 2; continue; }
    if (ch === '`') return content.slice(start, i + 1);
    i += 1;
  }
  return content.slice(start, i);
}

// 匹配失败（换行前没有收尾的 /）时返回空串，调用方按除号处理。
function scanRegex(content: string, start: number): string {
  let i = start + 1;
  let inClass = false;
  while (i < content.length) {
    const ch = content[i];
    if (ch === '\\') { i += 2; continue; }
    if (ch === '\n') return '';
    if (ch === '[') inClass = true;
    else if (ch === ']') inClass = false;
    else if (ch === '/' && !inClass) {
      i += 1;
      while (i < content.length && /[a-z]/.test(content[i])) i += 1;
      return content.slice(start, i);
    }
    i += 1;
  }
  return '';
}

function scanJs(content: string, emit: (text: string, cls: string) => void) {
  let i = 0;
  // 上一个有意义的 token（忽略空白与注释），用于区分正则与除号。
  let last = '';
  while (i < content.length) {
    const ch = content[i];
    if (ch === '\n') { emit('\n', ''); i += 1; continue; }
    if (ch === ' ' || ch === '\t' || ch === '\r') {
      let j = i;
      while (j < content.length && /[\s]/.test(content[j])) j += 1;
      emit(content.slice(i, j), '');
      i = j;
      continue;
    }
    if (ch === '/' && content[i + 1] === '/') {
      let j = content.indexOf('\n', i);
      if (j < 0) j = content.length;
      emit(content.slice(i, j), 'tok-comment');
      i = j;
      continue;
    }
    if (ch === '/' && content[i + 1] === '*') {
      let j = content.indexOf('*/', i + 2);
      j = j < 0 ? content.length : j + 2;
      emit(content.slice(i, j), 'tok-comment');
      i = j;
      continue;
    }
    if (ch === '"' || ch === "'") {
      const text = scanString(content, i, ch);
      emit(text, 'tok-str');
      i += text.length;
      last = text;
      continue;
    }
    if (ch === '`') {
      const text = scanTemplate(content, i);
      emit(text, 'tok-str');
      i += text.length;
      last = text;
      continue;
    }
    if (ch === '/' && (last === '' || REGEX_PRECEDING_PUNCT.has(last) || REGEX_PRECEDING_KEYWORDS.has(last))) {
      const text = scanRegex(content, i);
      if (text) {
        emit(text, 'tok-str');
        i += text.length;
        last = text;
        continue;
      }
    }
    if (/\d/.test(ch) || (ch === '.' && /\d/.test(content[i + 1] ?? ''))) {
      NUMBER_SOURCE.lastIndex = i;
      const match = NUMBER_SOURCE.exec(content);
      const text = match ? match[0] : ch;
      emit(text, 'tok-num');
      i += text.length;
      last = text;
      continue;
    }
    if (/[A-Za-z_$]/.test(ch)) {
      const text = scanIdentifier(content, i);
      let j = i + text.length;
      while (j < content.length && /[\s]/.test(content[j])) j += 1;
      if (JS_KEYWORDS.has(text)) emit(text, 'tok-kw');
      else if (JS_LITERALS.has(text)) emit(text, 'tok-lit');
      else if (content[j] === '(') emit(text, 'tok-fn');
      else emit(text, '');
      i += text.length;
      last = text;
      continue;
    }
    // 标点连跑合并成一个 token：压缩产物里 ));( 这种序列很多，逐字符 emit
    // 会把 1MB 文件的分词拖到几百毫秒。last 取段尾字符，正则/除号判断只用单字符。
    let j = i;
    while (j < content.length && !/[\w$\s"'`/]/.test(content[j])) j += 1;
    if (j === i) j = i + 1;
    emit(content.slice(i, j), '');
    i = j;
    last = content[j - 1];
  }
}

function scanJson(content: string, emit: (text: string, cls: string) => void) {
  let i = 0;
  while (i < content.length) {
    const ch = content[i];
    if (ch === '\n') { emit('\n', ''); i += 1; continue; }
    if (/\s/.test(ch)) { emit(ch, ''); i += 1; continue; }
    if (ch === '"') {
      const text = scanString(content, i, ch);
      let j = i + text.length;
      while (j < content.length && /[\s]/.test(content[j])) j += 1;
      // 后面跟冒号的是键名，和普通字符串值区分开（审计 app.json 时键值一眼可分）。
      emit(text, content[j] === ':' ? 'tok-attr' : 'tok-str');
      i += text.length;
      continue;
    }
    if (/[A-Za-z]/.test(ch)) {
      const text = scanIdentifier(content, i);
      emit(text, JS_LITERALS.has(text) ? 'tok-lit' : '');
      i += text.length;
      continue;
    }
    if (/-?\d/.test(ch)) {
      NUMBER_SOURCE.lastIndex = ch === '-' ? i + 1 : i;
      const match = NUMBER_SOURCE.exec(content);
      const text = ch === '-' ? `-${match ? match[0] : ''}` : match ? match[0] : ch;
      emit(text, 'tok-num');
      i += text.length;
      continue;
    }
    emit(ch, '');
    i += 1;
  }
}

const XML_NAME = /[\w.:-]+/y;

function scanXml(content: string, emit: (text: string, cls: string) => void) {
  let i = 0;
  let inTag = false;
  while (i < content.length) {
    if (!inTag) {
      const lt = content.indexOf('<', i);
      const brace = content.indexOf('{{', i);
      const stop = lt < 0 ? content.length : lt;
      // WXML 正文里的 {{ expr }} 是模板逻辑，单独着色；纯 HTML 没有它，不受影响。
      if (brace >= 0 && brace < stop) {
        if (brace > i) emit(content.slice(i, brace), '');
        const close = content.indexOf('}}', brace + 2);
        const end = close < 0 ? content.length : close + 2;
        emit(content.slice(brace, end), 'tok-expr');
        i = end;
        continue;
      }
      if (lt < 0) { emit(content.slice(i), ''); break; }
      if (lt > i) emit(content.slice(i, lt), '');
      if (content.startsWith('<!--', lt)) {
        let j = content.indexOf('-->', lt + 4);
        j = j < 0 ? content.length : j + 3;
        emit(content.slice(lt, j), 'tok-comment');
        i = j;
        continue;
      }
      if (content.startsWith('<!', lt) || content.startsWith('<?', lt)) {
        let j = content.indexOf('>', lt);
        j = j < 0 ? content.length : j + 1;
        emit(content.slice(lt, j), 'tok-comment');
        i = j;
        continue;
      }
      emit('<', 'tok-tag');
      i = lt + 1;
      if (content[i] === '/') { emit('/', 'tok-tag'); i += 1; }
      XML_NAME.lastIndex = i;
      const name = XML_NAME.exec(content);
      if (name) { emit(name[0], 'tok-tag'); i += name[0].length; }
      inTag = true;
      continue;
    }
    const ch = content[i];
    if (ch === '>') { emit('>', 'tok-tag'); inTag = false; i += 1; continue; }
    if (ch === '/' && content[i + 1] === '>') { emit('/>', 'tok-tag'); inTag = false; i += 2; continue; }
    if (/\s/.test(ch)) {
      let j = i;
      while (j < content.length && /\s/.test(content[j])) j += 1;
      emit(content.slice(i, j), '');
      i = j;
      continue;
    }
    if (ch === '=') { emit('=', ''); i += 1; continue; }
    if (ch === '"' || ch === "'") {
      const text = scanString(content, i, ch);
      emit(text, 'tok-str');
      i += text.length;
      continue;
    }
    let j = i;
    while (j < content.length && !/[\s=>"']/.test(content[j])) j += 1;
    if (j === i) { emit(ch, ''); i += 1; continue; }
    emit(content.slice(i, j), 'tok-attr');
    i = j;
  }
}

function scanCss(content: string, emit: (text: string, cls: string) => void) {
  let i = 0;
  let depth = 0;
  while (i < content.length) {
    const ch = content[i];
    if (ch === '\n') { emit('\n', ''); i += 1; continue; }
    if (/\s/.test(ch)) {
      let j = i;
      while (j < content.length && /\s/.test(content[j])) j += 1;
      emit(content.slice(i, j), '');
      i = j;
      continue;
    }
    if (ch === '/' && content[i + 1] === '*') {
      let j = content.indexOf('*/', i + 2);
      j = j < 0 ? content.length : j + 2;
      emit(content.slice(i, j), 'tok-comment');
      i = j;
      continue;
    }
    if (ch === '"' || ch === "'") {
      const text = scanString(content, i, ch);
      emit(text, 'tok-str');
      i += text.length;
      continue;
    }
    if (ch === '@') {
      let j = i + 1;
      while (j < content.length && /[\w-]/.test(content[j])) j += 1;
      emit(content.slice(i, j), 'tok-kw');
      i = j;
      continue;
    }
    if (ch === '#' && /[\da-fA-F]/.test(content[i + 1] ?? '') && /^#[\da-fA-F]{3,8}\b/.test(content.slice(i, i + 10))) {
      let j = i + 1;
      while (j < content.length && /[\da-fA-F]/.test(content[j])) j += 1;
      emit(content.slice(i, j), 'tok-num');
      i = j;
      continue;
    }
    if (/[A-Za-z_$-]/.test(ch)) {
      let j = i;
      while (j < content.length && /[\w$-]/.test(content[j])) j += 1;
      const text = content.slice(i, j);
      let k = j;
      while (k < content.length && /\s/.test(content[k])) k += 1;
      // 块内后跟冒号的是属性名，其余当选择器；反编译的 wxss 借此能分清结构与值。
      const isProperty = depth > 0 && content[k] === ':' && content[k + 1] !== ':';
      emit(text, text.startsWith('--') || isProperty ? 'tok-attr' : 'tok-tag');
      i = j;
      continue;
    }
    if (/\d/.test(ch) || (ch === '.' && /\d/.test(content[i + 1] ?? '')) || ch === '-') {
      let j = i + 1;
      while (j < content.length && /[\d.]/.test(content[j])) j += 1;
      while (j < content.length && /[a-zA-Z%]/.test(content[j])) j += 1;
      emit(content.slice(i, j), 'tok-num');
      i = j;
      continue;
    }
    if (ch === '{') depth += 1;
    if (ch === '}') depth = Math.max(0, depth - 1);
    emit(ch, '');
    i += 1;
  }
}

// 在某一行的着色 HTML 上把命中文本包进 <mark>。只能在「文本段」里替换：
// 直接对整段 HTML replaceAll 会把 class="tok-*" 这种属性文本也换掉。命中值
// 跨 token 边界（比如带引号的字符串只匹配到半截）时标记不上，就退回整行高亮。
export function markHitText(html: string, hitText: string): string {
  const needle = escapeHtml(hitText);
  if (!needle || !html.includes(needle)) return html;
  return html
    .split(/(<[^>]*>)/g)
    .map((part) => (part.startsWith('<') || !part.includes(needle) ? part : part.replaceAll(needle, `<mark class="code-hit-text">${needle}</mark>`)))
    .join('');
}
