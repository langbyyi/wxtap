import { resolve } from "node:path";

import type { EngineStatus, RuntimeAdapter } from "./engine.js";
import { createMacOSFridaRuntime, createWindowsFridaRuntime } from "./wmpf-frida-runtime.js";

/**
 * Picks the Frida runtime for the host platform. macOS needs its own host
 * process lookup and address-table directory; every other platform uses the
 * Windows-shaped layout (the only other supported target is Windows).
 */
class UnsupportedRuntime implements RuntimeAdapter {
  constructor(private readonly platform: string) {}

  async start(): Promise<EngineStatus> {
    throw new Error(`WxTap does not support the ${this.platform} desktop runtime`);
  }

  async stop(): Promise<void> {}

  status(): EngineStatus {
    return { frida: false, miniapp: false, devtools: false };
  }
}

export function createRuntimeForPlatform(platform: string, resourceRoot: string): RuntimeAdapter {
  switch (platform) {
    case "darwin":
      return createMacOSFridaRuntime(resourceRoot);
    case "win32":
      return createWindowsFridaRuntime(resourceRoot);
    default:
      return new UnsupportedRuntime(platform);
  }
}

/**
 * Resource root: WXTAP_RESOURCE_ROOT wins (release layouts set it), otherwise
 * the repo/release default `<moduleDir>/../../resources`.
 */
export function resolveResourceRoot(moduleDir: string, env: NodeJS.ProcessEnv = process.env): string {
  return env.WXTAP_RESOURCE_ROOT ?? resolve(moduleDir, "..", "..", "resources");
}

/** Hook sources ship next to the bundled Core: `<moduleDir>/../hooks`. */
export function resolveHooksDir(moduleDir: string): string {
  return resolve(moduleDir, "..", "hooks");
}