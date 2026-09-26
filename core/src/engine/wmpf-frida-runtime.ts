import { readFile } from "node:fs/promises";
import { join } from "node:path";

import type { EngineStatus, RuntimeAdapter, WeChatHost } from "./engine.js";
import { findMacWmpfTarget } from "./darwin-target.js";
import { addressTableCoverage } from "./address-table.js";
import { findWindowsWmpfTarget, isWindowsWeChatProcess, type ProcessInfo } from "./win32-target.js";
import { wmpfTargetFailureCode } from "./wmpf-target-error.js";
import { MODERN_LAYOUT_BUILD, parseDetectedWindowsTable, type DetectedWindowsOffsets, type WindowsAddressTable } from "./windows-auto-detect.js";

export type WmpfTarget = {
  pid: number;
  version: number;
};

/** Platform-specific seams: which process hosts WMPF and where its address
 * tables live. Both differ per OS; the attach/lifecycle flow is shared.
 * Windows reads the build number off the host's own path; macOS has to open
 * the host bundle's Info.plist, so finding the target may be asynchronous. */
export type FridaPlatform = {
  configDir: "win" | "mac";
  findTarget(processes: ProcessInfo[]): WmpfTarget | Promise<WmpfTarget>;
};
import { CdpBridge } from "../bridge/cdp-bridge.js";
import { WmpfSocketServers } from "../bridge/websocket-servers.js";

/** The miniapp runtime connects to this port by convention; it cannot be changed. */
export const DEBUG_PORT = 9421;

export type SocketServersHandle = {
  start(): Promise<unknown>;
  stop(): Promise<void>;
};

export type SocketServersFactory = (
  bridge: CdpBridge,
  ports: { debugPort: number; cdpPort: number },
) => SocketServersHandle;

export type FridaScript = {
  load(): Promise<void>;
  unload(): Promise<void>;
  message?: { connect(callback: (message: unknown) => void): void };
};

/** Forwards Frida script messages (hook console output and script exceptions)
 * to stderr; stdout carries the RPC protocol and must stay clean. Without
 * this, scene-hijack and detection logs from the injected script are dropped. */
function weChatHostNote(error: unknown): string {
  // 失败类型走 code，不看文案：这些 message 是中文的、面向用户的，会随文案
  // 调整而变化，按关键字匹配迟早静默失效。
  switch (wmpfTargetFailureCode(error)) {
    case "no_host":
    case "no_main_process":
    case "no_ancestor":
      return "微信在运行，小程序运行时还没起来";
    case "no_version":
      return "找到了小程序运行时，但读不出微信构建号";
    case "ambiguous_host":
    default:
      return error instanceof Error ? error.message : String(error);
  }
}

function logScriptMessage(message: unknown): void {
  const { type, payload, description, level } = message as {
    type?: string;
    payload?: unknown;
    description?: string;
    level?: string;
  };
  // send payloads already carry their own "[hook] …" context from the script.
  if (type === "send") {
    console.error(typeof payload === "string" ? payload : JSON.stringify(payload ?? ""));
  } else if (type === "log") {
    // Frida delivers the script's console.* output as log messages carrying
    // `level` and `payload`. hook.js reports whether it patched through
    // console.error — "[frida] no config for platform=… arch=…" is the only
    // place a dormant hook ever says why, and "[frida] module not found" the
    // only place a wrong attach does — so dropping these hid the reason an
    // attach could report success while hooking nothing.
    const text = typeof payload === "string" ? payload : JSON.stringify(payload ?? "");
    if (level === "error") console.error(`[hook:error] ${text}`);
    else if (level === "warning") console.error(`[hook:warn] ${text}`);
    else console.error(text);
  } else if (type === "error") {
    const detail = description ?? (typeof payload === "string" ? payload : "");
    console.error(`[hook:error] ${detail || "unknown script error"}`);
  }
}

export type FridaSession = {
  createScript(source: string): Promise<FridaScript>;
  detach(): Promise<void>;
};

export type FridaDevice = {
  enumerateProcesses(options?: { scope: "metadata" }): Promise<ProcessInfo[]>;
  attach(pid: number): Promise<FridaSession>;
};

export class WmpfFridaRuntime implements RuntimeAdapter {
  private session: FridaSession | undefined;
  private script: FridaScript | undefined;
  private bridge: CdpBridge | undefined;
  private servers: SocketServersHandle | undefined;
  private currentStatus: EngineStatus = { frida: false, miniapp: false, devtools: false };
  private lifecycle: Promise<void> = Promise.resolve();

