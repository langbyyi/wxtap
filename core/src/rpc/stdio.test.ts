import { PassThrough } from "node:stream";
import { describe, expect, it, vi } from "vitest";

import { RpcFailure, serve } from "./stdio.js";

describe("stdio JSON-RPC server", () => {
  it("dispatches every newline-delimited request and writes correlated results", async () => {
    const input = new PassThrough();
    const output = new PassThrough();
    let written = "";
    output.setEncoding("utf8");
    output.on("data", (chunk: string) => {
      written += chunk;
    });

    const served = serve(input, output, async (request) => ({ method: request.method }));
    input.end('{"id":1,"method":"engine.status","params":{}}\n{"id":"2","method":"engine.stop","params":{}}\n');

    await served;
    expect(written).toBe('{"id":1,"result":{"method":"engine.status"}}\n{"id":"2","result":{"method":"engine.stop"}}\n');
  });

  it("returns a structured error and continues with the next request", async () => {
    const input = new PassThrough();
    const output = new PassThrough();
    let written = "";
    output.setEncoding("utf8");
    output.on("data", (chunk: string) => {
      written += chunk;
    });
    const served = serve(input, output, async (request) => {
      if (request.method === "engine.start") {
        throw new RpcFailure(1001, "target unavailable", true);
      }
      return { frida: false, miniapp: false, devtools: false };
    });
    input.end('{"id":1,"method":"engine.start","params":{"cdpPort":62000}}\n{"id":2,"method":"engine.status","params":{}}\n');

    await served;
    expect(written).toBe(
      '{"id":1,"error":{"code":1001,"message":"target unavailable","retryable":true}}\n{"id":2,"result":{"frida":false,"miniapp":false,"devtools":false}}\n',
    );
  });

  it("answers a malformed line with a structured error and keeps serving", async () => {
    const input = new PassThrough();
    const output = new PassThrough();
    let written = "";
    output.setEncoding("utf8");
    output.on("data", (chunk: string) => {
      written += chunk;
    });

    const served = serve(input, output, async (request) => ({ method: request.method }));
    input.end("not-json\n{\"id\":1,\"method\":\"engine.status\",\"params\":{}}\n");

    await served;
    expect(written).toBe(
      '{"id":null,"error":{"code":1000,"message":"invalid request: Unexpected token \'o\', \\"not-json\\" is not valid JSON","retryable":false}}\n{"id":1,"result":{"method":"engine.status"}}\n',
    );
  });

  it("decodes UTF-8 split across chunk boundaries", async () => {
    // objectMode keeps each write a separate chunk; the default PassThrough
    // would coalesce them back into one and mask the boundary.
    const input = new PassThrough({ objectMode: true });
    const output = new PassThrough();
    let written = "";
    output.setEncoding("utf8");
    output.on("data", (chunk: string) => {
      written += chunk;
    });

    const served = serve(input, output, async (request) => ({ echo: request.params.content }));
    const line = Buffer.from(`${JSON.stringify({ id: 1, method: "code.format", params: { content: "小程序代码" } })}\n`);
    const split = line.indexOf(0xe4) + 1; // cut right after the first byte of 代

    input.write(line.subarray(0, split));
    input.write(line.subarray(split));
    input.end();

    await served;
    expect(JSON.parse(written)).toEqual({ id: 1, result: { echo: "小程序代码" } });
  });
});

describe("stdio EPIPE resilience", () => {
  it("exits cleanly instead of crashing when the output pipe breaks", async () => {
    const { EventEmitter } = await import("node:events");
    const exit = vi.spyOn(process, "exit").mockImplementation(((code?: number) => {
      throw new Error(`exit(${code})`);
    }) as never);
    const input = new PassThrough();
    // A Writable that errors like a closed pipe when written to.
    const broken = new (class extends EventEmitter {
      write() {
        this.emit("error", Object.assign(new Error("write EPIPE"), { code: "EPIPE" }));
        return false;
      }
    })() as unknown as import("node:stream").Writable;

    const served = serve(input, broken, async () => ({}));
    input.end('{"id":1,"method":"engine.status","params":{}}\n');
    await expect(served).rejects.toThrow("exit(0)");
    expect(exit).toHaveBeenCalledWith(0);
    exit.mockRestore();
  });
});
