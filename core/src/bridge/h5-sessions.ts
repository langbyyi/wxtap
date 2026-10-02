import type { Peer } from "./cdp-bridge.js";

type Packet = Record<string, unknown>;
export type H5SessionState = {
  clientId: number; targetId: string; state: "connecting" | "connected" | "closing" | "closed" | "error";
  active: boolean; error: string; url: string; title: string;
};
type Entry = {
  peer: Peer; info: H5SessionState; generation: number; root: string; children: Map<string, string>; extraRoots: Set<string>; targetGone: boolean; sourceGone: boolean;
  ready: Promise<void>; closed: boolean; attachPending: boolean; closing?: Promise<void>; requests: Set<number>;
  infoRevision: number; infoReading: boolean;
};
type Pending = {
  entry: Entry; method: string; sessionId: string; detachId: string; resolve(packet: Packet): void; reject(error: Error): void;
  timer: ReturnType<typeof setTimeout>; timedOut: boolean;
};
type Options = {
  generation(clientId: number): number;
  allocateId(): number;
  send(clientId: number, packet: Packet): void;
};
const object = (value: unknown): value is Packet => typeof value === "object" && value !== null && !Array.isArray(value);
const text = (value: unknown): string => typeof value === "string" ? value : "";
const message = (error: unknown): string => error instanceof Error ? error.message : String(error);

export const H5_PAGE_INFO_EXPRESSION = "({url:globalThis.location?.href??'',title:globalThis.document?.title??'',readyState:globalThis.document?.readyState??'',hasDocument:typeof document!=='undefined'})";
export function isH5Page(target: Packet): boolean {
  try {
    const url = new URL(text(target.url));
    return ["page", "iframe"].includes(text(target.type)) && ["http:", "https:"].includes(url.protocol) && url.hostname !== "servicewechat.com" &&
      !(url.hostname === "liteapp.weixin.qq.com" && url.pathname === "/");
  } catch { return false; }
}

/** A page endpoint hides its root flattened session, while preserving child
 * session ids used by DevTools for frames/workers. Every command gets a private
 * transport id; a sibling window can use the same frontend ids independently. */
export class H5Sessions {
  private readonly entries = new Map<Peer, Entry>();
  private readonly pending = new Map<number, Pending>();
  private readonly history = new Map<string, H5SessionState>();
  private readonly retired = new Map<number, Set<string>>();
  private readonly destroyed = new Map<number, Set<string>>();
  private readonly exhausted = new Set<number>();

  constructor(private readonly options: Options) {}

  list(): H5SessionState[] { return [...this.history.values()].map((entry) => ({ ...entry })); }
  connected(): boolean { return [...this.entries.values()].some((entry) => entry.info.state === "connected"); }
  owns(clientId: number, targetId: string): boolean {
    return [...this.entries.values()].some((entry) => entry.info.clientId === clientId && entry.info.targetId === targetId);
  }

  open(peer: Peer, clientId: number, targetId: string): Promise<void> {
    if (this.exhausted.has(clientId)) return Promise.reject(new Error("H5 会话记录已达上限，请重新连接小程序调试通道"));
    if (this.owns(clientId, targetId)) return Promise.reject(new Error("该目标已有 H5 调试会话，请先断开或重试释放"));
    if (this.entries.size >= 16) return Promise.reject(new Error("H5 调试窗口已达上限，请先关闭已有窗口"));
    let generation: number;
    try { generation = this.options.generation(clientId); } catch (error) { return Promise.reject(new Error(message(error), { cause: error })); }
    const entry: Entry = { peer, generation, root: "", children: new Map(), extraRoots: new Set(), targetGone: false, sourceGone: false, closed: false, attachPending: false,
      requests: new Set(), infoRevision: 0, infoReading: false, ready: Promise.resolve(), info: { clientId, targetId, state: "connecting", active: true, error: "", url: "", title: "" } };
    this.entries.set(peer, entry); this.save(entry);
    entry.ready = this.initialize(entry).catch(async (error: unknown) => {
      await this.close(entry, message(error)).catch(() => undefined);
      throw error;
    });
    // Socket close can precede any frontend command. Keep that rejected ready
    // promise observed even when there is no caller awaiting it.
    void entry.ready.catch(() => undefined);
    return entry.ready;
  }

