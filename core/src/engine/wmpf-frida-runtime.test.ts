import { mkdtemp, mkdir, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  WmpfFridaRuntime,
  type FridaDevice,
  type FridaScript,
  type FridaSession,
} from "./wmpf-frida-runtime.js";
import { findMacWmpfTarget } from "./darwin-target.js";
import { CdpBridge, type Peer } from "../bridge/cdp-bridge.js";

const temporaryRoots: string[] = [];

afterEach(async () => {
  await Promise.all(temporaryRoots.splice(0).map((root) => rm(root, { recursive: true, force: true })));
});

async function createResources(): Promise<string> {
  const root = await mkdtemp(join(tmpdir(), "wxtap-core-"));
  temporaryRoots.push(root);
  await mkdir(join(root, "frida", "config", "win"), { recursive: true });
  await writeFile(join(root, "frida", "hook.js"), "const config = @@CONFIG@@;", "utf8");
  await writeFile(join(root, "frida", "config", "win", "addresses.14161.json"), '{"offset":42}', "utf8");
  return root;
}

function createDevice(script: FridaScript): {
  device: FridaDevice;
  detachCalls: number[];
  processQueries: unknown[][];
  sources: string[];
} {
  const detachCalls: number[] = [];
  const processQueries: unknown[][] = [];
  const sources: string[] = [];
  const session: FridaSession = {
    createScript: async (source) => {
      sources.push(source);
      return script;
    },
    detach: async () => {
      detachCalls.push(1);
    },
  };
  return {
    device: {
      enumerateProcesses: async (...query) => {
        processQueries.push(query);
        return [
          { pid: 31, name: "WeChatAppEx.exe", parameters: { ppid: 10 } },
          { pid: 32, name: "WeChatAppEx.exe", parameters: { ppid: 10 } },
          { pid: 10, name: "WeChatAppEx.exe", parameters: { ppid: 1, path: "C:\\WeChat\\14161\\WeChatAppEx.exe" } },
          { pid: 1, name: "WeChat.exe", parameters: { path: "C:\\WeChat\\WeChat.exe" } },
        ];
      },
      attach: async (pid) => {
        expect(pid).toBe(10);
        return session;
      },
    },
    detachCalls,
    processQueries,
    sources,
  };
}

