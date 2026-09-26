import { describe, expect, it, vi } from "vitest";

import { CoreApp } from "./app.js";
import { Engine, type RuntimeAdapter } from "./engine/engine.js";
import { CdpBridge } from "./bridge/cdp-bridge.js";
import { decodeCdpMessage, encodeCdpMessage } from "./protocol/wmpf-codec.js";
import type { RpcRequest } from "./rpc/protocol.js";

const offlineRuntime: RuntimeAdapter = {
  start: async () => ({ frida: false, miniapp: false, devtools: false }),
  stop: async () => {},
  status: () => ({ frida: false, miniapp: false, devtools: false }),
};

function onlineRuntime(): RuntimeAdapter {
  return {
    start: async () => ({ frida: false, miniapp: false, devtools: false }),
    stop: async () => {},
    status: () => ({ frida: true, miniapp: true, devtools: false }),
  };
}

/** Drives a CdpBridge end to end: captures the outbound CDP command and feeds back a response. */
function connectFakeMiniapp(bridge: CdpBridge) {
  const sent: Buffer[] = [];
  const clientId = bridge.addMiniapp({ send: (message) => sent.push(message as Buffer) });
  return {
    clientId,
    sent,
    respond(result: unknown) {
      const outbound = decodeCdpMessage(sent[sent.length - 1]);
      const command = JSON.parse(outbound?.payload ?? "{}") as { id: number };
      bridge.receiveMiniapp(encodeCdpMessage({
        sequence: 1,
        category: "chromeDevtoolsResult",
        operationId: command.id,
        payload: JSON.stringify({ id: command.id, result }),
        jsContextId: "",
      }), clientId);
      return command.id;
    },
  };
}

/** Reads the Runtime.evaluate expression of the most recent outbound CDP command. */
function lastExpression(fake: ReturnType<typeof connectFakeMiniapp>): string {
  const outbound = decodeCdpMessage(fake.sent[fake.sent.length - 1]);
  const command = JSON.parse(outbound?.payload ?? "{}") as { params?: { expression?: string } };
  return command.params?.expression ?? "";
}

describe("CoreApp engine routing", () => {
  it("routes engine.status to the engine", async () => {
    const app = new CoreApp(new Engine(onlineRuntime()));

    await expect(app.handle({ id: 1, method: "engine.status", params: {} })).resolves.toEqual({
      frida: true,
      miniapp: true,
      devtools: false,
    });
  });

  it("reports WeChat process presence without starting the debug engine", async () => {
    const app = new CoreApp(new Engine({
      ...offlineRuntime,
      isWeChatRunning: async () => true,
    }));

    await expect(app.handle({ id: 2, method: "wechat.status", params: {} })).resolves.toEqual({ running: true });
  });

  it("returns the WMPF host description when the runtime can identify it", async () => {
    const app = new CoreApp(new Engine({
      ...offlineRuntime,
      describeWeChat: async () => ({ running: true, pid: 10, version: 14161, addressTable: false, note: "没有这份构建的静态地址表，启动时会自动检测" }),
    }));

    await expect(app.handle({ id: 3, method: "wechat.status", params: {} })).resolves.toEqual({
      running: true,
      pid: 10,
      version: 14161,
      addressTable: false,
      note: "没有这份构建的静态地址表，启动时会自动检测",
    });
  });

  it("rejects methods outside the Core contract", async () => {
    const app = new CoreApp(new Engine(offlineRuntime));

    await expect(app.handle({ id: 1, method: "engine.reset", params: {} })).rejects.toThrow("unknown method: engine.reset");
  });
});

