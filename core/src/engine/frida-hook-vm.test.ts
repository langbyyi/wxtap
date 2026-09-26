import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import vm from "node:vm";

import { describe, expect, it } from "vitest";

// hook.js runs inside Frida against the real WeChat client, where a wrong
// module name, arch entry or offset only shows up as a failed attach on a real
// device. The script is a plain Frida agent, so it is driven here in a VM with
// pointer/Interceptor stubs that record what the agent would patch.
const resourcesRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..", "..", "..", "resources");
const hookSource = readFileSync(resolve(resourcesRoot, "frida", "hook.js"), "utf8");

type WinTable = {
  Version: number;
  LoadStartHookOffset: string;
  CDPFilterHookOffset: string;
  SceneOffsets: number[];
};

/** Build 25710+ tables describe the launch config field by field. */
type ModernWinTable = {
  Version: number;
  LoadStartHookOffset: string;
  CDPFilterHookOffset: string;
  CastToJsonHookOffset: string;
  MiniAppConfigStructOffsets: {
    LaunchConfigOffsets: number[];
    RemoteDebugConfigOffsets: number[];
    SceneOffset: number;
    WebSocketURLStringOffset: number;
    RemoteDebugModeOffset: number;
  };
};

type MacArchEntry = {
  LoadStartHookOffset: string;
  CDPFilterHookOffset: string;
  SceneOffsets: number[];
};

type MacTable = {
  Version: number;
  Arch: Record<string, MacArchEntry>;
};

type Table = WinTable | ModernWinTable | MacTable;

function table(platform: "win" | "mac", build: number): Table {
  const path = resolve(resourcesRoot, "frida", "config", platform, `addresses.${build}.json`);
  return JSON.parse(readFileSync(path, "utf8")) as Table;
}

function parseOffset(offset: number | string): number {
  return typeof offset === "string" ? Number.parseInt(offset, 16) : offset;
}

class FakePointer {
  constructor(readonly address: number) {}

  // Frida's NativePointer.add accepts the "0x…" strings the address tables
  // carry, so the stub has to parse them the same way.
  add(offset: number | string): FakePointer {
    return new FakePointer(this.address + parseOffset(offset));
  }

  sub(other: FakePointer): FakePointer {
    return new FakePointer(this.address - other.address);
  }

  and(value: number): FakePointer {
    return new FakePointer(this.address & value);
  }

  or(value: number): FakePointer {
    return new FakePointer(this.address | value);
  }

  isNull(): boolean {
    return this.address < 0;
  }

  readPointer(): FakePointer {
    return new FakePointer(this.address + 0x100);
  }

  readU32(): number {
    return currentRun?.filterValue ?? 0;
  }

  readInt(): number {
    return currentRun?.scene ?? 0;
  }

  /** Frida's toInt32 sign-extends the low word; these addresses are small. */
  toInt32(): number {
    return this.address | 0;
  }

  readS8(): number {
    return currentRun?.stringMarker ?? 0;
  }

  writeU32(value: number): void {
    currentRun?.writes.push(value);
  }

  writeInt(value: number): void {
    currentRun?.writes.push(value);
  }

  writeU8(value: number): void {
    currentRun?.sizes.push(value);
  }

  writeU64(value: number): void {
    currentRun?.sizes.push(value);
  }

  writeUtf8String(value: string): void {
    currentRun?.strings.push(value);
  }
}

type HookHandlers = {
  onEnter?: (args: FakePointer[]) => void;
  onLeave?: (...args: unknown[]) => void;
};

type HookRun = {
  platform: string;
  arch: string;
  baseAddress: number;
  moduleName: string | null;
  scene: number;
  filterValue: number;
  stringMarker: number;
  moduleQueries: string[];
  attached: { offset: number; handlers: HookHandlers }[];
  replaced: { offset: number; replacement: unknown }[];
  nativeFunctions: { offset: number; retType: string; argTypes: string[] }[];
  nativeCallbacks: { retType: string; argTypes: string[] }[];
  nativeCalls: unknown[][];
  args: FakePointer[];
  logs: string[];
  errors: string[];
  sends: string[];
  writes: number[];
  sizes: number[];
  strings: string[];
};

