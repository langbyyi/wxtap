import { decodeCdpMessage, encodeCdpMessage } from "../protocol/wmpf-codec.js";

export type Peer = {
  send(message: Buffer | string): void;
};

export type MiniappEntry = {
  id: number;
  appid: string;
  name: string;
  locked: boolean;
};

export type AppInfo = { appid: string; name: string; icon?: string };

type PendingCommand = {
  resolve(value: Record<string, unknown>): void;
  reject(reason: Error): void;
  timeout: ReturnType<typeof setTimeout>;
};

/** One parsed script the target reported via Debugger.scriptParsed. */
export type DebuggerScript = {
  scriptId: string;
  url: string;
};

/** One scope of a paused frame: objectId is what Runtime.getProperties reads,
 * and it is only valid while the pause holds. */
export type DebuggerScope = {
  type: string;
  objectId?: string;
};

/** One call frame of a paused debug session, trimmed to what an MCP agent reads. */
export type DebuggerFrame = {
  callFrameId: string;
  functionName: string;
  url: string;
  lineNumber: number;
  columnNumber: number;
  scopes: DebuggerScope[];
};

/** Snapshot of the debug session on the active miniapp target. */
export type DebugState = {
  enabled: boolean;
  paused: boolean;
  pausedSeq: number;
  resumedSeq: number;
  pausedAt: number;
  reason: string;
  callFrames: DebuggerFrame[];
  scripts: DebuggerScript[];
  scriptsTruncated: number;
};

/** scriptParsed 环形缓冲上限：一个小程序的全部包脚本通常几十到几百个。 */
const MAX_DEBUG_SCRIPTS = 500;
/** paused 事件的帧数上限：栈再深，agent 读的也只是前几帧。 */
const MAX_DEBUG_FRAMES = 25;

type MiniappConnection = {
  peer: Peer;
  appid: string;
  name: string;
  icon: string;
  probed: boolean;
};

export type CdpBridgeOptions = {
  /** Delay before probing a fresh miniapp for its identity; also spaces retries. */
  probeDelayMs?: number;
};

/** Identity probe: read the account nickname from the miniapp's own config.
 * WMPF puts the display name on accountInfo (nickname/nickName + icon), and on
 * current builds the config lives only on nav.wxFrame: the page-level
 * window.__wxConfig is undefined there, which is what used to leave the target
 * title ("AppIndex") standing in as the name. Older builds carry the same
 * fields one level deeper, under accountInfo.appAccount / appContactInfo. */
export const APP_INFO_EXPRESSION = "(function(){function s(v){return typeof v==='string'?v:''}function nameOf(c){var ai=c.accountInfo||{};var aa=ai.appAccount||{};var ac=c.appContactInfo||{};return s(ai.nickname)||s(ai.nickName)||s(aa.nickname)||s(aa.nickName)||s(ac.nickname)||s(ac.nickName)||s(c.appname)||s(c.appName)||s(c.nickname)||s(c.nickName)}function iconOf(c){var ai=c.accountInfo||{};var aa=ai.appAccount||{};var ac=c.appContactInfo||{};return s(ai.icon)||s(aa.icon)||s(ac.icon)||s(ac.iconUrl)}function cfg(){try{var f=window.nav&&window.nav.wxFrame;if(f&&f.__wxConfig)return f.__wxConfig}catch(e){}try{if(window.__wxConfig)return window.__wxConfig}catch(e){}try{if(window.wx&&window.wx.__wxConfig)return window.wx.__wxConfig}catch(e){}try{var p=window.parent;if(p&&p!==window&&p.__wxConfig)return p.__wxConfig}catch(e){}return null}try{var c=cfg();if(c){var ai=c.accountInfo||{};var aa=ai.appAccount||{};return JSON.stringify({appid:s(aa.appId)||s(ai.appId)||s(c.appid),name:nameOf(c),icon:iconOf(c)})}var info=window.wx&&window.wx.getAccountInfoSync&&window.wx.getAccountInfoSync();var mp=info&&info.miniProgram;if(mp&&mp.appId)return JSON.stringify({appid:s(mp.appId),name:s(mp.nickname)||s(mp.nickName),icon:s(mp.icon)});return JSON.stringify({})}catch(e){return JSON.stringify({})}})()";

/** Target titles that are the shell, not the miniapp's own name. AppIndex and
 * GameIndex are WMPF's names for the appservice page frame itself (the mini
 * program's entry page frame), the same way 小程序 is the tab's. */