describe("CoreApp CDP and hook methods", () => {
  it("sends an arbitrary CDP command and returns its response", async () => {
    const bridge = new CdpBridge();
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge);
    const fake = connectFakeMiniapp(bridge);

    const pending = app.handle({ id: 2, method: "cdp.command", params: { method: "Page.enable", params: {}, timeoutMs: 500 } });
    fake.respond({});
    await expect(pending).resolves.toEqual({ id: expect.any(Number), result: {} });
  });

  it("evaluates JS through Runtime.evaluate and parses the value", async () => {
    const bridge = new CdpBridge();
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge);
    const fake = connectFakeMiniapp(bridge);

    const pending = app.handle({ id: 3, method: "runtime.evaluate", params: { expression: "6 * 7", timeoutMs: 500 } }) as Promise<Record<string, unknown>>;
    fake.respond({ result: { type: "string", value: "42" } });
    await expect(pending).resolves.toEqual({ value: "42" });
  });

  it("awaits the evaluated promise when awaitPromise is set", async () => {
    const bridge = new CdpBridge();
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge);
    const fake = connectFakeMiniapp(bridge);

    const pending = app.handle({ id: 12, method: "runtime.evaluate", params: { expression: "Promise.resolve(7)", awaitPromise: true, timeoutMs: 500 } }) as Promise<Record<string, unknown>>;
    const outbound = decodeCdpMessage(fake.sent[0]);
    const command = JSON.parse(outbound?.payload ?? "{}") as { params?: { awaitPromise?: boolean } };
    expect(command.params?.awaitPromise).toBe(true);
    fake.respond({ result: { type: "number", value: 7 } });
    await expect(pending).resolves.toEqual({ value: 7 });
  });

  it("installs a hook by name: injects the script source, then calls the install surface", async () => {
    const bridge = new CdpBridge();
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge, async (name) => `/*script:${name}*/`);
    const fake = connectFakeMiniapp(bridge);

    const pending = app.handle({ id: 4, method: "hook.install", params: { name: "wxapi" } }) as Promise<Record<string, unknown>>;
    await vi.waitFor(() => expect(fake.sent.length).toBe(1));
    expect(lastExpression(fake)).toBe("/*script:wxapi*/");
    fake.respond({ result: { type: "undefined" } });

    await vi.waitFor(() => expect(fake.sent.length).toBe(2));
    expect(lastExpression(fake)).toBe("JSON.stringify(window.wxApiAudit.install())");
    fake.respond({ result: { type: "string", value: JSON.stringify({ ok: true, hookedCount: 8 }) } });
    await expect(pending).resolves.toEqual({ ok: true, hookedCount: 8 });
  });

  it("drains hook records with afterSeq and limit", async () => {
    const bridge = new CdpBridge();
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge);
    const fake = connectFakeMiniapp(bridge);

    const pending = app.handle({ id: 5, method: "hook.drain", params: { name: "wxapi", afterSeq: 10, limit: 100 } }) as Promise<Record<string, unknown>>;
    await vi.waitFor(() => expect(fake.sent.length).toBe(1));
    // No update parameters: the page-side defaults (0 / 200) are passed through.
    expect(lastExpression(fake)).toBe("JSON.stringify(window.wxApiAudit.drain(10, 100, 0, 200))");
    fake.respond({ result: { type: "string", value: JSON.stringify({ records: [{ seq: 11, record: { name: "x" } }], nextSeq: 11, hasMore: false }) } });
    await expect(pending).resolves.toEqual({
      records: [{ seq: 11, record: { name: "x" } }],
      nextSeq: 11,
      hasMore: false,
    });
  });

  it("forwards the update-stream cursor and limit to the page", async () => {
    const bridge = new CdpBridge();
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge);
    const fake = connectFakeMiniapp(bridge);

    const pending = app.handle({
      id: 5,
      method: "hook.drain",
      params: { name: "wxapi", afterSeq: 4, limit: 50, afterUpdateSeq: 9, updateLimit: 25 },
    }) as Promise<Record<string, unknown>>;
    await vi.waitFor(() => expect(fake.sent.length).toBe(1));
    expect(lastExpression(fake)).toBe("JSON.stringify(window.wxApiAudit.drain(4, 50, 9, 25))");
    fake.respond({ result: { type: "string", value: JSON.stringify({ records: [], updates: [], nextSeq: 4, nextUpdateSeq: 9, hasMore: false }) } });
    await expect(pending).resolves.toEqual({ records: [], updates: [], nextSeq: 4, nextUpdateSeq: 9, hasMore: false });
  });

  it("forwards an explicit records-only updateLimit of 0", async () => {
    const bridge = new CdpBridge();
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge);
    const fake = connectFakeMiniapp(bridge);

    const pending = app.handle({
      id: 5,
      method: "hook.drain",
      params: { name: "wxapi", afterSeq: 4, limit: 50, afterUpdateSeq: 3, updateLimit: 0 },
    }) as Promise<Record<string, unknown>>;
    await vi.waitFor(() => expect(fake.sent.length).toBe(1));
    // The MCP tool predates the update stream and asks for records only.
    expect(lastExpression(fake)).toBe("JSON.stringify(window.wxApiAudit.drain(4, 50, 3, 0))");
    fake.respond({ result: { type: "string", value: JSON.stringify({ records: [], updates: [], nextSeq: 4, nextUpdateSeq: 3, hasMore: false }) } });
    await expect(pending).resolves.toEqual({ records: [], updates: [], nextSeq: 4, nextUpdateSeq: 3, hasMore: false });
  });

  it("rejects negative update-stream parameters", async () => {
    const bridge = new CdpBridge();
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge);
    connectFakeMiniapp(bridge);

    await expect(
      app.handle({ id: 5, method: "hook.drain", params: { name: "wxapi", afterSeq: 0, limit: 10, afterUpdateSeq: -1 } }),
    ).rejects.toThrow("afterUpdateSeq must be an integer >= 0");
    await expect(
      app.handle({ id: 6, method: "hook.drain", params: { name: "wxapi", afterSeq: 0, limit: 10, updateLimit: -5 } }),
    ).rejects.toThrow("updateLimit must be an integer >= 0");
  });

  it("fails with a retryable error when the engine has no bridge yet", async () => {
    const app = new CoreApp(new Engine(offlineRuntime));

    const request: RpcRequest = { id: 6, method: "runtime.evaluate", params: { expression: "1", timeoutMs: 10 } };
    const failure = await app.handle(request).catch((error: unknown) => error) as { retryable?: boolean; code?: number };
    expect(failure.retryable).toBe(true);
  });
});

