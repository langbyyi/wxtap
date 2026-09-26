import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { createContext, runInContext } from "node:vm";
import { afterEach, describe, expect, it } from "vitest";

import { DrainQueue, UpdateQueue } from "./drain.js";

// The page-side hooks ship as hand-written ES5 loaded verbatim into the
// miniapp, so they cannot be imported: run them in a VM with a fake wx to
// verify the shipped file, not a reimplementation of it.
const hookSource = readFileSync(
  fileURLToPath(new URL("../../hooks/wxapi.js", import.meta.url)),
  "utf8",
);

// The two page-side capacities are read out of hooks/wxapi.js itself. A
// hardcoded mirror stays green after the shipped bound changes, and the shell
// sizes its replay-dedup FIFO against the record bound (hookfeeder.go's
// deliveredCapacity), so a silent drift breaks dedup instead of failing a
// test. Parsing the shipped file keeps this reference honest.
function readCapacity(source: string, name: string): number {
  const match = new RegExp(`${name}\\s*=\\s*(\\d+)`).exec(source);
  if (match === null) {
    throw new Error(`hooks/wxapi.js no longer declares ${name}: the bound kept in lockstep with the shell's dedup FIFO must stay parseable`);
  }
  return Number(match[1]);
}

const PAGE_RECORD_BUFFER_CAPACITY = readCapacity(hookSource, "RECORD_BUFFER_CAPACITY");
const PAGE_UPDATE_BUFFER_CAPACITY = readCapacity(hookSource, "UPDATE_BUFFER_CAPACITY");

type DrainedRecord = { seq: number; record: Record<string, unknown> };
// An update entry mirrors a record entry: only the sequence sits outside, the
// outcome frame is the payload.
type UpdateFrame = {
  rid: string;
  status: string;
  result?: unknown;
  error?: string;
  durationMs: number;
  settledAt: number;
};
type DrainedUpdate = { seq: number; update: UpdateFrame };
type DrainPage = {
  records: DrainedRecord[];
  updates: DrainedUpdate[];
  nextSeq: number;
  nextUpdateSeq: number;
  hasMore: boolean;
  // Cumulative page-side evictions per stream (R12); the shell adds them to its
  // own `dropped` so an overload is visible instead of silent.
  droppedRecords: number;
  droppedUpdates: number;
};

type HookApi = {
  install(): { ok: boolean; hookedCount: number; installed: number; frames: number };
  uninstall(): void;
  drain(afterSeq: number, limit: number, afterUpdateSeq?: number, updateLimit?: number): DrainPage;
  clearHookedCalls(): void;
  replay(apiName: string, options: Record<string, unknown>): void;
};

type TestBridge = {
  invoke(this: void, method: string, params: unknown, callback?: (res: unknown) => void): unknown;
  _wxApiCoreBridgeHooked?: boolean;
};
type RequestOptions = {
  url?: string;
  method?: string;
  success?: (result: unknown) => void;
  fail?: (error: unknown) => void;
  __fail?: boolean;
  __promise?: boolean;
  __defer?: boolean;
  __promiseWithCallback?: boolean;
};

// `this: void` keeps the unbound-method rule quiet where tests compare the
// captured function references.
type FakeWx = {
  getAccountInfoSync(this: void): { miniProgram: { appId: string } };
  request(this: void, options: RequestOptions): unknown;
  getSystemInfoSync(this: void): { platform: string };
};

function createHarness(options: { bridge?: TestBridge } = {}) {
  const calls: RequestOptions[] = [];
  // Resolvers of `__defer` calls, so a test can settle a capture at the moment
  // it chooses instead of synchronously like the other fixtures.
  const deferred: Array<(result: unknown) => void> = [];
  const sandbox: Record<string, unknown> = {};
  const wx: FakeWx = {
    getAccountInfoSync: () => ({ miniProgram: { appId: "wxone" } }),
    request: (options) => {
      calls.push(options);
      if (options.__defer) {
        return new Promise((resolve) => { deferred.push(resolve); });
      }
      if (options.__fail) {
        options.fail?.({ errMsg: "request:fail timeout" });
        return undefined;
      }
      if (options.__promise) {
        return Promise.resolve({ statusCode: 200, data: "promised" });
      }
      if (options.__promiseWithCallback) {
        options.success?.({ statusCode: 200, data: "both" });
        return Promise.resolve({ statusCode: 200, data: "both" });
      }
      options.success?.({ statusCode: 200, data: "ok" });
      return undefined;
    },
    getSystemInfoSync: () => ({ platform: "windows" }),
  };
  // window.wx must resolve, so the sandbox is its own window object.
  sandbox.window = sandbox;
  sandbox.wx = wx;
  sandbox.__wxConfig = { accountInfo: { appId: "wxone" } };
  sandbox.setInterval = setInterval;
  sandbox.clearInterval = clearInterval;
  sandbox.setTimeout = setTimeout;
  if (options.bridge) sandbox.WeixinJSBridge = options.bridge;
  sandbox.clearTimeout = clearTimeout;
  createContext(sandbox);
  runInContext(hookSource, sandbox);

  const audit = sandbox.wxApiAudit as HookApi;
  return { audit, calls, wx, sandbox, deferred };
}