const GENERIC_TARGET_TITLES = new Set(["微信", "小程序", "wechat", "weixin", "about:blank", "appindex", "gameindex"]);

export function miniappNameFromTargetTitle(title: unknown, appid: string): string {
  if (typeof title !== "string") {
    return "";
  }
  const name = title.trim();
  if (name === "" || name === appid || GENERIC_TARGET_TITLES.has(name.toLowerCase())) {
    return "";
  }
  if (/^https?:\/\//i.test(name) || name.includes("page-frame.html") || name.includes("servicewechat.com")) {
    return "";
  }
  return name;
}

/** WMPF exposes each applet's service context as a page whose URL embeds the
 * appid (servicewechat.com/<appid>/<version>/page-frame.html); preload pages
 * share the shape with a non-appid segment and do not match. */
const APPID_TARGET_URL = /servicewechat\.com\/(wx[0-9a-f]{6,})\/\d+\/page-frame\.html/;

export class CdpBridge {
  /**
   * 小程序的 console 在这个 WMPF 版本里是 configurable:false 的访问器（真机实测
   * `Cannot redefine property: log`），页内 hook 包不上；但 CDP 的
   * Runtime.consoleAPICalled 事件不受页面实现影响，所以控制台采集走这里。
   */
  onConsole: ((entry: Record<string, unknown>) => void) | undefined;

  private readonly consoleEnabled = new Set<number>();
  private readonly debuggerEnabled = new Set<number>();
  // 页内 console 钩子对本 realm 的覆盖范围：errors 在 install() 成功后即成立
  // （hookErrors 无条件挂载），full 还要求 console.* 本身包得上（WMPF 锁住
  // console 时页内只剩错误钩子）。CDP 事件源对页内已覆盖的类别不再重复上报。
  private consolePageErrors = false;
  private consolePageFull = false;
  private debugWanted = false;
  private debugPaused = false;
  private debugPausedSeq = 0;
  private debugResumedSeq = 0;
  private debugPausedAt = 0;
  private debugReason = "";
  private debugCallFrames: DebuggerFrame[] = [];
  private readonly debugScripts = new Map<string, DebuggerScript>();
  private debugScriptsTruncated = 0;
  private readonly miniapps = new Map<number, MiniappConnection>();
  private readonly devtools = new Set<Peer>();
  private readonly pending = new Map<number, PendingCommand>();
  private nextSequence = 0;
  private nextCommandId = 80000;
  private nextClientId = 0;
  private pageGeneration = 0;
  private lockEnabled = true;
  private lockedClientId: number | undefined;
  private readonly probeDelayMs: number;

  constructor(options: CdpBridgeOptions = {}) {
    this.probeDelayMs = options.probeDelayMs ?? 1500;
  }

  /** Registers a miniapp connection; the newest one becomes the lock target
   * when locking is on and no connection is currently locked.
   * With the lock disabled the fan-out state is preserved for new peers. */
  addMiniapp(peer: Peer): number {
    this.nextClientId += 1;
    const id = this.nextClientId;
    this.miniapps.set(id, { peer, appid: "", name: "", icon: "", probed: false });
    if (this.lockEnabled && this.lockedClientId === undefined) {
      this.lockedClientId = id;
    }
    // cdp.debug 可能先于任何连接到达（debugWanted 已置位）：连接成为活动目标
    // 的这一刻必须真正 enable，否则 debugState 会一直挂着 enabled 的空承诺。
    this.enableDebuggerIfWanted(id, !this.lockEnabled || this.lockedClientId === undefined || this.lockedClientId === id);
    return id;
  }

  removeMiniapp(id: number): void {
    this.consoleEnabled.delete(id);
    this.debuggerEnabled.delete(id);
    if (this.miniapps.delete(id) && this.lockedClientId === id) {
      this.lockedClientId = undefined;
      // 锁定目标下线：覆盖标志随它的 realm 一起作废（同 setupContext 分支）。
      this.resetConsolePageCoverage();
    }
  }

  listMiniapps(): MiniappEntry[] {
    return [...this.miniapps.entries()].map(([id, entry]) => ({
      id,
      appid: entry.appid,
      name: entry.name,
      locked: this.lockedClientId === id,
    }));
  }

