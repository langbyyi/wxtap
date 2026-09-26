import type { Readable, Writable } from "node:stream";

import { CoreApp, type HookScriptLoader } from "./app.js";
import { Engine, type RuntimeAdapter } from "./engine/engine.js";
import { serve } from "./rpc/stdio.js";

export function runCore(
  input: Readable,
  output: Writable,
  runtime: RuntimeAdapter,
  loadHookScript?: HookScriptLoader,
): Promise<void> {
  // 控制台事件是服务端推送：stdout 上写 {event, payload} 通知行（Go 侧读循环
  // 已支持该信封）。写成一行是协议要求：读端按行切分。
  const consoleSink = (entry: Record<string, unknown>) => {
    output.write(`${JSON.stringify({ event: "console", payload: entry })}\n`);
  };
  const app = new CoreApp(new Engine(runtime), undefined, loadHookScript, consoleSink);
  return serve(input, output, (request) => app.handle(request));
}