describe("CoreApp miniapp management", () => {
  it("lists connected miniapps with lock state", async () => {
    const bridge = new CdpBridge();
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge);
    const first = connectFakeMiniapp(bridge);
    connectFakeMiniapp(bridge);

    await expect(app.handle({ id: 20, method: "miniapp.list", params: {} })).resolves.toEqual([
      { id: first.clientId, appid: "", name: "", locked: true },
      { id: first.clientId + 1, appid: "", name: "", locked: false },
    ]);
  });

  it("switches the debug lock to a listed miniapp id", async () => {
    const bridge = new CdpBridge();
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge);
    const fake = connectFakeMiniapp(bridge);

    await expect(app.handle({ id: 21, method: "miniapp.switch", params: { id: fake.clientId } })).resolves.toEqual({ ok: true });
    await expect(app.handle({ id: 22, method: "miniapp.switch", params: { id: 999 } })).resolves.toEqual({ ok: false });
  });

  it("reads and writes the lock switch", async () => {
    const bridge = new CdpBridge();
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge);

    await expect(app.handle({ id: 23, method: "miniapp.getLock", params: {} })).resolves.toEqual({ enabled: true });
    await expect(app.handle({ id: 24, method: "miniapp.setLock", params: { enabled: false } })).resolves.toEqual({});
    await expect(app.handle({ id: 25, method: "miniapp.getLock", params: {} })).resolves.toEqual({ enabled: false });
  });

  it("rejects miniapp methods with a retryable failure before the engine starts", async () => {
    const app = new CoreApp(new Engine(offlineRuntime));

    const request: RpcRequest = { id: 26, method: "miniapp.list", params: {} };
    const failure = await app.handle(request).catch((error: unknown) => error) as { retryable?: boolean };
    expect(failure.retryable).toBe(true);
  });

  it("merges the identified app info into engine.status", async () => {
    const bridge = new CdpBridge();
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge);
    connectFakeMiniapp(bridge);

    await expect(app.handle({ id: 27, method: "engine.status", params: {} })).resolves.toEqual({
      frida: true,
      miniapp: true,
      devtools: false,
    });

    bridge.setMiniappInfo(1, "wxabc", "识别到的应用");
    await expect(app.handle({ id: 28, method: "engine.status", params: {} })).resolves.toEqual({
      frida: true,
      miniapp: true,
      devtools: false,
      appInfo: { appid: "wxabc", name: "识别到的应用" },
    });
  });
});

