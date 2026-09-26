// Core 是英文诊断面：MCP 工具与 shell 日志读的都是它那份原文，但界面把它原样端给用户就成了
// 「core error 1000: no miniapp connected」—— 一条能查的错误、一条看不懂的提示。
//
// 只翻**已知且用户可操作**的这几种，其余一律原样透出：编不出来的中文比英文更糟，用户照着
// 错误码能搜到东西，照着含糊的译文搜不到。替换的是片段而非整句，所以 devtools 那种把 Core
// 错误拼进自有文案的（「读取调试目标失败：core error 1000: …」）也能翻到，前缀照旧保留。
//
// 这张表同时是判定依据（见 isNoMiniappError）：文案与判定各写一份 substring 就会漂。
const CORE_ERROR_TEXT: Array<{ match: RegExp; text: string }> = [
  // 小程序没连上：钩子无处可装，也不是「引擎坏了」。把前面的 core error NNNN 一起吃掉 ——
  // 译文后面跟一个孤零零的错误码，读起来像还有别的原因。
  { match: /(?:core error \d+: )?no miniapp connected/gi, text: '未连接小程序（请在微信里打开目标小程序后重试）' },
  { match: /core error \d+: core call timed out: (\S+)/g, text: '调试引擎调用超时（$1）：请重试，或先重新启动引擎' },
  { match: /core error \d+: core process has exited/g, text: '调试引擎已退出：请重新启动引擎' },
  { match: /core error \d+: core connection (?:closed|lost[^\n]*)/g, text: '调试引擎连接已断开：请重新启动引擎' },
];

/** 失败值里是否带着「没有连接小程序」这一条：判定与译文共用 CORE_ERROR_TEXT。 */
export function isNoMiniappError(value: unknown): boolean {
  return /no miniapp connected/i.test(rawMessageOf(value));
}

function rawMessageOf(value: unknown): string {
  return value instanceof Error ? value.message : String(value);
}

// 展示给用户的失败文案：已知的 Core 失败翻成中文，其余原样（含未知的 Core 错误码，它得留着
// 才能查）。凡是把后端失败显示出来的地方都该走这里，别再各自渲染原始字符串。
export function messageOf(value: unknown): string {
  const raw = rawMessageOf(value);
  let text = raw;
  for (const entry of CORE_ERROR_TEXT) {
    text = text.replace(entry.match, entry.text);
  }
  return text;
}

export function prettyJson(value: unknown): string {
  return JSON.stringify(value ?? {}, null, 2);
}

// 详情正文展示上限（字符数）：与历史页 TrafficView 的正文展示上限（BODY_DISPLAY_LIMIT）同口径。
// 页内钩子记录的返回体可能有几 MB，全量 stringify 再塞进 DOM 会把页面卡住数秒，超限只留前缀
// 并明说截断（复制走全量，不受这里影响）。
export const DETAIL_TEXT_LIMIT = 2_000_000;

/** prettyJson 的有界版：超过 limit 只保留前缀，并给出 shown / total 供界面明示截断。 */
export function boundedPrettyJson(value: unknown, limit = DETAIL_TEXT_LIMIT): { text: string; truncated?: { shown: number; total: number } } {
  const full = JSON.stringify(value ?? {}, null, 2);
  if (full.length <= limit) return { text: full };
  return { text: full.slice(0, limit), truncated: { shown: limit, total: full.length } };
}