  switchMiniapp(id: number): boolean {
    if (!this.miniapps.has(id)) {
      return false;
    }
    // An explicit switch is the user re-locking; it must not leave
    // getLock reporting "disabled" while the routing is pinned to one peer.
    // A real target change is also a new debug generation, so the desktop
    // status poller reinstall hooks in the newly selected realm.
    const switching = this.lockedClientId !== id;
    if (switching) {
      this.pageGeneration += 1;
      // 页内覆盖标志描述的是旧锁定 realm：新 realm 还没装过页内钩子，不清零的
      // 话 CDP 事件源会按旧状态丢掉新 realm 的 console 事件，直到重装才自愈。
      this.resetConsolePageCoverage();
    }
    this.lockEnabled = true;
    this.lockedClientId = id;
    // The debug session may have been requested before this target was
    // locked (its realm connected in the background): the newly locked
    // realm must really enable, or debugState would advertise an empty on.
    this.enableDebuggerIfWanted(id, true);
    // 同理 console：后台连接当时可能没 enable 成功（回复被锁定过滤等），切换
    // 成为主目标这一刻补一次 enable（每连接只发一次，已启用时是空操作）。
    this.enableConsoleEvents(id);
    return true;
  }

  setLock(enabled: boolean): void {
    this.lockEnabled = enabled;
    if (!enabled) {
      this.lockedClientId = undefined;
      // 解锁后事件来自所有连接：旧 realm 的覆盖状态对新来源一无所知，
      // 宁可让 CDP 多报（有 hook.install 的回执再压回去），也不能漏报。
      this.resetConsolePageCoverage();
      return;
    }
    // Re-locking without a target must not silently resume fan-out: pin the
    // most recently connected miniapp, or (none yet) let addMiniapp pin the
    // next connection.
    if (this.lockedClientId === undefined) {
      const ids = [...this.miniapps.keys()];
      if (ids.length > 0) {
        this.lockedClientId = ids[ids.length - 1];
        this.pageGeneration += 1;
        this.resetConsolePageCoverage();
      }
    }
  }

  isLockEnabled(): boolean {
    return this.lockEnabled;
  }

  setMiniappInfo(id: number, appid: string, name: string, icon = ""): void {
    const entry = this.miniapps.get(id);
    if (entry) {
      entry.appid = appid;
      entry.name = name;
      entry.icon = icon;
    }
  }

  onAppInfo: ((info: AppInfo) => void) | undefined;

  lastAppInfo(): AppInfo | undefined {
    const source = this.lockedClientId !== undefined && this.miniapps.has(this.lockedClientId)
      ? this.miniapps.get(this.lockedClientId)
      : [...this.miniapps.values()].find((entry) => entry.appid !== "");
    if (source === undefined || (source.appid === "" && source.name === "")) {
      return undefined;
    }
    return { appid: source.appid, name: source.name };
  }

  addDevtools(peer: Peer): void {
    this.devtools.add(peer);
  }

  removeDevtools(peer: Peer): void {
    this.devtools.delete(peer);
  }

  connectionStatus(): { miniapp: boolean; devtools: boolean } {
    return { miniapp: this.miniapps.size > 0, devtools: this.devtools.size > 0 };
  }

  generation(): number {
    return this.pageGeneration;
  }

  forwardDevtools(payload: string): void {
    this.sendToActive(payload, this.nextCommandId++);
  }

  sendCommand(method: string, params: Record<string, unknown>, timeoutMs: number): Promise<Record<string, unknown>> {
    if (this.miniapps.size === 0) {
      return Promise.reject(new Error("no miniapp connected"));
    }
    const id = this.nextCommandId++;
    const payload = JSON.stringify({ id, method, params });
    this.sendToActive(payload, id);
    return new Promise((resolve, reject) => {
      const timeout = setTimeout(() => {
        this.pending.delete(id);
        reject(new Error(`CDP command timed out: ${method}`));
      }, timeoutMs);
      this.pending.set(id, { resolve, reject, timeout });
    });
  }

