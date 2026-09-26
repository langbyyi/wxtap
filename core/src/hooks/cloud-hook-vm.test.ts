import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { createContext, runInContext } from "node:vm";
import { afterEach, describe, expect, it } from "vitest";

import { DrainQueue, UpdateQueue } from "./drain.js";

// hooks/cloud.js ships verbatim into the miniapp, so verify the shipped file
// inside a VM with a fake wx.cloud instead of reimplementing its logic.
const hookSource = readFileSync(
  fileURLToPath(new URL("../../hooks/cloud.js", import.meta.url)),
  "utf8",
);

// The two page-side capacities are read out of hooks/cloud.js itself. A
// hardcoded mirror stays green after the shipped bound changes, and the shell
// sizes its replay-dedup FIFO against the record bound (hookfeeder.go's
// deliveredCapacity), so a silent drift breaks dedup instead of failing a test.
function readCapacity(source: string, name: string): number {
  const match = new RegExp(`${name}\\s*=\\s*(\\d+)`).exec(source);
  if (match === null) {
    throw new Error(
      `hooks/cloud.js no longer declares ${name}: the bound kept in lockstep with the shell's dedup FIFO must stay parseable`,
    );
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

type TestBridge = {
  invoke(this: void, method: string, params: unknown, callback?: (res: unknown) => void): unknown;
  _cloudAuditHooked?: boolean;
};
type CloudAudit = {
  install(): { ok: boolean; totalHooked: number; directHooked: boolean };
  uninstallHook(): void;
  stopAutoHook(): void;
  drain(afterSeq: number, limit: number, afterUpdateSeq?: number, updateLimit?: number): DrainPage;
  scanCloudFunctions(): unknown[];
  callFunction(name: string, data: unknown): Promise<Record<string, unknown>>;
  getDiscoveredFunctions(): Array<Record<string, unknown>>;
  clearHookedCalls(): void;
};

type CallOptions = {
  name?: string;
  data?: unknown;
  success?: (result: unknown) => void;
  fail?: (error: unknown) => void;
  __fail?: boolean;
  __defer?: boolean;
  // Answer through the success callback only when the test releases it, and
  // resolve a promise only when the test releases it: both make the wrapper
  // write its record well after the call, which is when the call's timestamp
  // and duration have to be right.
  __deferCallback?: boolean;
  __latePromise?: boolean;
  // A promise that lands only when the test rejects it: the wrapper learns the
  // failure from the promise's catch path, long after the call moment.
  __lateReject?: boolean;
};

// A database terminal operation settles through one of four sites, and the
// wrapper passes `calledAt` at every one of them, so the harness has to be able
// to drive all four. Only the promise modes may be given a callback: the
// wrapper's promise branch is skipped as soon as one is present.
type TerminalOptions = {
  data?: unknown;
  success?: (res: unknown) => void;
  fail?: (error: unknown) => void;
  __terminal?: "callbackSuccess" | "callbackFail" | "promiseSuccess" | "promiseReject";
};

// `this: void` keeps the unbound-method rule quiet where tests compare the
// captured function references.
type FakeTerminal = {
  get(this: void, options?: TerminalOptions): unknown;
};

// The platform fails with a plain {errMsg} object rather than an Error. Carrying
// that shape on a real Error keeps the rejections lint-clean without changing
// what the wrapper reads out of them.
function platformError(errMsg: string): Error {
  return Object.assign(new Error(errMsg), { errMsg });
}

type FakeCollection = FakeTerminal;

type FakeDb = {
  collection(this: void, name: string): FakeCollection;
};

type FakeCloud = {
  callFunction(this: void, options: CallOptions): unknown;
  database(this: void): FakeDb;
};

function createHarness(options: { bridge?: TestBridge; directHook?: boolean; appId?: string } = {}) {
  const calls: CallOptions[] = [];
  const collectionNames: string[] = [];
  // Answering a deferred call: promise resolvers (`__defer` / `__latePromise`),
  // promise rejecters (`__lateReject` / the terminal reject mode) and callback
  // settlers (`__deferCallback` / the terminal callback modes) the test fires
  // when it chooses.
  const deferred: Array<(result?: unknown) => void> = [];
  const rejections: Array<(error: Error) => void> = [];
  const pendingSettle: Array<() => void> = [];
  const sandbox: Record<string, unknown> = {};
  const db: FakeDb = {
    collection: (name) => {
      collectionNames.push(name);
      return {
        get: (options) => {
          switch (options?.__terminal) {
            // Callback modes: the test releases the outcome long after the call,
            // which is the only way a missing call-moment capture is visible.
            case "callbackSuccess":
              pendingSettle.push(() => {
                options?.success?.({ data: [{ _id: "late" }] });
              });
              return undefined;
            case "callbackFail":
              pendingSettle.push(() => {
                options?.fail?.({ errMsg: "db.get:fail" });
              });
              return undefined;
            // Promise modes: no callback is passed, so the wrapper has to learn
            // the outcome from the returned promise, long after the call.
            case "promiseSuccess":
              return new Promise((resolve) => {
                deferred.push(resolve);
              });
            case "promiseReject":
              return new Promise((_resolve, reject) => {
                rejections.push((error) => reject(error));
              });
            default:
              options?.success?.({ data: [] });
              return undefined;
          }
        },
      };
    },
  };
  const cloud: FakeCloud = {
    callFunction: (options) => {
      calls.push(options);
      if (options.__deferCallback) {
        pendingSettle.push(() => {
          options.success?.({ result: { late: true } });
        });
        return undefined;
      }
      if (options.__latePromise) {
        // No callback fires at all: the wrapper learns the outcome from the
        // promise, long after the record's call moment.
        return new Promise((resolve) => {
          deferred.push(resolve);
        });
      }
      if (options.__lateReject) {
        // Same, on the failure side: the wrapper only learns of it from the
        // promise's catch path.
        return new Promise((_resolve, reject) => {
          rejections.push((error) => reject(error));
        });
      }
      if (options.__defer) {
        // A call still in flight: it may settle through the callback right away
        // and through the promise later, exactly like wx.cloud does.
        options.success?.({ result: { ok: true } });
        return new Promise((resolve) => {
          deferred.push(() => resolve({ result: { deferred: true } }));
        });
      }
      if (options.__fail || options.name === "boom") {
        options.fail?.({ errMsg: "cloud.callFunction:fail" });
        return undefined;
      }
      options.success?.({ result: { ok: true }, requestID: "rid-1" });
      return undefined;
    },
    database: () => db,
  };
  if (options.directHook) {
    // The frame scan wraps every enumerable method of the cloud instance, and
    // it runs before the direct wx.cloud hook. Hiding callFunction from the
    // enumeration leaves only the direct hook to wrap it - the one path that
    // records a call while it is pending and settles it from the callback.
    Object.defineProperty(cloud, "callFunction", {
      value: cloud.callFunction,
      enumerable: false,
      writable: true,
      configurable: true,
    });
  }
  const appId = options.appId ?? "wxone";
  const wx = {
    cloud,
    getAccountInfoSync: () => ({ miniProgram: { appId } }),
  };
  sandbox.window = sandbox;
  sandbox.wx = wx;
  // appId "" models a page whose identity is unavailable: the record keeps the
  // empty segment instead of a stand-in.
  sandbox.__wxConfig = { accountInfo: { appId } };
  sandbox.setInterval = setInterval;
  sandbox.clearInterval = clearInterval;
  sandbox.setTimeout = setTimeout;
  if (options.bridge) sandbox.WeixinJSBridge = options.bridge;
  sandbox.clearTimeout = clearTimeout;
  createContext(sandbox);
  runInContext(hookSource, sandbox);

  return { audit: sandbox.cloudAudit as CloudAudit, calls, cloud, sandbox, db, deferred, rejections, pendingSettle };
}

const harnesses: Array<{ audit: CloudAudit }> = [];

function harness(options: { bridge?: TestBridge; directHook?: boolean; appId?: string } = {}) {
  const created = createHarness(options);
  harnesses.push(created);
  return created;
}

afterEach(() => {
  for (const created of harnesses.splice(0)) {
    created.audit.stopAutoHook();
    created.audit.uninstallHook();
  }
});

describe("hooks/cloud.js capture pipeline", () => {
  it("hooks wx.cloud.callFunction and drains one settled record per call", () => {
    const { audit, cloud } = harness();

    expect(audit.install()).toMatchObject({ ok: true, directHooked: true });

    cloud.callFunction({ name: "hello", data: { a: 1 }, success: () => undefined });

    const page = audit.drain(0, 10);
    expect(page.records).toHaveLength(1);
    expect(page.records[0].record).toMatchObject({
      type: "function",
      name: "hello",
      appId: "wxone",
      status: "success",
    });
    expect(page.records[0].record.result).toMatchObject({ requestID: "rid-1" });
    // Callbacks are stripped from the captured payload.
    expect(page.records[0].record.data).toEqual({ name: "hello", data: { a: 1 } });
  });

  it("settles calls that pass no callbacks", () => {
    const { audit, cloud } = harness();
    audit.install();

    cloud.callFunction({ name: "noCallbacks", data: {} });

    const page = audit.drain(0, 10);
    expect(page.records).toHaveLength(1);
    expect(page.records[0].record).toMatchObject({ name: "noCallbacks", status: "success" });
  });

  it("records failures with the platform error message", () => {
    const { audit, cloud } = harness();
    audit.install();

    cloud.callFunction({ name: "boom", fail: () => undefined, __fail: true });

    const page = audit.drain(0, 10);
    expect(page.records[0].record).toMatchObject({ name: "boom", status: "fail" });
    expect(page.records[0].record.error).toBe("cloud.callFunction:fail");
  });

  it("honours the drain ack contract", () => {
    const { audit, cloud } = harness();
    audit.install();

    for (const name of ["a", "b", "c"]) {
      cloud.callFunction({ name, data: {}, success: () => undefined });
    }

    const first = audit.drain(0, 2);
    expect(first.records.map((entry) => entry.seq)).toEqual([1, 2]);
    expect(first.hasMore).toBe(true);

    const second = audit.drain(first.nextSeq, 2);
    expect(second.records.map((entry) => entry.record.name)).toEqual(["c"]);
    expect(audit.drain(second.nextSeq, 2).records).toEqual([]);
  });

  it("captures operateCloudFunction bridge calls and stops after uninstall", () => {
    const invoked: string[] = [];
    const bridge: TestBridge = {
      invoke: (method, _params, callback) => {
        invoked.push(method);
        callback?.({ result: { ok: true } });
        return undefined;
      },
    };
    const { audit } = harness({ bridge });
    audit.install();

    bridge.invoke(
      "operateCloudFunction",
      { app_id: "wxone", data: JSON.stringify({ api_name: "hello" }) },
      () => undefined,
    );
    bridge.invoke("operateWXData", { app_id: "wxone", data: JSON.stringify({ api_name: "dbQuery" }) }, () => undefined);

    const page = audit.drain(0, 10);
    expect(page.records).toHaveLength(2);
    expect(page.records.map((entry) => entry.record.type)).toEqual(["function", "function"]);
    expect(page.records.map((entry) => entry.record.name)).toEqual(["hello", "dbQuery"]);
    expect(page.records[0].record).toMatchObject({ status: "success", appId: "wxone" });

    audit.uninstallHook();
    expect(bridge._cloudAuditHooked).toBeUndefined();

    bridge.invoke("operateCloudFunction", { app_id: "wxone", data: JSON.stringify({ api_name: "after" }) }, () => undefined);
    const after = audit.drain(0, 10);
    expect(after.records).toHaveLength(2);
    expect(after.records.map((entry) => entry.record.name)).toEqual(["hello", "dbQuery"]);
    expect(invoked).toEqual(["operateCloudFunction", "operateWXData", "operateCloudFunction"]);
  });
  it("resolves manual cloud calls for the shell", async () => {
    const { audit } = harness();
    audit.install();

    await expect(audit.callFunction("hello", { a: 1 })).resolves.toMatchObject({
      ok: true,
      status: "success",
      result: { ok: true },
    });
    await expect(audit.callFunction("boom", {})).resolves.toMatchObject({
      ok: true,
      status: "fail",
      error: "cloud.callFunction:fail",
    });
  });

  it("falls back to the live cloud API when the hook was never installed", async () => {
    const { audit } = harness();

    await expect(audit.callFunction("hello", {})).resolves.toMatchObject({ ok: true, status: "success" });
  });

  it("aggregates captured calls into discovered functions", () => {
    const { audit, cloud } = harness();
    audit.install();

    cloud.callFunction({ name: "hello", data: { a: 1 }, success: () => undefined });
    cloud.callFunction({ name: "hello", data: { a: 2 }, success: () => undefined });

    const discovered = audit.getDiscoveredFunctions();
    expect(discovered).toHaveLength(1);
    expect(discovered[0]).toMatchObject({ name: "hello", type: "function", appId: "wxone", count: 2 });
  });
  it("captures database terminal calls once, however often database() is called", () => {
    const { audit, cloud } = harness();
    audit.install();

    // Miniapps commonly re-acquire the database handle per operation.
    cloud.database().collection("users").get({ success: () => undefined });
    cloud.database().collection("users").get({ success: () => undefined });

    const page = audit.drain(0, 10);
    expect(page.records.map((entry) => entry.record.type)).toEqual(["db.get", "db.get"]);
    expect(page.records[0].record).toMatchObject({
      type: "db.get",
      name: "users",
      status: "success",
      appId: "wxone",
    });
  });

  it("statically discovers cloud functions, collections and storage calls", () => {
    const { audit, sandbox } = harness();
    sandbox.__wxAppCode__ = {
      "app.js": "function f(){ wx.cloud.callFunction({ name: 'hello', data: { a: 1, b: 2 } }) }",
      "db.js": "function g(){ db.collection('users').where({}).get() }",
      "file.js": "function h(){ wx.cloud.uploadFile({ filePath: 'a', cloudPath: 'b' }) }",
    };

    const items = audit.scanCloudFunctions() as Array<Record<string, unknown>>;
    const byName = new Map(items.map((item) => [String(item.name), item]));

    expect(byName.get("hello")).toMatchObject({ type: "function", appId: "wxone", count: 1 });
    expect(byName.get("hello")?.params).toEqual(["a", "b"]);
    expect(byName.get("users")).toMatchObject({ type: "database", appId: "wxone" });
    expect(byName.get("users")?.params).toEqual(expect.arrayContaining(["get", "where"]));
    expect(byName.get("uploadFile")).toMatchObject({ type: "storage" });
  });

  it("returns no discoveries when no app code is loaded", () => {
    const { audit } = harness();

    expect(audit.scanCloudFunctions()).toEqual([]);
  });
  it("restores the original cloud API on uninstall", () => {
    const { audit, cloud } = harness();
    const original = cloud.callFunction;

    audit.install();
    expect(cloud.callFunction).not.toBe(original);
    expect(cloud.callFunction.name).not.toBe("callFunction");

    audit.uninstallHook();
    expect(cloud.callFunction).not.toBe(original);
    // The restored function is the bound original: calling it must not record.
    cloud.callFunction({ name: "afterUninstall" });
    expect(audit.drain(0, 10).records).toEqual([]);
  });
});

// These two used to sit outside every describe, so filtering by suite name
// silently skipped them. They belong with the identity assertions below.
describe("hooks/cloud.js account identity", () => {
  it("reads the nested appAccount identity", () => {
    const created = harness();
    created.sandbox.__wxConfig = { accountInfo: { appAccount: { appId: "wxnested" } } };

    expect(created.audit.install()).toMatchObject({ ok: true });
    created.cloud.callFunction({ name: "nested", data: {}, success: () => undefined });

    const page = created.audit.drain(0, 10);
    expect(page.records[0].record).toMatchObject({ appId: "wxnested" });
  });

  it("falls back to account info API", () => {
    const created = harness();
    created.sandbox.__wxConfig = {};

    expect(created.audit.install()).toMatchObject({ ok: true });
    created.cloud.callFunction({ name: "fallback", data: {}, success: () => undefined });

    const page = created.audit.drain(0, 10);
    expect(page.records[0].record).toMatchObject({ appId: "wxone" });
  });
});

describe("hooks/cloud.js record identity", () => {
  it("derives rid exactly like traffic_records.id", () => {
    const { audit, cloud } = harness();
    audit.install();

    cloud.callFunction({ name: "a", data: {}, success: () => undefined });
    cloud.callFunction({ name: "b", data: {}, success: () => undefined });

    const page = audit.drain(0, 10);
    const first = page.records[0];
    const second = page.records[1];
    // The formula matches desktop/internal/cloud/drainer.go's Convert fallback
    // field by field, which is what makes the two delivery paths idempotent.
    expect(first.record.rid).toBe(`function-wxone-${String(first.record.ts)}-${first.seq}`);
    expect(second.record.rid).toBe(`function-wxone-${String(second.record.ts)}-${second.seq}`);
    expect(first.record.rid).not.toBe(second.record.rid);
    // The update stream settles the very same identity.
    expect(page.updates.map((entry) => entry.update.rid)).toEqual([
      first.record.rid,
      second.record.rid,
    ]);
  });

  it("keeps the rid's appId segment empty when the page exposes no identity", () => {
    const { audit, cloud } = harness({ appId: "" });
    audit.install();

    cloud.database().collection("users").get({ success: () => undefined });

    const page = audit.drain(0, 10);
    const record = page.records[0].record;
    expect(record).toMatchObject({ type: "db.get", name: "users", appId: "" });
    // An unknown appId stays an empty segment rather than a stand-in: the shell
    // recomputes the same id in drainer.go's Convert fallback, and a placeholder
    // here would send the event path and the poll path to two different rows.
    expect(record.rid).toBe(`db.get--${String(record.ts)}-${page.records[0].seq}`);
  });
});

describe("hooks/cloud.js update stream", () => {
  it("appends one settled frame per record", () => {
    const { audit, cloud } = harness();
    audit.install();

    cloud.callFunction({ name: "hello", data: { a: 1 }, success: () => undefined });

    const page = audit.drain(0, 10);
    expect(page.records).toHaveLength(1);
    expect(page.updates.map((entry) => entry.seq)).toEqual([1]);
    // Entry shape mirrors the record stream: the sequence outside, the frame inside.
    expect(Object.keys(page.updates[0]).sort()).toEqual(["seq", "update"]);
    const frame = page.updates[0].update;
    expect(frame).toMatchObject({
      rid: page.records[0].record.rid,
      status: "success",
      result: { result: { ok: true }, requestID: "rid-1" },
    });
    expect(Number.isInteger(frame.durationMs)).toBe(true);
    expect(frame.durationMs).toBeGreaterThanOrEqual(0);
    expect(frame.settledAt).toBeGreaterThanOrEqual(Number(page.records[0].record.ts));
    // result and error are mutually exclusive.
    expect("error" in frame).toBe(false);
  });

  it("carries the error instead of a result on failure", () => {
    const { audit, cloud } = harness();
    audit.install();

    cloud.callFunction({ name: "boom", fail: () => undefined, __fail: true });

    const page = audit.drain(0, 10);
    const frame = page.updates[0].update;
    expect(frame).toMatchObject({
      rid: page.records[0].record.rid,
      status: "fail",
      error: "cloud.callFunction:fail",
    });
    expect("result" in frame).toBe(false);
  });

  it("delivers the outcome of a call that settles after it was drained", async () => {
    const { audit, cloud, deferred } = harness({ directHook: true });
    expect(audit.install()).toMatchObject({ ok: true, directHooked: true });

    cloud.callFunction({ name: "slow", data: {}, __defer: true });

    const first = audit.drain(0, 10);
    expect(first.records[0].record).toMatchObject({ name: "slow", status: "pending" });
    expect(first.updates).toEqual([]);

    // The call lands on a microtask, after the record was already acked.
    await new Promise((resolve) => setTimeout(resolve, 20));
    deferred[0]();
    await new Promise((resolve) => setTimeout(resolve, 0));

    const second = audit.drain(first.nextSeq, 10, first.nextUpdateSeq, 200);
    expect(second.records).toEqual([]);
    expect(second.updates.map((entry) => entry.seq)).toEqual([1]);
    expect(second.updates[0].update).toMatchObject({
      rid: first.records[0].record.rid,
      status: "success",
      result: { result: { deferred: true } },
    });
    // The record was stamped when the call started, so the frame carries the
    // real duration of the call instead of a zero.
    expect(second.updates[0].update.durationMs).toBeGreaterThan(0);
    expect(second.nextUpdateSeq).toBe(1);
  });

  it("settles a record once even when the callback fires twice and the promise lands too", async () => {
    const { audit, cloud, calls, deferred } = harness({ directHook: true });
    audit.install();

    cloud.callFunction({ name: "both", data: {}, success: () => undefined, __defer: true });
    // The platform may report the same call more than once: the callback twice,
    // or the callback and the promise. The outcome frame is appended once per
    // rid, not once per settle.
    calls[0].success?.({ result: { ok: true } });
    await new Promise((resolve) => setTimeout(resolve, 0));
    deferred[0]();
    await new Promise((resolve) => setTimeout(resolve, 0));

    const page = audit.drain(0, 10);
    expect(page.records).toHaveLength(1);
    expect(page.updates).toHaveLength(1);
    expect(page.updates[0].update).toMatchObject({ status: "success" });
  });

  it("treats updateLimit 0 as records-only without advancing the update cursor", () => {
    const { audit, cloud } = harness();
    audit.install();

    cloud.callFunction({ name: "a", data: {}, success: () => undefined });

    const recordsOnly = audit.drain(0, 10, 0, 0);
    expect(recordsOnly.records).toHaveLength(1);
    expect(recordsOnly.updates).toEqual([]);
    expect(recordsOnly.nextUpdateSeq).toBe(0);

    // The frame is still there for a caller that does want it.
    const withUpdates = audit.drain(recordsOnly.nextSeq, 10, recordsOnly.nextUpdateSeq, 200);
    expect(withUpdates.updates.map((entry) => entry.seq)).toEqual([1]);
    expect(withUpdates.nextUpdateSeq).toBe(1);

    // 0 is a cursor no-op, not an ack: it never skips an update.
    expect(audit.drain(0, 10, 0, 0).nextUpdateSeq).toBe(0);
  });

  it("defaults the update cursor and limit, and never resends an acked update", () => {
    const { audit, cloud } = harness();
    audit.install();

    for (const name of ["a", "b", "c"]) {
      cloud.callFunction({ name, data: {}, success: () => undefined });
    }

    // Omitted update parameters read as 0 / 200.
    const all = audit.drain(0, 10);
    expect(all.updates.map((entry) => entry.seq)).toEqual([1, 2, 3]);
    expect(all.nextUpdateSeq).toBe(3);

    const acked = audit.drain(all.nextSeq, 10, all.nextUpdateSeq, 200);
    expect(acked.updates).toEqual([]);
    expect(acked.nextUpdateSeq).toBe(3);

    // A limit page hands back the cursor of what it actually delivered, while
    // hasMore keeps describing the record stream alone.
    const limited = audit.drain(all.nextSeq, 10, 0, 2);
    expect(limited.updates.map((entry) => entry.seq)).toEqual([1, 2]);
    expect(limited.nextUpdateSeq).toBe(2);
    expect(limited.hasMore).toBe(false);
    expect(audit.drain(all.nextSeq, 10, limited.nextUpdateSeq, 2).updates.map((entry) => entry.seq))
      .toEqual([3]);
  });

  it("clears both buffers without rewinding the update sequence", () => {
    const { audit, cloud } = harness();
    audit.install();

    cloud.callFunction({ name: "a", data: {}, success: () => undefined });
    expect(audit.drain(0, 10).nextUpdateSeq).toBe(1);

    audit.clearHookedCalls();
    const cleared = audit.drain(0, 10, 0, 200);
    expect(cleared.records).toEqual([]);
    expect(cleared.updates).toEqual([]);
    expect(cleared.nextUpdateSeq).toBe(0);

    // New captures keep counting records and updates alike.
    cloud.callFunction({ name: "b", data: {}, success: () => undefined });
    const next = audit.drain(0, 10, 0, 200);
    expect(next.records.map((entry) => entry.seq)).toEqual([2]);
    expect(next.updates.map((entry) => entry.seq)).toEqual([2]);
  });

  it("keeps both buffers bounded exactly like the DrainQueue and UpdateQueue models", () => {
    const { audit, cloud } = harness();
    audit.install();

    const recordModel = new DrainQueue<{ name: string }>(PAGE_RECORD_BUFFER_CAPACITY);
    const updateModel = new UpdateQueue<{ rid: string }>(PAGE_UPDATE_BUFFER_CAPACITY);
    for (let index = 0; index < PAGE_RECORD_BUFFER_CAPACITY + 5; index += 1) {
      cloud.callFunction({ name: `fn-${index}`, data: {}, success: () => undefined });
      recordModel.push({ name: `fn-${index}` });
      updateModel.pushUpdate({ rid: `rid-${index}` });
    }

    const page = audit.drain(0, PAGE_RECORD_BUFFER_CAPACITY * 2, 0, PAGE_UPDATE_BUFFER_CAPACITY * 2);
    const recordPage = recordModel.drain(0, PAGE_RECORD_BUFFER_CAPACITY * 2);
    const updatePage = updateModel.drainUpdates(0, PAGE_UPDATE_BUFFER_CAPACITY * 2);

    expect(page.records.map((entry) => entry.seq)).toEqual(
      recordPage.records.map((entry) => entry.seq),
    );
    expect(page.updates.map((entry) => entry.seq)).toEqual(
      updatePage.updates.map((entry) => entry.seq),
    );
    expect(page.nextSeq).toBe(recordPage.nextSeq);
    expect(page.nextUpdateSeq).toBe(updatePage.nextUpdateSeq);
    // Oldest entries are evicted; sequences keep counting.
    expect(page.records).toHaveLength(PAGE_RECORD_BUFFER_CAPACITY);
    expect(page.records[0].seq).toBe(6);
    expect(page.updates).toHaveLength(PAGE_UPDATE_BUFFER_CAPACITY);
    expect(page.updates[0].seq).toBe(6);
    expect(page.updates[PAGE_UPDATE_BUFFER_CAPACITY - 1].seq).toBe(PAGE_UPDATE_BUFFER_CAPACITY + 5);
  });
});

describe("hooks/cloud.js call timing", () => {
  it("stamps a wrapped call at its call moment so the settled frame carries the real duration", async () => {
    const { audit, cloud, pendingSettle } = harness();
    audit.install();

    const before = Date.now();
    cloud.callFunction({ name: "slow", data: {}, success: () => undefined, __deferCallback: true });
    const after = Date.now();

    // Nothing is buffered while the call is in flight: the wrappers write their
    // record at the settle point, which is exactly why the call moment has to
    // be captured by the wrapper.
    expect(audit.drain(0, 10).records).toEqual([]);

    await new Promise((resolve) => setTimeout(resolve, 25));
    pendingSettle[0]();

    const page = audit.drain(0, 10);
    expect(page.records).toHaveLength(1);
    const record = page.records[0].record;
    const frame = page.updates[0].update;
    // ts is the call moment, not the callback's - the rid formula is built on it.
    expect(Number(record.ts)).toBeGreaterThanOrEqual(before);
    expect(Number(record.ts)).toBeLessThanOrEqual(after);
    expect(record.rid).toBe(`function-wxone-${String(record.ts)}-${page.records[0].seq}`);
    // The duration is measured from that moment to the settle, so a delayed
    // answer can no longer report a ~0ms call.
    expect(frame.durationMs).toBeGreaterThanOrEqual(20);
    expect(frame.durationMs).toBe(frame.settledAt - Number(record.ts));
  });

  it("does the same for a call that settles through its promise", async () => {
    const { audit, cloud, deferred } = harness();
    audit.install();

    const before = Date.now();
    cloud.callFunction({ name: "slowPromise", data: {}, __latePromise: true });
    const after = Date.now();

    await new Promise((resolve) => setTimeout(resolve, 25));
    deferred[0]({ result: { late: true } });
    await new Promise((resolve) => setTimeout(resolve, 0));

    const page = audit.drain(0, 10);
    const record = page.records[0].record;
    const frame = page.updates[0].update;
    expect(Number(record.ts)).toBeGreaterThanOrEqual(before);
    expect(Number(record.ts)).toBeLessThanOrEqual(after);
    expect(frame).toMatchObject({ status: "success", result: { result: { late: true } } });
    expect(frame.durationMs).toBeGreaterThanOrEqual(20);
    expect(frame.durationMs).toBe(frame.settledAt - Number(record.ts));
  });

  it("stamps a call that fails through its promise", async () => {
    const { audit, cloud, rejections } = harness();
    audit.install();

    const before = Date.now();
    cloud.callFunction({ name: "rejecting", data: {}, __lateReject: true });
    const after = Date.now();

    expect(audit.drain(0, 10).records).toEqual([]);

    await new Promise((resolve) => setTimeout(resolve, 25));
    rejections[0](platformError("cloud.callFunction:fail"));
    await new Promise((resolve) => setTimeout(resolve, 0));

    const page = audit.drain(0, 10);
    expect(page.records).toHaveLength(1);
    const record = page.records[0].record;
    const frame = page.updates[0].update;
    // The catch path writes the record too, so it has the same duty to carry
    // the call moment.
    expect(Number(record.ts)).toBeGreaterThanOrEqual(before);
    expect(Number(record.ts)).toBeLessThanOrEqual(after);
    expect(frame).toMatchObject({ status: "fail", error: "cloud.callFunction:fail" });
    // result and error are mutually exclusive.
    expect("result" in frame).toBe(false);
    expect(frame.durationMs).toBeGreaterThanOrEqual(20);
    expect(frame.durationMs).toBe(frame.settledAt - Number(record.ts));
  });

  it("stamps a bridge-observed cloud call at the invoke moment", async () => {
    let answer: ((res: unknown) => void) | undefined;
    const bridge: TestBridge = {
      invoke: (_method, _params, callback) => {
        answer = callback;
        return undefined;
      },
    };
    const { audit } = harness({ bridge });
    audit.install();

    const before = Date.now();
    bridge.invoke(
      "operateCloudFunction",
      { app_id: "wxone", data: JSON.stringify({ api_name: "slow" }) },
      () => undefined,
    );
    const after = Date.now();

    await new Promise((resolve) => setTimeout(resolve, 25));
    answer?.({ result: { ok: true } });

    const page = audit.drain(0, 10);
    const record = page.records[0].record;
    const frame = page.updates[0].update;
    expect(Number(record.ts)).toBeGreaterThanOrEqual(before);
    expect(Number(record.ts)).toBeLessThanOrEqual(after);
    expect(frame.durationMs).toBeGreaterThanOrEqual(20);
    expect(frame.durationMs).toBe(frame.settledAt - Number(record.ts));
  });

  it("leaves an immediately settled call on the same formula", () => {
    const { audit, cloud } = harness();
    audit.install();

    cloud.callFunction({ name: "fast", data: {}, success: () => undefined });

    const page = audit.drain(0, 10);
    const record = page.records[0].record;
    const frame = page.updates[0].update;
    // The common path is untouched: one record, one frame, the same formula,
    // and the call settles inside the call statement.
    expect(page.records).toHaveLength(1);
    expect(page.updates).toHaveLength(1);
    expect(frame.durationMs).toBe(frame.settledAt - Number(record.ts));
    expect(frame.durationMs).toBeLessThan(20);
  });

  // A database terminal call has four settle sites (callback success/fail and
  // promise success/fail) and each one writes the record itself, so each one
  // has to carry the call moment the wrapper captured at the call.

  it("stamps a database terminal call that settles through its promise", async () => {
    const { audit, cloud, deferred } = harness();
    audit.install();

    const before = Date.now();
    cloud.database().collection("users").get({ __terminal: "promiseSuccess" });
    const after = Date.now();

    // Nothing is buffered while the call is in flight: the record is written at
    // the settle point.
    expect(audit.drain(0, 10).records).toEqual([]);

    await new Promise((resolve) => setTimeout(resolve, 25));
    deferred[0]({ data: [{ _id: "late" }] });
    await new Promise((resolve) => setTimeout(resolve, 0));

    const page = audit.drain(0, 10);
    expect(page.records).toHaveLength(1);
    const record = page.records[0].record;
    const frame = page.updates[0].update;
    expect(Number(record.ts)).toBeGreaterThanOrEqual(before);
    expect(Number(record.ts)).toBeLessThanOrEqual(after);
    expect(record).toMatchObject({ type: "db.get", name: "users", appId: "wxone" });
    expect(record.rid).toBe(`db.get-wxone-${String(record.ts)}-${page.records[0].seq}`);
    expect(frame).toMatchObject({ status: "success", result: { data: [{ _id: "late" }] } });
    expect(frame.durationMs).toBeGreaterThanOrEqual(20);
    expect(frame.durationMs).toBe(frame.settledAt - Number(record.ts));
  });

  it("stamps a database terminal call that fails through its promise", async () => {
    const { audit, cloud, rejections } = harness();
    audit.install();

    const before = Date.now();
    cloud.database().collection("users").get({ __terminal: "promiseReject" });
    const after = Date.now();

    expect(audit.drain(0, 10).records).toEqual([]);

    await new Promise((resolve) => setTimeout(resolve, 25));
    rejections[0](platformError("db.get:fail"));
    await new Promise((resolve) => setTimeout(resolve, 0));

    const page = audit.drain(0, 10);
    expect(page.records).toHaveLength(1);
    const record = page.records[0].record;
    const frame = page.updates[0].update;
    expect(Number(record.ts)).toBeGreaterThanOrEqual(before);
    expect(Number(record.ts)).toBeLessThanOrEqual(after);
    expect(frame).toMatchObject({ status: "fail", error: "db.get:fail" });
    expect("result" in frame).toBe(false);
    expect(frame.durationMs).toBeGreaterThanOrEqual(20);
    expect(frame.durationMs).toBe(frame.settledAt - Number(record.ts));
  });

  it("stamps a database terminal call that settles through its success callback", async () => {
    const { audit, cloud, pendingSettle } = harness();
    audit.install();

    const before = Date.now();
    cloud.database().collection("users").get({ success: () => undefined, __terminal: "callbackSuccess" });
    const after = Date.now();

    expect(audit.drain(0, 10).records).toEqual([]);

    await new Promise((resolve) => setTimeout(resolve, 25));
    pendingSettle[0]();

    const page = audit.drain(0, 10);
    expect(page.records).toHaveLength(1);
    const record = page.records[0].record;
    const frame = page.updates[0].update;
    expect(Number(record.ts)).toBeGreaterThanOrEqual(before);
    expect(Number(record.ts)).toBeLessThanOrEqual(after);
    expect(frame).toMatchObject({ status: "success" });
    expect(frame.durationMs).toBeGreaterThanOrEqual(20);
    expect(frame.durationMs).toBe(frame.settledAt - Number(record.ts));
  });

  it("stamps a database terminal call that fails through its fail callback", async () => {
    const { audit, cloud, pendingSettle } = harness();
    audit.install();

    const before = Date.now();
    cloud.database().collection("users").get({ fail: () => undefined, __terminal: "callbackFail" });
    const after = Date.now();

    expect(audit.drain(0, 10).records).toEqual([]);

    await new Promise((resolve) => setTimeout(resolve, 25));
    pendingSettle[0]();

    const page = audit.drain(0, 10);
    expect(page.records).toHaveLength(1);
    const record = page.records[0].record;
    const frame = page.updates[0].update;
    expect(Number(record.ts)).toBeGreaterThanOrEqual(before);
    expect(Number(record.ts)).toBeLessThanOrEqual(after);
    expect(frame).toMatchObject({ status: "fail", error: "db.get:fail" });
    expect("result" in frame).toBe(false);
    expect(frame.durationMs).toBeGreaterThanOrEqual(20);
    expect(frame.durationMs).toBe(frame.settledAt - Number(record.ts));
  });
});

describe("hooks/cloud.js drop accounting", () => {
  it("reports zero dropped counts while nothing is evicted", () => {
    const { audit, cloud } = harness();
    audit.install();

    cloud.callFunction({ name: "a", data: {}, success: () => undefined });

    const page = audit.drain(0, 10);
    expect(page.records).toHaveLength(1);
    expect(page.droppedRecords).toBe(0);
    expect(page.droppedUpdates).toBe(0);
  });

  it("counts every evicted record and update exactly like the model queues", () => {
    const { audit, cloud } = harness();
    audit.install();

    const recordModel = new DrainQueue<{ name: string }>(PAGE_RECORD_BUFFER_CAPACITY);
    const updateModel = new UpdateQueue<{ rid: string }>(PAGE_UPDATE_BUFFER_CAPACITY);
    const overflow = 7;
    for (let index = 0; index < PAGE_RECORD_BUFFER_CAPACITY + overflow; index += 1) {
      cloud.callFunction({ name: `fn-${index}`, data: {}, success: () => undefined });
      recordModel.push({ name: `fn-${index}` });
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
    const { audit, cloud } = harness();
    audit.install();

    for (let index = 0; index < PAGE_RECORD_BUFFER_CAPACITY + 1; index += 1) {
      cloud.callFunction({ name: `fn-${index}`, data: {}, success: () => undefined });
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
    const { audit, cloud } = harness();
    audit.install();

    const overflow = 3;
    for (let index = 0; index < PAGE_RECORD_BUFFER_CAPACITY + overflow; index += 1) {
      cloud.callFunction({ name: `fn-${index}`, data: {}, success: () => undefined });
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