describe("WmpfFridaRuntime", () => {
  it("recognizes only the desktop WeChat process and ignores WMPF helper names", async () => {
    const root = await createResources();
    const device: FridaDevice = {
      enumerateProcesses: async () => [
        { pid: 1, name: "QQMusic.exe", parameters: {} },
        { pid: 2, name: "WeChatAppEx.exe", parameters: {} },
        { pid: 3, name: "WeChat.exe", parameters: {} },
      ],
      attach: async () => { throw new Error("process check must not attach"); },
    };
    const runtime = new WmpfFridaRuntime(root, async () => device);

    await expect(runtime.isWeChatRunning()).resolves.toBe(true);
    await expect(runtime.describeWeChat()).resolves.toEqual({ running: true, note: "微信在运行，小程序运行时还没起来" });
  });

  it("reports the verified WMPF build and whether its address table is present", async () => {
    const root = await createResources();
    const device: FridaDevice = {
      enumerateProcesses: async () => [
        { pid: 1, name: "WeChat.exe", parameters: { path: "C:\\Tencent\\WeChat\\WeChat.exe" } },
        { pid: 31, name: "WeChatAppEx.exe", parameters: { ppid: 10 } },
        { pid: 10, name: "WeChatAppEx.exe", parameters: { ppid: 1, path: "C:\\Tencent\\WeChat\\14161\\WeChatAppEx.exe" } },
      ],
      attach: async () => { throw new Error("process check must not attach"); },
    };
    const runtime = new WmpfFridaRuntime(root, async () => device);

    await expect(runtime.describeWeChat()).resolves.toEqual({
      running: true,
      pid: 10,
      version: 14161,
      path: "C:\\Tencent\\WeChat\\14161\\WeChatAppEx.exe",
      addressTable: true,
    });
  });

  it("does not treat QQ Music's WMPF helper as WeChat", async () => {
    const root = await createResources();
    const device: FridaDevice = {
      enumerateProcesses: async () => [
        { pid: 1, name: "QQMusic.exe", parameters: {} },
        { pid: 2, name: "WeChatAppEx.exe", parameters: {} },
      ],
      attach: async () => { throw new Error("process check must not attach"); },
    };
    const runtime = new WmpfFridaRuntime(root, async () => device);

    await expect(runtime.isWeChatRunning()).resolves.toBe(false);
  });

  it("recognizes the current Weixin desktop process as WeChat", async () => {
    const root = await createResources();
    const device: FridaDevice = {
      enumerateProcesses: async () => [{ pid: 1, name: "Weixin.exe", parameters: { path: "D:\\Tencent\\Weixin\\Weixin.exe" } }],
      attach: async () => { throw new Error("process check must not attach"); },
    };
    const runtime = new WmpfFridaRuntime(root, async () => device);

    await expect(runtime.isWeChatRunning()).resolves.toBe(true);
  });

  it("does not treat an unrelated Weixin.exe as the desktop WeChat process", async () => {
    const root = await createResources();
    const device: FridaDevice = {
      enumerateProcesses: async () => [{ pid: 1, name: "Weixin.exe", parameters: { path: "D:\\OtherProduct\\Weixin.exe" } }],
      attach: async () => { throw new Error("process check must not attach"); },
    };
    const runtime = new WmpfFridaRuntime(root, async () => device);

    await expect(runtime.isWeChatRunning()).resolves.toBe(false);
  });

  it("loads the version-specific Hook and releases it on stop", async () => {
    const root = await createResources();
    const calls: string[] = [];
    const { device, detachCalls, processQueries, sources } = createDevice({
      load: async () => {
        calls.push("load");
      },
      unload: async () => {
        calls.push("unload");
      },
    });
    const runtime = new WmpfFridaRuntime(root, async () => device);

    await expect(runtime.start(62000)).resolves.toEqual({ frida: true, miniapp: false, devtools: false });
    expect(processQueries).toEqual([[{ scope: "metadata" }]]);
    expect(sources).toEqual(['const config = {"offset":42};']);

    await runtime.stop();
    expect(calls).toEqual(["load", "unload"]);
    expect(detachCalls).toHaveLength(1);
    expect(runtime.status()).toEqual({ frida: false, miniapp: false, devtools: false });
  });

  it("is idempotent while the hook is already loaded", async () => {
    const root = await createResources();
    const loads: string[] = [];
    let attaches = 0;
    const { device } = createDevice({
      load: async () => {
        loads.push("load");
      },
      unload: async () => undefined,
    });
    const counted: FridaDevice = {
      enumerateProcesses: () => device.enumerateProcesses(),
      attach: async (pid) => {
        attaches += 1;
        return device.attach(pid);
      },
    };
    const runtime = new WmpfFridaRuntime(root, async () => counted, () => ({
      start: async () => undefined,
      stop: async () => undefined,
    }));

    await runtime.start(62000);
    // The control view may send start twice; the second call must not attach
    // or load the hook again.
    await expect(runtime.start(62000)).resolves.toEqual({ frida: true, miniapp: false, devtools: false });
    expect(attaches).toBe(1);
    expect(loads).toEqual(["load"]);
    await runtime.stop();
  });

  it("serializes stop behind an in-flight start", async () => {
    const root = await createResources();
    let finishLoad: (() => void) | undefined;
    const calls: string[] = [];
    const { device, detachCalls } = createDevice({
      load: async () => await new Promise<void>((resolve) => { finishLoad = resolve; }),
      unload: async () => { calls.push("unload"); },
    });
    const runtime = new WmpfFridaRuntime(root, async () => device);

    const starting = runtime.start(62000);
    await vi.waitFor(() => expect(finishLoad).toBeTypeOf("function"));
    const stopping = runtime.stop();
    finishLoad?.();
    await Promise.all([starting, stopping]);

    expect(runtime.status()).toEqual({ frida: false, miniapp: false, devtools: false });
    expect(calls).toEqual(["unload"]);
    expect(detachCalls).toHaveLength(1);
  });

  it("drops the bridge and hook references on stop", async () => {
    const root = await createResources();
    const unloads: string[] = [];
    let started: CdpBridge | undefined;
    const { device } = createDevice({
      load: async () => undefined,
      unload: async () => {
        unloads.push("unload");
      },
    });
    const runtime = new WmpfFridaRuntime(root, async () => device, (bridge) => {
      started = bridge;
      return { start: async () => undefined, stop: async () => undefined };
    });

    await runtime.start(62000);
    expect(runtime.getBridge()).toBe(started);

    await runtime.stop();
    expect(runtime.getBridge()).toBeUndefined();
    // A second stop must not touch the already-released hook.
    await runtime.stop();
    expect(unloads).toEqual(["unload"]);
  });

  it("detaches when Hook loading fails", async () => {
    const root = await createResources();
    const calls: string[] = [];
    const { device, detachCalls } = createDevice({
      load: async () => {
        throw new Error("load failed");
      },
      unload: async () => {
        calls.push("unload");
      },
    });
    const runtime = new WmpfFridaRuntime(root, async () => device);

    await expect(runtime.start(62000)).rejects.toThrow("load failed");
    expect(calls).toEqual(["unload"]);
    expect(detachCalls).toHaveLength(1);
    expect(runtime.status().frida).toBe(false);
  });

  it("starts the WMPF socket servers on the fixed debug port and reports bridge connections", async () => {
    const root = await createResources();
    const { device } = createDevice({ load: async () => {}, unload: async () => {} });
    const starts: Array<{ debugPort: number; cdpPort: number }> = [];
    const stops: number[] = [];
    let bridge: CdpBridge | undefined;
    const runtime = new WmpfFridaRuntime(root, async () => device, (created, ports) => {
      bridge = created;
      starts.push(ports);
      return {
        start: async () => undefined,
        stop: async () => {
          stops.push(1);
        },
      };
    });
    const connectedPeer: Peer = { send: () => undefined };

    await runtime.start(62000);
    expect(starts).toEqual([{ debugPort: 9421, cdpPort: 62000 }]);

    bridge?.addMiniapp(connectedPeer);
    bridge?.addDevtools(connectedPeer);
    expect(runtime.status()).toEqual({ frida: true, miniapp: true, devtools: true });

    await runtime.stop();
    expect(stops).toHaveLength(1);
    expect(runtime.status()).toEqual({ frida: false, miniapp: false, devtools: false });
  });

  it("forwards hook script messages to stderr", async () => {
    const root = await createResources();
    const logged: string[] = [];
    const spy = vi.spyOn(console, "error").mockImplementation((...args: unknown[]) => {
      logged.push(args.map(String).join(" "));
    });
    let connect: ((message: unknown) => void) | undefined;
    const { device } = createDevice({
      load: async () => undefined,
      unload: async () => undefined,
      message: {
        connect: (callback) => {
          connect = callback;
        },
      },
    });
    const runtime = new WmpfFridaRuntime(root, async () => device);
    try {
      await runtime.start(62000);
      expect(connect).toBeDefined();
      // start() itself logs table-selection lines; only the forwarded hook
      // messages are under test here.
      logged.length = 0;
      connect?.({ type: "send", payload: "[hook] scene: 1035" });
      connect?.({ type: "error", description: "access violation" });
      // Frida routes the script's console.* here (level + payload). These are
      // the [frida] lines hook.js emits, and the dormant-hook one is the only
      // diagnosis of an attach that succeeded without patching anything.
      connect?.({ type: "log", level: "info", payload: "[frida] module base: 0x100000000" });
      connect?.({ type: "log", level: "warning", payload: "[patch] unexpected layout" });
      connect?.({ type: "log", level: "error", payload: "[frida] no config for platform=darwin arch=x86_64" });
      expect(logged).toEqual([
        "[hook] scene: 1035",
        "[hook:error] access violation",
        "[frida] module base: 0x100000000",
        "[hook:warn] [patch] unexpected layout",
        "[hook:error] [frida] no config for platform=darwin arch=x86_64",
      ]);
    } finally {
      spy.mockRestore();
      await runtime.stop();
    }
  });

  it("releases the Hook when the socket servers fail to start", async () => {
    const root = await createResources();
    const calls: string[] = [];
    const { device, detachCalls } = createDevice({
      load: async () => {
        calls.push("load");
      },
      unload: async () => {
        calls.push("unload");
      },
    });
    const runtime = new WmpfFridaRuntime(root, async () => device, () => ({
      start: async () => {
        throw new Error("port already in use");
      },
      stop: async () => undefined,
    }));

    await expect(runtime.start(62000)).rejects.toThrow("port already in use");
    expect(calls).toEqual(["load", "unload"]);
    expect(detachCalls).toHaveLength(1);
  });
});