let currentRun: HookRun | undefined;

type RunOptions = {
  platform: string;
  arch: string;
  config?: Table;
  rawSource?: string;
  moduleName?: string | null;
  baseAddress?: number;
  scene?: number;
  filterValue?: number;
  /** Incoming second argument of OnLoadStart: the debug flag word. */
  debugFlag?: number;
  /** Byte 23 of the launch config's websocket URL string: negative selects the
   * heap-backed string representation instead of the inline one. */
  stringMarker?: number;
};

function runHook(options: RunOptions): HookRun {
  const run: HookRun = {
    platform: options.platform,
    arch: options.arch,
    baseAddress: options.baseAddress ?? 0x10000000,
    moduleName: options.moduleName === undefined ? null : options.moduleName,
    scene: options.scene ?? 0,
    filterValue: options.filterValue ?? 0,
    stringMarker: options.stringMarker ?? 0,
    moduleQueries: [],
    attached: [],
    replaced: [],
    nativeFunctions: [],
    nativeCallbacks: [],
    nativeCalls: [],
    args: [new FakePointer(0x2000), new FakePointer(options.debugFlag ?? 0)],
    logs: [],
    errors: [],
    sends: [],
    writes: [],
    sizes: [],
    strings: [],
  };
  currentRun = run;
  run.moduleName = options.moduleName === undefined
    ? options.platform === "darwin" ? "WeChatAppEx Framework" : "WeChatAppEx.exe"
    : options.moduleName;

  const source = options.rawSource ?? (options.config === undefined
    ? hookSource
    : hookSource.replace("@@CONFIG@@", JSON.stringify(options.config)));

  const base = new FakePointer(run.baseAddress);
  const sandbox = {
    Process: {
      platform: options.platform,
      arch: options.arch,
      findModuleByName: (name: string) => {
        run.moduleQueries.push(name);
        return run.moduleName === name ? { base } : null;
      },
    },
    Interceptor: {
      attach: (address: unknown, handlers: HookHandlers) => {
        const pointer = address as FakePointer;
        run.attached.push({ offset: pointer.address - run.baseAddress, handlers });
      },
      replace: (address: unknown, replacement: unknown) => {
        const pointer = address as FakePointer;
        run.replaced.push({ offset: pointer.address - run.baseAddress, replacement });
      },
    },
    // NativeFunction returns a callable that records what the agent called it
    // with; NativeCallback hands the JS implementation back so a test can drive
    // the shim the way Frida would. Both are declared as functions because the
    // agent uses them with `new`.
    NativeFunction: function (address: unknown, retType: string, argTypes: string[]) {
      const pointer = address as FakePointer;
      run.nativeFunctions.push({ offset: pointer.address - run.baseAddress, retType, argTypes });
      return (...args: unknown[]) => {
        run.nativeCalls.push(args);
        return args[0];
      };
    },
    NativeCallback: function (impl: unknown, retType: string, argTypes: string[]) {
      run.nativeCallbacks.push({ retType, argTypes });
      return impl;
    },
    console: {
      log: (message: string) => run.logs.push(String(message)),
      error: (message: string) => run.errors.push(String(message)),
    },
    send: (message: string) => run.sends.push(String(message)),
  };

  vm.runInNewContext(source, sandbox, { filename: "hook.js" });
  return run;
}

/** Drives the OnLoadStart hook. main() patches the CDP filter first — by
 * attaching or, on the newer layout, by replacing it — so OnLoadStart is always
 * the last thing attached. */
function enterLoadStart(run: HookRun): void {
  run.attached.at(-1)?.handlers.onEnter?.(run.args);
}