describe("CoreApp code formatting", () => {
  it("formats JSON content through code.format", async () => {
    const app = new CoreApp(new Engine(onlineRuntime()));
    await expect(
      app.handle({ id: 30, method: "code.format", params: { content: '{"a":1}', language: "json" } }),
    ).resolves.toEqual({ formatted: '{\n  "a": 1\n}' });
  });

  it("rejects code.format without string content", async () => {
    const app = new CoreApp(new Engine(onlineRuntime()));
    const failure = await app
      .handle({ id: 31, method: "code.format", params: { language: "json" } })
      .catch((error: unknown) => error) as { code?: number };
    expect(failure.code).toBe(1000);
  });

  it("passes unsupported languages through untouched", async () => {
    const app = new CoreApp(new Engine(onlineRuntime()));
    await expect(
      app.handle({ id: 32, method: "code.format", params: { content: "as is", language: "text" } }),
    ).resolves.toEqual({ formatted: "as is" });
  });
});

describe("CoreApp hook dispatch by name", () => {
  it("dispatches cloud installs to window.cloudAudit after injecting the script", async () => {
    const bridge = new CdpBridge();
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge, async (name) => `/*script:${name}*/`);
    const fake = connectFakeMiniapp(bridge);

    const pending = app.handle({ id: 7, method: "hook.install", params: { name: "cloud" } }) as Promise<Record<string, unknown>>;
    await vi.waitFor(() => expect(fake.sent.length).toBe(1));
    expect(lastExpression(fake)).toBe("/*script:cloud*/");
    fake.respond({ result: { type: "undefined" } });

    await vi.waitFor(() => expect(fake.sent.length).toBe(2));
    expect(lastExpression(fake)).toBe("JSON.stringify(window.cloudAudit.install())");
    fake.respond({ result: { type: "string", value: JSON.stringify({ ok: true, totalHooked: 1 }) } });
    await expect(pending).resolves.toEqual({ ok: true, totalHooked: 1 });
  });

  it("runs cloud.scan through the injected cloud audit scanner", async () => {
    const bridge = new CdpBridge();
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge, async (name) => `/*script:${name}*/`);
    const fake = connectFakeMiniapp(bridge);

    const pending = app.handle({ id: 33, method: "cloud.scan", params: {} }) as Promise<Record<string, unknown>[]>;
    await vi.waitFor(() => expect(fake.sent.length).toBe(1));
    expect(lastExpression(fake)).toBe("/*script:cloud*/");
    fake.respond({ result: { type: "undefined" } });

    await vi.waitFor(() => expect(fake.sent.length).toBe(2));
    expect(lastExpression(fake)).toBe("JSON.stringify(window.cloudAudit.install())");
    fake.respond({ result: { type: "string", value: JSON.stringify({ ok: true }) } });

    await vi.waitFor(() => expect(fake.sent.length).toBe(3));
    expect(lastExpression(fake)).toBe("JSON.stringify(window.cloudAudit.scanCloudFunctions())");
    fake.respond({ result: { type: "string", value: JSON.stringify([{ name: "hello", type: "function", params: ["id"], count: 1 }]) } });

    await expect(pending).resolves.toEqual([{ name: "hello", type: "function", params: ["id"], count: 1 }]);
  });

  it("dispatches navigator installs to window.nav and reports its presence", async () => {
    const bridge = new CdpBridge();
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge, async (name) => `/*script:${name}*/`);
    const fake = connectFakeMiniapp(bridge);

    const pending = app.handle({ id: 8, method: "hook.install", params: { name: "navigator" } }) as Promise<Record<string, unknown>>;
    await vi.waitFor(() => expect(fake.sent.length).toBe(1));
    expect(lastExpression(fake)).toBe("/*script:navigator*/");
    fake.respond({ result: { type: "undefined" } });

    await vi.waitFor(() => expect(fake.sent.length).toBe(2));
    expect(lastExpression(fake)).toBe("JSON.stringify({ ok: !!(window.nav && window.nav.wxFrame) })");
    fake.respond({ result: { type: "string", value: JSON.stringify({ ok: true }) } });
    await expect(pending).resolves.toEqual({ ok: true });
  });

  it("installs the console hook script and reports its capture surface", async () => {
    const bridge = new CdpBridge();
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge, async (name) => `/*script:${name}*/`);
    const fake = connectFakeMiniapp(bridge);

    const pending = app.handle({ id: 12, method: "hook.install", params: { name: "console" } }) as Promise<Record<string, unknown>>;
    await vi.waitFor(() => expect(fake.sent.length).toBe(1));
    expect(lastExpression(fake)).toBe("/*script:console*/");
    fake.respond({ result: { type: "undefined" } });

    await vi.waitFor(() => expect(fake.sent.length).toBe(2));
    expect(lastExpression(fake)).toBe("JSON.stringify(window.consoleAudit.install())");
    fake.respond({ result: { type: "string", value: JSON.stringify({ ok: true, hookedConsoles: 1, levels: 5 }) } });
    await expect(pending).resolves.toEqual({ ok: true, hookedConsoles: 1, levels: 5 });
  });

  it("dispatches console drains to window.consoleAudit.drain", async () => {
    const bridge = new CdpBridge();
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge);
    const fake = connectFakeMiniapp(bridge);

    const pending = app.handle({ id: 13, method: "hook.drain", params: { name: "console", afterSeq: 7, limit: 200 } }) as Promise<Record<string, unknown>>;
    await vi.waitFor(() => expect(fake.sent.length).toBe(1));
    expect(lastExpression(fake)).toBe("JSON.stringify(window.consoleAudit.drain(7, 200, 0, 200))");
    fake.respond({ result: { type: "string", value: JSON.stringify({ records: [], nextSeq: 7, hasMore: false }) } });
    await expect(pending).resolves.toEqual({ records: [], nextSeq: 7, hasMore: false });
  });

  it("clears the page-side console buffer", async () => {
    const bridge = new CdpBridge();
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge);
    const fake = connectFakeMiniapp(bridge);

    const pending = app.handle({ id: 14, method: "hook.clear", params: { name: "console" } });
    await vi.waitFor(() => expect(fake.sent.length).toBe(1));
    expect(lastExpression(fake)).toBe("JSON.stringify({ok:(window.consoleAudit.clearHookedCalls(),true)})");
    fake.respond({ result: { type: "string", value: JSON.stringify({ ok: true }) } });
    await expect(pending).resolves.toEqual({ ok: true });
  });

  it("dispatches cloud drains to window.cloudAudit.drain without re-injecting the script", async () => {
    const bridge = new CdpBridge();
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge);
    const fake = connectFakeMiniapp(bridge);

    const pending = app.handle({ id: 9, method: "hook.drain", params: { name: "cloud", afterSeq: 5, limit: 50 } }) as Promise<Record<string, unknown>>;
    await vi.waitFor(() => expect(fake.sent.length).toBe(1));
    expect(lastExpression(fake)).toBe("JSON.stringify(window.cloudAudit.drain(5, 50, 0, 200))");
    fake.respond({ result: { type: "string", value: JSON.stringify({ records: [], nextSeq: 5, hasMore: false }) } });
    await expect(pending).resolves.toEqual({ records: [], nextSeq: 5, hasMore: false });
  });

  it("clears the page-side wxapi hook buffer", async () => {
    const bridge = new CdpBridge();
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge);
    const fake = connectFakeMiniapp(bridge);

    const pending = app.handle({ id: 34, method: "hook.clear", params: { name: "wxapi" } });
    await vi.waitFor(() => expect(fake.sent.length).toBe(1));
    expect(lastExpression(fake)).toBe("JSON.stringify({ok:(window.wxApiAudit.clearHookedCalls(),true)})");
    fake.respond({ result: { type: "string", value: JSON.stringify({ ok: true }) } });
    await expect(pending).resolves.toEqual({ ok: true });
  });

  it("uninstalls the page-side cloud hook", async () => {
    const bridge = new CdpBridge();
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge);
    const fake = connectFakeMiniapp(bridge);

    const pending = app.handle({ id: 35, method: "hook.uninstall", params: { name: "cloud" } });
    await vi.waitFor(() => expect(fake.sent.length).toBe(1));
    expect(lastExpression(fake)).toBe("JSON.stringify({ok:(window.cloudAudit.uninstallHook(),true)})");
    fake.respond({ result: { type: "string", value: JSON.stringify({ ok: true }) } });
    await expect(pending).resolves.toEqual({ ok: true });
  });

  it("rejects navigator drains with a typed non-retryable failure", async () => {
    const app = new CoreApp(new Engine(offlineRuntime));

    const request: RpcRequest = { id: 10, method: "hook.drain", params: { name: "navigator", afterSeq: 0, limit: 10 } };
    const failure = await app.handle(request).catch((error: unknown) => error) as { retryable?: boolean; code?: number; message?: string };
    expect(failure.code).toBe(1000);
    expect(failure.retryable).toBe(false);
    expect(failure.message).toContain("navigator");
  });

  it("fails retryably when the hook script cannot be loaded", async () => {
    const bridge = new CdpBridge();
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge, async () => {
      throw new Error("ENOENT: no such file");
    });
    const fake = connectFakeMiniapp(bridge);

    const request: RpcRequest = { id: 11, method: "hook.install", params: { name: "wxapi" } };
    const failure = await app.handle(request).catch((error: unknown) => error) as { retryable?: boolean; code?: number; message?: string };
    expect(failure.retryable).toBe(true);
    expect(failure.message).toContain("failed to load hook script wxapi");
    expect(fake.sent.length).toBe(0);
  });
});

