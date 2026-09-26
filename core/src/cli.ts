import { readFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import { createRuntimeForPlatform, resolveHooksDir, resolveResourceRoot } from "./engine/platform-runtime.js";
import { hookScriptFileName } from "./hook-scripts.js";
import { runCore } from "./main.js";

const moduleDir = dirname(fileURLToPath(import.meta.url));
// resources/ holds frida/, skills/, … (repo root and release layout).
const resourceRoot = resolveResourceRoot(moduleDir);
const hooksDir = resolveHooksDir(moduleDir);

const loadHookScript = (name: string) => readFile(join(hooksDir, hookScriptFileName(name)), "utf8");

// The Frida attach flow is shared; only the host-process lookup and the
// address-table directory differ between WeChat builds.
runCore(process.stdin, process.stdout, createRuntimeForPlatform(process.platform, resourceRoot), loadHookScript).catch((error: unknown) => {
  const message = error instanceof Error ? error.message : "core terminated unexpectedly";
  process.stderr.write(`${message}\n`);
  process.exitCode = 1;
});