describe("Frida hook agent", () => {
  it("picks the WeChat module from the injected table on Windows", () => {
    const oldTable = table("win", 11581) as WinTable;
    const old = runHook({ platform: "windows", arch: "x64", config: oldTable });
    expect(old.moduleQueries).toEqual(["WeChatAppEx.exe"]);
    expect(old.attached.map((hook) => hook.offset)).toEqual([
      Number(oldTable.CDPFilterHookOffset),
      Number(oldTable.LoadStartHookOffset),
    ]);

    const recent = runHook({ platform: "windows", arch: "x64", config: table("win", 19823) });
    expect(recent.moduleQueries).toEqual(["flue.dll"]);
  });

  it("resolves the macOS table per arch and attaches the same two hooks", () => {
    const mac = table("mac", 269136) as MacTable;
    const arm = runHook({ platform: "darwin", arch: "arm64", config: mac });
    expect(arm.moduleQueries).toEqual(["WeChatAppEx Framework"]);
    // 自述日志必须走 stderr：agent 的 console.log 落到宿主 stdout，而 stdout 是
    // Core 的 JSON-RPC 通道，Go 读循环会把非 JSON 行丢掉，界面上就看不到了。
    expect(arm.errors.join("\n")).toContain("arch=arm64");
    expect(arm.attached.map((hook) => hook.offset)).toEqual([
      Number(mac.Arch.arm64.CDPFilterHookOffset),
      Number(mac.Arch.arm64.LoadStartHookOffset),
    ]);

    // The table only carries the arch it was recovered for: an Intel mac must
    // stay dormant rather than hook the arm64 offsets. Frida reports x86_64.
    const x64 = runHook({ platform: "darwin", arch: "x86_64", config: mac });
    expect(x64.errors.join("\n")).toContain("no config");
    expect(x64.attached).toHaveLength(0);
  });

  it("forces the debug flag through args[1] on both darwin ABIs", () => {
    const mac: MacTable = {
      Version: 269136,
      Arch: {
        arm64: { LoadStartHookOffset: "0x4F744C4", CDPFilterHookOffset: "0x8436B98", SceneOffsets: [56, 1504, 8, 1440, 16, 456] },
        x64: { LoadStartHookOffset: "0x5994BA0", CDPFilterHookOffset: "0x91F1C20", SceneOffsets: [56, 1504, 8, 1440, 16, 456] },
      },
    };

    for (const arch of ["arm64", "x86_64"]) {
      const run = runHook({ platform: "darwin", arch, config: mac, debugFlag: 0x1200 });
      enterLoadStart(run);
      expect(run.args[1].address, arch).toBe(0x1201);
    }

    // An already-set flag is left alone, so repeated loads are not rewritten.
    const set = runHook({ platform: "darwin", arch: "arm64", config: mac, debugFlag: 0x1201 });
    enterLoadStart(set);
    expect(set.args[1].address).toBe(0x1201);
  });

  it("stays dormant instead of patching when the arch has no table entry", () => {
    const run = runHook({ platform: "darwin", arch: "ia32", config: table("mac", 269136) });

    expect(run.errors.join("\n")).toContain("no config");
    expect(run.attached).toHaveLength(0);
  });

  it("reports a missing WeChat module instead of throwing", () => {
    const run = runHook({ platform: "windows", arch: "x64", config: table("win", 19823), moduleName: null });

    expect(run.moduleQueries).toEqual(["flue.dll"]);
    expect(run.errors.join("\n")).toContain("module not found");
    expect(run.attached).toHaveLength(0);
  });

  it("patches the CDP filter through each platform's pointer shape", () => {
    // macOS hands the struct back as the return value itself.
    const mac = runHook({ platform: "darwin", arch: "arm64", config: table("mac", 269136), filterValue: 6 });
    mac.attached[0].handlers.onLeave?.call({}, new FakePointer(0x3000));
    expect(mac.writes).toEqual([0]);

    // Windows returns a pointer that has to be dereferenced first.
    const win = runHook({ platform: "windows", arch: "x64", config: table("win", 19823), moduleName: "flue.dll", filterValue: 6 });
    const input = new FakePointer(0x4000);
    win.attached[0].handlers.onEnter?.([input]);
    win.attached[0].handlers.onLeave?.call({ inputValue: input }, new FakePointer(0x5000));
    expect(win.writes).toEqual([0]);

    // Any other marker means this candidate is the wrong function: leave it be.
    const other = runHook({ platform: "windows", arch: "x64", config: table("win", 19823), moduleName: "flue.dll", filterValue: 7 });
    const otherInput = new FakePointer(0x4000);
    other.attached[0].handlers.onEnter?.([otherInput]);
    other.attached[0].handlers.onLeave?.call({ inputValue: otherInput }, new FakePointer(0x5000));
    expect(other.writes).toEqual([]);
  });

  it("rewrites only whitelisted scenes", () => {
    const win = table("win", 19823) as WinTable;
    const hit = runHook({ platform: "windows", arch: "x64", config: win, scene: 1005, moduleName: "flue.dll" });
    enterLoadStart(hit);

    expect(hit.args[1].address).toBe(1);
    expect(hit.writes).toEqual([1101]);
    expect(hit.sends.join("\n")).toContain("hook scene -> 1101");

    // The scene chain is shared with macOS, so the darwin entry must reach it.
    const mac = runHook({ platform: "darwin", arch: "arm64", config: table("mac", 269136), scene: 1005 });
    enterLoadStart(mac);
    expect(mac.writes).toEqual([1101]);

    const miss = runHook({ platform: "windows", arch: "x64", config: win, scene: 999, moduleName: "flue.dll" });
    enterLoadStart(miss);
    expect(miss.writes).toEqual([]);
  });

  it("replaces the CDP filter through CastToJson on the newer layout", () => {
    const modern = table("win", 25715) as ModernWinTable;
    const run = runHook({ platform: "windows", arch: "x64", config: modern, moduleName: "flue.dll" });

    // This layout replaces the filter instead of watching it, so only the
    // OnLoadStart hook remains as an attach and the === 6 guard does not apply.
    expect(run.attached.map((hook) => hook.offset)).toEqual([Number(modern.LoadStartHookOffset)]);
    expect(run.replaced.map((entry) => entry.offset)).toEqual([Number(modern.CDPFilterHookOffset)]);
    expect(run.nativeFunctions).toEqual([{
      offset: Number(modern.CastToJsonHookOffset),
      retType: "pointer",
      argTypes: ["pointer", "pointer"],
    }]);
    expect(run.nativeCallbacks).toEqual([{ retType: "pointer", argTypes: ["pointer", "pointer", "pointer"] }]);

    // The shim must call through to CastToJson and hand back its output pointer.
    const shim = run.replaced[0].replacement as (thiz: unknown, jsonOut: unknown, cborInput: unknown) => unknown;
    const jsonOut = new FakePointer(0x9000);
    const cborInput = new FakePointer(0x9100);
    expect(shim({}, jsonOut, cborInput)).toBe(jsonOut);
    expect(run.nativeCalls).toEqual([[jsonOut, cborInput]]);
  });

  it("sets up the remote debug channel on the newer layout", () => {
    const url = "ws://localhost:9421";
    const modern = table("win", 25715) as ModernWinTable;
    const run = runHook({ platform: "windows", arch: "x64", config: modern, moduleName: "flue.dll", scene: 1005 });
    enterLoadStart(run);

    // Scene first, then the remote-debug switch the miniapp reads back.
    expect(run.writes).toEqual([1101, 1]);
    expect(run.strings).toEqual([url]);
    expect(run.sizes).toEqual([url.length]);
    expect(run.sends.join("\n")).toContain(`websocket url -> ${url}`);

    // A scene we do not hijack leaves the launch config alone.
    const skipped = runHook({ platform: "windows", arch: "x64", config: modern, moduleName: "flue.dll", scene: 999 });
    enterLoadStart(skipped);
    expect(skipped.writes).toEqual([]);
    expect(skipped.strings).toEqual([]);
  });

  it("writes the websocket URL through the heap-backed string when the marker says so", () => {
    const url = "ws://localhost:9421";
    const run = runHook({
      platform: "windows",
      arch: "x64",
      config: table("win", 25710),
      moduleName: "flue.dll",
      scene: 1005,
      stringMarker: -1,
    });
    enterLoadStart(run);

    expect(run.strings).toEqual([url]);
    expect(run.sizes).toEqual([url.length]);
    expect(run.writes).toEqual([1101, 1]);
  });

  it("falls back to its built-in table when the placeholder was never replaced", () => {
    const run = runHook({ platform: "darwin", arch: "arm64" });

    expect(run.attached.map((hook) => hook.offset)).toEqual([0x8436b98, 0x4f744c4]);
    expect(run.errors.join("\n")).toContain("version=269136");
  });
});