const harnesses: Array<{ audit: HookApi }> = [];

function harness(options: { bridge?: TestBridge } = {}) {
  const created = createHarness({ bridge: options.bridge });
  harnesses.push(created);
  return created;
}

afterEach(() => {
  for (const created of harnesses.splice(0)) {
    created.audit.uninstall();
  }
});

describe("hooks/wxapi.js drain protocol", () => {
  it("hooks wx.request and drains a settled record", () => {
    const { audit, wx } = harness();

    const status = audit.install();
    expect(status.ok).toBe(true);
    expect(status.hookedCount).toBeGreaterThan(0);

    wx.request({ url: "https://api.test/x", method: "GET", success: () => undefined });

    const page = audit.drain(0, 10);
    expect(page.records).toHaveLength(1);
    expect(page.records[0].record).toMatchObject({
      type: "wx.request",
      name: "GET https://api.test/x",
      status: "success",
      appId: "wxone",
    });
    expect(page.nextSeq).toBe(1);
    expect(page.hasMore).toBe(false);
  });

  it("settles failing and promised async calls, and records sync calls", async () => {
    const { audit, wx } = harness();
    audit.install();

    wx.request({ url: "https://api.test/fail", method: "POST", fail: () => undefined, __fail: true });
    wx.request({ url: "https://api.test/promise", method: "GET", __promise: true });
    wx.getSystemInfoSync();

    // The promise settles on a microtask, like the real API.
    await new Promise((resolve) => setTimeout(resolve, 0));

    const page = audit.drain(0, 10);
    const byName = new Map(page.records.map((entry) => [String(entry.record.name), entry.record]));
    expect(byName.get("POST https://api.test/fail")).toMatchObject({
      status: "fail",
      error: "request:fail timeout",
    });
    expect(byName.get("GET https://api.test/promise")).toMatchObject({
      status: "success",
      result: { statusCode: 200, data: "promised" },
    });
    expect(byName.get("getSystemInfoSync")).toMatchObject({
      status: "success",
      result: { platform: "windows" },
    });
  });

  it("captures WeixinJSBridge calls and skips the cloud-owned methods", () => {
    const invoked: string[] = [];
    const bridge = {
      invoke: (method: string, _params: unknown, callback?: (res: unknown) => void) => {
        invoked.push(method);
        callback?.({ ok: true });
        return { ok: true };
      },
    };
    const { audit } = harness({ bridge });
    audit.install();

    bridge.invoke("getSystemInfo", { a: 1 }, () => undefined);
    bridge.invoke("operateCloudFunction", {}, () => undefined);

    const page = audit.drain(0, 10);
    expect(page.records.map((entry) => entry.record.name)).toEqual(["getSystemInfo"]);
    expect(page.records[0].record).toMatchObject({
      type: "wx.bridge",
      status: "success",
      appId: "wxone",
      result: { ok: true },
    });
    // The cloud hook owns those two methods; the wxapi hook must not double-record them.
    expect(invoked).toEqual(["getSystemInfo", "operateCloudFunction"]);
  });

  it("stops bridge capture when the hook is uninstalled", () => {
    const bridge: TestBridge = {
      invoke: (_method, _params, callback) => {
        callback?.({ ok: true });
        return undefined;
      },
    };
    const { audit } = harness({ bridge });
    audit.install();
    expect(bridge._wxApiCoreBridgeHooked).toBe(true);

    audit.uninstall();
    expect(bridge._wxApiCoreBridgeHooked).toBeUndefined();

    // The restored original no longer records.
    bridge.invoke("getSystemInfo", {}, () => undefined);
    expect(audit.drain(0, 10).records).toEqual([]);
  });

  it("records the replay outcome for the shell to poll", () => {
    const { audit, sandbox } = harness();
    audit.install();

    audit.replay("request", { url: "https://api.test/replay", method: "GET" });
    expect(JSON.parse(String(sandbox._wxApiReplayDone))).toEqual({
      ok: true,
      status: "success",
      result: { statusCode: 200, data: "ok" },
    });

    audit.replay("request", { url: "https://api.test/replay", method: "GET", __fail: true });
    const failed = JSON.parse(String(sandbox._wxApiReplayDone)) as { ok: boolean; status: string; error?: string };
    expect(failed.ok).toBe(true);
    expect(failed.status).toBe("fail");
    expect(failed.error).toContain("request:fail");

    audit.replay("notAnApi", {});
    expect(JSON.parse(String(sandbox._wxApiReplayDone))).toMatchObject({ ok: false });
  });

  it("restores the original wx APIs on uninstall", () => {
    const { audit, wx } = harness();
    const original = wx.request;

    audit.install();
    expect(wx.request).not.toBe(original);

    audit.uninstall();
    expect(wx.request).toBe(original);
  });

  it("never resends acknowledged records and reports hasMore with a limit", () => {
    const { audit, wx } = harness();
    audit.install();

    for (const path of ["a", "b", "c"]) {
      wx.request({ url: `https://api.test/${path}`, method: "GET", success: () => undefined });
    }

    const first = audit.drain(0, 2);
    expect(first.records.map((entry) => entry.seq)).toEqual([1, 2]);
    expect(first.hasMore).toBe(true);
    expect(first.nextSeq).toBe(2);

    const second = audit.drain(first.nextSeq, 2);
    expect(second.records.map((entry) => entry.seq)).toEqual([3]);
    expect(second.hasMore).toBe(false);

    // Acknowledged up to 3: nothing is resent.
    expect(audit.drain(second.nextSeq, 2).records).toEqual([]);

    wx.request({ url: "https://api.test/d", method: "GET", success: () => undefined });
    const third = audit.drain(second.nextSeq, 2);
    expect(third.records.map((entry) => entry.seq)).toEqual([4]);
  });

  it("keeps the buffer bounded exactly like the DrainQueue model", () => {
    const { audit, wx } = harness();
    audit.install();

    const capacity = PAGE_RECORD_BUFFER_CAPACITY;
    const model = new DrainQueue<Record<string, unknown>>(capacity);
    for (let index = 0; index < capacity + 5; index += 1) {
      wx.request({ url: `https://api.test/${index}`, method: "GET", success: () => undefined });
      model.push({ index });
    }

    const page = audit.drain(0, capacity * 2);
    const modelPage = model.drain(0, capacity * 2);

    expect(page.records.map((entry) => entry.seq)).toEqual(modelPage.records.map((entry) => entry.seq));
    expect(page.nextSeq).toBe(modelPage.nextSeq);
    expect(page.hasMore).toBe(modelPage.hasMore);
    // Oldest records are evicted, sequences keep counting.
    expect(page.records).toHaveLength(capacity);
    expect(page.records[0].seq).toBe(6);
    expect(page.records[capacity - 1].seq).toBe(capacity + 5);
  });

  it("clears the buffer without rewinding the sequence", () => {
    const { audit, wx } = harness();
    audit.install();

    wx.request({ url: "https://api.test/a", method: "GET", success: () => undefined });
    wx.request({ url: "https://api.test/b", method: "GET", success: () => undefined });
    const first = audit.drain(0, 10);
    expect(first.nextSeq).toBe(2);

    wx.request({ url: "https://api.test/c", method: "GET", success: () => undefined });
    audit.clearHookedCalls();
    expect(audit.drain(first.nextSeq, 10).records).toEqual([]);

    // New records continue the sequence, so an acked consumer still sees them.
    wx.request({ url: "https://api.test/d", method: "GET", success: () => undefined });
    const next = audit.drain(first.nextSeq, 10);
    expect(next.records.map((entry) => entry.record.name)).toEqual(["GET https://api.test/d"]);
    expect(next.records.map((entry) => entry.seq)).toEqual([4]);
  });
  it("is install-idempotent across repeated auto-scans", () => {
    const { audit, wx } = harness();
    audit.install();
    const hooked = wx.request;

    audit.install();

    expect(wx.request).toBe(hooked);
    expect(audit.drain(0, 10).records).toEqual([]);
  });

  it("reports a cumulative install status, not a per-call one", () => {
    const { audit, wx } = harness();

    const first = audit.install();
    expect(first.ok).toBe(true);
    expect(first.hookedCount).toBeGreaterThan(0);
    expect(first.installed).toBe(1);

    // Second install in the same realm: the frame is already marked, so
    // nothing is newly hooked (hookedCount 0) - but the capture is live and
    // the shell turns ok:false into a failed "start capture", so ok must stay
    // true and `installed` must report the frame hooked right now.
    const second = audit.install();
    expect(second.ok).toBe(true);
    expect(second.hookedCount).toBe(0);
    expect(second.installed).toBe(1);
    expect(second.frames).toBe(1);

    wx.request({ url: "https://api.test/after-reinstall", method: "GET", success: () => undefined });
    expect(audit.drain(0, 10).records).toHaveLength(1);

    // uninstall() clears hookedFrames, so a later install hooks from scratch
    // again: the cumulative status is not a latch.
    audit.uninstall();
    const third = audit.install();
    expect(third.ok).toBe(true);
    expect(third.hookedCount).toBeGreaterThan(0);
    expect(third.installed).toBe(1);
  });

  it("fails the install when no frame carries wx", () => {
    const { audit, sandbox } = harness();
    // Nothing to hook at all: the page must say so, otherwise the shell's
    // "start capture" would silently report success with no records ever.
    sandbox.wx = undefined;

    expect(audit.install()).toMatchObject({ ok: false, reason: "no frames with wx found" });
  });
});
  it("reads the nested appAccount identity", () => {
    const created = harness();
    created.sandbox.__wxConfig = { accountInfo: { appAccount: { appId: "wxnested" } } };

    expect(created.audit.install()).toMatchObject({ ok: true });
    created.wx.request({ url: "https://api.test/nested", success: () => undefined });

    const page = created.audit.drain(0, 10);
    expect(page.records[0].record).toMatchObject({ appId: "wxnested" });
  });

  it("falls back to account info API", () => {
    const created = harness();
    created.sandbox.__wxConfig = {};

    expect(created.audit.install()).toMatchObject({ ok: true });
    created.wx.request({ url: "https://api.test/fallback", success: () => undefined });

    const page = created.audit.drain(0, 10);
    expect(page.records[0].record).toMatchObject({ appId: "wxone" });
  });

