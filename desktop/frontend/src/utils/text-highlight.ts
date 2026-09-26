// 关键词命中分段：把文本按命中位置切成 [未命中, 命中] 交替段，供模板渲染 <mark>。
// 与后端 query 过滤同口径：大小写不敏感；term 为空时整段未命中。
export interface HighlightSegment {
  text: string;
  hit: boolean;
}

export function splitHighlight(text: string, term: string): HighlightSegment[] {
  const needle = term.trim().toLowerCase();
  if (!needle) return [{ text, hit: false }];
  const haystack = text.toLowerCase();
  const segments: HighlightSegment[] = [];
  let cursor = 0;
  for (let index = haystack.indexOf(needle); index !== -1; index = haystack.indexOf(needle, cursor)) {
    if (index > cursor) segments.push({ text: text.slice(cursor, index), hit: false });
    segments.push({ text: text.slice(index, index + needle.length), hit: true });
    cursor = index + needle.length;
  }
  if (cursor < text.length) segments.push({ text: text.slice(cursor), hit: false });
  return segments.length ? segments : [{ text, hit: false }];
}
