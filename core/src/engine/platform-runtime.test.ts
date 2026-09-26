import { resolve } from "node:path";

import { describe, expect, it } from "vitest";

import {
  createRuntimeForPlatform,
  resolveHooksDir,
  resolveResourceRoot,
} from "./platform-runtime.js";
import { createMacOSFridaRuntime, createWindowsFridaRuntime } from "./wmpf-frida-runtime.js";

describe("createRuntimeForPlatform", () => {
  it("selects the macOS runtime only for darwin", async () => {
    expect(createRuntimeForPlatform("darwin", "/res")).toBeInstanceOf(
      createMacOSFridaRuntime("/res").constructor,
    );
    expect(createRuntimeForPlatform("win32", "/res")).toBeInstanceOf(
      createWindowsFridaRuntime("/res").constructor,
    );
    const linuxRuntime = createRuntimeForPlatform("linux", "/res");
    expect(linuxRuntime.status()).toEqual({ frida: false, miniapp: false, devtools: false });
    await expect(linuxRuntime.start(62000)).rejects.toThrow("WxTap does not support the linux desktop runtime");
  });
});

describe("resolveResourceRoot / resolveHooksDir", () => {
  it("prefers WXTAP_RESOURCE_ROOT and falls back to the sibling resources dir", () => {
    expect(resolveResourceRoot("/opt/wxtap/core/dist", { WXTAP_RESOURCE_ROOT: "/custom/res" })).toBe(
      "/custom/res",
    );
    expect(resolveResourceRoot("/opt/wxtap/core/dist", {})).toBe(resolve("/opt/wxtap/core/dist", "..", "..", "resources"));
  });

  it("resolves the bundled hooks directory next to dist", () => {
    expect(resolveHooksDir("/opt/wxtap/core/dist")).toBe(resolve("/opt/wxtap/core/dist", "..", "hooks"));
  });
});