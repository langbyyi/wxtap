import type { Engine } from "./engine/engine.js";
import type { CdpBridge } from "./bridge/cdp-bridge.js";
import { RpcFailure } from "./rpc/stdio.js";
import type { RpcRequest } from "./rpc/protocol.js";
import { formatContent } from "./formatter.js";

type EngineApi = Pick<Engine, "start" | "stop" | "status" | "describeWeChat" | "getBridge">;

type BridgeProvider = () => CdpBridge | undefined;

/** Loads the injectable source of a hook script (core/hooks/<name>.js). */
export type HookScriptLoader = (name: string) => string | Promise<string>;

/**
 * Page-side surface of the record-stream hooks: the global each hook installs,
 * and the method names the Core calls on it. `navigator` is a control hook
 * (it has no record stream) and stays out of this table — HOOK_NAMES is
 * derived from it so the two lists cannot drift apart.
 */
const HOOK_SURFACES: Record<string, { object: string; install: string; uninstall: string }> = {
  wxapi: { object: "window.wxApiAudit", install: "install", uninstall: "uninstall" },
  cloud: { object: "window.cloudAudit", install: "install", uninstall: "uninstallHook" },
  console: { object: "window.consoleAudit", install: "install", uninstall: "uninstallHook" },
};

const CONTROL_HOOKS = new Set(["navigator"]);

const HOOK_NAMES = new Set([...Object.keys(HOOK_SURFACES), ...CONTROL_HOOKS]);

/**
 * Page-side surface of one record-stream hook. Callers reach this only after
 * the control hooks are handled, so a miss means a name was added to
 * CONTROL_HOOKS without its own branch — fail loudly instead of evaluating an
 * empty expression in the page.
 */
function recordHook(name: string): { object: string; install: string; uninstall: string } {
  const surface = HOOK_SURFACES[name];
  if (surface === undefined) {
    throw new RpcFailure(1000, `hook ${name} is not a record hook`, false);
  }
  return surface;
}

function installExpression(name: string): string {
  if (CONTROL_HOOKS.has(name)) {
    // 只有 wxFrame 在，window.nav 才真的可用：只看 window.nav 会把「装上了但
    // 找不到小程序 frame」报成成功，之后每个导航调用都在页面里抛 TypeError。
    return "{ ok: !!(window.nav && window.nav.wxFrame) }";
  }
  const surface = recordHook(name);
  return `${surface.object}.${surface.install}()`;
}

function drainExpression(name: string): string {
  const surface = HOOK_SURFACES[name];
  if (surface === undefined) {
    throw new RpcFailure(1000, `hook ${name} does not support drain`, false);
  }
  return `${surface.object}.drain`;
}

export class CoreApp {
  constructor(
    private readonly engine: EngineApi,
    private readonly bridgeProvider?: BridgeProvider,
    private readonly loadHookScript?: HookScriptLoader,
    private readonly consoleSink?: (entry: Record<string, unknown>) => void,
  ) {}

  async handle(request: RpcRequest): Promise<unknown> {
    switch (request.method) {
      case "engine.start":
        return this.engine.start(this.readCdpPort(request.params));
      case "engine.stop":
        await this.engine.stop();
        return {};
      case "engine.status":
        return this.status();
      case "wechat.status":
        return this.engine.describeWeChat();
      case "cdp.command":
        return this.sendCdpCommand(request.params);
      case "cdp.debug":
        return this.debugState(request.params);
      case "runtime.evaluate":
        return this.evaluate(request.params);
      case "hook.install":
        return this.installHook(request.params);
      case "hook.drain":
        return this.drainHook(request.params);
      case "hook.clear":
        return this.clearHook(request.params);
      case "hook.uninstall":
        return this.uninstallHook(request.params);
      case "cloud.scan":
        return this.scanCloud();
      case "miniapp.list":
        return this.bridge().listMiniapps();
      case "miniapp.switch":
        return { ok: this.bridge().switchMiniapp(this.readClientId(request.params)) };
      case "miniapp.setLock":
        this.bridge().setLock(this.readLockEnabled(request.params));
        return {};
      case "miniapp.getLock":
        return { enabled: this.bridge().isLockEnabled() };
      case "code.format":
        return { formatted: formatContent(this.readFormatContent(request.params), this.readLanguage(request.params)) };
      default:
        throw new Error(`unknown method: ${request.method}`);
    }
  }

  private status(): unknown {
    const base = this.engine.status();
    const bridge = this.bridgeOptional();
    const appInfo = bridge?.lastAppInfo();
    const generation = bridge?.generation() ?? 0;
    const status = generation > 0 ? { ...base, generation } : base;
    return appInfo === undefined ? status : { ...status, appInfo };
  }