  receiveMiniapp(frame: Uint8Array, clientId?: number): void {
    let message: ReturnType<typeof decodeCdpMessage>;
    try {
      message = decodeCdpMessage(frame);
    } catch {
      // A truncated or corrupt protobuf frame from the wire; drop it rather
      // than letting the exception escape into the socket callback.
      return;
    }
    if (message === undefined) {
      return;
    }
    if (message.category === "setupContext" && clientId !== undefined) {
      // Only the active realm drives hook auto-injection. A background
      // miniapp refreshing must not make the status poller reinstall hooks
      // in the locked realm.
      const activeRealm = !this.lockEnabled || this.lockedClientId === undefined || clientId === this.lockedClientId;
      if (activeRealm) {
        this.pageGeneration += 1;
      }
      // 新 realm 的页内 console 钩子随旧 realm 一起消失了：覆盖标志归零，
      // 在 hook.install 重装成功之前 CDP 事件源照常上报，错误不会丢。
      this.resetConsolePageCoverage();
      this.scheduleIdentityProbe(clientId);
      this.enableConsoleEvents(clientId);
    }
    if (message.category !== "chromeDevtoolsResult") {
      return;
    }
    // Locking is an isolation boundary, not only an outbound routing rule:
    // unsolicited results from another miniapp must not reach DevTools or
    // resolve commands that were sent to the locked target.
    if (clientId !== undefined && this.lockEnabled && this.lockedClientId !== undefined && clientId !== this.lockedClientId) {
      return;
    }
    this.broadcast(message.payload);
    const response = parseObject(message.payload);
    if (response === undefined) {
      return;
    }
    if (typeof response.method === "string") {
      this.captureDebuggerEvent(response);
      this.forwardConsoleEvent(response, clientId);
      return;
    }
    if (typeof response.id !== "number") {
      return;
    }
    const id = response.id;
    const pending = this.pending.get(id);
    if (pending === undefined) {
      return;
    }
    clearTimeout(pending.timeout);
    this.pending.delete(id);
    pending.resolve(response);
  }

  /** 让 miniapp 连接开始上报 console 事件（每个连接一次；realm 重建后事件照旧）。
   * 必须发给这条连接本身（Runtime 域按连接开关）：sendCommand 会路由到锁定的
   * 活动目标 —— 锁在 A 时新连上的 B 永远收不到 enable，等用户切到 B，这里又因
   * 已标记而跳过，console 采集就静默失效了。 */
  private enableConsoleEvents(clientId: number): void {
    if (this.consoleEnabled.has(clientId)) {
      return;
    }
    this.consoleEnabled.add(clientId);
    void this.sendCommandTo(clientId, "Runtime.enable", {}, 8000).catch(() => {
      // 启用失败不致命：事件收不到时界面就是没有 console 输出，不影响其它功能。
      // 标记撤掉，让该连接下次 setupContext（或被切换为锁定目标）时重试。
      this.consoleEnabled.delete(clientId);
    });
  }

  /** hook.install(console) 的回执：告诉 CDP 事件源页内钩子覆盖了哪些类别，
   * 已覆盖的类别不再重复上报。realm 重建时由 setupContext 归零。 */
  setConsolePageCoverage(report: { ok?: unknown; hookedConsoles?: unknown; unwrappableConsoles?: unknown; errorsHooked?: unknown }): void {
    const hooked = typeof report.hookedConsoles === "number" ? report.hookedConsoles : 0;
    const unwrappable = typeof report.unwrappableConsoles === "number" ? report.unwrappableConsoles : 0;
    // 错误出口（onerror / unhandledrejection）与 console 是否可包无关：WMPF 锁住
    // console 时 install 报 ok:false，但 hookErrors 已挂上、错误记录会随 drain
    // 进环 —— 这时 CDP 的 exceptionThrown 必须闭嘴，否则每次异常两份。
    const errorsHooked = report.errorsHooked === true || report.ok === true;
    this.consolePageErrors = errorsHooked;
    this.consolePageFull = report.ok === true && hooked > 0 && unwrappable === 0;
  }

  /** 页内覆盖标志整体作废：锁定目标变化（切换/解锁/下线）时，旧 realm 的状态
   * 不能算到新来源头上。清零后 CDP 照常上报 —— 宁可等 hook.install 的回执来
   * 恢复去重，也不能按旧状态把新 realm 的记录丢掉。 */
  private resetConsolePageCoverage(): void {
    this.consolePageErrors = false;
    this.consolePageFull = false;
  }

  /** Debug session requested: sends Debugger.enable on the active target once
   * per connection, so paused / scriptParsed events start flowing. Enable is
   * idempotent at the protocol level; the set only avoids a repeat round trip.
   * The request can arrive before any miniapp connected — debugWanted records
   * that intent and every later active realm enables on arrival, otherwise
   * debugState would keep advertising enabled:true with nothing attached. */
  enableDebugger(): void {
    this.debugWanted = true;
    const id = this.activeDebugTarget();
    if (id !== undefined) {
      this.enableDebuggerOnConnection(id);
    }
  }

