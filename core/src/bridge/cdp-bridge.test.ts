import { describe, expect, it, vi } from "vitest";
import { runInNewContext } from "node:vm";

import { APP_INFO_EXPRESSION, CdpBridge, miniappNameFromTargetTitle, type Peer } from "./cdp-bridge.js";
import { decodeCdpMessage, encodeCdpMessage } from "../protocol/wmpf-codec.js";

function peer(): { value: Peer; sent: Array<Buffer | string> } {
  const sent: Array<Buffer | string> = [];
  return { value: { send: (message) => sent.push(message) }, sent };
}

/** Feeds a chromeDevtoolsResult frame back into the bridge. */
function respond(bridge: CdpBridge, clientId: number, commandId: number, result: unknown): void {
  bridge.receiveMiniapp(encodeCdpMessage({
    sequence: 1,
    category: "chromeDevtoolsResult",
    operationId: commandId,
    payload: JSON.stringify({ id: commandId, result }),
    jsContextId: "",
  }), clientId);
}

/** Reads the outbound CDP command payload of the last frame a peer received. */
function lastPayload(sent: Array<Buffer | string>): Record<string, unknown> {
  const outbound = decodeCdpMessage(sent[sent.length - 1] as Buffer);
  return JSON.parse(outbound?.payload ?? "{}") as Record<string, unknown>;
}

describe("CdpBridge", () => {
  it("encodes a DevTools command for the connected miniapp", () => {
    const bridge = new CdpBridge();
    const miniapp = peer();
    bridge.addMiniapp(miniapp.value);

    bridge.forwardDevtools('{"id":7,"method":"Runtime.enable"}');

    expect(miniapp.sent).toHaveLength(1);
    expect(decodeCdpMessage(miniapp.sent[0] as Buffer)).toMatchObject({
      category: "chromeDevtools",
      payload: '{"id":7,"method":"Runtime.enable"}',
    });
  });

  it("resolves a pending command and broadcasts its response", async () => {
    const bridge = new CdpBridge();
    const miniapp = peer();
    const devtools = peer();
    bridge.addMiniapp(miniapp.value);
    bridge.addDevtools(devtools.value);

    const response = bridge.sendCommand("Runtime.evaluate", { expression: "1 + 1" }, 1000);
    const command = lastPayload(miniapp.sent);
    respond(bridge, 1, command.id as number, { value: 2 });

    await expect(response).resolves.toEqual({ id: command.id, result: { value: 2 } });
    expect(devtools.sent).toEqual([JSON.stringify({ id: command.id, result: { value: 2 } })]);
  });

  it("rejects a command before any miniapp has connected", async () => {
    await expect(new CdpBridge().sendCommand("Runtime.enable", {}, 100)).rejects.toThrow("no miniapp connected");
  });

  it("reports connection status for the miniapp and devtools peers", () => {
    const bridge = new CdpBridge();
    expect(bridge.connectionStatus()).toEqual({ miniapp: false, devtools: false });

    const miniapp = peer();
    const devtools = peer();
    bridge.addMiniapp(miniapp.value);
    bridge.addDevtools(devtools.value);
    expect(bridge.connectionStatus()).toEqual({ miniapp: true, devtools: true });

    bridge.removeMiniapp(1);
    bridge.removeDevtools(devtools.value);
    expect(bridge.connectionStatus()).toEqual({ miniapp: false, devtools: false });
  });
});