  private bridge(): CdpBridge {
    const bridge = this.bridgeOptional();
    if (!bridge) {
      throw new RpcFailure(2000, "engine not started", true);
    }
    return bridge;
  }

  private bridgeOptional(): CdpBridge | undefined {
    const bridge = this.bridgeProvider ? this.bridgeProvider() : this.engine.getBridge();
    // 每次拿到 bridge 都挂一次接收端：bridge 是 start() 时惰性创建的，而控制台
    // 事件要走 stdout 通知线回 Go（那里已支持 {event, payload} 信封）。
    if (bridge !== undefined && this.consoleSink !== undefined) {
      bridge.onConsole = this.consoleSink;
    }
    return bridge;
  }

  private async sendCdpCommand(params: Record<string, unknown>): Promise<unknown> {
    const method = params.method;
    if (typeof method !== "string" || method.length === 0) {
      throw new RpcFailure(1000, "cdp.command requires a method", false);
    }
    const commandParams = isObject(params.params) ? params.params : {};
    const timeoutMs = readTimeout(params.timeoutMs);
    return this.bridge().sendCommand(method, commandParams, timeoutMs);
  }

  /** cdp.debug: the debugger session snapshot; enable=true also flips on
   * Debugger.enable so paused / scriptParsed events start flowing. */
  private debugState(params: Record<string, unknown>): unknown {
    const bridge = this.bridge();
    if (params.enable === true) {
      bridge.enableDebugger();
    }
    return bridge.debugState();
  }

  private async evaluate(params: Record<string, unknown>): Promise<unknown> {
    const expression = params.expression;
    if (typeof expression !== "string" || expression.length === 0) {
      throw new RpcFailure(1000, "runtime.evaluate requires an expression", false);
    }
    const timeoutMs = readTimeout(params.timeoutMs);
    const response = await this.bridge().sendCommand(
      "Runtime.evaluate",
      { expression, returnByValue: true, awaitPromise: params.awaitPromise === true },
      timeoutMs,
    );
    const evaluateResult = (response as {
      result?: {
        result?: { value?: unknown };
        exceptionDetails?: { exception?: { description?: string; text?: string } };
      };
    }).result;
    // Surface page-side exceptions the same way evaluateRaw does; swallowing
    // them made rejected promises look like successful null results.
    if (evaluateResult?.exceptionDetails) {
      const detail = evaluateResult.exceptionDetails.exception;
      throw new RpcFailure(2001, detail?.description ?? detail?.text ?? "evaluation failed in page", true);
    }
    return { value: evaluateResult?.result?.value };
  }

  private async installHook(params: Record<string, unknown>): Promise<unknown> {
    const name = this.readHookName(params);
    // Inject the hook script first (an idempotent IIFE guarded by window
    // flags), then invoke the name-specific install surface.
    const source = await this.loadHookSource(name);
    await this.evaluateRaw(source);
    const report = await this.evaluateHookExpression(`JSON.stringify(${installExpression(name)})`);
    // console 钩子覆盖了哪些类别要告诉 CDP 事件源：页内已覆盖的类别再上报，
    // 同一次调用/异常就会两份入环。
    if (name === "console" && typeof report === "object" && report !== null) {
      const coverage = report as { ok?: unknown; hookedConsoles?: unknown; unwrappableConsoles?: unknown; errorsHooked?: unknown };
      this.bridgeOptional()?.setConsolePageCoverage(coverage);
    }
    return report;
  }

  private async drainHook(params: Record<string, unknown>): Promise<unknown> {
    const name = this.readHookName(params);
    const afterSeq = readIndex(params.afterSeq, "afterSeq");
    // limit=0 would make the page-side drain return hasMore without
    // advancing the sequence, spinning any hasMore-driven paginator forever.
    const limit = readIndex(params.limit, "limit", 1);
    // The update stream is an additive part of the drain page: callers that
    // predate it omit both parameters and get the page-side defaults (0 / 200),
    // while an explicit value is validated like every other index. updateLimit
    // 0 is legal and means "records only" — the page then leaves the update
    // cursor untouched instead of advancing it past updates nobody read.
    const afterUpdateSeq = params.afterUpdateSeq === undefined
      ? 0
      : readIndex(params.afterUpdateSeq, "afterUpdateSeq");
    const updateLimit = params.updateLimit === undefined
      ? 200
      : readIndex(params.updateLimit, "updateLimit");
    return this.evaluateHookExpression(
      `JSON.stringify(${drainExpression(name)}(${afterSeq}, ${limit}, ${afterUpdateSeq}, ${updateLimit}))`,
    );
  }