  /** The target Debugger commands route to: the locked miniapp, or the most
   * recent connection when fan-out is on. */
  private activeDebugTarget(): number | undefined {
    if (this.miniapps.size === 0) {
      return undefined;
    }
    return this.lockedClientId ?? [...this.miniapps.keys()][this.miniapps.size - 1];
  }

  /** A realm (re)connection only enables the debugger when a session was
   * requested earlier and the connection is the active target — the same
   * active-realm rule the page-generation bump above uses. */
  enableDebuggerIfWanted(clientId: number, activeRealm: boolean): void {
    if (!this.debugWanted || !activeRealm) {
      return;
    }
    this.enableDebuggerOnConnection(clientId);
  }

  private enableDebuggerOnConnection(id: number): void {
    if (this.debuggerEnabled.has(id)) {
      return;
    }
    this.debuggerEnabled.add(id);
    void this.sendCommand("Debugger.enable", {}, 8000).catch(() => {
      // 与 console 同一策略：启用失败只意味着拿不到调试事件，不拖垮其它功能。
      this.debuggerEnabled.delete(id);
    });
  }

  /** Snapshot of the debug session on the active target. */
  debugState(): DebugState {
    return {
      enabled: this.debugWanted,
      paused: this.debugPaused,
      pausedSeq: this.debugPausedSeq,
      resumedSeq: this.debugResumedSeq,
      pausedAt: this.debugPausedAt,
      reason: this.debugReason,
      callFrames: this.debugCallFrames,
      scripts: [...this.debugScripts.values()],
      scriptsTruncated: this.debugScriptsTruncated,
    };
  }

  /** Files the paused / resumed / scriptParsed events into bounded state for
   * cdp.debug readers. Events from non-locked connections never reach here —
   * receiveMiniapp drops them before the event branch. */
  private captureDebuggerEvent(message: Record<string, unknown>): void {
    const method = message.method;
    if (typeof method !== "string" || !method.startsWith("Debugger.")) {
      return;
    }
    const params = isRecord(message.params) ? message.params : {};
    if (method === "Debugger.scriptParsed") {
      const scriptId = textOf(params.scriptId);
      if (scriptId === "") {
        return;
      }
      if (!this.debugScripts.has(scriptId) && this.debugScripts.size >= MAX_DEBUG_SCRIPTS) {
        const oldest = this.debugScripts.keys().next().value;
        if (oldest !== undefined) {
          this.debugScripts.delete(oldest);
          this.debugScriptsTruncated += 1;
        }
      }
      this.debugScripts.set(scriptId, { scriptId, url: textOf(params.url) });
      return;
    }
    if (method === "Debugger.paused") {
      this.debugPausedSeq += 1;
      this.debugPaused = true;
      this.debugPausedAt = Date.now();
      this.debugReason = textOf(params.reason, "other");
      const rawFrames = Array.isArray(params.callFrames) ? params.callFrames : [];
      this.debugCallFrames = rawFrames.slice(0, MAX_DEBUG_FRAMES).flatMap((frame) => {
        if (!isRecord(frame)) {
          return [];
        }
        const scopeChain = Array.isArray(frame.scopeChain) ? frame.scopeChain : [];
        return [{
          callFrameId: textOf(frame.callFrameId),
          functionName: textOf(frame.functionName, "(anonymous)"),
          url: textOf(frame.url),
          lineNumber: typeof frame.lineNumber === "number" ? frame.lineNumber : 0,
          columnNumber: typeof frame.columnNumber === "number" ? frame.columnNumber : 0,
          scopes: scopeChain.flatMap((scope) => {
            if (!isRecord(scope) || typeof scope.type !== "string") {
              return [];
            }
            return [{
              type: scope.type,
              ...(typeof scope.object === "object" && scope.object !== null
                && typeof (scope.object as Record<string, unknown>).objectId === "string"
                ? { objectId: (scope.object as Record<string, unknown>).objectId as string }
                : {}),
            }];
          }),
        }];
      });
      return;
    }
    if (method === "Debugger.resumed") {
      this.debugPaused = false;
      this.debugResumedSeq += 1;
      // 恢复后旧帧的 objectId 已失效，留着只会让 agent 拿它去求值然后失败。
      this.debugCallFrames = [];
      this.debugReason = "";
      return;
    }
    if (method === "Debugger.globalObjectCleared") {
      // 导航 / realm 重建后旧脚本清单整体作废。
      this.debugScripts.clear();
      this.debugScriptsTruncated = 0;
    }
  }