describe("WmpfFridaRuntime (macOS platform)", () => {
  it("reads the mac address table and passes the arch-specific config to the hook", async () => {
    const root = await mkdtemp(join(tmpdir(), "wxtap-core-"));
    temporaryRoots.push(root);
    await mkdir(join(root, "frida", "config", "mac"), { recursive: true });
    await writeFile(join(root, "frida", "hook.js"), "const config = @@CONFIG@@;", "utf8");
    const macConfig = {
      Version: 269136,
      Arch: {
        arm64: {
          LoadStartHookOffset: "0x4F744C4",
          CDPFilterHookOffset: "0x8436B98",
          SceneOffsets: [56, 1504, 8, 1440, 16, 456],
        },
      },
    };
    await writeFile(
      join(root, "frida", "config", "mac", "addresses.269136.json"),
      JSON.stringify(macConfig),
      "utf8",
    );

    const sources: string[] = [];
    const session: FridaSession = {
      createScript: async (source) => {
        sources.push(source);
        return { load: async () => {}, unload: async () => {} };
      },
      detach: async () => {},
    };
    const device: FridaDevice = {
      enumerateProcesses: async () => [
        { pid: 31, name: "WeChatAppEx Helper", parameters: { ppid: 10 } },
        {
          pid: 10,
          name: "WeChatAppEx",
          parameters: { path: "/Applications/WeChat.app/Contents/MacOS/WeChatAppEx.app/Contents/MacOS/WeChatAppEx" },
        },
      ],
      attach: async (pid) => {
        expect(pid).toBe(10);
        return session;
      },
    };

    const runtime = new WmpfFridaRuntime(root, async () => device, undefined, {
      configDir: "mac",
      // The shipped reader opens /Applications/.../Info.plist, which only exists
      // on a mac; the build number it yields is the address table's name.
      findTarget: (processes) =>
        findMacWmpfTarget(
          processes,
          async () => '<plist><dict><key>CFBundleVersion</key><string>269136</string></dict></plist>',
        ),
    });

    await expect(runtime.start(62000)).resolves.toEqual({ frida: true, miniapp: false, devtools: false });
    await runtime.stop();
    expect(sources).toEqual([`const config = ${JSON.stringify(macConfig)};`]);
  });

  it("stops on a mac build with no address table instead of auto-detecting", async () => {
    // The offset detector is a PE scanner; there is no Mach-O counterpart, and
    // attaching without a table would report a healthy engine that hooks
    // nothing. Name the file that has to be recovered instead.
    const root = await mkdtemp(join(tmpdir(), "wxtap-core-"));
    temporaryRoots.push(root);
    await mkdir(join(root, "frida", "config", "mac"), { recursive: true });
    await writeFile(join(root, "frida", "hook.js"), "const config = @@CONFIG@@;", "utf8");

    const device: FridaDevice = {
      enumerateProcesses: async () => [
        { pid: 31, name: "WeChatAppEx Helper", parameters: { ppid: 10 } },
        { pid: 10, name: "WeChatAppEx", parameters: {} },
      ],
      attach: async () => {
        throw new Error("attach must not be reached without an address table");
      },
    };
    const runtime = new WmpfFridaRuntime(root, async () => device, undefined, {
      configDir: "mac",
      findTarget: (processes) =>
        findMacWmpfTarget(
          processes,
          async () => '<plist><dict><key>CFBundleVersion</key><string>269136</string></dict></plist>',
        ),
    });

    await expect(runtime.start(62000)).rejects.toThrow(
      "需补充 resources/frida/config/mac/addresses.269136.json",
    );
  });

  it("tells the mac user a missing table has to be recovered, not auto-detected", async () => {
    const root = await mkdtemp(join(tmpdir(), "wxtap-core-"));
    temporaryRoots.push(root);
    await mkdir(join(root, "frida", "config", "mac"), { recursive: true });

    const device: FridaDevice = {
      enumerateProcesses: async () => [
        { pid: 31, name: "WeChatAppEx Helper", parameters: { ppid: 10 } },
        {
          pid: 10,
          name: "WeChatAppEx",
          parameters: { path: "/Applications/WeChat.app/Contents/MacOS/WeChatAppEx.app/Contents/MacOS/WeChatAppEx" },
        },
        { pid: 1, name: "WeChat", parameters: { path: "/Applications/WeChat.app/Contents/MacOS/WeChat" } },
      ],
      attach: async () => {
        throw new Error("process check must not attach");
      },
    };
    const runtime = new WmpfFridaRuntime(root, async () => device, undefined, {
      configDir: "mac",
      findTarget: (processes) =>
        findMacWmpfTarget(
          processes,
          async () => '<plist><dict><key>CFBundleVersion</key><string>269136</string></dict></plist>',
        ),
    });

    // The status tooltip is the only place the user learns why the build is
    // unsupported; promising autodetection here would be a lie on macOS.
    const host = await runtime.describeWeChat();
    expect(host.addressTable).toBe(false);
    expect(host.note).toContain("不做自动检测");
    expect(host.note).toContain("addresses.269136.json");
  });

  // The shipped macOS table carries a single arch. On an Intel Mac the hook
  // stays dormant (it will not patch with arm64 offsets), so claiming a usable
  // table is what made that machine look like a working engine that never
  // hooked anything.
  it("reports an arch-keyed table that only covers another architecture", async () => {
    const root = await createMacTableResources();
    const runtime = macRuntime(root, "x64");

    const host = await runtime.describeWeChat();
    expect(host.addressTable).toBe(false);
    expect(host.note).toContain("arm64");
    expect(host.note).toContain("x64");
    expect(host.note).toContain("addresses.269136.json");
  });

  it("accepts that table on the architecture it was recovered for", async () => {
    const root = await createMacTableResources();
    const runtime = macRuntime(root, "arm64");

    const host = await runtime.describeWeChat();
    expect(host.addressTable).toBe(true);
    expect(host.note).toBeUndefined();
  });

  // macOS needs SIP off before Frida may attach, which is far heavier than
  // anything Windows requires. The raw Frida error cannot say that, and the E2E
  // checklist asks for that error to be recorded, so both have to survive.
  it("appends the macOS attach prerequisite without replacing the raw error", async () => {
    const root = await createMacTableResources();
    const runtime = macRuntime(root, "arm64");

    // Both halves must survive: the raw Frida error, which the E2E checklist
    // asks to be recorded, and the macOS prerequisite appended to it.
    const failure: unknown = await runtime.start(62000).then(
      () => undefined,
      (reason: unknown) => reason,
    );
    const message = failure instanceof Error ? failure.message : String(failure);
    expect(message).toContain("status check must not attach");
    // The hint follows upstream WMPFDebugger's FAQ, which recommends Ad-Hoc
    // re-signing and marks disabling SIP as the discouraged fallback.
    expect(message).toContain("Unable to access process with pid");
    expect(message).toContain("Ad-Hoc 重签名");
    expect(message).toContain("关闭 SIP 是备选且不推荐");
  });
});

