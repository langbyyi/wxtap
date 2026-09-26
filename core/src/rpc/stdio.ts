import type { Writable } from "node:stream";

import { parseRequest, serialiseError, serialiseResult, type RpcRequest } from "./protocol.js";

export type RpcHandler = (request: RpcRequest) => Promise<unknown>;

export class RpcFailure extends Error {
  constructor(
    readonly code: number,
    message: string,
    readonly retryable: boolean,
  ) {
    super(message);
  }
}

export async function serve(
  input: AsyncIterable<Buffer | string>,
  output: Writable,
  handle: RpcHandler,
): Promise<void> {
  // A broken output pipe (the host died or closed its read end) must end
  // this process gracefully; an unhandled 'error' event would crash it.
  output.on("error", () => process.exit(0));

  // Accumulate raw bytes and split on 0x0A so a multibyte UTF-8 sequence
  // crossing a chunk boundary is only decoded once the line is complete.
  let pending = Buffer.alloc(0);

  for await (const chunk of input) {
    pending = Buffer.concat([pending, Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk)]);
    let index: number;
    while ((index = pending.indexOf(0x0a)) !== -1) {
      const line = pending.subarray(0, index).toString("utf8");
      pending = pending.subarray(index + 1);
      if (line.length > 0) {
        await handleLine(line, output, handle);
      }
    }
  }

  if (pending.length > 0) {
    await handleLine(pending.toString("utf8"), output, handle);
  }
}

async function handleLine(line: string, output: Writable, handle: RpcHandler): Promise<void> {
  let request: RpcRequest;
  try {
    request = parseRequest(line);
  } catch (error) {
    const message = error instanceof Error ? error.message : "invalid request";
    output.write(`${serialiseError(null, 1000, `invalid request: ${message}`, false)}\n`);
    return;
  }
  try {
    const result = await handle(request);
    output.write(`${serialiseResult(request.id, result)}\n`);
  } catch (error) {
    const failure = error instanceof RpcFailure
      ? error
      : new RpcFailure(1000, error instanceof Error ? error.message : "internal error", false);
    output.write(`${serialiseError(request.id, failure.code, failure.message, failure.retryable)}\n`);
  }
}