  constructor(
    private readonly resourceRoot: string,
    private readonly getDevice: () => Promise<FridaDevice>,
    private readonly createServers: SocketServersFactory = (bridge, ports) =>
      new WmpfSocketServers(bridge, ports),
    private readonly platform: FridaPlatform = {
      configDir: "win",
      findTarget: findWindowsWmpfTarget,
    },
    /** The arch this host runs as, used only to describe an arch-keyed table. */
    private readonly hostArch: () => string = () => process.arch,
  ) {}

  async start(cdpPort: number): Promise<EngineStatus> {
    return await this.runExclusive(async () => await this.startUnsafe(cdpPort));
  }

  /** Attaches, or explains a failure.
   *
   * Both platforms have a prerequisite the raw Frida error never mentions, so
   * each gets its own hint. The original text is kept verbatim in both cases
   * because docs/WECHAT_E2E_CHECKLIST.md asks for that error to be recorded.
   *
   * macOS: Frida cannot attach while SIP / the code signature forbids debugging.
   * Upstream WMPFDebugger's FAQ lists Ad-Hoc re-signing as the recommended fix
   * and disabling SIP as the discouraged one, so that is the order here —
   * recommending the weaker-security option first would be the wrong advice.
   *
   * Windows: nothing is echoed in Chinese for a bare Frida attach failure, and
   * the common cause is an integrity-level mismatch — WeChat started elevated
   * while this process is not, which Frida reports as a plain access failure.
   * Upstream's FAQ names the same cause and the same remedy (elevate this
   * process to match). Security software blocking the injection is the next
   * thing to rule out. */
  private async attachTarget(device: FridaDevice, pid: number): Promise<FridaSession> {
    try {
      return await device.attach(pid);
    } catch (error) {
      const detail = error instanceof Error ? error.message : String(error);
      if (this.platform.configDir === "mac") {
        throw new Error(
          `附加微信进程失败：${detail}。macOS 上 “Unable to access process with pid … from the current user account” ` +
            "是 SIP / 代码签名不允许调试：推荐对 WeChatAppEx 做 Ad-Hoc 重签名，关闭 SIP 是备选且不推荐" +
            "（命令与核实步骤见 docs/WECHAT_E2E_CHECKLIST.md 的 macOS 前置项）",
          { cause: error },
        );
      }
      throw new Error(
        `附加微信进程失败：${detail}。Windows 上常见原因是权限不匹配：` +
          "微信若以管理员身份运行，本程序也要以管理员身份启动，Frida 无法从低完整性级别附加高完整性进程；" +
          "其次确认安全软件没有拦截注入（见 docs/WECHAT_E2E_CHECKLIST.md 的 Windows 前置项）",
        { cause: error },
      );
    }
  }

  private async startUnsafe(cdpPort: number): Promise<EngineStatus> {
    if (this.currentStatus.frida) {
      return this.status();
    }
    const device = await this.getDevice();
    const target = await this.platform.findTarget(await device.enumerateProcesses({ scope: "metadata" }));
    const hook = await readFile(join(this.resourceRoot, "frida", "hook.js"), "utf8");
    let config = await this.readStaticConfig(target.version);
    let session: FridaSession | undefined;
    if (config !== undefined) {
      console.error(`[core] 使用静态地址表：微信构建 ${target.version}`);
    } else {
      if (!this.canAutoDetect(target.version)) {
        // Offset autodetection is a Windows-PE scanner (frida/autodetect/win.js).
        // macOS has no counterpart for Mach-O, and even on Windows it only
        // recovers the older struct layout, so either way the build needs a
        // table rather than an attach that would hook nothing.
        throw new Error(`缺少微信版本 ${target.version} 的地址表：${this.missingTableNote(target.version)}`);
      }
      console.error(`[core] 构建 ${target.version} 无静态地址表，开始自动检测偏移`);
      session = await this.attachTarget(device, target.pid);
      try {
        const detected = await this.detectWindowsConfig(session, target.version);
        config = detected;
        console.error(`[core] 自动检测完成：LoadStart=${detected.LoadStartHookOffset} CDPFilter候选=${detected.CDPFilterHookOffsets?.length ?? 1} 个`);
      } catch (error) {
        await this.release(session);
        // 自动检测的原始错误是英文内部信息，界面会原样展示，这里补上中文语境。
        const detail = error instanceof Error ? error.message : String(error);
        throw new Error(`微信构建 ${target.version} 无静态地址表，自动检测失败：${detail}`, { cause: error });
      }
    }
    if (!hook.includes("@@CONFIG@@")) throw new Error("frida hook is missing @@CONFIG@@ placeholder");
    const source = hook.replace("@@CONFIG@@", JSON.stringify(config));
    session ??= await this.attachTarget(device, target.pid);

    try {
      const script = await session.createScript(source);
      script.message?.connect(logScriptMessage);
      this.session = session;
      this.script = script;
      await script.load();
      const bridge = new CdpBridge();
      const servers = this.createServers(bridge, { debugPort: DEBUG_PORT, cdpPort });
      await servers.start();
      this.bridge = bridge;
      this.servers = servers;
      this.currentStatus = { frida: true, miniapp: false, devtools: false };
      return this.status();
    } catch (error) {
      await this.release(session);
      this.script = undefined;
      this.session = undefined;
      throw error;
    }
  }