/** A resources root whose macOS table has offsets for arm64 only. */
async function createMacTableResources(): Promise<string> {
  const root = await createResources();
  await mkdir(join(root, "frida", "config", "mac"), { recursive: true });
  await writeFile(
    join(root, "frida", "config", "mac", "addresses.269136.json"),
    JSON.stringify({ Version: 269136, Arch: { arm64: { LoadStartHookOffset: "0x4F744C4" } } }),
    "utf8",
  );
  return root;
}

/** The mac platform seams plus the host arch the status note is written against. */
function macRuntime(root: string, arch: string): WmpfFridaRuntime {
  const device: FridaDevice = {
    enumerateProcesses: async () => [
      { pid: 31, name: "WeChatAppEx Helper", parameters: { ppid: 10 } },
      {
        pid: 10,
        name: "WeChatAppEx",
        parameters: { path: "/Applications/WeChat.app/Contents/MacOS/WeChatAppEx.app/Contents/MacOS/WeChatAppEx" },
      },
      // weChatProcessPresent wants the desktop host by name before it looks at
      // the WMPF helpers at all.
      { pid: 1, name: "WeChat", parameters: { path: "/Applications/WeChat.app/Contents/MacOS/WeChat" } },
    ],
    attach: async () => {
      throw new Error("status check must not attach");
    },
  };
  return new WmpfFridaRuntime(
    root,
    async () => device,
    undefined,
    {
      configDir: "mac",
      findTarget: (processes) =>
        findMacWmpfTarget(
          processes,
          async () => '<plist><dict><key>CFBundleVersion</key><string>269136</string></dict></plist>',
        ),
    },
    () => arch,
  );
}