  /**
   * 把 CDP 的 console 相关事件规范成与控制台面板一致的记录形状
   * （{type, level, text, appId, timestamp, ts}，与页内 hook 的记录一致）。
   */
  private forwardConsoleEvent(message: Record<string, unknown>, clientId?: number): void {
    if (this.onConsole === undefined) {
      return;
    }
    const appId = clientId === undefined ? "" : this.miniapps.get(clientId)?.appid ?? "";
    const method = message.method;
    const params = isRecord(message.params) ? message.params : {};
    if (method === "Runtime.consoleAPICalled") {
      // 页内钩子把 console.* 完整包上时 CDP 的同名事件是重复上报（同一次调用
      // 两份入环）；只有 WMPF 锁住 console（页内包不上）时 CDP 才是唯一来源。
      if (this.consolePageFull) {
        return;
      }
      const level = consoleLevel(textOf(params.type, "log"));
      this.onConsole({
        type: "console",
        level,
        text: describeConsoleArgs(params.args),
        appId,
        timestamp: new Date().toLocaleTimeString(),
        ts: typeof params.timestamp === "number" ? Math.round(params.timestamp) : Date.now(),
      });
      return;
    }
    if (method === "Runtime.exceptionThrown") {
      // 页内 console 钩子的 hookErrors 对未捕获异常/Promise 拒绝是无条件挂载的，
      // install 一旦成功就覆盖了这条事件：CDP 再报一份就是双份入环。realm 重建
      // 会把覆盖标志归零，重装成功前这里照常上报，错误不会丢。
      if (this.consolePageErrors) {
        return;
      }
      const details = isRecord(params.exceptionDetails) ? params.exceptionDetails : {};
      const exception = isRecord(details.exception) ? details.exception : {};
      const text = textOf(exception.description) || textOf(details.text) || "未捕获异常";
      this.onConsole({
        type: "console",
        level: "error",
        text,
        appId,
        timestamp: new Date().toLocaleTimeString(),
        ts: Date.now(),
        extra: {
          source: textOf(details.url),
          line: textOf(details.lineNumber),
          // 页内 hook 的错误记录带 column；CDP 路径补齐同一形状，面板的
          // extraText 逐项渲染，缺省值不显示。
          column: textOf(details.columnNumber),
        },
      });
      return;
    }
    if (method === "Log.entryAdded") {
      // 不主动发 Log.enable：Runtime 与 Log 同时开启时同一行输出可能两份入环。
      // 这里只接住目标自己推上来的条目（个别 WMPF 构建会主动推浏览器级错误）。
      const entry = isRecord(params.entry) ? params.entry : {};
      this.onConsole({
        type: "console",
        level: consoleLevel(textOf(entry.level, "info")),
        text: textOf(entry.text),
        appId,
        timestamp: new Date().toLocaleTimeString(),
        ts: typeof entry.timestamp === "number" ? Math.round(entry.timestamp) : Date.now(),
      });
    }
  }

  private scheduleIdentityProbe(id: number): void {
    const entry = this.miniapps.get(id);
    if (entry === undefined || entry.probed) {
      return;
    }
    entry.probed = true;
    setTimeout(() => {
      void this.probeIdentity(id);
    }, this.probeDelayMs);
  }

  private async probeIdentity(id: number): Promise<void> {
    const entry = this.miniapps.get(id);
    if (entry === undefined) {
      return;
    }
    // The appservice JS realm is not CDP-reachable on current WMPF builds, so
    // __wxConfig cannot be evaluated there; the appid is instead read from the
    // browser target list, whose appservice page URLs embed it. Targets may
    // not be registered yet right after connect, so retry a few times before
    // falling back to the page-side __wxConfig expression.
    let targetAppID = "";
    let targetId = "";
    for (let attempt = 0; attempt < 3; attempt += 1) {
      if (attempt > 0) {
        await new Promise((resolve) => setTimeout(resolve, this.probeDelayMs));
      }
      if (this.miniapps.get(id) === undefined) {
        return;
      }
      const hit = await this.appidFromTargets(id);
      if (hit !== undefined) {
        targetAppID = hit.appid;
        targetId = hit.targetId;
        // 目标标题经常就是小程序名。__wxConfig 昵称稍后能覆盖它；空结果不能把名字清掉。
        this.publishIdentity(id, hit.appid, miniappNameFromTargetTitle(hit.title, hit.appid));
        break;
      }
    }
    await this.probeIdentityExpression(id, targetAppID, targetId);
  }

