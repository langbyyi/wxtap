import { describe, expect, it, vi } from "vitest";
import { CdpBridge } from "./cdp-bridge.js";
import { H5Sessions, isH5Page } from "./h5-sessions.js";
import { decodeCdpMessage, encodeCdpMessage } from "../protocol/wmpf-codec.js";

const target = { targetId: "h5-pay", type: "page", title: "支付页", url: "https://example.com/pay" };
const page = { ...target, readyState: "complete", hasDocument: true };
const turns = async () => { for (let i = 0; i < 12; i++) await Promise.resolve(); };

function fixture() {
  const bridge = new CdpBridge();
  const sent: Buffer[] = [];
  const clientId = bridge.addMiniapp({ send: (message) => sent.push(message as Buffer) });
  const received: Record<string, unknown>[] = [];
  const close = vi.fn();
  const peer = { send: (message: string | Buffer) => received.push(JSON.parse(String(message))), close };
  const command = () => JSON.parse(decodeCdpMessage(sent[sent.length - 1])?.payload ?? "{}");
  const receive = (response: Record<string, unknown>, source = clientId) => bridge.receiveMiniapp(encodeCdpMessage({
    sequence: 1, category: "chromeDevtoolsResult", operationId: 0, payload: JSON.stringify(response), jsContextId: "",
  }), source);
  const reply = async (result: unknown, sessionId = "") => {
    receive({ id: command().id, result, ...(sessionId ? { sessionId } : {}) }); await turns();
  };
  const connect = async () => {
    const ready = bridge.addH5Devtools(peer, clientId, target.targetId);
    await reply({ targetInfos: [target] });
    expect(command()).toMatchObject({ method: "Target.attachToTarget", params: { targetId: target.targetId, flatten: true } });
    await reply({ sessionId: "h5-session" });
    expect(command()).toMatchObject({ method: "Runtime.evaluate", sessionId: "h5-session" });
    await reply({ result: { value: page } }, "h5-session");
    await ready;
  };
  return { bridge, sent, clientId, peer, received, close, command, receive, reply, connect };
}