// A WeChat build with no address table must fail with an actionable message:
// the operator needs the build number and the directory to populate, not a
// bare ENOENT from deep inside the attach flow.
describe("WmpfFridaRuntime address table coverage", () => {
  it("preserves a detector failure when its cleanup finds a destroyed script", async () => {
    const root = await mkdtemp(join(tmpdir(), "wxtap-core-"));
    temporaryRoots.push(root);
    await mkdir(join(root, "frida", "autodetect"), { recursive: true });
    await writeFile(join(root, "frida", "hook.js"), "const config = @@CONFIG@@;", "utf8");
    await writeFile(join(root, "frida", "autodetect", "win.js"), "// detector", "utf8");

    let emit: ((message: unknown) => void) | undefined;
    const detector: FridaScript = {
      load: async () => emit?.({ type: "error", description: "detector failed" }),
      unload: async () => {
        throw new Error("Script is destroyed");
      },
      message: { connect: (callback) => { emit = callback; } },
    };
    const session: FridaSession = {
      createScript: async () => detector,
      detach: async () => undefined,
    };
    const device: FridaDevice = {
      enumerateProcesses: async () => [
        { pid: 31, name: "WeChatAppEx.exe", parameters: { ppid: 10 } },
        { pid: 10, name: "WeChatAppEx.exe", parameters: { ppid: 1, path: "C:\\WeChat\\25599\\WeChatAppEx.exe" } },
        { pid: 1, name: "WeChat.exe", parameters: { path: "C:\\WeChat\\WeChat.exe" } },
      ],
      attach: async () => session,
    };
    const runtime = new WmpfFridaRuntime(root, async () => device);

    await expect(runtime.start(62000)).rejects.toThrow("自动检测地址表失败：detector failed");
  });

  it("attaches only to begin automatic detection when no address table exists", async () => {
    const root = await mkdtemp(join(tmpdir(), "wxtap-core-"));
    temporaryRoots.push(root);
    await mkdir(join(root, "frida", "config", "win"), { recursive: true });
    await writeFile(join(root, "frida", "hook.js"), "const config = @@CONFIG@@;", "utf8");

    const device: FridaDevice = {
      enumerateProcesses: async () => [
        { pid: 31, name: "WeChatAppEx.exe", parameters: { ppid: 10 } },
        { pid: 10, name: "WeChatAppEx.exe", parameters: { ppid: 1, path: "C:\\WeChat\\25599\\WeChatAppEx.exe" } },
        { pid: 1, name: "WeChat.exe", parameters: { path: "C:\\WeChat\\WeChat.exe" } },
      ],
      attach: async () => {
        throw new Error("attach must not be reached without an address table");
      },
    };

    const runtime = new WmpfFridaRuntime(root, async () => device);
    await expect(runtime.start(62000)).rejects.toThrow("attach must not be reached without an address table");
  });

  it("refuses to auto-detect a build that uses the newer struct layout", async () => {
    // The detector recovers the older SceneOffsets layout only, so synthesizing
    // a table for a 25710+ build would attach and then hook nothing. Say which
    // file has to be recovered instead of scanning for a minute to no effect.
    const root = await mkdtemp(join(tmpdir(), "wxtap-core-"));
    temporaryRoots.push(root);
    await mkdir(join(root, "frida", "config", "win"), { recursive: true });
    await writeFile(join(root, "frida", "hook.js"), "const config = @@CONFIG@@;", "utf8");

    const device: FridaDevice = {
      enumerateProcesses: async () => [
        { pid: 31, name: "WeChatAppEx.exe", parameters: { ppid: 10 } },
        { pid: 10, name: "WeChatAppEx.exe", parameters: { ppid: 1, path: "C:\\WeChat\\25715\\WeChatAppEx.exe" } },
        { pid: 1, name: "WeChat.exe", parameters: { path: "C:\\WeChat\\WeChat.exe" } },
      ],
      attach: async () => {
        throw new Error("attach must not be reached: the scan cannot produce a usable table");
      },
    };

    const runtime = new WmpfFridaRuntime(root, async () => device);
    await expect(runtime.start(62000)).rejects.toThrow(
      "需补充 resources/frida/config/win/addresses.25715.json",
    );
    // The status tooltip has to say the same, or the user is told to wait for a
    // scan that will never run.
    const host = await runtime.describeWeChat();
    expect(host.addressTable).toBe(false);
    expect(host.note).toContain("新结构布局");
  });
});

// A socket teardown failure must still leave the runtime fully released:
// the hook is unloaded, the session detached, and no stale bridge survives.
it("still releases the hook when socket stop fails", async () => {
  const root = await createResources();
  const calls: string[] = [];
  const { device, detachCalls } = createDevice({
    load: async () => {
      calls.push("load");
    },
    unload: async () => {
      calls.push("unload");
    },
  });
  const runtime = new WmpfFridaRuntime(root, async () => device, () => ({
    start: async () => undefined,
    stop: async () => {
      throw new Error("socket stop failed");
    },
  }));

  await runtime.start(62000);
  await expect(runtime.stop()).rejects.toThrow("socket stop failed");
  expect(calls).toEqual(["load", "unload"]);
  expect(detachCalls).toHaveLength(1);
  expect(runtime.status()).toEqual({ frida: false, miniapp: false, devtools: false });
  expect(runtime.getBridge()).toBeUndefined();
});