describe("CoreApp evaluate error propagation", () => {
  it("fails runtime.evaluate when the page throws instead of returning null", async () => {
    const bridge = new CdpBridge();
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge);
    const fake = connectFakeMiniapp(bridge);

    const pending = app.handle({ id: 3, method: "runtime.evaluate", params: { expression: "window.boom()", timeoutMs: 500 } });
    // The page-side promise rejects: CDP surfaces exceptionDetails, no value.
    fake.respond({ result: { type: "object", subtype: "error" }, exceptionDetails: { exception: { description: "Error: boom" } } });
    await expect(pending).rejects.toThrow("Error: boom");
  });

  it("reports the page generation through engine.status", async () => {
    const bridge = new CdpBridge({ probeDelayMs: 60_000 });
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge);
    const fake = connectFakeMiniapp(bridge);
    bridge.receiveMiniapp(encodeCdpMessage({
      sequence: 1,
      category: "setupContext",
      operationId: 0,
      payload: "{}",
      jsContextId: "",
    }), fake.clientId);

    await expect(app.handle({ id: 29, method: "engine.status", params: {} })).resolves.toMatchObject({ generation: 1 });
  });
});

describe("CoreApp hook.drain limits", () => {
  it("rejects a non-positive drain limit instead of looping forever", async () => {
    const bridge = new CdpBridge();
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge);
    const fake = connectFakeMiniapp(bridge);

    const pending = app.handle({ id: 4, method: "hook.drain", params: { name: "wxapi", afterSeq: 0, limit: 0 } });
    const bound = pending.then(() => fake, () => fake);
    await expect(pending).rejects.toThrow();
    await bound;
  });
});