  private async initialize(entry: Entry): Promise<void> {
    const discovered = await this.request(entry, "Target.getTargets", {});
    const targets = object(discovered.result) ? discovered.result.targetInfos : undefined;
    const target = Array.isArray(targets) ? targets.find((value: unknown) => object(value) && value.targetId === entry.info.targetId) : undefined;
    if (!object(target) || !isH5Page(target)) throw new Error("H5 目标不存在或不是可调试网页，请刷新目标清单");
    const attached = await this.request(entry, "Target.attachToTarget", { targetId: entry.info.targetId, flatten: true });
    const sessionId = object(attached.result) ? text(attached.result.sessionId) : "";
    if (!sessionId) throw new Error("Target.attachToTarget: 未返回 sessionId");
    if (entry.closed || this.retired.get(entry.info.clientId)?.has(sessionId)) throw new Error("H5 调试连接已关闭");
    entry.root = sessionId;
    const evaluated = await this.request(entry, "Runtime.evaluate", {
      expression: H5_PAGE_INFO_EXPRESSION, returnByValue: true, throwOnSideEffect: true,
    }, sessionId);
    const result = object(evaluated.result) ? evaluated.result : {};
    const value = object(result.result) ? result.result.value : undefined;
    if (object(result.exceptionDetails) || !object(value) || value.hasDocument !== true || !isH5Page({ type: "page", url: value.url })) {
      throw new Error("H5 页面上下文尚未就绪或页面信息读取失败");
    }
    this.check(entry);
    entry.info.url = text(value.url); entry.info.title = text(value.title); entry.info.state = "connected";
    this.save(entry);
  }

  forward(peer: Peer, payload: string): void {
    const entry = this.entries.get(peer);
    if (!entry || entry.closed) return;
    let command: Packet;
    try { const value: unknown = JSON.parse(payload); if (!object(value)) return; command = value; } catch { return; }
    const id = command.id;
    if (!Number.isSafeInteger(id) || (id as number) < 0 || (id as number) >= 1_000_000_000 || typeof command.method !== "string") {
      this.emit(entry, { id: id ?? null, error: { code: -32600, message: "H5 CDP 命令要求有效方法和非负整数 ID" } }); return;
    }
    if (entry.requests.has(id as number) || entry.requests.size >= 256) {
      this.emit(entry, { id, error: { code: -32600, message: "H5 命令重复或在途请求过多" } }); return;
    }
    entry.requests.add(id as number);
    void this.forwardCommand(entry, command).finally(() => entry.requests.delete(id as number));
  }

  private async forwardCommand(entry: Entry, command: Packet): Promise<void> {
    const frontendSession = text(command.sessionId);
    try {
      await entry.ready; this.check(entry);
      if (frontendSession && !entry.children.has(frontendSession)) throw new Error("H5 子会话不属于当前窗口");
      const params = object(command.params) ? command.params : {};
      // DevTools gets descendant sessions from auto-attach. A page endpoint
      // cannot be used to attach to another browser/page or drive a guessed id.
      if (["Target.attachToTarget", "Target.attachToBrowserTarget", "Target.sendMessageToTarget"].includes(text(command.method)) ||
        (typeof params.targetId === "string" && params.targetId !== entry.info.targetId && command.method !== "Target.detachFromTarget") ||
        (command.method === "Target.detachFromTarget" && !entry.children.has(text(params.sessionId)))) {
        throw new Error("H5 页面端点不能操作其他目标或会话");
      }
      const response = await this.request(entry, text(command.method), params, frontendSession || entry.root, 30000, false);
      const body = { ...response }; delete body.id; delete body.sessionId;
      this.emit(entry, { ...body, id: command.id, ...(frontendSession ? { sessionId: frontendSession } : {}) });
    } catch (error) {
      this.emit(entry, { id: command.id, ...(frontendSession ? { sessionId: frontendSession } : {}), error: { code: -32000, message: message(error) } });
    }
  }

