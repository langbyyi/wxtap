import { readFileSync, readdirSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

import { MODERN_LAYOUT_BUILD } from "./windows-auto-detect.js";

// The release ships the Frida address tables the Core injects into hook.js.
// A table that parses but carries a different build number, or that lost the
// offsets hook.js reads, only shows up on a real device — so the shape is
// locked here, where a broken file fails the build instead.
const configRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..", "..", "..", "resources", "frida", "config");

const HEX_OFFSET = /^0x[0-9A-Fa-f]+$/;

type TableFile = { build: number; path: string };

type WindowsTable = {
  Version: number;
  LoadStartHookOffset: string;
  CDPFilterHookOffset: string;
  SceneOffsets?: number[];
  CastToJsonHookOffset?: string;
  MiniAppConfigStructOffsets?: {
    LaunchConfigOffsets: number[];
    RemoteDebugConfigOffsets: number[];
    SceneOffset: number;
    WebSocketURLStringOffset: number;
    RemoteDebugModeOffset: number;
  };
};

type DarwinArchEntry = {
  LoadStartHookOffset: string;
  CDPFilterHookOffset: string;
  SceneOffsets: number[];
};

type DarwinTable = {
  Version: number;
  Arch: Record<string, DarwinArchEntry>;
};

function tables(platform: string): TableFile[] {
  return readdirSync(join(configRoot, platform))
    .filter((name) => name.startsWith("addresses."))
    .map((name) => {
      const match = /^addresses\.(\d+)\.json$/.exec(name);
      if (!match) {
        throw new Error(`unexpected address table name: ${name}`);
      }
      return { build: Number(match[1]), path: join(configRoot, platform, name) };
    });
}

function readJson<T>(path: string): T {
  return JSON.parse(readFileSync(path, "utf8")) as T;
}

describe("shipped Frida address tables", () => {
  it("names every Windows table after the build it carries, in one of the two layouts", () => {
    const entries = tables("win");
    expect(entries.length).toBeGreaterThan(0);

    for (const { build, path } of entries) {
      const table = readJson<WindowsTable>(path);
      expect(table.Version, path).toBe(build);
      expect(table.LoadStartHookOffset, path).toMatch(HEX_OFFSET);
      expect(table.CDPFilterHookOffset, path).toMatch(HEX_OFFSET);
      // hook.js reads MiniAppConfigStructOffsets when it is there and ignores
      // SceneOffsets, so a table carrying both would silently drop one layout.
      expect(
        table.SceneOffsets !== undefined && table.MiniAppConfigStructOffsets !== undefined,
        `${path}: table mixes both struct layouts`,
      ).toBe(false);

      if (table.MiniAppConfigStructOffsets !== undefined) {
        // Newer builds (25710+): the launch config is described field by field
        // and CastToJson takes over the CDP filter.
        expect(table.CastToJsonHookOffset, path).toMatch(HEX_OFFSET);
        const struct = table.MiniAppConfigStructOffsets;
        expect(struct.LaunchConfigOffsets, path).toHaveLength(3);
        expect(struct.RemoteDebugConfigOffsets, path).toHaveLength(2);
        const fields = [
          ...struct.LaunchConfigOffsets,
          ...struct.RemoteDebugConfigOffsets,
          struct.SceneOffset,
          struct.WebSocketURLStringOffset,
          struct.RemoteDebugModeOffset,
        ];
        for (const offset of fields) {
          expect(Number.isInteger(offset), path).toBe(true);
        }
        continue;
      }

      expect(table.SceneOffsets, path).toHaveLength(6);
      for (const offset of table.SceneOffsets!) {
        expect(Number.isInteger(offset), path).toBe(true);
      }
    }
  });

  it("keeps the layout boundary consistent with the tables it ships", () => {
    // MODERN_LAYOUT_BUILD decides whether a table-less build may be handed to the
    // offset detector (which only recovers the older layout), so a shipped table
    // on the wrong side of it means the constant is wrong — and with it the
    // decision to auto-detect or to ask for a table.
    for (const { build, path } of tables("win")) {
      const table = readJson<WindowsTable>(path);
      expect(
        table.MiniAppConfigStructOffsets !== undefined,
        `${path}: build ${build} sits on the wrong side of MODERN_LAYOUT_BUILD`,
      ).toBe(build >= MODERN_LAYOUT_BUILD);
    }
  });

  it("names every macOS table after the build it carries and nests per arch", () => {
    const entries = tables("mac");
    expect(entries.length).toBeGreaterThan(0);

    for (const { build, path } of entries) {
      const table = readJson<DarwinTable>(path);
      expect(table.Version, path).toBe(build);

      const arches = Object.keys(table.Arch ?? {});
      expect(arches.length, path).toBeGreaterThan(0);
      for (const arch of arches) {
        expect(["arm64", "x64"], path).toContain(arch);
        const entry = table.Arch[arch];
        expect(entry.LoadStartHookOffset, path).toMatch(HEX_OFFSET);
        expect(entry.CDPFilterHookOffset, path).toMatch(HEX_OFFSET);
        // hook.js walks the same six-element scene chain on both platforms.
        expect(entry.SceneOffsets, path).toHaveLength(6);
        for (const offset of entry.SceneOffsets) {
          expect(Number.isInteger(offset), path).toBe(true);
        }
      }
    }
  });

  it("keeps the placeholder hook.js reads its config from", () => {
    const hook = readFileSync(resolve(configRoot, "..", "hook.js"), "utf8");

    expect(hook).toContain("@@CONFIG@@");
  });

  it("keeps hook.js's hand-run fallback in step with the tables it mirrors", () => {
    // When @@CONFIG@@ was never substituted — hook.js run by hand against a
    // real client — the file falls back to a hardcoded table per platform. It
    // mirrors one shipped build each (darwin 269136 arm64, win 18955), so a
    // re-measured table would leave the fallback hooking stale offsets, and
    // that only shows up on a device. Pin it to the tables it claims to mirror.
    const hook = readFileSync(resolve(configRoot, "..", "hook.js"), "utf8");
    const fallback = hook.slice(
      hook.indexOf("// Fallback test config"),
      hook.indexOf("const resolveArchConfig"),
    );
    const branches = fallback.split("return {");
    expect(branches, "hook.js's fallback should return one table per platform").toHaveLength(3);
    const darwinBranch = branches[1];
    const windowsBranch = branches[2];

    const darwinTable = readJson<DarwinTable>(join(configRoot, "mac", "addresses.269136.json"));
    const darwin = darwinTable.Arch.arm64;
    const windowsTable = readJson<WindowsTable>(join(configRoot, "win", "addresses.18955.json"));
    const windowsScenes = windowsTable.SceneOffsets;
    if (darwin === undefined || windowsScenes === undefined) {
      throw new Error("hook.js's fallback mirrors a scene-offset table per platform; one is missing");
    }

    expect(darwinBranch).toContain(`Version: ${darwinTable.Version}`);
    expect(darwinBranch).toContain(`LoadStartHookOffset: "${darwin.LoadStartHookOffset}"`);
    expect(darwinBranch).toContain(`CDPFilterHookOffset: "${darwin.CDPFilterHookOffset}"`);
    expect(darwinBranch).toContain(`SceneOffsets: [${darwin.SceneOffsets.join(", ")}]`);

    expect(windowsBranch).toContain(`Version: ${windowsTable.Version}`);
    expect(windowsBranch).toContain(`LoadStartHookOffset: "${windowsTable.LoadStartHookOffset}"`);
    expect(windowsBranch).toContain(`CDPFilterHookOffset: "${windowsTable.CDPFilterHookOffset}"`);
    expect(windowsBranch).toContain(`SceneOffsets: [${windowsScenes.join(", ")}]`);
  });
});