  /** Reads Target.getTargets through the miniapp's own browser connection and
   * extracts the first appservice URL appid not already claimed by another
   * entry (the browser-wide list covers every running applet). */
  private async appidFromTargets(id: number): Promise<{ appid: string; title: string; targetId: string } | undefined> {
    try {
      const response = await this.sendCommandTo(id, "Target.getTargets", {}, 8000);
      const infos = (response.result as { targetInfos?: Array<{ url?: unknown; title?: unknown; targetId?: unknown }> } | undefined)?.targetInfos;
      if (!Array.isArray(infos)) {
        return undefined;
      }
      const claimed = new Set([...this.miniapps.values()].map((item) => item.appid).filter((appid) => appid !== ""));
      for (const info of infos) {
        const match = typeof info.url === "string" ? APPID_TARGET_URL.exec(info.url) : null;
        if (match !== null && !claimed.has(match[1])) {
          return {
            appid: match[1],
            title: typeof info.title === "string" ? info.title : "",
            targetId: typeof info.targetId === "string" ? info.targetId : "",
          };
        }
      }
    } catch {
      // The connection may be gone or the browser busy; retried by the caller.
    }
    return undefined;
  }

  private async probeIdentityExpression(id: number, fallbackAppID = "", targetId = ""): Promise<void> {
    const entry = this.miniapps.get(id);
    if (entry === undefined) {
      return;
    }
    try {
      const response = await this.evaluateIdentity(id, targetId);
      const evaluateResult = (response as { result?: { result?: { value?: unknown } } }).result;
      const value = evaluateResult?.result?.value;
      if (typeof value !== "string") {
        return;
      }
      const info = parseObject(value);
      if (info === undefined) {
        return;
      }
      const appid = typeof info.appid === "string" ? info.appid : "";
      const name = typeof info.name === "string" ? info.name : "";
      const icon = typeof info.icon === "string" ? info.icon : "";
      const resolvedAppID = appid || fallbackAppID;
      if (resolvedAppID === "" && name === "" && icon === "") {
        return;
      }
      this.publishIdentity(id, resolvedAppID, name, icon);
    } catch {
      if (fallbackAppID !== "") {
        this.publishIdentity(id, fallbackAppID, "");
      }
    }
  }

  /** Browser-level evaluate misses __wxConfig. Attach to the page target when we have one. */
  private async evaluateIdentity(id: number, targetId: string): Promise<Record<string, unknown>> {
    if (targetId !== "") {
      try {
        const attached = await this.sendCommandTo(id, "Target.attachToTarget", { targetId, flatten: true }, 8000);
        const sessionId = sessionIdOf(attached);
        if (sessionId !== "") {
          try {
            return await this.sendCommandTo(id, "Runtime.evaluate", {
              expression: APP_INFO_EXPRESSION,
              returnByValue: true,
            }, 8000, sessionId);
          } finally {
            void this.sendCommandTo(id, "Target.detachFromTarget", { sessionId }, 2000).catch(() => undefined);
          }
        }
      } catch {
        // The page target did not attach. The browser context is the remaining probe.
      }
    }
    return this.sendCommandTo(id, "Runtime.evaluate", {
      expression: APP_INFO_EXPRESSION,
      returnByValue: true,
    }, 8000);
  }

  private publishIdentity(id: number, appid: string, name: string, icon = ""): void {
    const entry = this.miniapps.get(id);
    if (entry === undefined || appid === "") {
      return;
    }
    const nextName = name !== "" ? name : entry.name;
    const nextIcon = icon !== "" ? icon : entry.icon;
    if (entry.appid === appid && entry.name === nextName && entry.icon === nextIcon) {
      return;
    }
    this.setMiniappInfo(id, appid, nextName, nextIcon);
    this.onAppInfo?.({ appid, name: nextName, ...(nextIcon !== "" ? { icon: nextIcon } : {}) });
  }

  /** Sends a command to one specific miniapp connection. */
  private sendCommandTo(id: number, method: string, params: Record<string, unknown>, timeoutMs: number, sessionId = ""): Promise<Record<string, unknown>> {
    const entry = this.miniapps.get(id);
    if (entry === undefined) {
      return Promise.reject(new Error("no miniapp connected"));
    }
    const commandId = this.nextCommandId++;
    const command: Record<string, unknown> = { id: commandId, method, params };
    if (sessionId !== "") {
      command.sessionId = sessionId;
    }
    const payload = JSON.stringify(command);
    this.sendTo(entry.peer, payload, commandId);
    return new Promise((resolve, reject) => {
      const timeout = setTimeout(() => {
        this.pending.delete(commandId);
        reject(new Error(`CDP command timed out: ${method}`));
      }, timeoutMs);
      this.pending.set(commandId, { resolve, reject, timeout });
    });
  }