  /** Called before the main miniapp's lock filter and Console forwarding. */
  receive(clientId: number, packet: Packet): boolean {
    if (typeof packet.id === "number") {
      const pending = this.pending.get(packet.id);
      if (!pending) return false;
      if (pending.entry.info.clientId !== clientId || text(packet.sessionId) !== pending.sessionId) return true;
      this.pending.delete(packet.id); clearTimeout(pending.timer);
      const entry = pending.entry;
      if (pending.method === "Target.attachToTarget") {
        entry.attachPending = false;
        const sessionId = object(packet.result) ? text(packet.result.sessionId) : "";
        if (entry.targetGone) {
          if (sessionId) this.retireId(clientId, sessionId);
          pending.reject(new Error("H5 网页目标已关闭")); return true;
        }
        if (sessionId && !this.retired.get(clientId)?.has(sessionId)) {
          if (entry.root && entry.root !== sessionId) entry.extraRoots.add(entry.root);
          entry.root = sessionId;
          if (entry.closed && !this.entries.has(entry.peer)) { this.entries.set(entry.peer, entry); entry.info.active = true; }
        }
        if (entry.closed || pending.timedOut) {
          pending.reject(new Error("H5 调试连接已关闭"));
          void this.close(entry, entry.info.error).catch(() => undefined);
          return true;
        }
      }
      if (pending.timedOut) return true;
      pending.resolve(packet);
      return true;
    }
    const params = object(packet.params) ? packet.params : {};
    const sessionId = text(packet.sessionId);
    const eventSession = text(params.sessionId);
    if (this.retired.get(clientId)?.has(sessionId) || this.retired.get(clientId)?.has(eventSession)) return true;
    if (!sessionId && packet.method === "Target.attachedToTarget" && object(params.targetInfo) &&
      this.destroyed.get(clientId)?.has(text(params.targetInfo.targetId))) {
      if (eventSession) this.retireId(clientId, eventSession);
      return true;
    }
    for (const entry of this.entries.values()) {
      if (entry.info.clientId !== clientId) continue;
      if (packet.method === "Target.targetInfoChanged" && object(params.targetInfo) && params.targetInfo.targetId === entry.info.targetId &&
        (!sessionId || sessionId === entry.root)) {
        if (entry.info.state === "connected" && !entry.closed) {
          if ((typeof params.targetInfo.url === "string" && params.targetInfo.url !== entry.info.url) ||
            (typeof params.targetInfo.title === "string" && params.targetInfo.title !== entry.info.title)) ++entry.infoRevision;
          if (typeof params.targetInfo.url === "string") entry.info.url = params.targetInfo.url;
          if (typeof params.targetInfo.title === "string") entry.info.title = params.targetInfo.title;
          this.save(entry);
          void this.refreshPageInfo(entry);
          const event = { ...packet }; delete event.sessionId;
          this.emit(entry, event);
        }
        return true;
      }
      if (!sessionId && packet.method === "Target.detachedFromTarget" && entry.extraRoots.has(eventSession)) {
        entry.extraRoots.delete(eventSession); this.retireId(clientId, eventSession);
        this.confirmDetach(entry, eventSession); return true;
      }
      if (!sessionId && packet.method === "Target.attachedToTarget" && object(params.targetInfo) && params.targetInfo.targetId === entry.info.targetId) {
        if (entry.root && eventSession && entry.root !== eventSession) {
          entry.extraRoots.add(eventSession);
          void this.close(entry, "H5 目标出现重复附加会话，请刷新后重试").catch(() => undefined);
        } else if (!entry.root && eventSession) entry.root = eventSession;
        if (entry.closed) void this.close(entry, entry.info.error).catch(() => undefined);
        return true;
      }
      if (!sessionId && ((packet.method === "Target.detachedFromTarget" && eventSession === entry.root && !!entry.root) ||
        (packet.method === "Target.targetDestroyed" && params.targetId === entry.info.targetId))) {
        const targetGone = packet.method === "Target.targetDestroyed";
        if (entry.root && entry.attachPending) entry.attachPending = false;
        if (targetGone) {
          entry.targetGone = true; entry.attachPending = false;
          const targets = this.destroyed.get(clientId) ?? new Set<string>();
          if (targets.size < 4096) targets.add(entry.info.targetId);
          else this.exhaust(clientId);
          this.destroyed.set(clientId, targets);
        }
        if (targetGone) { this.retire(entry); entry.extraRoots.clear(); }
        else {
          this.retireId(clientId, entry.root);
          for (const child of entry.children.keys()) this.retireId(clientId, child);
          entry.children.clear();
        }
        entry.root = "";
        for (const [id, pending] of this.pending) {
          if (pending.entry === entry && targetGone && pending.method === "Target.attachToTarget") {
            pending.reject(new Error("H5 网页目标已关闭"));
          }
          if (pending.entry === entry && pending.method === "Target.detachFromTarget" && (targetGone || pending.detachId === eventSession)) {
            clearTimeout(pending.timer); this.pending.delete(id); pending.resolve({ result: {} });
          }
        }
        void this.close(entry, entry.closed ? entry.info.error : "H5 网页目标已关闭").catch(() => undefined);
        return true;
      }
      if (entry.extraRoots.has(sessionId)) return true;
      if (this.exhausted.has(clientId) && sessionId) return true;
      if (!sessionId || (sessionId !== entry.root && !entry.children.has(sessionId))) continue;
      if (packet.method === "Target.attachedToTarget" && eventSession) entry.children.set(eventSession, sessionId);
      if (packet.method === "Target.detachedFromTarget" && eventSession) {
        this.retireChild(entry, eventSession);
      }
      if (entry.info.state === "connected" && !entry.closed) {
        if (sessionId === entry.root && packet.method === "Page.frameNavigated" && object(params.frame) &&
          !text(params.frame.parentId) && typeof params.frame.url === "string") {
          ++entry.infoRevision;
          entry.info.url = params.frame.url;
          entry.info.title = "";
          this.save(entry);
        }
        if (sessionId === entry.root && ["Page.loadEventFired", "Page.navigatedWithinDocument"].includes(text(packet.method))) {
          ++entry.infoRevision;
          void this.refreshPageInfo(entry);
        }
        const event = { ...packet }; delete event.sessionId;
        this.emit(entry, sessionId === entry.root ? event : packet);
      }
      return true;
    }
    return this.exhausted.has(clientId) && !!(sessionId || eventSession);

  }

