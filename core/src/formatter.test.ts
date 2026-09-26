import { describe, expect, it } from "vitest";

import { formatContent } from "./formatter.js";

describe("formatContent", () => {
  it("formats minified JavaScript with 2-space indentation", () => {
    const formatted = formatContent("function a(){return {x:1}}", "javascript");
    expect(formatted).toContain("\n");
    expect(formatted).toContain("function a()");
  });

  it("formats JSON with 2-space indentation", () => {
    const formatted = formatContent('{"a":1,"b":[2,3]}', "json");
    expect(formatted).toBe('{\n  "a": 1,\n  "b": [\n    2,\n    3\n  ]\n}');
  });

  it("throws on malformed JSON so the caller can report the error", () => {
    expect(() => formatContent("{not json", "json")).toThrow();
  });

  it("formats CSS with one rule per line", () => {
    const formatted = formatContent("body{color:red;margin:0}", "css");
    expect(formatted).toContain("\n");
    expect(formatted).toContain("color: red");
  });

  it("formats HTML by adding tag breaks", () => {
    const formatted = formatContent("<div><p>hi</p></div>", "html");
    expect(formatted).toContain("\n");
  });

  it("formats the xml language the same way as html", () => {
    const formatted = formatContent("<div><p>hi</p></div>", "xml");
    expect(formatted).toContain("\n");
  });

  it("treats typescript like javascript", () => {
    const formatted = formatContent("const a: number = 1;", "typescript");
    expect(formatted).toContain("const a: number = 1;");
  });

  it("returns unknown languages untouched", () => {
    const content = "plain text\nno change";
    expect(formatContent(content, "text")).toBe(content);
  });
});
