import { PassThrough } from "node:stream";
import { describe, expect, it } from "vitest";

import type { RuntimeAdapter } from "./engine/engine.js";
import { runCore } from "./main.js";

describe("runCore", () => {
  it("serves engine status over standard input and output", async () => {
    const input = new PassThrough();
    const output = new PassThrough();
    let written = "";
    output.setEncoding("utf8");
    output.on("data", (chunk: string) => {
      written += chunk;
    });
    const runtime: RuntimeAdapter = {
      start: async () => ({ frida: false, miniapp: false, devtools: false }),
      stop: async () => {},
      status: () => ({ frida: true, miniapp: false, devtools: true }),
    };

    const running = runCore(input, output, runtime);
    input.end('{"id":"status-1","method":"engine.status","params":{}}\n');

    await running;
    expect(written).toBe('{"id":"status-1","result":{"frida":true,"miniapp":false,"devtools":true}}\n');
  });
});
