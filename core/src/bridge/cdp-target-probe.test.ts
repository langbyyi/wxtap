import { describe, expect, it, vi } from "vitest";
import { CdpBridge } from "./cdp-bridge.js";
import { decodeCdpMessage, encodeCdpMessage } from "../protocol/wmpf-codec.js";

function fixture() {
  const bridge = new CdpBridge();
  const sent: Buffer[] = [];
  const id = bridge.addMiniapp({ send: (message) => sent.push(message as Buffer) });
  const command = () => JSON.parse(decodeCdpMessage(sent[sent.length - 1])?.payload ?? "{}") as Record<string, unknown>;
  const receive = (payload: Record<string, unknown>, clientId = id) => bridge.receiveMiniapp(encodeCdpMessage({
    sequence: 1, category: "chromeDevtoolsResult", operationId: 0, payload: JSON.stringify(payload), jsContextId: "",
  }), clientId);
  const reply = async (result: unknown, sessionId = "", clientId = id) => {
    receive({ id: command().id, result, ...(sessionId ? { sessionId } : {}) }, clientId);
    for (let turn = 0; turn < 6; turn += 1) await Promise.resolve();
  };
  return { bridge, sent, id, command, receive, reply };
}

const h5 = { targetId: "h5-1", type: "page", url: "https://example.com/test", title: "测试 H5" };
const page = { url: h5.url, title: h5.title, readyState: "complete", hasDocument: true };