describe("CoreApp input validation", () => {
  it("rejects an invalid cdpPort instead of starting with a bogus port", async () => {
    const app = new CoreApp(new Engine(onlineRuntime()));
    for (const cdpPort of [0, -1, 70000, 1.5, "62000", null]) {
      await expect(app.handle({ id: 90, method: "engine.start", params: { cdpPort } })).rejects.toThrow(
        "cdpPort must be a valid TCP port",
      );
    }
  });

  it("rejects hook names outside the contract", async () => {
    const app = new CoreApp(new Engine(onlineRuntime()));
    await expect(
      app.handle({ id: 91, method: "hook.install", params: { name: "unknown" } }),
    ).rejects.toThrow(/hook name must be one of/);
    await expect(app.handle({ id: 92, method: "hook.install", params: {} })).rejects.toThrow(
      /hook name must be one of/,
    );
  });

  it("rejects malformed client ids and lock flags", async () => {
    // miniapp.* gate on the bridge first (documented contract), so provide
    // one before exercising the id/lock validation.
    const bridge = new CdpBridge();
    connectFakeMiniapp(bridge);
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge);
    for (const id of [0, -3, 2.5, "1", null]) {
      await expect(app.handle({ id: 93, method: "miniapp.switch", params: { id } })).rejects.toThrow(
        "id must be a positive integer",
      );
    }
    await expect(
      app.handle({ id: 94, method: "miniapp.setLock", params: { enabled: "true" } }),
    ).rejects.toThrow("enabled must be a boolean");
  });

  it("starts with a valid port and stops through the engine", async () => {
    const app = new CoreApp(new Engine(onlineRuntime()));

    // Engine.start reports the Frida attach result; the connection flags come
    // from engine.status.
    await expect(app.handle({ id: 96, method: "engine.start", params: { cdpPort: 62000 } })).resolves.toEqual({
      frida: false,
    });
    await expect(app.handle({ id: 97, method: "engine.stop", params: {} })).resolves.toEqual({});
  });

  it("rejects a CDP command without a method and an evaluate without an expression", async () => {
    const app = new CoreApp(new Engine(onlineRuntime()), () => new CdpBridge());

    await expect(app.handle({ id: 98, method: "cdp.command", params: {} })).rejects.toThrow(
      "cdp.command requires a method",
    );
    await expect(app.handle({ id: 99, method: "runtime.evaluate", params: {} })).rejects.toThrow(
      "runtime.evaluate requires an expression",
    );
  });

  it("reports navigator hook operations as unsupported", async () => {
    const app = new CoreApp(new Engine(onlineRuntime()));

    await expect(app.handle({ id: 100, method: "hook.clear", params: { name: "navigator" } })).rejects.toThrow(
      "hook navigator does not support clear",
    );
    await expect(app.handle({ id: 101, method: "hook.uninstall", params: { name: "navigator" } })).rejects.toThrow(
      "hook navigator does not support uninstall",
    );
  });

  it("surfaces a missing hook loader instead of a silent no-op install", async () => {
    const bridge = new CdpBridge();
    connectFakeMiniapp(bridge);
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge);

    await expect(app.handle({ id: 102, method: "hook.install", params: { name: "wxapi" } })).rejects.toThrow(
      "hook script loader unavailable",
    );
  });

  it("rejects hook responses that are not JSON objects the shell can read", async () => {
    const bridge = new CdpBridge();
    const fake = connectFakeMiniapp(bridge);
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge, async () => "void 0;");

    const missing = app.handle({ id: 103, method: "hook.drain", params: { name: "wxapi", afterSeq: 0, limit: 1 } });
    fake.respond({});
    await expect(missing).rejects.toThrow("hook expression returned no value");

    const invalid = app.handle({ id: 104, method: "hook.drain", params: { name: "wxapi", afterSeq: 0, limit: 1 } });
    fake.respond({ result: { type: "string", value: "{not json" } });
    await expect(invalid).rejects.toThrow("hook returned invalid JSON");
  });

  it("surfaces page-side hook exceptions with their description", async () => {
    const bridge = new CdpBridge();
    const fake = connectFakeMiniapp(bridge);
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge, async () => "void 0;");

    const pending = app.handle({ id: 105, method: "hook.drain", params: { name: "wxapi", afterSeq: 0, limit: 1 } });
    fake.respond({ exceptionDetails: { exception: { description: "boom in page" } } });
    await expect(pending).rejects.toThrow("boom in page");
  });
  it("rejects non-string format content", async () => {
    const app = new CoreApp(new Engine(onlineRuntime()));
    await expect(app.handle({ id: 95, method: "code.format", params: { content: 42 } })).rejects.toThrow(
      "content must be a string",
    );
  });
});

describe("CoreApp cdp.debug", () => {
  it("returns the debug snapshot and enables the debugger on demand", async () => {
    const bridge = new CdpBridge();
    const fake = connectFakeMiniapp(bridge);
    const app = new CoreApp(new Engine(onlineRuntime()), () => bridge);

    const snapshot = await app.handle({ id: 30, method: "cdp.debug", params: { enable: true } }) as { enabled: boolean; paused: boolean };
    expect(snapshot.enabled).toBe(true);
    expect(snapshot.paused).toBe(false);

    const methods = fake.sent.map((frame) => {
      const payload = (typeof frame === "string" ? undefined : decodeCdpMessage(frame))?.payload ?? "{}";
      return (JSON.parse(payload) as { method?: string }).method ?? "";
    });
    expect(methods).toContain("Debugger.enable");
  });

  it("rejects cdp.debug before the engine has a bridge", async () => {
    const app = new CoreApp(new Engine(offlineRuntime));
    await expect(app.handle({ id: 31, method: "cdp.debug", params: {} })).rejects.toThrow("engine not started");
  });
});