  async stop(): Promise<void> {
    await this.runExclusive(async () => await this.stopUnsafe());
  }

  private async stopUnsafe(): Promise<void> {
    const servers = this.servers;
    this.servers = undefined;
    this.bridge = undefined;
    let serverError: Error | undefined;
    if (servers !== undefined) {
      try {
        await servers.stop();
      } catch (error) {
        serverError = error instanceof Error ? error : new Error(String(error));
      }
    }
    try {
      await this.release();
    } finally {
      // stop() is idempotent: a second call must not touch the released hook
      // or the detached session, even when the socket server failed to stop.
      this.script = undefined;
      this.session = undefined;
      this.currentStatus = { frida: false, miniapp: false, devtools: false };
    }
    if (serverError !== undefined) {
      throw serverError;
    }
  }

  status(): EngineStatus {
    const connections = this.bridge?.connectionStatus() ?? { miniapp: false, devtools: false };
    return { frida: this.currentStatus.frida, ...connections };
  }

  async isWeChatRunning(): Promise<boolean> {
    const device = await this.getDevice();
    const processes = await device.enumerateProcesses({ scope: "metadata" });
    return this.weChatProcessPresent(processes);
  }

  async describeWeChat(): Promise<WeChatHost> {
    const device = await this.getDevice();
    const processes = await device.enumerateProcesses({ scope: "metadata" });
    if (!this.weChatProcessPresent(processes)) return { running: false };
    try {
      const target = await this.platform.findTarget(processes);
      const host = processes.find((process) => process.pid === target.pid);
      const path = typeof host?.parameters.path === "string" ? host.parameters.path : undefined;
      const config = await this.readStaticConfig(target.version);
      const coverage = config === undefined ? undefined : addressTableCoverage(config, this.hostArch());
      // A table keyed for another arch is one the hook cannot use: reporting it
      // as available is exactly how an Intel Mac looked like a working engine
      // that never hooked anything.
      const addressTable = coverage !== undefined && coverage.kind !== "missing-arch";
      return {
        running: true,
        pid: target.pid,
        version: target.version,
        ...(path === undefined ? {} : { path }),
        addressTable,
        ...(addressTable ? {} : {
          note: coverage?.kind === "missing-arch"
            ? this.missingArchNote(target.version, coverage.available)
            : this.missingTableNote(target.version),
        }),
      };
    } catch (error) {
      return { running: true, note: weChatHostNote(error) };
    }
  }

  private weChatProcessPresent(processes: ProcessInfo[]): boolean {
    return this.platform.configDir === "win"
      ? processes.some(isWindowsWeChatProcess)
      : processes.some((process) => process.name.toLowerCase() === "wechat");
  }

  getBridge(): CdpBridge | undefined {
    return this.bridge;
  }

  /** Whether the offset detector can stand in for a missing address table. */
  private canAutoDetect(version: number): boolean {
    return this.platform.configDir === "win" && version < MODERN_LAYOUT_BUILD;
  }

  /** A build with no static table reads very differently per platform: Windows
   * falls back to the offset detector (for the layouts it knows), macOS can only
   * ask for the table to be recovered. Both the start failure and the status
   * tooltip say this, so the wording lives here instead of drifting apart. */
  private missingTableNote(version: number): string {
    if (this.platform.configDir !== "win") {
      return `${this.platform.configDir} 平台不做自动检测，需补充 resources/frida/config/${this.platform.configDir}/addresses.${version}.json`;
    }
    if (version >= MODERN_LAYOUT_BUILD) {
      return `该构建用新结构布局（WMPF ≥ ${MODERN_LAYOUT_BUILD}），自动检测只认旧布局：` +
        `需补充 resources/frida/config/win/addresses.${version}.json`;
    }
    return "没有这份构建的静态地址表，启动时会自动检测偏移";
  }