  private async refreshPageInfo(entry: Entry): Promise<void> {
    if (entry.infoReading || entry.closed || entry.info.state !== "connected") return;
    entry.infoReading = true;
    const revision = entry.infoRevision;
    try {
      const evaluated = await this.request(entry, "Runtime.evaluate", {
        expression: H5_PAGE_INFO_EXPRESSION, returnByValue: true, throwOnSideEffect: true,
      }, entry.root);
      if (entry.closed || entry.info.state !== "connected" || revision !== entry.infoRevision) return;
      const result = object(evaluated.result) ? evaluated.result : {};
      const value = object(result.result) ? result.result.value : undefined;
      if (object(result.exceptionDetails) || !object(value) || value.hasDocument !== true || typeof value.url !== "string" || typeof value.title !== "string") {
        throw new Error("未返回有效页面信息");
      }
      entry.info.url = value.url; entry.info.title = value.title; entry.info.error = "";
      this.save(entry);
    } catch (error) {
      if (!entry.closed && entry.info.state === "connected" && revision === entry.infoRevision) {
        entry.info.error = `H5 页面信息刷新失败：${message(error)}`;
        this.save(entry);
      }
    } finally {
      entry.infoReading = false;
      // A navigation during the read invalidates its reply; reread the current document.
      if (revision !== entry.infoRevision) void this.refreshPageInfo(entry);
    }
  }