  /** Routing: while a lock target exists only it receives traffic,
   * otherwise every connected miniapp does. Traffic arriving before any
   * miniapp has connected has nowhere to go and is dropped. */
  private sendToActive(payload: string, operationId: number): void {
    if (this.miniapps.size === 0) {
      return;
    }
    const locked = this.lockedClientId !== undefined ? this.miniapps.get(this.lockedClientId) : undefined;
    const targets = locked !== undefined ? [locked] : [...this.miniapps.values()];
    for (const entry of targets) {
      this.sendTo(entry.peer, payload, operationId);
    }
  }

  private sendTo(peer: Peer, payload: string, operationId: number): void {
    this.nextSequence += 1;
    peer.send(encodeCdpMessage({
      sequence: this.nextSequence,
      category: "chromeDevtools",
      operationId,
      payload,
      jsContextId: "",
    }));
  }

  private broadcast(payload: string): void {
    for (const peer of this.devtools) {
      peer.send(payload);
    }
  }
}

/** 面板与页内 hook 共用的五个级别档位。 */
const CONSOLE_LEVELS = new Set(["log", "info", "warn", "error", "debug"]);

/**
 * CDP 的 console 类型名与面板的级别名对齐。级别筛选下拉只提供上面五档，
 * 所以除对齐改名（warning→warn、verbose→debug）外，其余类型都要归到五档里：
 * assert 按语义归 error，dir/table/trace/group 这类呈现型调用归 log —— 否则
 * 会产出下拉选不中、级别计数也不算的"幽灵级别"。
 */
function consoleLevel(type: string): string {
  if (type === "warning") {
    return "warn";
  }
  if (type === "verbose") {
    return "debug";
  }
  if (type === "assert") {
    return "error";
  }
  return CONSOLE_LEVELS.has(type) ? type : "log";
}

/**
 * 单条记录的文本上限，与页内 hook 的 MAX_ARG_TEXT（core/hooks/console.js）
 * 同为 2000：CDP 是 WMPF 上的主采集路径，一条 console.log(巨大字符串/对象)
 * 不截断的话会依次撑爆 stdout 行、环形缓冲和面板 DOM。
 */
const MAX_CONSOLE_TEXT = 2000;

function truncateConsoleText(text: string): string {
  return text.length > MAX_CONSOLE_TEXT ? `${text.slice(0, MAX_CONSOLE_TEXT)}…[截断]` : text;
}

/** Runtime.consoleAPICalled 的参数数组 → 一行文本（对象取 description/preview）。 */
function describeConsoleArgs(args: unknown): string {
  if (!Array.isArray(args)) {
    return "";
  }
  return truncateConsoleText(
    args
      .map((arg) => {
        if (!isRecord(arg)) {
          return String(arg);
        }
        if (arg.value !== undefined) {
          return typeof arg.value === "string" ? arg.value : JSON.stringify(arg.value);
        }
        if (typeof arg.description === "string") {
          return arg.description;
        }
        if (isRecord(arg.preview) && Array.isArray(arg.preview.properties)) {
          return `{${arg.preview.properties
            .map((property) => (isRecord(property) ? `${String(property.name)}: ${String(property.value)}` : ""))
            .join(", ")}}`;
        }
        return textOf(arg.type);
      })
      .join(" "),
  );
}

/** 只把标量转成文本：参数来自 CDP 的 unknown，直接 String() 会得到 [object Object]。 */
function textOf(value: unknown, fallback = ""): string {
  if (typeof value === "string") {
    return value;
  }
  if (typeof value === "number" || typeof value === "boolean") {
    return String(value);
  }
  return fallback;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function sessionIdOf(response: Record<string, unknown>): string {
  const result = response.result;
  if (typeof result !== "object" || result === null) {
    return "";
  }
  const sessionId = (result as { sessionId?: unknown }).sessionId;
  return typeof sessionId === "string" ? sessionId : "";
}

function parseObject(value: string): Record<string, unknown> | undefined {
  try {
    const parsed: unknown = JSON.parse(value);
    return typeof parsed === "object" && parsed !== null && !Array.isArray(parsed)
      ? parsed as Record<string, unknown>
      : undefined;
  } catch {
    return undefined;
  }
}