  /** An arch-keyed table that only carries another architecture's offsets
   * cannot be used: resources/frida/hook.js stays dormant rather than patching
   * with the wrong arch's numbers, so the status has to say so instead of
   * calling the table present.
   *
   * The arch named here is this host process's (Node's), which is what the
   * shipped frida binding is built for. hook.js reads the *target's* arch, so
   * the two can differ on a Rosetta Node over a native WeChat — the note gives
   * the operator both facts to compare rather than asserting a cause. */
  private missingArchNote(version: number, available: string[]): string {
    const listed = available.length === 0 ? "空" : available.join(" / ");
    return `该构建的地址表只有 ${listed} 的偏移，本机运行的是 ${this.hostArch()}：` +
      `需补充 resources/frida/config/${this.platform.configDir}/addresses.${version}.json 的 ${this.hostArch()} 段`;
  }

  private async readStaticConfig(version: number): Promise<unknown> {
    const configDir = join(this.resourceRoot, "frida", "config", this.platform.configDir);
    const configPath = join(configDir, `addresses.${version}.json`);
    return await readFile(configPath, "utf8")
      .then((text) => JSON.parse(text))
      .catch((error: unknown) => {
        // WeChat ships builds faster than address tables can be recovered:
        // say which build is missing and where its table belongs instead of
        // surfacing a bare ENOENT from the attach flow.
        if ((error as { code?: string }).code === "ENOENT") {
          return undefined;
        }
        throw error;
      });
  }

  private async detectWindowsConfig(session: FridaSession, version: number): Promise<WindowsAddressTable> {
    const source = await readFile(join(this.resourceRoot, "frida", "autodetect", "win.js"), "utf8");
    const detector = await session.createScript(source);
    // The timeout must be cleared on every exit path: when script load itself
    // fails the result promise is never awaited again, and a surviving timer
    // would reject an orphaned promise 90s later (unhandled rejection).
    let timer: ReturnType<typeof setTimeout> | undefined;
    const result = new Promise<WindowsAddressTable>((resolve, reject) => {
      timer = setTimeout(() => reject(new Error("自动检测地址表超时；请确认微信中已打开小程序后重试")), 300000);
      detector.message?.connect((message) => {
        const { type, payload, description } = message as { type?: string; payload?: { type?: string; config?: DetectedWindowsOffsets; moduleSize?: number; error?: string; done?: number; total?: number } | undefined; description?: string };
        if (type === "error") {
          // A script-level exception (e.g. an API missing on this Frida build)
          // would otherwise surface only as the timeout.
          clearTimeout(timer); reject(new Error(`自动检测地址表失败：${payload?.error ?? description ?? "脚本异常"}`));
        } else if (payload?.type === "wmpf-offsets" && payload.config && typeof payload.moduleSize === "number") {
          clearTimeout(timer); resolve(parseDetectedWindowsTable(payload.config, payload.moduleSize, version));
        } else if (payload?.type === "wmpf-offsets-progress") {
          console.error(`[core] 自动检测进度 ${payload.done}/${payload.total}`);
        } else if (payload?.type === "wmpf-offsets-error") {
          clearTimeout(timer); reject(new Error(`自动检测地址表失败：${payload.error ?? "未知错误"}`));
        }
      });
    });
    try {
      await detector.load();
      return await result;
    } finally {
      if (timer !== undefined) clearTimeout(timer);
      try {
        await detector.unload();
      } catch {
        // A failed detector or detached target can destroy the script before
        // cleanup. Do not replace the detection result with that secondary
        // cleanup error.
      }
    }
  }

  private async release(session = this.session): Promise<void> {
    if (this.script !== undefined) {
      try {
        await this.script.unload();
      } catch {
        // A disconnected target has already released the script.
      }
    }
    if (session !== undefined) {
      try {
        await session.detach();
      } catch {
        // A disconnected target has already released the session.
      }
    }
  }

  private async runExclusive<T>(operation: () => Promise<T>): Promise<T> {
    const previous = this.lifecycle;
    let release: (() => void) | undefined;
    this.lifecycle = new Promise<void>((resolve) => { release = resolve; });
    await previous;
    try {
      return await operation();
    } finally {
      release?.();
    }
  }
}

export function createWindowsFridaRuntime(resourceRoot: string): WmpfFridaRuntime {
  return new WmpfFridaRuntime(resourceRoot, createLocalMetadataDevice);
}

export function createMacOSFridaRuntime(resourceRoot: string): WmpfFridaRuntime {
  return new WmpfFridaRuntime(resourceRoot, createLocalMetadataDevice, undefined, {
    configDir: "mac",
    findTarget: findMacWmpfTarget,
  });
}

async function createLocalMetadataDevice(): Promise<FridaDevice> {
  const frida = await import("frida");
  const device = await frida.getLocalDevice();
  return {
    enumerateProcesses: async () => await device.enumerateProcesses({ scope: frida.Scope.Metadata }),
    attach: async (pid) => await device.attach(pid),
  };
}