  disconnect(peer: Peer): Promise<void> {
    const entry = this.entries.get(peer);
    return entry ? this.close(entry) : Promise.resolve();
  }
  closeTarget(clientId: number, targetId: string): Promise<void> {
    const entry = [...this.entries.values()].find((entry) => entry.info.clientId === clientId && entry.info.targetId === targetId);
    if (entry?.attachPending && !entry.root) return Promise.reject(new Error("H5 附加回执尚未确认，请等待或重新连接小程序调试通道"));
    if (entry) entry.info.error = "";
    return entry ? this.close(entry) : Promise.resolve();
  }
  invalidate(clientId: number, reason: string, sourceGone = false): void {
    for (const entry of this.entries.values()) {
      if (entry.info.clientId === clientId) void this.close(entry, reason, sourceGone).catch(() => undefined);
    }
    if (sourceGone) {
      for (const [id, pending] of this.pending) {
        if (pending.entry.info.clientId !== clientId) continue;
        clearTimeout(pending.timer); this.pending.delete(id); pending.reject(new Error(reason));
      }
      this.retired.delete(clientId); this.destroyed.delete(clientId); this.exhausted.delete(clientId);
    }
  }

  private close(entry: Entry, reason = "", sourceGone = false): Promise<void> {
    if (entry.closing && !sourceGone) return entry.closing;
    const wasClosed = entry.closed;
    if (sourceGone) entry.sourceGone = true;
    entry.closed = true;
    if (reason) entry.info.error = reason;
    entry.info.state = "closing"; this.save(entry);
    if (!wasClosed) {
      if (reason) this.emit(entry, { method: "Inspector.detached", params: { reason } }, true);
      try { entry.peer.close?.(1000, "H5 session closed"); } catch { /* Socket already gone. */ }
    }
    for (const [id, pending] of this.pending) {
      if (pending.entry !== entry || (!sourceGone && ["Target.attachToTarget", "Target.detachFromTarget"].includes(pending.method))) continue;
      clearTimeout(pending.timer); this.pending.delete(id); pending.reject(new Error(reason || "H5 调试连接已关闭"));
    }
    if (sourceGone) { entry.attachPending = false; this.retire(entry); entry.root = ""; entry.extraRoots.clear(); }
    const closing = (async () => {
      try {
        // A late attached event can arrive while a detach is in flight. Drain
        // until every root is confirmed released before dropping ownership.
        while (entry.root || entry.extraRoots.size) {
          const root = entry.extraRoots.values().next().value ?? entry.root;
          await this.request(entry, "Target.detachFromTarget", { sessionId: root }, "", 2000);
          if (entry.sourceGone) break;
          this.retireId(entry.info.clientId, root); entry.extraRoots.delete(root);
          if (entry.root === root) {
            for (const child of entry.children.keys()) this.retireId(entry.info.clientId, child);
            entry.children.clear(); entry.root = ""; entry.attachPending = false;
          }
        }
        entry.info.state = entry.info.error ? "error" : entry.attachPending ? "closing" : "closed";
        if (!entry.attachPending && !entry.root && !entry.extraRoots.size) {
          this.entries.delete(entry.peer); entry.info.active = false;
        }
      } catch (error) {
        if (entry.sourceGone || !entry.info.active) return;
        entry.info.state = "error"; entry.info.error = `H5 会话释放失败：${message(error)}`;
        throw new Error(entry.info.error, { cause: error });
      } finally { this.save(entry); }
    })();
    entry.closing = closing;
    // Retrying a failed or not-yet-acknowledged attachment must be possible.
    void closing.finally(() => { if (entry.closing === closing) entry.closing = undefined; }).catch(() => undefined);
    return closing;
  }