describe("hooks/wxapi.js record identity", () => {
  it("derives rid exactly like traffic_records.id", () => {
    const { audit, wx } = harness();
    audit.install();

    wx.request({ url: "https://api.test/a", method: "GET", success: () => undefined });
    wx.request({ url: "https://api.test/b", method: "POST", success: () => undefined });

    const page = audit.drain(0, 10);
    const first = page.records[0];
    const second = page.records[1];
    expect(first.record.rid).toBe(`wx.request-wxone-${String(first.record.ts)}-${first.seq}`);
    expect(second.record.rid).toBe(`wx.request-wxone-${String(second.record.ts)}-${second.seq}`);
    expect(first.record.rid).not.toBe(second.record.rid);
    // The identity is the same on both delivery paths, which is what makes the
    // event stream and the poll idempotent, and it survives settling.
    expect(page.updates.map((entry) => entry.update.rid)).toEqual([
      first.record.rid,
      second.record.rid,
    ]);
  });
});

describe("hooks/wxapi.js update stream", () => {
  it("appends one update frame per settled call", () => {
    const { audit, wx } = harness();
    audit.install();

    wx.request({ url: "https://api.test/fail", method: "POST", fail: () => undefined, __fail: true });

    const page = audit.drain(0, 10);
    const record = page.records[0].record;
    expect(page.updates.map((entry) => entry.seq)).toEqual([1]);
    // Entry shape mirrors the record stream: the sequence outside, the frame inside.
    expect(Object.keys(page.updates[0]).sort()).toEqual(["seq", "update"]);
    const frame = page.updates[0].update;
    expect(frame).toMatchObject({
      rid: record.rid,
      status: "fail",
      error: "request:fail timeout",
    });
    expect(frame.durationMs).toBeGreaterThanOrEqual(0);
    expect(Number.isInteger(frame.durationMs)).toBe(true);
    expect(frame.settledAt).toBeGreaterThanOrEqual(Number(record.ts));
    // result and error are mutually exclusive.
    expect("result" in frame).toBe(false);
  });

  it("delivers the outcome of a call that settles after it was drained", async () => {
    const { audit, wx } = harness();
    audit.install();

    wx.request({ url: "https://api.test/slow", method: "GET", __promise: true });

    const first = audit.drain(0, 10);
    expect(first.records[0].record).toMatchObject({ status: "pending" });
    expect(first.updates).toEqual([]);

    // The promise lands on a microtask, after the record was already acked.
    await new Promise((resolve) => setTimeout(resolve, 0));

    const second = audit.drain(first.nextSeq, 10, first.nextUpdateSeq, 200);
    expect(second.records).toEqual([]);
    expect(second.updates.map((entry) => entry.seq)).toEqual([1]);
    expect(second.updates[0].update).toMatchObject({
      rid: first.records[0].record.rid,
      status: "success",
      result: { statusCode: 200, data: "promised" },
    });
    expect(second.nextUpdateSeq).toBe(1);
  });

  it("settles a record once even when both the callback and the promise fire", async () => {
    const { audit, wx } = harness();
    audit.install();

    wx.request({ url: "https://api.test/both", method: "GET", success: () => undefined, __promiseWithCallback: true });
    await new Promise((resolve) => setTimeout(resolve, 0));

    const page = audit.drain(0, 10);
    expect(page.records).toHaveLength(1);
    expect(page.updates).toHaveLength(1);
  });

  it("emits no update frame for a pending record that was evicted", async () => {
    const capacity = PAGE_RECORD_BUFFER_CAPACITY;
    const { audit, wx, deferred } = harness();
    audit.install();

    for (let index = 0; index < capacity + 1; index += 1) {
      wx.request({ url: `https://api.test/slow/${index}`, method: "GET", __defer: true });
    }

    const page = audit.drain(0, capacity * 2);
    expect(page.records).toHaveLength(capacity);
    expect(page.records[0].seq).toBe(2);

    // The first call's record fell out of the buffer: settling it is a no-op.
    deferred[0]({ statusCode: 200 });
    deferred[capacity]({ statusCode: 200 });
    await new Promise((resolve) => setTimeout(resolve, 0));

    const settled = audit.drain(page.nextSeq, 10, 0, 200);
    expect(settled.updates.map((entry) => entry.update.rid)).toEqual([
      page.records[capacity - 1].record.rid,
    ]);
  });

  it("keeps the update buffer bounded exactly like the UpdateQueue model", () => {
    const { audit, wx } = harness();
    audit.install();

    const capacity = PAGE_UPDATE_BUFFER_CAPACITY;
    const model = new UpdateQueue<{ rid: string }>(capacity);
    for (let index = 0; index < capacity + 5; index += 1) {
      wx.request({ url: `https://api.test/${index}`, method: "GET", success: () => undefined });
      model.pushUpdate({ rid: `rid-${index}` });
    }

    const page = audit.drain(0, capacity * 2, 0, capacity * 2);
    const modelPage = model.drainUpdates(0, capacity * 2);

    expect(page.updates.map((entry) => entry.seq)).toEqual(
      modelPage.updates.map((entry) => entry.seq),
    );
    // Entries are shaped like the model's: the sequence outside, the frame inside.
    expect(Object.keys(page.updates[0])).toEqual(Object.keys(modelPage.updates[0]));
    expect(page.nextUpdateSeq).toBe(modelPage.nextUpdateSeq);
    // Oldest updates are evicted, sequences keep counting.
    expect(page.updates).toHaveLength(capacity);
    expect(page.updates[0].seq).toBe(6);
    expect(page.updates[capacity - 1].seq).toBe(capacity + 5);
  });

  it("defaults the update cursor and limit, and never resends an acked update", () => {
    const { audit, wx } = harness();
    audit.install();

    for (const path of ["a", "b", "c"]) {
      wx.request({ url: `https://api.test/${path}`, method: "GET", success: () => undefined });
    }

    // Omitted update parameters read as 0 / 200.
    const all = audit.drain(0, 10);
    expect(all.updates.map((entry) => entry.seq)).toEqual([1, 2, 3]);
    expect(all.nextUpdateSeq).toBe(3);

    const acked = audit.drain(all.nextSeq, 10, all.nextUpdateSeq, 200);
    expect(acked.updates).toEqual([]);
    expect(acked.nextUpdateSeq).toBe(3);

    // A limit page hands back the cursor of what it actually delivered.
    const limited = audit.drain(all.nextSeq, 10, 0, 2);
    expect(limited.updates.map((entry) => entry.seq)).toEqual([1, 2]);
    expect(limited.nextUpdateSeq).toBe(2);
    const rest = audit.drain(all.nextSeq, 10, limited.nextUpdateSeq, 2);
    expect(rest.updates.map((entry) => entry.seq)).toEqual([3]);

    // hasMore still describes the record stream alone.
    expect(limited.hasMore).toBe(false);
  });

  it("treats updateLimit 0 as records-only without advancing the update cursor", () => {
    const { audit, wx } = harness();
    audit.install();

    wx.request({ url: "https://api.test/a", method: "GET", success: () => undefined });

    const recordsOnly = audit.drain(0, 10, 0, 0);
    expect(recordsOnly.records).toHaveLength(1);
    expect(recordsOnly.updates).toEqual([]);
    expect(recordsOnly.nextUpdateSeq).toBe(0);

    // The update is still there for a caller that does want it.
    const withUpdates = audit.drain(recordsOnly.nextSeq, 10, recordsOnly.nextUpdateSeq, 200);
    expect(withUpdates.updates.map((entry) => entry.seq)).toEqual([1]);

    // 0 is a cursor no-op, not an ack: it never skips an update.
    const again = audit.drain(0, 10, 0, 0);
    expect(again.updates).toEqual([]);
    expect(again.nextUpdateSeq).toBe(0);
  });

  it("clears both buffers without rewinding the update sequence", () => {
    const { audit, wx } = harness();
    audit.install();

    wx.request({ url: "https://api.test/a", method: "GET", success: () => undefined });
    expect(audit.drain(0, 10).nextUpdateSeq).toBe(1);

    audit.clearHookedCalls();
    const cleared = audit.drain(0, 10, 0, 200);
    expect(cleared.records).toEqual([]);
    expect(cleared.updates).toEqual([]);
    expect(cleared.nextUpdateSeq).toBe(0);

    // New captures keep counting records and updates alike.
    wx.request({ url: "https://api.test/b", method: "GET", success: () => undefined });
    const next = audit.drain(0, 10, 0, 200);
    expect(next.records.map((entry) => entry.seq)).toEqual([2]);
    expect(next.updates.map((entry) => entry.seq)).toEqual([2]);
  });
});
// 这两条原先落在所有 describe 之外，按套件名筛选时会静默跳过它们。
describe("hooks/wxapi.js account identity", () => {

  describe("hooks/wxapi.js drop accounting", () => {
    it("reports zero dropped counts while nothing is evicted", () => {
      const { audit, wx } = harness();
      audit.install();

      wx.request({ url: "https://api.test/a", method: "GET", success: () => undefined });

      const page = audit.drain(0, 10);
      expect(page.records).toHaveLength(1);
      expect(page.droppedRecords).toBe(0);
      expect(page.droppedUpdates).toBe(0);
    });

    it("counts every evicted record and update exactly like the model queues", () => {
      const { audit, wx } = harness();
      audit.install();

      const recordModel = new DrainQueue<{ url: string }>(PAGE_RECORD_BUFFER_CAPACITY);
      const updateModel = new UpdateQueue<{ rid: string }>(PAGE_UPDATE_BUFFER_CAPACITY);
      const overflow = 7;
      for (let index = 0; index < PAGE_RECORD_BUFFER_CAPACITY + overflow; index += 1) {
        wx.request({ url: `https://api.test/${index}`, method: "GET", success: () => undefined });
        recordModel.push({ url: `https://api.test/${index}` });
        updateModel.pushUpdate({ rid: `rid-${index}` });
      }

      const page = audit.drain(0, PAGE_RECORD_BUFFER_CAPACITY * 2, 0, PAGE_UPDATE_BUFFER_CAPACITY * 2);
      expect(page.droppedRecords).toBe(overflow);
      expect(page.droppedUpdates).toBe(overflow);
      // The counts are the model's, not a second hardcoded expectation.
      expect(page.droppedRecords).toBe(recordModel.dropped);
      expect(page.droppedUpdates).toBe(updateModel.dropped);
    });

    it("keeps counting across drains and restarts with clearHookedCalls", () => {
      const { audit, wx } = harness();
      audit.install();

      for (let index = 0; index < PAGE_RECORD_BUFFER_CAPACITY + 1; index += 1) {
        wx.request({ url: `https://api.test/${index}`, method: "GET", success: () => undefined });
      }

      // A drain reports the cumulative count without consuming or clearing it.
      const first = audit.drain(0, 10);
      expect(first.droppedRecords).toBe(1);
      expect(first.droppedUpdates).toBe(1);
      const second = audit.drain(first.nextSeq, 10, first.nextUpdateSeq, 200);
      expect(second.droppedRecords).toBe(1);
      expect(second.droppedUpdates).toBe(1);

      // Clearing the buffers clears what was dropped out of them.
      audit.clearHookedCalls();
      const cleared = audit.drain(0, 10, 0, 200);
      expect(cleared.droppedRecords).toBe(0);
      expect(cleared.droppedUpdates).toBe(0);
    });

    it("keeps the drain contract intact while dropping the oldest records", () => {
      const { audit, wx } = harness();
      audit.install();

      const overflow = 3;
      for (let index = 0; index < PAGE_RECORD_BUFFER_CAPACITY + overflow; index += 1) {
        wx.request({ url: `https://api.test/${index}`, method: "GET", success: () => undefined });
      }

      // Eviction is not an ack: the buffer still starts at seq 4 and the cursors
      // keep describing the newest entry.
      const first = audit.drain(0, 2, 0, 0);
      expect(first.records.map((entry) => entry.seq)).toEqual([overflow + 1, overflow + 2]);
      expect(first.nextSeq).toBe(overflow + 2);
      expect(first.hasMore).toBe(true);
      // updateLimit 0 still reads records only and leaves the update cursor put.
      expect(first.updates).toEqual([]);
      expect(first.nextUpdateSeq).toBe(0);
      expect(first.droppedRecords).toBe(overflow);
      expect(first.droppedUpdates).toBe(overflow);

      // Acking resumes right after the acknowledged sequence.
      const second = audit.drain(first.nextSeq, 2, first.nextUpdateSeq, 2);
      expect(second.records.map((entry) => entry.seq)).toEqual([overflow + 3, overflow + 4]);
      expect(second.updates.map((entry) => entry.seq)).toEqual([overflow + 1, overflow + 2]);
    });
  });
});
describe("hooks/wxapi.js call timing", () => {
  it("stamps bridge records with the invoke time and delivers a real duration", async () => {
    let answer: ((res: unknown) => void) | undefined;
    const bridge = {
      invoke: (_method: string, _params: unknown, callback?: (res: unknown) => void) => {
        answer = callback;
        return undefined;
      },
    };
    const { audit } = harness({ bridge });
    audit.install();

    const injectedAt = Date.now();
    bridge.invoke("getSystemInfo", { a: 1 }, () => undefined);
    await new Promise((resolve) => setTimeout(resolve, 25));
    answer?.({ ok: true });

    const page = audit.drain(0, 10, 0, 10);
    expect(page.records).toHaveLength(1);
    const record = page.records[0].record as { ts: number; type: string };
    expect(record.type).toBe("wx.bridge");
    // ts is the invoke moment: the record is only created once the bridge
    // answers, so stamping it there would report every bridge call as instant
    // and drift its capturedAt (and rid) late.
    expect(Math.abs(record.ts - injectedAt)).toBeLessThan(15);
    const frame = page.updates[0].update;
    expect(frame.status).toBe("success");
    expect(frame.durationMs).toBeGreaterThanOrEqual(20);
  });

  it("delivers a settle frame for records created already settled", () => {
    const { audit, wx } = harness();
    audit.install();

    wx.getSystemInfoSync();

    const page = audit.drain(0, 10, 0, 10);
    expect(page.records).toHaveLength(1);
    // A sync call never reaches settleBySeq, so without its own frame the
    // outcome and duration would never reach storage or the panel.
    expect(page.updates.map((entry) => entry.update.status)).toEqual(["success"]);
    expect(page.updates[0].update.rid).toBe(page.records[0].record.rid);
  });
});