describe("persistent H5 DevTools relay", () => {
  it("rereads metadata when loading finishes during an earlier read of the same URL", async () => {
    const f = fixture(); await f.connect(); const before = f.sent.length;
    f.receive({ method: "Target.targetInfoChanged", params: { targetInfo: { ...target, title: "loading" } } }); await turns();
    f.receive({ method: "Page.loadEventFired", sessionId: "h5-session", params: {} });
    await f.reply({ result: { value: { ...page, title: "loading" } } }, "h5-session");
    expect(f.sent).toHaveLength(before + 2);
    await f.reply({ result: { value: { ...page, title: "Loaded" } } }, "h5-session");
    expect(f.bridge.h5Sessions()[0].title).toBe("Loaded");
    const closed = f.bridge.removeH5Devtools(f.peer); await f.reply({}); await closed;
  });

  it("reads the actual document after loading and ignores a metadata reply from an older navigation", async () => {
    const f = fixture(); await f.connect(); const before = f.sent.length;
    f.receive({ method: "Page.loadEventFired", sessionId: "h5-session", params: {} }); await turns();
    expect(f.sent).toHaveLength(before + 1);
    expect(f.command()).toMatchObject({ method: "Runtime.evaluate", sessionId: "h5-session", params: { returnByValue: true, throwOnSideEffect: true } });
    f.receive({ method: "Page.frameNavigated", sessionId: "h5-session", params: { frame: { id: "root", url: "https://example.com/receipt" } } });
    f.receive({ method: "Page.loadEventFired", sessionId: "h5-session", params: {} });
    await f.reply({ result: { value: page } }, "h5-session");
    expect(f.bridge.h5Sessions()[0].url).toBe("https://example.com/receipt");
    expect(f.sent).toHaveLength(before + 2);
    await f.reply({ result: { value: { ...page, url: "https://example.com/receipt", title: "Receipt" } } }, "h5-session");
    expect(f.bridge.h5Sessions()).toMatchObject([{ url: "https://example.com/receipt", title: "Receipt", error: "", active: true }]);
    const closed = f.bridge.removeH5Devtools(f.peer); await f.reply({}); await closed;
  });

  it("refreshes same-document navigation and preserves a working connection when its metadata read fails", async () => {
    const f = fixture(); await f.connect();
    f.receive({ method: "Page.navigatedWithinDocument", sessionId: "h5-session", params: { frameId: "root", url: "https://example.com/pay#done" } }); await turns();
    f.receive({ id: f.command().id, sessionId: "h5-session", error: { message: "context destroyed" } }); await turns();
    expect(f.bridge.h5Sessions()).toMatchObject([{ state: "connected", active: true, error: expect.stringContaining("context destroyed") }]);
    f.receive({ method: "Page.loadEventFired", sessionId: "h5-session", params: {} }); await turns();
    await f.reply({ result: { value: { ...page, url: "https://example.com/pay#done", title: "Done" } } }, "h5-session");
    expect(f.bridge.h5Sessions()).toMatchObject([{ url: "https://example.com/pay#done", title: "Done", error: "" }]);
    const closed = f.bridge.removeH5Devtools(f.peer); await f.reply({}); await closed;
  });

  it("updates the session URL after root navigation without confusing child frames or losing the connection", async () => {
    const f = fixture(); await f.connect();
    const main: unknown[] = []; f.bridge.addDevtools({ send: (packet) => main.push(packet) });
    const navigated = { method: "Page.frameNavigated", sessionId: "h5-session", params: { frame: { id: "root-frame", url: "https://example.com/receipt" } } };
    f.receive(navigated);
    expect(f.bridge.h5Sessions()).toMatchObject([{ url: "https://example.com/receipt", title: "", state: "connected", active: true }]);
    expect(f.received).toContainEqual({ method: navigated.method, params: navigated.params });
    f.receive({ method: "Page.frameNavigated", sessionId: "h5-session", params: { frame: { id: "nested", parentId: "root-frame", url: "https://example.com/frame" } } });
    f.receive({ method: "Target.attachedToTarget", sessionId: "h5-session", params: { sessionId: "iframe", targetInfo: { targetId: "child" } } });
    f.receive({ method: "Page.frameNavigated", sessionId: "iframe", params: { frame: { id: "oopif", url: "https://other.example/frame" } } });
    expect(f.bridge.h5Sessions()[0].url).toBe("https://example.com/receipt");
    f.receive({ method: "Page.frameNavigated", sessionId: "h5-session", params: { frame: { id: "root-frame", url: "about:blank" } } });
    expect(f.bridge.h5Sessions()).toMatchObject([{ url: "about:blank", title: "", active: true }]);
    expect(main).toHaveLength(0);
    const closed = f.bridge.removeH5Devtools(f.peer); await f.reply({}); await closed;
  });

  it.each(["", "h5-session"])("updates only its own live target metadata from session %s", async (sessionId) => {
    const f = fixture(); await f.connect();
    const main: unknown[] = []; f.bridge.addDevtools({ send: (packet) => main.push(packet) });
    const event = { method: "Target.targetInfoChanged", params: { targetInfo: { ...target, url: "https://example.com/receipt", title: "Receipt" } }, ...(sessionId ? { sessionId } : {}) };
    const other = f.bridge.addMiniapp({ send: () => {} });
    f.receive(event, other);
    expect(f.bridge.h5Sessions()[0].url).toBe(target.url);
    main.length = 0;
    f.receive(event);
    expect(f.bridge.h5Sessions()).toMatchObject([{ url: "https://example.com/receipt", title: "Receipt", active: true }]);
    expect(f.received).toContainEqual({ method: event.method, params: event.params });
    expect(main).toHaveLength(0);
    const closed = f.bridge.removeH5Devtools(f.peer); await f.reply({}); await closed;
    f.receive({ ...event, params: { targetInfo: { ...target, title: "late" } } });
    expect(f.bridge.h5Sessions()[0].title).toBe("Receipt");
  });

  it("does not treat the WeChat LiteApp root as a business H5 page", () => {
    for (const url of ["https://liteapp.weixin.qq.com/", "https://LITEAPP.WEIXIN.QQ.COM?debug=1"]) {
      expect(isH5Page({ type: "page", url })).toBe(false);
    }
    for (const url of ["https://liteapp.weixin.qq.com/article", "https://liteapp.weixin.qq.com.example.com/", "https://mp.weixin.qq.com/s/article"]) {
      expect(isH5Page({ type: "page", url })).toBe(true);
    }
  });

  it("rejects a container before attaching and releases a business target that evaluates to a container", async () => {
    const f = fixture();
    const rejectedContainer = expect(f.bridge.addH5Devtools(f.peer, f.clientId, target.targetId)).rejects.toThrow("H5");
    await f.reply({ targetInfos: [{ ...target, url: "https://liteapp.weixin.qq.com/" }] });
    await rejectedContainer;
    expect(f.sent).toHaveLength(1);
    expect(f.bridge.h5Sessions()).toMatchObject([{ active: false }]);

    const rejectedContext = expect(f.bridge.addH5Devtools(f.peer, f.clientId, target.targetId)).rejects.toThrow("页面上下文");
    await f.reply({ targetInfos: [target] }); await f.reply({ sessionId: "container-session" });
    await f.reply({ result: { value: { ...page, url: "https://liteapp.weixin.qq.com/" } } }, "container-session");
    expect(f.command()).toMatchObject({ method: "Target.detachFromTarget", params: { sessionId: "container-session" } });
    await f.reply({}); await rejectedContext;
    expect(f.bridge.h5Sessions()).toMatchObject([{ active: false }]);
  });

  it("does not report a window as released while its attachment is still uncertain", async () => {
    const f = fixture(); const ready = f.bridge.addH5Devtools(f.peer, f.clientId, target.targetId);
    const rejected = expect(ready).rejects.toThrow();
    await f.reply({ targetInfos: [target] });
    await f.bridge.removeH5Devtools(f.peer);
    expect(f.bridge.h5Sessions()).toMatchObject([{ state: 'closing', active: true }]);
    await expect(f.bridge.closeH5Target(f.clientId, target.targetId)).rejects.toThrow('尚未确认');
    f.bridge.removeMiniapp(f.clientId); await rejected;
    expect(f.bridge.h5Sessions()).toMatchObject([{ active: false }]);
  });
  it("does not recreate source retirement records after disconnect races with a detach ACK", async () => {
    const commands: Record<string, unknown>[] = [];
    let nextId = 1_000_000_000;
    const sessions = new H5Sessions({ generation: () => 0, allocateId: () => nextId++, send: (_, packet) => commands.push(packet) });
    const peer = { send: () => {}, close: () => {} };
    const reply = async (result: unknown, sessionId = '') => {
      sessions.receive(7, { id: commands.at(-1)!.id, result, ...(sessionId ? { sessionId } : {}) }); await turns();
    };
    const ready = sessions.open(peer, 7, target.targetId);
    await reply({ targetInfos: [target] }); await reply({ sessionId: 'root' });
    await reply({ result: { value: page } }, 'root'); await ready;
    const closed = sessions.disconnect(peer);
    sessions.receive(7, { id: commands.at(-1)!.id, result: {} });
    sessions.invalidate(7, 'source disconnected', true);
    await closed; await turns();
    const retired: unknown = Reflect.get(sessions, 'retired');
    expect(retired).toBeInstanceOf(Map);
    if (!(retired instanceof Map)) throw new Error('missing retirement registry');
    expect(retired.size).toBe(0);
    expect(sessions.list()).toMatchObject([{ active: false }]);
  });
  it("frees the window slot when an early root detaches before its attach reply", async () => {
    const f = fixture(); const ready = f.bridge.addH5Devtools(f.peer, f.clientId, target.targetId);
    const rejected = expect(ready).rejects.toThrow();
    await f.reply({ targetInfos: [target] }); const attachId = f.command().id;
    f.receive({ method: "Target.attachedToTarget", params: { sessionId: "early", targetInfo: target } });
    f.receive({ method: "Target.detachedFromTarget", params: { sessionId: "early" } }); await turns();
    expect(f.bridge.h5Sessions()[0].active).toBe(false);
    f.receive({ id: attachId, result: { sessionId: "early" } }); await rejected;
    expect(f.bridge.h5Sessions()[0].active).toBe(false);
  });

  it("bounds retirement records without sending evicted sessions into the main stream", async () => {
    const f = fixture(); await f.connect(); const main: unknown[] = [];
    f.bridge.addDevtools({ send: (packet) => main.push(packet) });
    for (let i = 0; i < 4100; i++) {
      f.receive({ method: "Target.attachedToTarget", sessionId: "h5-session", params: { sessionId: `bounded-${i}` } });
      f.receive({ method: "Target.detachedFromTarget", sessionId: "h5-session", params: { sessionId: `bounded-${i}` } });
    }
    f.receive({ method: "Runtime.consoleAPICalled", sessionId: "bounded-4099", params: {} });
    expect(main).toHaveLength(0); expect(f.close).toHaveBeenCalled();
    f.receive({ method: "Target.attachedToTarget", params: { sessionId: "cap-late-root", targetInfo: target } });
    await f.reply({});
    expect(f.command()).toMatchObject({ method: "Target.detachFromTarget", params: { sessionId: "cap-late-root" } });
    await f.reply({});
    await expect(f.bridge.addH5Devtools({ send: () => {} }, f.clientId, "another")).rejects.toThrow("上限");
  });

  it("records normal socket closure as released even if the browser emits detached before the ACK", async () => {
    const f = fixture(); await f.connect();
    const closed = f.bridge.removeH5Devtools(f.peer); await turns();
    f.receive({ method: "Target.detachedFromTarget", params: { sessionId: "h5-session" } }); await closed;
    expect(f.bridge.h5Sessions()).toMatchObject([{ state: "closed", active: false, error: "" }]);
  });
  it("drains a duplicate root arriving while the original root is being released", async () => {
    const f = fixture(); await f.connect();
    const closed = f.bridge.removeH5Devtools(f.peer); await turns();
    const detachId = f.command().id;
    f.receive({ method: "Target.attachedToTarget", params: { sessionId: "late-duplicate", targetInfo: target } });
    f.receive({ id: detachId, result: {} }); await turns();
    expect(f.command()).toMatchObject({ method: "Target.detachFromTarget", params: { sessionId: "late-duplicate" } });
    expect(f.bridge.h5Sessions()[0].active).toBe(true);
    await f.reply({}); await closed;
    expect(f.bridge.h5Sessions()[0].active).toBe(false);
  });

  it("does not confuse the main root's detached event with a duplicate root's release acknowledgement", async () => {
    const f = fixture(); await f.connect();
    f.receive({ method: "Target.attachedToTarget", params: { sessionId: "duplicate", targetInfo: target } }); await turns();
    const detachId = f.command().id;
    f.receive({ method: "Target.detachedFromTarget", params: { sessionId: "h5-session" } }); await turns();
    expect(f.bridge.h5Sessions()[0].active).toBe(true);
    f.receive({ id: detachId, result: {} }); await turns();
    expect(f.bridge.h5Sessions()[0].active).toBe(false);
  });

  it("retires descendant sessions when their parent iframe detaches", async () => {
    const f = fixture(); await f.connect(); const main: unknown[] = [];
    f.bridge.addDevtools({ send: (packet) => main.push(packet) });
    f.receive({ method: "Target.attachedToTarget", sessionId: "h5-session", params: { sessionId: "frame" } });
    f.receive({ method: "Target.attachedToTarget", sessionId: "frame", params: { sessionId: "worker" } });
    f.receive({ method: "Target.detachedFromTarget", sessionId: "h5-session", params: { sessionId: "frame" } });
    f.receive({ method: "Runtime.consoleAPICalled", sessionId: "worker", params: {} });
    const count = f.sent.length;
    f.bridge.forwardH5Devtools(f.peer, '{"id":10,"method":"Runtime.enable","sessionId":"worker"}'); await turns();
    expect(f.sent).toHaveLength(count); expect(main).toHaveLength(0);
  });
  it("rejects main-window attachments to an H5 target already owned by a window", async () => {
    const f = fixture(); await f.connect();
    const main: unknown[] = []; f.bridge.addDevtools({ send: (packet) => main.push(JSON.parse(String(packet))) });
    const count = f.sent.length;
    f.bridge.forwardDevtools(JSON.stringify({ id: 12, method: "Target.attachToTarget", params: { targetId: target.targetId } }));
    await expect(f.bridge.sendCommand("Target.attachToTarget", { targetId: target.targetId }, 100)).rejects.toThrow("独立 H5");
    expect(f.sent).toHaveLength(count);
    expect(main).toContainEqual(expect.objectContaining({ id: 12, error: expect.any(Object) }));
  });

  it("does not resurrect retired workers or leak late events after more than 512 detachments", async () => {
    const f = fixture(); await f.connect(); const main: unknown[] = [];
    f.bridge.addDevtools({ send: (packet) => main.push(packet) });
    for (let i = 0; i < 514; i++) {
      const params = { sessionId: `child-${i}` };
      f.receive({ method: "Target.attachedToTarget", sessionId: "h5-session", params });
      f.receive({ method: "Target.detachedFromTarget", sessionId: "h5-session", params });
    }
    f.receive({ method: "Target.attachedToTarget", sessionId: "h5-session", params: { sessionId: "child-0" } });
    f.receive({ method: "Runtime.consoleAPICalled", sessionId: "child-0", params: { marker: "retired" } });
    const count = f.sent.length;
    f.bridge.forwardH5Devtools(f.peer, '{"id":10,"method":"Runtime.enable","sessionId":"child-0"}'); await turns();
    expect(f.sent).toHaveLength(count); expect(main).toHaveLength(0);
    expect(f.received).toContainEqual(expect.objectContaining({ id: 10, error: expect.any(Object) }));
  });

  it("frees ownership when the target is destroyed during attachment and suppresses its late acknowledgement", async () => {
    const f = fixture(); const ready = f.bridge.addH5Devtools(f.peer, f.clientId, target.targetId);
    const rejected = expect(ready).rejects.toThrow("目标已关闭");
    await f.reply({ targetInfos: [target] }); const attachId = f.command().id;
    f.receive({ method: "Target.targetDestroyed", params: { targetId: target.targetId } }); await turns(); await rejected;
    expect(f.bridge.h5Sessions()).toMatchObject([{ active: false, state: "error" }]);
    const main: unknown[] = []; f.bridge.addDevtools({ send: (packet) => main.push(packet) });
    f.receive({ id: attachId, result: { sessionId: "destroyed-late" } });
    f.receive({ method: "Target.attachedToTarget", params: { sessionId: "destroyed-late", targetInfo: target } });
    f.receive({ method: "Runtime.consoleAPICalled", sessionId: "destroyed-late", params: {} });
    expect(main).toHaveLength(0);
  });

  it("tracks and releases unexpected duplicate root attachments instead of leaking their events", async () => {
    const f = fixture(); await f.connect(); const main: unknown[] = [];
    f.bridge.addDevtools({ send: (packet) => main.push(packet) });
    f.receive({ method: "Target.attachedToTarget", params: { sessionId: "duplicate", targetInfo: target } }); await turns();
    f.receive({ method: "Runtime.consoleAPICalled", sessionId: "duplicate", params: {} });
    expect(f.command()).toMatchObject({ method: "Target.detachFromTarget", params: { sessionId: "duplicate" } });
    await f.reply({});
    expect(f.command()).toMatchObject({ method: "Target.detachFromTarget", params: { sessionId: "h5-session" } });
    await f.reply({});
    expect(main).toHaveLength(0); expect(f.bridge.h5Sessions()).toMatchObject([{ active: false }]);
  });
  it("routes Console, scripts, Network events and response bodies only to the selected H5 window", async () => {
    const f = fixture(); const main: unknown[] = [];
    f.bridge.addDevtools({ send: (message) => main.push(message) });
    await f.connect();
    f.bridge.forwardH5Devtools(f.peer, JSON.stringify({ id: 1, method: "Network.getResponseBody", params: { requestId: "request-1" } }));
    await turns();
    expect(f.command()).toMatchObject({ method: "Network.getResponseBody", sessionId: "h5-session" });
    expect(f.command().id).not.toBe(1);
    await f.reply({ body: "{\"ok\":true}", base64Encoded: false }, "h5-session");
    expect(f.received).toContainEqual({ id: 1, result: { body: "{\"ok\":true}", base64Encoded: false } });
    for (const method of ["Runtime.consoleAPICalled", "Debugger.scriptParsed", "Network.responseReceived"]) {
      f.receive({ method, sessionId: "h5-session", params: { marker: "h5" } });
      expect(f.received).toContainEqual({ method, params: { marker: "h5" } });
    }
    expect(main).toHaveLength(0);
    expect(f.bridge.h5Sessions()).toMatchObject([{ clientId: f.clientId, targetId: target.targetId, state: "connected", active: true }]);
    f.receive({ method: "Runtime.consoleAPICalled", params: { marker: "miniapp" } });
    expect(f.received).not.toContainEqual(expect.objectContaining({ params: { marker: "miniapp" } }));
  });

  it("buffers initialization commands until the selected page is attached", async () => {
    const f = fixture(); const ready = f.bridge.addH5Devtools(f.peer, f.clientId, target.targetId);
    f.bridge.forwardH5Devtools(f.peer, '{"id":1,"method":"Runtime.enable"}');
    expect(f.command().method).toBe("Target.getTargets");
    await f.reply({ targetInfos: [target] }); await f.reply({ sessionId: "h5-session" });
    await f.reply({ result: { value: page } }, "h5-session"); await ready; await turns();
    expect(f.command()).toMatchObject({ method: "Runtime.enable", sessionId: "h5-session" });
    await f.reply({}, "h5-session");
    expect(f.received).toContainEqual({ id: 1, result: {} });
  });

  it("does not allow another source, guessed sessions, miniapp resources or duplicate windows", async () => {
    const f = fixture(); await f.connect();
    const other = f.bridge.addMiniapp({ send: () => {} });
    f.receive({ method: "Network.responseReceived", sessionId: "h5-session", params: { wrong: true } }, other);
    expect(f.received).toHaveLength(0);
    const before = f.sent.length;
    f.bridge.forwardH5Devtools(f.peer, '{"id":2,"method":"Runtime.evaluate","sessionId":"other-session","params":{"expression":"1"}}');
    await turns();
    expect(f.sent).toHaveLength(before);
    expect(f.received).toContainEqual(expect.objectContaining({ id: 2, error: expect.any(Object) }));
    await expect(f.bridge.addH5Devtools({ send: () => {} }, f.clientId, target.targetId)).rejects.toThrow("已有");
  });

  it("preserves child-session routing for iframes and workers without leaking events", async () => {
    const f = fixture(); const main: unknown[] = [];
    f.bridge.addDevtools({ send: (message) => main.push(message) }); await f.connect();
    f.receive({ method: "Target.attachedToTarget", sessionId: "h5-session", params: { sessionId: "child", targetInfo: { targetId: "frame", type: "iframe" } } });
    f.bridge.forwardH5Devtools(f.peer, '{"id":3,"method":"Runtime.enable","sessionId":"child"}');
    await turns(); expect(f.command().sessionId).toBe("child"); await f.reply({}, "child");
    expect(f.received).toContainEqual({ id: 3, sessionId: "child", result: {} });
    f.receive({ method: "Runtime.consoleAPICalled", sessionId: "child", params: {} });
    expect(f.received).toContainEqual({ method: "Runtime.consoleAPICalled", sessionId: "child", params: {} });
    expect(main).toHaveLength(0);
  });

  it("releases a session when its window closes and rejects another probe while it is owned", async () => {
    const f = fixture(); await f.connect();
    await expect(f.bridge.probeTarget(f.clientId, target.targetId)).rejects.toThrow("调试会话");
    const closed = f.bridge.removeH5Devtools(f.peer); await turns();
    expect(f.command()).toMatchObject({ method: "Target.detachFromTarget", params: { sessionId: "h5-session" } });
    await f.reply({}); await closed;
    expect(f.bridge.h5Sessions()).toMatchObject([{ state: "closed", active: false }]);
    f.receive({ method: "Network.responseReceived", sessionId: "h5-session", params: {} });
    expect(f.received).toHaveLength(0);
  });

  it("cleans up an attach acknowledgement that arrives after the window closes", async () => {
    const f = fixture(); const ready = f.bridge.addH5Devtools(f.peer, f.clientId, target.targetId);
    const rejected = expect(ready).rejects.toThrow();
    await f.reply({ targetInfos: [target] });
    const attachId = f.command().id;
    await f.bridge.removeH5Devtools(f.peer);
    f.receive({ id: attachId, result: { sessionId: "late" } }); await turns();
    expect(f.command()).toMatchObject({ method: "Target.detachFromTarget", params: { sessionId: "late" } });
    await f.reply({}); await rejected;
    expect(f.bridge.h5Sessions()).toMatchObject([{ state: "closed", active: false }]);
  });

  it("fails visibly and allows release retry when detach is not confirmed", async () => {
    const f = fixture(); await f.connect();
    const closed = f.bridge.closeH5Target(f.clientId, target.targetId);
    const rejected = expect(closed).rejects.toThrow("释放"); await turns();
    f.receive({ id: f.command().id, error: { message: "detach failed" } }); await rejected;
    expect(f.bridge.h5Sessions()).toMatchObject([{ state: "error", active: true, error: expect.stringContaining("detach failed") }]);
    const retry = f.bridge.closeH5Target(f.clientId, target.targetId); await turns(); await f.reply({}); await retry;
    expect(f.bridge.h5Sessions()).toMatchObject([{ state: "closed", active: false }]);
  });

  it.each(["switch", "reload", "disconnect"])("invalidates the H5 window on source %s", async (action) => {
    const f = fixture(); await f.connect();
    if (action === "switch") f.bridge.switchMiniapp(f.bridge.addMiniapp({ send: () => {} }));
    if (action === "reload") f.bridge.receiveMiniapp(encodeCdpMessage({ sequence: 1, category: "setupContext", operationId: 0, payload: "{}", jsContextId: "" }), f.clientId);
    if (action === "disconnect") f.bridge.removeMiniapp(f.clientId);
    await turns();
    expect(f.close).toHaveBeenCalled();
    expect(f.bridge.h5Sessions()[0].state).not.toBe("connected");
    if (action !== "disconnect") {
      const detach = f.sent.map((buffer) => JSON.parse(decodeCdpMessage(buffer)?.payload ?? "{}")).find((command) => command.method === "Target.detachFromTarget");
      expect(detach).toBeDefined(); f.receive({ id: detach.id, result: {} }); await turns();
    }
  });
});