  private check(entry: Entry): void {
    if (this.exhausted.has(entry.info.clientId)) throw new Error("H5 会话记录已达上限，请重新连接小程序调试通道");
    if (entry.closed || this.options.generation(entry.info.clientId) !== entry.generation) throw new Error("H5 来源已变化或连接已关闭");
  }
  private request(entry: Entry, method: string, params: Packet, sessionId = "", timeoutMs = 8000, rejectProtocolError = true): Promise<Packet> {
    if (method !== "Target.detachFromTarget") this.check(entry);
    const id = this.options.allocateId();
    if (id > 2_147_483_647) return Promise.reject(new Error("CDP 命令 ID 已耗尽，请重启引擎"));
    return new Promise<Packet>((resolve, reject) => {
      if (method === "Target.attachToTarget") entry.attachPending = true;
      const timer = setTimeout(() => {
        const pending = this.pending.get(id);
        if (!pending) return;
        pending.timedOut = true;
        // An uncertain attachment remains tracked until its late reply or
        // source disconnect. Reopening must not create an orphaned session.
        if (method !== "Target.attachToTarget" || entry.targetGone) this.pending.delete(id);
        reject(new Error(`CDP command timed out: ${method}`));
      }, timeoutMs);
      this.pending.set(id, { entry, method, sessionId, detachId: method === "Target.detachFromTarget" ? text(params.sessionId) : "", resolve, reject, timer, timedOut: false });
      try { this.options.send(entry.info.clientId, { id, method, params, ...(sessionId ? { sessionId } : {}) }); }
      catch (error) { clearTimeout(timer); this.pending.delete(id); entry.attachPending = false; reject(new Error(message(error))); }
    }).then((packet) => {
      if (rejectProtocolError && object(packet.error)) throw new Error(`${method}: ${text(packet.error.message) || "CDP 命令失败"}`);
      if (rejectProtocolError && !object(packet.result)) throw new Error(`${method}: 未返回有效回执`);
      return packet;
    });
  }
  private save(entry: Entry): void {
    const key = `${entry.info.clientId}:${entry.info.targetId}`;
    this.history.delete(key); this.history.set(key, { ...entry.info });
    if (this.history.size > 64) {
      const oldestClosed = [...this.history].find(([, state]) => !state.active);
      if (oldestClosed) this.history.delete(oldestClosed[0]);
    }
  }
  private emit(entry: Entry, packet: Packet, closing = false): void {
    if (entry.closed && !closing) return;
    try { entry.peer.send(JSON.stringify(packet)); } catch { /* Socket close drives cleanup. */ }
  }
  private retire(entry: Entry): void {
    if (entry.sourceGone) { entry.children.clear(); return; }
    if (entry.root) this.retireId(entry.info.clientId, entry.root);
    for (const child of entry.children.keys()) this.retireId(entry.info.clientId, child);
    for (const root of entry.extraRoots) this.retireId(entry.info.clientId, root);
    entry.children.clear();
  }
  private retireId(clientId: number, id: string): void {
    const ids = this.retired.get(clientId) ?? new Set<string>();
    // Never evict a tombstone into the main data channel. At the limit, close
    // H5 windows and quarantine flattened events until the source reconnects.
    if (!ids.has(id) && ids.size >= 4096) this.exhaust(clientId);
    else ids.add(id);
    this.retired.set(clientId, ids);
  }
  private retireChild(entry: Entry, sessionId: string): void {
    for (const [child, parent] of entry.children) {
      if (parent === sessionId) this.retireChild(entry, child);
    }
    entry.children.delete(sessionId); this.retireId(entry.info.clientId, sessionId);
  }
  private confirmDetach(entry: Entry, sessionId: string): void {
    for (const [id, pending] of this.pending) {
      if (pending.entry !== entry || pending.method !== "Target.detachFromTarget" || pending.detachId !== sessionId) continue;
      clearTimeout(pending.timer); this.pending.delete(id); pending.resolve({ result: {} });
    }
  }
  private exhaust(clientId: number): void {
    if (this.exhausted.has(clientId)) return;
    this.exhausted.add(clientId);
    this.invalidate(clientId, "H5 会话记录已达上限，请重新连接小程序调试通道");
  }
}
