import { describe, expect, it } from "vitest";
import { CdpBridge } from "./cdp-bridge.js";
import { decodeCdpMessage, encodeCdpMessage } from "../protocol/wmpf-codec.js";

function fixture() {
  const bridge = new CdpBridge();
  const sent: Buffer[] = [];
  const id = bridge.addMiniapp({ send: (message) => sent.push(message as Buffer) });
  const command = () => JSON.parse(decodeCdpMessage(sent[sent.length - 1])?.payload ?? "{}");
  const receive = (payload: Record<string, unknown>, source = id) => bridge.receiveMiniapp(encodeCdpMessage({
    sequence: 1, category: "chromeDevtoolsResult", operationId: 0, payload: JSON.stringify(payload), jsContextId: "",
  }), source);
  return { bridge, sent, id, command, receive };
}

describe("target discovery snapshot", () => {
  it("pins enumeration to one source and keeps replies out of external DevTools", async () => {
    const f = fixture(); const other: unknown[] = []; const external: unknown[] = [];
    const otherId = f.bridge.addMiniapp({ send: (message) => other.push(message) });
    f.bridge.addDevtools({ send: (message) => external.push(message) });
    const pending = f.bridge.listTargets();
    expect(f.command().method).toBe("Target.getTargets");
    expect(other).toHaveLength(0);
    f.receive({ id: f.command().id, result: { targetInfos: [{ targetId: "wrong" }] } }, otherId);
    f.receive({ id: f.command().id, result: { targetInfos: [{ targetId: "h5" }] } });
    await expect(pending).resolves.toEqual({ clientId: f.id, locked: true, targets: [{ targetId: "h5" }] });
    expect(external).toHaveLength(0);
  });

  it("enumerates only the newest connection when the lock is off", async () => {
    const f = fixture(); const newest: Buffer[] = [];
    const id = f.bridge.addMiniapp({ send: (message) => newest.push(message as Buffer) });
    f.bridge.setLock(false);
    const pending = f.bridge.listTargets();
    expect(f.sent).toHaveLength(0);
    const command = JSON.parse(decodeCdpMessage(newest[0])?.payload ?? "{}");
    f.receive({ id: command.id, result: { targetInfos: [] } }, id);
    await expect(pending).resolves.toEqual({ clientId: id, locked: false, targets: [] });
  });

  it.each(["switch", "unlock", "reload"])("rejects stale targets after %s", async (change) => {
    const f = fixture();
    const pending = f.bridge.listTargets();
    const commandId = f.command().id;
    const rejected = expect(pending).rejects.toThrow("目标已变化");
    if (change === "switch") f.bridge.switchMiniapp(f.bridge.addMiniapp({ send: () => {} }));
    if (change === "unlock") f.bridge.setLock(false);
    if (change === "reload") f.bridge.receiveMiniapp(encodeCdpMessage({ sequence: 1, category: "setupContext", operationId: 0, payload: "{}", jsContextId: "" }), f.id);
    f.receive({ id: commandId, result: { targetInfos: [{ targetId: "old" }] } });
    await rejected;
  });

  it("rejects a disconnected source immediately", async () => {
    const f = fixture(); const pending = f.bridge.listTargets();
    const rejected = expect(pending).rejects.toThrow("调试目标连接已断开");
    f.bridge.removeMiniapp(f.id);
    await rejected;
  });

  it.each([{ error: { message: "Target.getTargets unsupported" } }, { result: {} }, { result: { targetInfos: null } }])("reports unsupported and invalid replies: %j", async (response) => {
    const f = fixture(); const pending = f.bridge.listTargets();
    f.receive({ id: f.command().id, ...response });
    await expect(pending).rejects.toThrow(response.error ? "unsupported" : "未返回有效目标清单");
  });
});