describe("isolated H5 connection probe", () => {
  it("rejects external command ids reserved for private probes instead of swallowing replies", () => {
    const f = fixture(); const forwarded: unknown[] = [];
    f.bridge.addDevtools({ send: (message) => forwarded.push(JSON.parse(message as string)) });
    f.bridge.forwardDevtools(JSON.stringify({ id: 1_000_000_000, method: "Runtime.evaluate", params: { expression: "1" } }));
    expect(f.sent).toHaveLength(0);
    expect(forwarded).toEqual([{ id: 1_000_000_000, error: { code: -32600, message: "CDP 命令 ID 位于内部验证保留区间，请使用小于 1000000000 的非负整数" } }]);
  });

  it("keeps delayed detached events private after successful cleanup", async () => {
    const f = fixture(); const forwarded: unknown[] = [];
    f.bridge.addDevtools({ send: (message) => forwarded.push(message) });
    const probe = f.bridge.probeTarget(f.id, h5.targetId);
    await f.reply({ targetInfos: [h5] }); await f.reply({ sessionId: "session-h5" });
    await f.reply({ result: { value: page } }, "session-h5"); await f.reply({}); await probe;
    f.receive({ method: "Target.detachedFromTarget", params: { sessionId: "session-h5", targetId: h5.targetId } });
    expect(forwarded).toHaveLength(0);
  });

  it("isolates an attached event that arrives before the attach acknowledgement", async () => {
    const f = fixture(); const forwarded: unknown[] = [];
    f.bridge.addDevtools({ send: (message) => forwarded.push(message) });
    const probe = f.bridge.probeTarget(f.id, h5.targetId);
    await f.reply({ targetInfos: [h5] });
    f.receive({ method: "Target.attachedToTarget", params: { sessionId: "session-h5", targetInfo: h5 } });
    expect(forwarded).toHaveLength(0);
    await f.reply({ sessionId: "session-h5" });
    await f.reply({ result: { value: page } }, "session-h5");
    await f.reply({}); await probe;
  });

  it.each(["before", "after", "before a repeated attached event and"])("does not revive a session detached %s the attach acknowledgement and allows another probe", async (timing) => {
    const f = fixture(); const forwarded: unknown[] = [];
    f.bridge.addDevtools({ send: (message) => forwarded.push(message) });
    const probe = f.bridge.probeTarget(f.id, h5.targetId);
    const rejected = expect(probe).rejects.toThrow("H5 临时会话已关闭");
    await f.reply({ targetInfos: [h5] });
    f.receive({ method: "Target.attachedToTarget", params: { sessionId: "session-gone", targetInfo: h5 } });
    const detach = () => f.receive({ method: "Target.detachedFromTarget", params: { sessionId: "session-gone", targetId: h5.targetId } });
    if (timing !== "after") detach();
    if (timing === "before a repeated attached event and") {
      f.receive({ method: "Target.attachedToTarget", params: { sessionId: "session-gone", targetInfo: h5 } });
    }
    f.receive({ id: f.command().id, result: { sessionId: "session-gone" } });
    if (timing === "after") detach();
    for (let turn = 0; turn < 6; turn += 1) await Promise.resolve();
    // Complete unexpected commands too, so a regression fails without leaving timers behind.
    if (f.command().method === "Runtime.evaluate") {
      f.receive({ id: f.command().id, sessionId: "session-gone", error: { message: "Session not found" } });
      for (let turn = 0; turn < 6; turn += 1) await Promise.resolve();
      f.receive({ id: f.command().id, error: { message: "No session" } });
    }
    await rejected;
    expect(f.sent).toHaveLength(2);
    const retry = f.bridge.probeTarget(f.id, h5.targetId);
    expect(f.command().method).toBe("Target.getTargets");
    await f.reply({ targetInfos: [h5] }); await f.reply({ sessionId: "session-new" });
    await f.reply({ result: { value: page } }, "session-new"); await f.reply({});
    await expect(retry).resolves.toMatchObject({ verified: true, released: true });
    expect(forwarded).toHaveLength(0);
  });

  it.each(["discovery", "attachment", "evaluation", "release"])("rejects a disconnect during %s promptly and allows probing the reconnected peer", async (stage) => {
    const f = fixture(); const probe = f.bridge.probeTarget(f.id, h5.targetId);
    const rejected = expect(probe).rejects.toThrow("H5 验证连接已断开");
    if (stage !== "discovery") await f.reply({ targetInfos: [h5] });
    if (["evaluation", "release"].includes(stage)) await f.reply({ sessionId: "session-old" });
    if (stage === "release") await f.reply({ result: { value: page } }, "session-old");
    f.bridge.removeMiniapp(f.id);
    await rejected;
    const reconnectedId = f.bridge.addMiniapp({ send: (message) => f.sent.push(message as Buffer) });
    const retry = f.bridge.probeTarget(reconnectedId, h5.targetId);
    expect(f.command().method).toBe("Target.getTargets");
    await f.reply({ targetInfos: [h5] }, "", reconnectedId);
    await f.reply({ sessionId: "session-new" }, "", reconnectedId);
    await f.reply({ result: { value: page } }, "session-new", reconnectedId);
    await f.reply({}, "", reconnectedId);
    await expect(retry).resolves.toMatchObject({ clientId: reconnectedId, verified: true, released: true });
  });

  it("isolates late attach replies and releases the late-created session after a timeout", async () => {
    vi.useFakeTimers();
    try {
      const f = fixture(); const forwarded: unknown[] = [];
      f.bridge.addDevtools({ send: (message) => forwarded.push(message) });
      const probe = f.bridge.probeTarget(f.id, h5.targetId);
      const rejected = expect(probe).rejects.toThrow("timed out: Target.attachToTarget");
      await f.reply({ targetInfos: [h5] }); const attachId = f.command().id;
      await vi.advanceTimersByTimeAsync(8000); await rejected;
      await expect(f.bridge.probeTarget(f.id, h5.targetId)).rejects.toThrow("上次附加");
      f.receive({ id: attachId, result: { sessionId: "session-late" } });
      for (let turn = 0; turn < 6; turn += 1) await Promise.resolve();
      expect(f.command()).toMatchObject({ method: "Target.detachFromTarget", params: { sessionId: "session-late" } });
      await f.reply({});
      expect(forwarded).toHaveLength(0);
    } finally { vi.clearAllTimers(); vi.useRealTimers(); }
  });

  it("keeps evaluation replies private even after their timeout and confirmed cleanup", async () => {
    vi.useFakeTimers();
    try {
      const f = fixture(); const forwarded: unknown[] = [];
      f.bridge.addDevtools({ send: (message) => forwarded.push(message) });
      const probe = f.bridge.probeTarget(f.id, h5.targetId);
      const rejected = expect(probe).rejects.toThrow("timed out: Runtime.evaluate");
      await f.reply({ targetInfos: [h5] }); await f.reply({ sessionId: "session-h5" });
      const evaluationId = f.command().id;
      await vi.advanceTimersByTimeAsync(8000); await f.reply({}); await rejected;
      f.receive({ id: evaluationId, sessionId: "session-h5", result: { result: { value: page } } });
      expect(forwarded).toHaveLength(0);
    } finally { vi.clearAllTimers(); vi.useRealTimers(); }
  });

  it("attaches to the selected H5, isolates session traffic and only succeeds after detach", async () => {
    const f = fixture(); const forwarded: unknown[] = []; const logs: unknown[] = [];
    f.bridge.addDevtools({ send: (message) => forwarded.push(message) });
    f.bridge.onConsole = (entry) => logs.push(entry);
    const probe = f.bridge.probeTarget(f.id, h5.targetId);
    expect(f.command().method).toBe("Target.getTargets");
    await f.reply({ targetInfos: [h5] });
    expect(f.command()).toMatchObject({ method: "Target.attachToTarget", params: { targetId: h5.targetId, flatten: true } });
    await f.reply({ sessionId: "session-h5" });
    expect(f.command()).toMatchObject({ method: "Runtime.evaluate", sessionId: "session-h5", params: { returnByValue: true } });
    f.receive({ method: "Runtime.consoleAPICalled", sessionId: "session-h5", params: { type: "log", args: [{ value: "h5-private" }] } });
    // A matching id from another session must not confirm this probe.
    f.receive({ id: f.command().id, sessionId: "other-session", result: { result: { value: page } } });
    expect(f.command().method).toBe("Runtime.evaluate");
    await f.reply({ result: { value: page } }, "session-h5");
    expect(f.command()).toMatchObject({ method: "Target.detachFromTarget", params: { sessionId: "session-h5" } });
    let settled = false; void probe.then(() => { settled = true; });
    await Promise.resolve(); expect(settled).toBe(false);
    await f.reply({});
    await expect(probe).resolves.toMatchObject({ clientId: f.id, targetId: h5.targetId, verified: true, released: true, url: h5.url });
    expect(forwarded).toHaveLength(0); expect(logs).toHaveLength(0);
  });

  it("rejects stale and unlocked source connections without sending", async () => {
    const f = fixture(); const other = f.bridge.addMiniapp({ send: () => {} });
    await expect(f.bridge.probeTarget(other, h5.targetId)).rejects.toThrow("目标已变化");
    f.bridge.setLock(false);
    await expect(f.bridge.probeTarget(f.id, h5.targetId)).rejects.toThrow("锁定");
    expect(f.sent).toHaveLength(0);
  });

  it.each([
    { ...h5, url: "https://servicewechat.com/wxabc1234567890a/8/page-frame.html" },
    { ...h5, url: "https://liteapp.weixin.qq.com/" },
    { ...h5, type: "worker" },
  ])("refuses to probe non-H5 targets", async (target) => {
    const f = fixture(); const probe = f.bridge.probeTarget(f.id, h5.targetId);
    const rejected = expect(probe).rejects.toThrow("H5");
    await f.reply({ targetInfos: [target] }); await rejected;
    expect(f.sent).toHaveLength(1);
  });

  it("preserves evaluation errors and still releases the temporary session", async () => {
    const f = fixture(); const probe = f.bridge.probeTarget(f.id, h5.targetId);
    const rejected = expect(probe).rejects.toThrow("Runtime.evaluate: evaluation unsupported");
    await f.reply({ targetInfos: [h5] }); await f.reply({ sessionId: "session-h5" });
    f.receive({ id: f.command().id, sessionId: "session-h5", error: { message: "evaluation unsupported" } });
    for (let turn = 0; turn < 6; turn += 1) await Promise.resolve();
    expect(f.command().method).toBe("Target.detachFromTarget");
    await f.reply({}); await rejected;
  });

  it.each(["about:blank", "https://liteapp.weixin.qq.com/"])("does not confirm a discovered H5 that is executing in %s", async (url) => {
    const f = fixture(); const probe = f.bridge.probeTarget(f.id, h5.targetId);
    const rejected = expect(probe).rejects.toThrow("尚未进入 H5 网页");
    await f.reply({ targetInfos: [h5] }); await f.reply({ sessionId: "session-h5" });
    await f.reply({ result: { value: { ...page, url } } }, "session-h5");
    expect(f.command().method).toBe("Target.detachFromTarget");
    await f.reply({}); await rejected;
  });

  it("fails promptly when the temporary session is detached while reading the page", async () => {
    const f = fixture(); const probe = f.bridge.probeTarget(f.id, h5.targetId);
    const rejected = expect(probe).rejects.toThrow("H5 临时会话已关闭");
    await f.reply({ targetInfos: [h5] }); await f.reply({ sessionId: "session-h5" });
    f.receive({ method: "Target.detachedFromTarget", params: { sessionId: "session-h5", targetId: h5.targetId } });
    await rejected;
    expect(f.command().method).toBe("Runtime.evaluate");
  });

  it("cleans up a late attach acknowledgement after the locked target changes", async () => {
    vi.useFakeTimers();
    try {
      const f = fixture(); const probe = f.bridge.probeTarget(f.id, h5.targetId);
      const rejected = expect(probe).rejects.toThrow("目标已变化");
      await f.reply({ targetInfos: [h5] });
      const attachId = f.command().id;
      const other = f.bridge.addMiniapp({ send: () => {} }); f.bridge.switchMiniapp(other);
      f.receive({ id: attachId, result: { sessionId: "session-h5" } });
      for (let turn = 0; turn < 6; turn += 1) await Promise.resolve();
      expect(f.command()).toMatchObject({ method: "Target.detachFromTarget", params: { sessionId: "session-h5" } });
      await f.reply({}); await rejected;
    } finally { vi.clearAllTimers(); vi.useRealTimers(); }
  });

  it("retries failed cleanup before creating another session", async () => {
    const f = fixture(); const probe = f.bridge.probeTarget(f.id, h5.targetId);
    const rejected = expect(probe).rejects.toThrow("临时会话释放失败");
    await f.reply({ targetInfos: [h5] }); await f.reply({ sessionId: "session-h5" });
    await f.reply({ result: { value: page } }, "session-h5");
    f.receive({ id: f.command().id, error: { message: "detach failed" } });
    await rejected;
    const retry = f.bridge.probeTarget(f.id, h5.targetId);
    expect(f.command()).toMatchObject({ method: "Target.detachFromTarget", params: { sessionId: "session-h5" } });
    await f.reply({}); expect(f.command().method).toBe("Target.getTargets");
    const noTarget = expect(retry).rejects.toThrow("H5");
    await f.reply({ targetInfos: [] }); await noTarget;
  });
});
