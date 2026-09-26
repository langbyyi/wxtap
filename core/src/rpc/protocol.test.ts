import { describe, expect, it } from "vitest";

import { parseRequest, serialiseError, serialiseResult } from "./protocol.js";

describe("Core JSON-RPC protocol", () => {
  it("accepts a request with id, method and object params", () => {
    expect(parseRequest('{"id":"start-1","method":"engine.start","params":{"cdpPort":62000}}')).toEqual({
      id: "start-1",
      method: "engine.start",
      params: { cdpPort: 62000 },
    });
  });

  it("rejects a request without object params", () => {
    expect(() => parseRequest('{"id":"start-1","method":"engine.start","params":null}')).toThrow(
      "params must be an object",
    );
  });

  it("rejects a line that is not a JSON object", () => {
    expect(() => parseRequest("[]")).toThrow("request must be an object");
    expect(() => parseRequest('"engine.start"')).toThrow("request must be an object");
  });

  it("rejects requests with a missing or non-scalar id", () => {
    expect(() => parseRequest('{"method":"engine.start","params":{}}')).toThrow(
      "id must be a string or number",
    );
    expect(() => parseRequest('{"id":{"nested":1},"method":"engine.start","params":{}}')).toThrow(
      "id must be a string or number",
    );
  });

  it("rejects requests with a missing or empty method", () => {
    expect(() => parseRequest('{"id":1,"params":{}}')).toThrow("method must be a non-empty string");
    expect(() => parseRequest('{"id":1,"method":"","params":{}}')).toThrow(
      "method must be a non-empty string",
    );
  });
  it("serialises success and retryable error responses", () => {
    expect(serialiseResult("start-1", { frida: true })).toBe('{"id":"start-1","result":{"frida":true}}');
    expect(serialiseError("start-1", 1001, "target unavailable", true)).toBe(
      '{"id":"start-1","error":{"code":1001,"message":"target unavailable","retryable":true}}',
    );
  });
});