describe("CdpBridge multi-miniapp lock", () => {
  it("assigns incremental ids and locks the first connection over newer ones", () => {
    const bridge = new CdpBridge();
    const first = peer();
    const second = peer();

    const firstId = bridge.addMiniapp(first.value);
    const secondId = bridge.addMiniapp(second.value);

    expect([firstId, secondId]).toEqual([1, 2]);
    expect(bridge.listMiniapps()).toEqual([
      { id: 1, appid: "", name: "", locked: true },
      { id: 2, appid: "", name: "", locked: false },
    ]);
  });

  it("switches the lock by id and rejects unknown ids", () => {
    const bridge = new CdpBridge();
    const first = peer();
    const second = peer();
    bridge.addMiniapp(first.value);
    const secondId = bridge.addMiniapp(second.value);

    expect(bridge.switchMiniapp(1)).toBe(true);
    expect(bridge.listMiniapps().find((entry) => entry.locked)?.id).toBe(1);
    expect(bridge.switchMiniapp(99)).toBe(false);
    expect(bridge.switchMiniapp(secondId)).toBe(true);
  });

  it("sends only to the locked connection while locked", () => {
    const bridge = new CdpBridge();
    const first = peer();
    const second = peer();
    bridge.addMiniapp(first.value);
    bridge.addMiniapp(second.value);

    bridge.forwardDevtools('{"id":7,"method":"Runtime.enable"}');

    expect(first.sent).toHaveLength(1);
    expect(second.sent).toHaveLength(0);
  });

  it("fans out to every connection once the lock is disabled", () => {
    const bridge = new CdpBridge();
    const first = peer();
    const second = peer();
    bridge.addMiniapp(first.value);
    bridge.addMiniapp(second.value);
    bridge.setLock(false);

    bridge.forwardDevtools('{"id":7,"method":"Runtime.enable"}');

    expect(first.sent).toHaveLength(1);
    expect(second.sent).toHaveLength(1);
  });

  it("does not re-lock silently when a miniapp connects after the lock is disabled", () => {
    const bridge = new CdpBridge();
    bridge.setLock(false);

    const id = bridge.addMiniapp(peer().value);

    expect(bridge.listMiniapps()).toEqual([{ id, appid: "", name: "", locked: false }]);
  });

  it("restores the lock when the user explicitly switches", () => {
    const bridge = new CdpBridge();
    bridge.setLock(false);
    bridge.addMiniapp(peer().value);
    bridge.addMiniapp(peer().value);

    expect(bridge.switchMiniapp(1)).toBe(true);

    expect(bridge.isLockEnabled()).toBe(true);
    expect(bridge.listMiniapps().find((entry) => entry.locked)?.id).toBe(1);
  });

  it("silently drops DevTools traffic with no miniapp connected", () => {
    const bridge = new CdpBridge();

    expect(() => bridge.forwardDevtools('{"id":7,"method":"Runtime.enable"}')).not.toThrow();
  });

  it("drops malformed miniapp frames instead of crashing", () => {
    const bridge = new CdpBridge();
    bridge.addMiniapp(peer().value);

    // A truncated protobuf frame and a frame that claims zlib compression
    // over garbage bytes both blow up inside decodeCdpMessage.
    expect(() => bridge.receiveMiniapp(Buffer.from([0x0a]), 1)).not.toThrow();
    expect(() => bridge.receiveMiniapp(Buffer.from([0x22, 0x02, 0xde, 0xad, 0x28, 0x01]), 1)).not.toThrow();
  });

  it("unlocks automatically when the locked connection drops", () => {
    const bridge = new CdpBridge();
    const first = peer();
    const second = peer();
    bridge.addMiniapp(first.value);
    bridge.addMiniapp(second.value);

    bridge.removeMiniapp(1);

    expect(bridge.listMiniapps()).toEqual([{ id: 2, appid: "", name: "", locked: false }]);
    expect(bridge.connectionStatus()).toEqual({ miniapp: true, devtools: false });
  });

  it("keeps pending commands alive for surviving connections", async () => {
    const bridge = new CdpBridge();
    const first = peer();
    const second = peer();
    bridge.addMiniapp(first.value);
    bridge.addMiniapp(second.value);

    const response = bridge.sendCommand("Page.enable", {}, 1000);
    bridge.removeMiniapp(2);
    respond(bridge, 1, lastPayload(first.sent).id as number, {});

    await expect(response).resolves.toEqual({ id: expect.any(Number), result: {} });
  });

  it("carries appid and name info into the list", () => {
    const bridge = new CdpBridge();
    const miniapp = peer();
    bridge.addMiniapp(miniapp.value);

    bridge.setMiniappInfo(1, "wx1234567890", "Demo 小程序");

    expect(bridge.listMiniapps()).toEqual([
      { id: 1, appid: "wx1234567890", name: "Demo 小程序", locked: true },
    ]);
  });

  it("extracts the appid from the target list after setupContext", async () => {
    vi.useFakeTimers();
    try {
      const bridge = new CdpBridge({ probeDelayMs: 1000 });
      const miniapp = peer();
      const seenAppInfo: Array<{ appid: string; name: string }> = [];
      bridge.onAppInfo = (info) => seenAppInfo.push(info);
      bridge.addMiniapp(miniapp.value);

      bridge.receiveMiniapp(encodeCdpMessage({
        sequence: 1,
        category: "setupContext",
        operationId: 0,
        payload: "{}",
        jsContextId: "",
      }), 1);

      await vi.advanceTimersByTimeAsync(1000);
      const command = lastPayload(miniapp.sent);
      expect(command.method).toBe("Target.getTargets");

      // The preload page shares the URL shape but carries no appid segment.
      respond(bridge, 1, command.id as number, {
        targetInfos: [
          { url: "https://servicewechat.com/preload-22/22/page-frame.html" },
          { url: "https://servicewechat.com/wxdeadbeefdeadbeef/103/page-frame.html" },
        ],
      });
      await vi.advanceTimersByTimeAsync(0);

      const identity = lastPayload(miniapp.sent);
      expect(identity.method).toBe("Runtime.evaluate");
      respond(bridge, 1, identity.id as number, { result: { type: "string", value: JSON.stringify({ appid: "wxdeadbeefdeadbeef", name: "演示小程序", icon: "https://wx.qlogo.cn/icon.png" }) } });
      await vi.advanceTimersByTimeAsync(0);

      expect(bridge.listMiniapps()[0]).toMatchObject({ appid: "wxdeadbeefdeadbeef", name: "演示小程序" });
      expect(seenAppInfo).toEqual([
        { appid: "wxdeadbeefdeadbeef", name: "" },
        { appid: "wxdeadbeefdeadbeef", name: "演示小程序", icon: "https://wx.qlogo.cn/icon.png" },
      ]);
    } finally {
      vi.useRealTimers();
    }
  });

  it("shows the target title and lets a page nickname replace it", async () => {
    vi.useFakeTimers();
    try {
      const bridge = new CdpBridge({ probeDelayMs: 1000 });
      const miniapp = peer();
      bridge.addMiniapp(miniapp.value);
      bridge.receiveMiniapp(encodeCdpMessage({
        sequence: 1,
        category: "setupContext",
        operationId: 0,
        payload: "{}",
        jsContextId: "",
      }), 1);

      await vi.advanceTimersByTimeAsync(1000);
      const listed = lastPayload(miniapp.sent);
      respond(bridge, 1, listed.id as number, {
        targetInfos: [{
          targetId: "page-1",
          title: "示例小程序",
          url: "https://servicewechat.com/wxabc1234567890a/8/page-frame.html",
        }],
      });
      await vi.advanceTimersByTimeAsync(0);
      expect(bridge.listMiniapps()[0]).toMatchObject({ appid: "wxabc1234567890a", name: "示例小程序" });

      const attach = lastPayload(miniapp.sent);
      expect(attach.method).toBe("Target.attachToTarget");
      respond(bridge, 1, attach.id as number, { sessionId: "sess-1" });
      await vi.advanceTimersByTimeAsync(0);

      const identity = lastPayload(miniapp.sent);
      expect(identity.method).toBe("Runtime.evaluate");
      expect(identity.sessionId).toBe("sess-1");
      respond(bridge, 1, identity.id as number, { result: { type: "string", value: JSON.stringify({ appid: "wxabc1234567890a", name: "真实昵称" }) } });
      await vi.advanceTimersByTimeAsync(0);

      expect(bridge.listMiniapps()[0]).toMatchObject({ name: "真实昵称" });
      await vi.advanceTimersByTimeAsync(2000);
    } finally {
      vi.useRealTimers();
    }
  });

  it("keeps the target title when the page config has no nickname", async () => {
    vi.useFakeTimers();
    try {
      const bridge = new CdpBridge({ probeDelayMs: 1000 });
      const miniapp = peer();
      bridge.addMiniapp(miniapp.value);
      bridge.receiveMiniapp(encodeCdpMessage({
        sequence: 1,
        category: "setupContext",
        operationId: 0,
        payload: "{}",
        jsContextId: "",
      }), 1);

      await vi.advanceTimersByTimeAsync(1000);
      const listed = lastPayload(miniapp.sent);
      respond(bridge, 1, listed.id as number, {
        targetInfos: [{
          targetId: "page-1",
          title: "示例小程序",
          url: "https://servicewechat.com/wxabc1234567890a/8/page-frame.html",
        }],
      });
      await vi.advanceTimersByTimeAsync(0);

      const attach = lastPayload(miniapp.sent);
      respond(bridge, 1, attach.id as number, {});
      await vi.advanceTimersByTimeAsync(0);
      const identity = lastPayload(miniapp.sent);
      expect(identity.method).toBe("Runtime.evaluate");
      respond(bridge, 1, identity.id as number, { result: { type: "string", value: JSON.stringify({ appid: "wxabc1234567890a", name: "" }) } });
      await vi.advanceTimersByTimeAsync(0);

      expect(bridge.listMiniapps()[0]).toMatchObject({ name: "示例小程序" });
    } finally {
      vi.useRealTimers();
    }
  });

  it("retries the target list and falls back to the __wxConfig expression", async () => {
    vi.useFakeTimers();
    try {
      const bridge = new CdpBridge({ probeDelayMs: 1000 });
      const miniapp = peer();
      bridge.addMiniapp(miniapp.value);

      bridge.receiveMiniapp(encodeCdpMessage({
        sequence: 1,
        category: "setupContext",
        operationId: 0,
        payload: "{}",
        jsContextId: "",
      }), 1);

      // Three target-list attempts without an appid URL, then the expression.
      for (let i = 0; i < 3; i += 1) {
        await vi.advanceTimersByTimeAsync(1000);
        const command = lastPayload(miniapp.sent);
        expect(command.method).toBe("Target.getTargets");
        respond(bridge, 1, command.id as number, { targetInfos: [{ url: "https://example.com/" }] });
        await vi.advanceTimersByTimeAsync(0);
      }
      await vi.advanceTimersByTimeAsync(1000);
      const fallback = lastPayload(miniapp.sent);
      expect(fallback.method).toBe("Runtime.evaluate");
      expect(String(fallback.params && (fallback.params as Record<string, unknown>).expression)).toContain("__wxConfig");

      respond(bridge, 1, fallback.id as number, { result: { type: "string", value: JSON.stringify({ appid: "wxxyz", name: "识别到的应用" }) } });
      await vi.advanceTimersByTimeAsync(0);

      expect(bridge.listMiniapps()[0]).toMatchObject({ appid: "wxxyz", name: "识别到的应用" });
    } finally {
      vi.useRealTimers();
    }
  });

  it("does not probe the same connection twice", async () => {
    vi.useFakeTimers();
    try {
      const bridge = new CdpBridge({ probeDelayMs: 1000 });
      const miniapp = peer();
      bridge.addMiniapp(miniapp.value);

      const setupFrame = encodeCdpMessage({
        sequence: 1,
        category: "setupContext",
        operationId: 0,
        payload: "{}",
        jsContextId: "",
      });
      bridge.receiveMiniapp(setupFrame, 1);
      bridge.receiveMiniapp(setupFrame, 1);
      await vi.advanceTimersByTimeAsync(1000);

      // 连接上还会有一次性的 Runtime.enable（控制台事件采集），所以按方法名计数：
      // 探针只应发一次，enable 也只发一次。
      const methods = miniapp.sent.map((frame) => {
        const payload = (typeof frame === "string" ? undefined : decodeCdpMessage(frame))?.payload ?? "{}";
        return (JSON.parse(payload) as { method?: string }).method ?? "";
      });
      expect(methods.filter((method) => method === "Target.getTargets")).toHaveLength(1);
      expect(methods.filter((method) => method === "Runtime.enable")).toHaveLength(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it("increments the page generation for every setupContext frame", () => {
    const bridge = new CdpBridge({ probeDelayMs: 60_000 });
    bridge.addMiniapp(peer().value);
    const setupFrame = encodeCdpMessage({
      sequence: 1,
      category: "setupContext",
      operationId: 0,
      payload: "{}",
      jsContextId: "",
    });

    expect(bridge.generation()).toBe(0);
    bridge.receiveMiniapp(setupFrame, 1);
    expect(bridge.generation()).toBe(1);
    bridge.receiveMiniapp(setupFrame, 1);
    expect(bridge.generation()).toBe(2);
  });
});

describe("CdpBridge re-locking semantics", () => {
  it("pins an existing connection when the lock is re-enabled without a target", () => {
    const bridge = new CdpBridge();
    const first = peer();
    const second = peer();
    bridge.addMiniapp(first.value);
    bridge.addMiniapp(second.value);

    bridge.setLock(false);
    bridge.setLock(true);

    // The user explicitly re-locked; fan-out must not silently resume. The
    // most recently connected miniapp is pinned.
    bridge.forwardDevtools('{"id":1,"method":"Page.enable"}');
    expect(second.sent.length).toBe(1);
    expect(first.sent.length).toBe(0);
    expect(bridge.listMiniapps().map((entry) => entry.locked)).toEqual([false, true]);
  });

  it("pins the next connection when the lock is enabled with no miniapp yet", () => {
    const bridge = new CdpBridge();
    bridge.setLock(false);
    bridge.setLock(true);

    const newcomer = peer();
    bridge.addMiniapp(newcomer.value);

    bridge.forwardDevtools('{"id":1,"method":"Page.enable"}');
    expect(newcomer.sent.length).toBe(1);
  });
});

  it("drops non-locked miniapp responses", async () => {
    const bridge = new CdpBridge();
    const locked = peer();
    const other = peer();
    const devtools = peer();
    const lockedId = bridge.addMiniapp(locked.value);
    bridge.addMiniapp(other.value);
    bridge.addDevtools(devtools.value);

    const pending = bridge.sendCommand("Runtime.evaluate", {}, 1000);
    const outbound = decodeCdpMessage(locked.sent[0] as Buffer);
    const commandId = JSON.parse(outbound?.payload ?? "{}").id as number;
    const payload = JSON.stringify({ id: commandId, result: { ok: true } });

    bridge.receiveMiniapp(encodeCdpMessage({
      sequence: 1,
      category: "chromeDevtoolsResult",
      operationId: commandId,
      payload,
      jsContextId: "",
    }), lockedId + 1);

    let resolved = false;
    void pending.then(() => { resolved = true; });
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(resolved).toBe(false);
    expect(devtools.sent).toHaveLength(0);

    bridge.receiveMiniapp(encodeCdpMessage({
      sequence: 2,
      category: "chromeDevtoolsResult",
      operationId: commandId,
      payload,
      jsContextId: "",
    }), lockedId);
    await expect(pending).resolves.toEqual({ id: commandId, result: { ok: true } });
    expect(devtools.sent).toEqual([payload]);
  });

  it("does not count background miniapp refreshes", () => {
    const bridge = new CdpBridge({ probeDelayMs: 60_000 });
    const firstId = bridge.addMiniapp(peer().value);
    bridge.addMiniapp(peer().value);
    const setupFrame = encodeCdpMessage({
      sequence: 1,
      category: "setupContext",
      operationId: 0,
      payload: "{}",
      jsContextId: "",
    });

    bridge.receiveMiniapp(setupFrame, firstId);
    expect(bridge.generation()).toBe(1);
    bridge.receiveMiniapp(setupFrame, firstId + 1);
    expect(bridge.generation()).toBe(1);
  });

  it("counts an explicit miniapp switch as a generation change", () => {
    const bridge = new CdpBridge();
    const firstId = bridge.addMiniapp(peer().value);
    const secondId = bridge.addMiniapp(peer().value);

    expect(bridge.switchMiniapp(secondId)).toBe(true);
    expect(bridge.generation()).toBe(1);
    expect(bridge.switchMiniapp(secondId)).toBe(true);
    expect(bridge.generation()).toBe(1);
    expect(bridge.switchMiniapp(firstId)).toBe(true);
    expect(bridge.generation()).toBe(2);
  });

// 控制台采集走 CDP 事件：小程序的 console 在当前 WMPF 版本里是
// configurable:false 的访问器（页内 hook 包不上，真机实测 `Cannot redefine
// property: log`），所以 Runtime.consoleAPICalled 是唯一可靠来源。
describe("CdpBridge console events", () => {
  function consoleFrame(payload: unknown): Buffer {
    return encodeCdpMessage({
      sequence: 1,
      category: "chromeDevtoolsResult",
      operationId: 0,
      payload: JSON.stringify(payload),
      jsContextId: "",
    });
  }

  it("normalizes consoleAPICalled into the panel's record shape", () => {
    const bridge = new CdpBridge();
    const entries: Record<string, unknown>[] = [];
    bridge.onConsole = (entry) => entries.push(entry);
    const miniapp = peer();
    const clientId = bridge.addMiniapp(miniapp.value);

    bridge.receiveMiniapp(consoleFrame({
      method: "Runtime.consoleAPICalled",
      params: {
        type: "warning",
        timestamp: 1700000000000,
        args: [{ type: "string", value: "回调超时" }, { type: "object", description: "Object" }],
      },
    }), clientId);

    expect(entries).toHaveLength(1);
    expect(entries[0]).toMatchObject({ type: "console", level: "warn", text: "回调超时 Object", ts: 1700000000000 });
  });

  // 级别筛选下拉只提供 log/info/warn/error/debug 五档：CDP 的其余类型必须
  // 归进五档（assert 按语义归 error，dir/table/trace 等呈现型归 log），
  // 否则会出现下拉选不中、级别计数也不算的"幽灵级别"。
  it("maps every CDP console type into the panel's five levels", () => {
    const bridge = new CdpBridge();
    const entries: Array<{ level?: unknown }> = [];
    bridge.onConsole = (entry) => entries.push(entry);
    const miniapp = peer();
    const clientId = bridge.addMiniapp(miniapp.value);

    for (const type of ["log", "info", "error", "debug", "warning", "verbose", "assert", "table", "dir", "trace", "startGroup"]) {
      bridge.receiveMiniapp(consoleFrame({
        method: "Runtime.consoleAPICalled",
        params: { type, args: [{ type: "string", value: type }] },
      }), clientId);
    }

    expect(entries.map((entry) => entry.level)).toEqual([
      "log", "info", "error", "debug", "warn", "debug", "error",
      "log", "log", "log", "log",
    ]);
  });

  // 一条 console.log(巨大字符串) 不能把 stdout 行、环形缓冲和面板 DOM 一起
  // 撑爆：CDP 是 WMPF 上的主采集路径，文本必须与页内 hook 同样封顶。
  it("truncates oversized console text to the same cap as the page hook", () => {
    const bridge = new CdpBridge();
    const entries: Array<{ text?: unknown }> = [];
    bridge.onConsole = (entry) => entries.push(entry);
    const miniapp = peer();
    const clientId = bridge.addMiniapp(miniapp.value);

    bridge.receiveMiniapp(consoleFrame({
      method: "Runtime.consoleAPICalled",
      params: {
        type: "log",
        args: [{ type: "string", value: "x".repeat(100_000) }],
      },
    }), clientId);

    expect(entries).toHaveLength(1);
    expect(String(entries[0].text).length).toBeLessThan(2100);
    expect(String(entries[0].text)).toContain("…[截断]");
  });

  it("normalizes uncaught exceptions with their source location", () => {
    const bridge = new CdpBridge();
    const entries: Record<string, unknown>[] = [];
    bridge.onConsole = (entry) => entries.push(entry);
    const miniapp = peer();
    const clientId = bridge.addMiniapp(miniapp.value);

    bridge.receiveMiniapp(consoleFrame({
      method: "Runtime.exceptionThrown",
      params: {
        exceptionDetails: { text: "Uncaught", url: "app.js", lineNumber: 42, columnNumber: 7, exception: { description: "Error: boom" } },
      },
    }), clientId);

    expect(entries[0]).toMatchObject({ level: "error", text: "Error: boom", extra: { source: "app.js", line: "42", column: "7" } });
  });

  it("enables Runtime once per connection, and not for every setupContext", () => {
    const bridge = new CdpBridge({ probeDelayMs: 60_000 });
    const miniapp = peer();
    bridge.addMiniapp(miniapp.value);
    const setup = encodeCdpMessage({ sequence: 1, category: "setupContext", operationId: 0, payload: "{}", jsContextId: "" });

    bridge.receiveMiniapp(setup, 1);
    bridge.receiveMiniapp(setup, 1);

    const methods = miniapp.sent.map((frame) => {
      const payload = (typeof frame === "string" ? undefined : decodeCdpMessage(frame))?.payload ?? "{}";
      return (JSON.parse(payload) as { method?: string }).method ?? "";
    });
    expect(methods.filter((method) => method === "Runtime.enable")).toHaveLength(1);
  });

  // Runtime 域按连接开关：锁在 A 时新连上的 B 也必须在自己的连接上收到
  // enable。发给"活动目标"的话，B 会被标记为已启用却从未启用 —— 用户切到
  // B 后 console 采集就静默失效了。
  it("routes Runtime.enable to the connecting peer even while another realm is locked", () => {
    const bridge = new CdpBridge({ probeDelayMs: 60_000 });
    const first = peer();
    const second = peer();
    bridge.addMiniapp(first.value);
    const secondId = bridge.addMiniapp(second.value);
    const setup = encodeCdpMessage({ sequence: 1, category: "setupContext", operationId: 0, payload: "{}", jsContextId: "" });

    bridge.receiveMiniapp(setup, 1);
    bridge.receiveMiniapp(setup, secondId);

    const sentMethods = (frames: Array<Buffer | string>) =>
      frames.map((frame) => {
        const payload = (typeof frame === "string" ? undefined : decodeCdpMessage(frame))?.payload ?? "{}";
        return (JSON.parse(payload) as { method?: string }).method ?? "";
      });
    expect(sentMethods(first.sent).filter((method) => method === "Runtime.enable")).toHaveLength(1);
    expect(sentMethods(second.sent).filter((method) => method === "Runtime.enable")).toHaveLength(1);
  });

  // 页内覆盖标志描述的是旧锁定 realm：切换目标后，新 realm 还没装过页内钩子，
  // 不能沿用旧状态把它的 console 事件当"重复上报"丢掉。
  it("keeps reporting console events after switching to a target whose page hook was never installed", () => {
    const bridge = new CdpBridge();
    const entries: Record<string, unknown>[] = [];
    bridge.onConsole = (entry) => entries.push(entry);
    bridge.addMiniapp(peer().value);
    const secondId = bridge.addMiniapp(peer().value);

    bridge.setConsolePageCoverage({ ok: true, hookedConsoles: 1, unwrappableConsoles: 0 });
    bridge.switchMiniapp(secondId);

    bridge.receiveMiniapp(consoleFrame({
      method: "Runtime.consoleAPICalled",
      params: { type: "log", args: [{ type: "string", value: "from second" }] },
    }), secondId);

    expect(entries).toHaveLength(1);
    expect(entries[0]).toMatchObject({ text: "from second" });
  });

  // errorsHooked 与 ok 相互独立：WMPF 锁住 console 时回执是 ok:false，但错误
  // 出口（onerror / unhandledrejection）已挂上 —— CDP 的异常事件必须让位给
  // 页内钩子（否则每次异常两份），console.* 照报；覆盖标志清零（切换/解锁）
  // 后 CDP 接管错误上报，错误不丢。
  it("uses errorsHooked, not ok, to decide whether CDP reports exception events", () => {
    const bridge = new CdpBridge();
    const entries: Array<{ level?: unknown }> = [];
    bridge.onConsole = (entry) => entries.push(entry);
    const miniapp = peer();
    const clientId = bridge.addMiniapp(miniapp.value);

    bridge.setConsolePageCoverage({ ok: false, hookedConsoles: 0, unwrappableConsoles: 1, errorsHooked: true });
    bridge.receiveMiniapp(consoleFrame({
      method: "Runtime.exceptionThrown",
      params: { exceptionDetails: { text: "Uncaught" } },
    }), clientId);
    bridge.receiveMiniapp(consoleFrame({
      method: "Runtime.consoleAPICalled",
      params: { type: "log", args: [{ type: "string", value: "kept" }] },
    }), clientId);

    expect(entries.map((entry) => entry.level)).toEqual(["log"]);

    bridge.setLock(false);
    bridge.receiveMiniapp(consoleFrame({
      method: "Runtime.exceptionThrown",
      params: { exceptionDetails: { text: "Uncaught" } },
    }), clientId);
    expect(entries).toHaveLength(2);
    expect(entries[1]).toMatchObject({ level: "error" });
  });
});

// 调试事件在 CDP 层捕获（paused / scriptParsed 等 DevTools 域事件没有页面内
// 等价物），cdp.debug 的读者（MCP 调试工具）只能从这份快照了解断点会话。
describe("CdpBridge debugger events", () => {
  function eventFrame(payload: unknown): Buffer {
    return encodeCdpMessage({
      sequence: 1,
      category: "chromeDevtoolsResult",
      operationId: 0,
      payload: JSON.stringify(payload),
      jsContextId: "",
    });
  }

  it("records paused call frames and clears them on resumed", () => {
    const bridge = new CdpBridge();
    const miniapp = peer();
    const clientId = bridge.addMiniapp(miniapp.value);

    bridge.receiveMiniapp(eventFrame({
      method: "Debugger.paused",
      params: {
        reason: "other",
        callFrames: [{
          callFrameId: "frame-1",
          functionName: "onLoad",
          url: "appservice/app.js",
          lineNumber: 9,
          columnNumber: 4,
          scopeChain: [{ type: "local" }, { type: "global" }],
        }],
      },
    }), clientId);

    let state = bridge.debugState();
    expect(state.paused).toBe(true);
    expect(state.pausedSeq).toBe(1);
    expect(state.reason).toBe("other");
    expect(state.callFrames).toEqual([{
      callFrameId: "frame-1",
      functionName: "onLoad",
      url: "appservice/app.js",
      lineNumber: 9,
      columnNumber: 4,
      scopes: [{ type: "local" }, { type: "global" }],
    }]);

    bridge.receiveMiniapp(eventFrame({ method: "Debugger.resumed", params: {} }), clientId);
    state = bridge.debugState();
    expect(state.paused).toBe(false);
    expect(state.resumedSeq).toBe(1);
    expect(state.callFrames).toEqual([]);
  });

  it("collects scriptParsed entries, dedupes by id, and bounds the ring", () => {
    const bridge = new CdpBridge();
    const miniapp = peer();
    const clientId = bridge.addMiniapp(miniapp.value);

    for (let index = 0; index < 520; index += 1) {
      bridge.receiveMiniapp(eventFrame({
        method: "Debugger.scriptParsed",
        params: { scriptId: String(index % 510), url: `index${index % 510}.js` },
      }), clientId);
    }

    const state = bridge.debugState();
    expect(state.scripts).toHaveLength(500);
    expect(state.scriptsTruncated).toBeGreaterThan(0);
    expect(state.scripts.some((script) => script.scriptId === "509")).toBe(true);
  });

  it("clears the script list when the global object is cleared", () => {
    const bridge = new CdpBridge();
    const miniapp = peer();
    const clientId = bridge.addMiniapp(miniapp.value);

    bridge.receiveMiniapp(eventFrame({
      method: "Debugger.scriptParsed",
      params: { scriptId: "1", url: "a.js" },
    }), clientId);
    bridge.receiveMiniapp(eventFrame({ method: "Debugger.globalObjectCleared", params: {} }), clientId);

    expect(bridge.debugState().scripts).toEqual([]);
  });

  it("sends Debugger.enable once per connection when enabled", () => {
    const bridge = new CdpBridge();
    const miniapp = peer();
    const clientId = bridge.addMiniapp(miniapp.value);

    bridge.enableDebugger();
    bridge.enableDebugger();

    const methods = miniapp.sent.map((frame) => {
      const payload = (typeof frame === "string" ? undefined : decodeCdpMessage(frame))?.payload ?? "{}";
      return (JSON.parse(payload) as { method?: string }).method ?? "";
    });
    expect(methods.filter((method) => method === "Debugger.enable")).toHaveLength(1);
    expect(bridge.debugState().enabled).toBe(true);
    void clientId;
  });

  it("enables the debugger on a target that connects after the request", () => {
    const bridge = new CdpBridge();
    // cdp.debug 先于小程序连接到达：只记意图，不发命令。
    bridge.enableDebugger();

    const miniapp = peer();
    bridge.addMiniapp(miniapp.value);
    const methods = miniapp.sent.map((frame) => {
      const payload = (typeof frame === "string" ? undefined : decodeCdpMessage(frame))?.payload ?? "{}";
      return (JSON.parse(payload) as { method?: string }).method ?? "";
    });
    expect(methods).toContain("Debugger.enable");
  });

  it("enables the debugger on the newly locked target after a switch", () => {
    const bridge = new CdpBridge();
    const first = peer();
    const second = peer();
    const firstId = bridge.addMiniapp(first.value);
    bridge.addMiniapp(second.value);

    bridge.enableDebugger();
    bridge.switchMiniapp(firstId + 1);

    const methods = second.sent.map((frame) => {
      const payload = (typeof frame === "string" ? undefined : decodeCdpMessage(frame))?.payload ?? "{}";
      return (JSON.parse(payload) as { method?: string }).method ?? "";
    });
    expect(methods).toContain("Debugger.enable");
  });
});

/** Runs the identity probe the way CDP does: inside the miniapp's own realm. */
function runIdentity(window: Record<string, unknown>): Record<string, unknown> {
  return JSON.parse(runInNewContext(APP_INFO_EXPRESSION, { window }) as string) as Record<string, unknown>;
}

describe("miniapp identity", () => {
  it("treats WMPF's own appservice frame titles as shell names, not the miniapp's", () => {
    // 真机实测：appid 那个页面的 CDP 标题就是 AppIndex（小游戏是 GameIndex），
    // 它是 WMPF 给 appservice 帧起的名字，和小程序名无关。
    expect(miniappNameFromTargetTitle("AppIndex", "wxabc1234567890a")).toBe("");
    expect(miniappNameFromTargetTitle("GameIndex", "wxabc1234567890a")).toBe("");
    expect(miniappNameFromTargetTitle("小程序", "wxabc1234567890a")).toBe("");
    expect(miniappNameFromTargetTitle("wxabc1234567890a", "wxabc1234567890a")).toBe("");
    expect(miniappNameFromTargetTitle("演示小程序", "wxabc1234567890a")).toBe("演示小程序");
  });

  it("reads the nickname from nav.wxFrame.__wxConfig, where current WMPF keeps it", () => {
    // 当前 WMPF 版本的真实形状：页面级 window.__wxConfig 不存在，
    // window.nav.wxFrame.__wxConfig.accountInfo 才是昵称和图标所在。
    const window = {
      nav: {
        wxFrame: {
          __wxConfig: {
            accountInfo: { appId: "wxabc1234567890a", nickname: "演示小程序", icon: "https://mmbiz.qpic.cn/icon.png" },
          },
        },
      },
    };
    expect(runIdentity(window)).toEqual({
      appid: "wxabc1234567890a",
      name: "演示小程序",
      icon: "https://mmbiz.qpic.cn/icon.png",
    });
  });

  it("still reads the older page-level __wxConfig with a nested appAccount", () => {
    const window = {
      __wxConfig: {
        accountInfo: { appId: "wxabc1234567890a", appAccount: { nickname: "旧版昵称", icon: "https://example.com/i.png" } },
      },
    };
    expect(runIdentity(window)).toEqual({
      appid: "wxabc1234567890a",
      name: "旧版昵称",
      icon: "https://example.com/i.png",
    });
  });

  it("returns nothing when no config is reachable", () => {
    expect(runIdentity({})).toEqual({});
  });
});