  private async scanCloud(): Promise<unknown> {
    // Live cloud.scan uses the injected __wxAppCode__ scanner
    // (window.cloudAudit.scanCloudFunctions).
    await this.installHook({ name: "cloud" });
    return this.evaluateHookExpression("JSON.stringify(window.cloudAudit.scanCloudFunctions())");
  }

  private async clearHook(params: Record<string, unknown>): Promise<unknown> {
    const name = this.readHookName(params);
    if (CONTROL_HOOKS.has(name)) {
      throw new RpcFailure(1000, `hook ${name} does not support clear`, false);
    }
    const target = recordHook(name).object;
    return this.evaluateHookExpression(`JSON.stringify({ok:(${target}.clearHookedCalls(),true)})`);
  }

  private async uninstallHook(params: Record<string, unknown>): Promise<unknown> {
    const name = this.readHookName(params);
    if (CONTROL_HOOKS.has(name)) {
      throw new RpcFailure(1000, `hook ${name} does not support uninstall`, false);
    }
    const surface = recordHook(name);
    return this.evaluateHookExpression(`JSON.stringify({ok:(${surface.object}.${surface.uninstall}(),true)})`);
  }

  private loadHookSource(name: string): Promise<string> {
    if (!this.loadHookScript) {
      return Promise.reject(new RpcFailure(2000, "hook script loader unavailable", true));
    }
    return Promise.resolve(this.loadHookScript(name)).catch((error: unknown) => {
      const message = error instanceof Error ? error.message : String(error);
      throw new RpcFailure(2000, `failed to load hook script ${name}: ${message}`, true);
    });
  }

  private async evaluateHookExpression(expression: string): Promise<unknown> {
    const value = await this.evaluateRaw(expression);
    if (typeof value !== "string") {
      throw new RpcFailure(2001, "hook expression returned no value", true);
    }
    try {
      return JSON.parse(value);
    } catch {
      throw new RpcFailure(2001, "hook returned invalid JSON", true);
    }
  }

  private async evaluateRaw(expression: string): Promise<unknown> {
    const timeoutMs = 10000;
    const response = await this.bridge().sendCommand(
      "Runtime.evaluate",
      { expression, returnByValue: true },
      timeoutMs,
    );
    const evaluateResult = (response as {
      result?: {
        result?: { value?: unknown };
        exceptionDetails?: { exception?: { description?: string; text?: string } };
      };
    }).result;
    if (evaluateResult?.exceptionDetails) {
      const detail = evaluateResult.exceptionDetails.exception;
      throw new RpcFailure(2001, detail?.description ?? detail?.text ?? "hook evaluation failed in page", true);
    }
    return evaluateResult?.result?.value;
  }

  private readHookName(params: Record<string, unknown>): string {
    const name = params.name;
    if (typeof name !== "string" || !HOOK_NAMES.has(name)) {
      throw new RpcFailure(1000, `hook name must be one of ${[...HOOK_NAMES].join(", ")}`, false);
    }
    return name;
  }

  private readClientId(params: Record<string, unknown>): number {
    const id = params.id;
    if (typeof id !== "number" || !Number.isInteger(id) || id < 1) {
      throw new RpcFailure(1000, "id must be a positive integer", false);
    }
    return id;
  }

  private readLockEnabled(params: Record<string, unknown>): boolean {
    const enabled = params.enabled;
    if (typeof enabled !== "boolean") {
      throw new RpcFailure(1000, "enabled must be a boolean", false);
    }
    return enabled;
  }

  private readFormatContent(params: Record<string, unknown>): string {
    const content = params.content;
    if (typeof content !== "string") {
      throw new RpcFailure(1000, "content must be a string", false);
    }
    return content;
  }

  private readLanguage(params: Record<string, unknown>): string {
    const language = params.language;
    return typeof language === "string" && language.length > 0 ? language : "text";
  }

  private readCdpPort(params: Record<string, unknown>): number {
    const cdpPort = params.cdpPort;
    if (typeof cdpPort !== "number" || !Number.isInteger(cdpPort) || cdpPort < 1 || cdpPort > 65535) {
      throw new Error("cdpPort must be a valid TCP port");
    }
    return cdpPort;
  }
}

function readTimeout(value: unknown): number {
  if (typeof value !== "number" || !Number.isFinite(value) || value <= 0) {
    return 5000;
  }
  return value;
}

function readIndex(value: unknown, label: string, min = 0): number {
  if (typeof value !== "number" || !Number.isInteger(value) || value < min) {
    throw new RpcFailure(1000, `${label} must be an integer >= ${min}`, false);
  }
  return value;
}

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
