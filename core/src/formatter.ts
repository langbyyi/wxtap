import beautify from "js-beautify";

// formatContent pretty-prints source: JSON is re-stringified
// with indent 2, scripts/styles/markup go through js-beautify, and anything
// else comes back untouched so the desktop shell can store it as-is.
export function formatContent(content: string, language: string): string {
  switch (language) {
    case "json":
      // Let JSON.parse throw: the caller surfaces the parse error.
      return JSON.stringify(JSON.parse(content), null, 2);
    case "javascript":
    case "typescript":
      return beautify.js(content, { indent_size: 2 });
    case "css":
      return beautify.css(content, { indent_size: 2 });
    case "html":
    case "xml":
      return beautify.html(content, { indent_size: 2 });
    default:
      return content;
  }
}
