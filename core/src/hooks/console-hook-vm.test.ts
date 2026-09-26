import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { createContext, runInContext } from "node:vm";
import { describe, expect, it, vi } from "vitest";

// The page-side hooks ship as hand-written ES5 loaded verbatim into the
// miniapp, so they cannot be imported: run them in a VM with a fake console to
// verify the shipped file, not a reimplementation of it.
const hookSource = readFileSync(
  fileURLToPath(new URL("../../hooks/console.js", import.meta.url)),
  "utf8",
);

type DrainedRecord = { seq: number; record: Record<string, unknown> };
// Cumulative page-side evictions (R12); the shell adds them to its own `dropped`
// so an overload is visible instead of silent. Same field cloud-hook-vm's
// TestHookApi carries.
type DrainPage = { records: DrainedRecord[]; nextSeq: number; hasMore: boolean; droppedRecords: number };

type HookApi = {
  install(): { ok: boolean; hookedConsoles: number; levels: number; appId: string };
  uninstallHook(): { ok: boolean };
  drain(afterSeq: number, limit: number): DrainPage;
  clearHookedCalls(): void;
};

type FakeConsole = Record<string, unknown> & {
  log: (...args: unknown[]) => void;
  warn: (...args: unknown[]) => void;
};

function createHarness() {
  const printed: string[] = [];
  const errorHandlers: Array<(event: unknown) => void> = [];
  const fakeConsole: FakeConsole = {
    log: (...args: unknown[]) => { printed.push(`log:${args.map(String).join(" ")}`); },
    info: () => {},
    warn: () => {},
    error: () => {},
    debug: () => {},
  };
  const sandbox: Record<string, unknown> = {};
  sandbox.window = sandbox;
  sandbox.console = fakeConsole;
  sandbox.__wxConfig = { accountInfo: { appAccount: { appId: "wxconsole" } } };
  sandbox.addEventListener = (event: string, handler: (payload: unknown) => void) => {
    if (event === "unhandledrejection") errorHandlers.push(handler);
  };
  createContext(sandbox);
  runInContext(hookSource, sandbox);

  const audit = sandbox.consoleAudit as HookApi;
  return { audit, printed, errorHandlers, sandbox, fakeConsole };
}

describe("console hook (shipped core/hooks/console.js)", () => {
  it("captures each level with its text and the page appid", () => {
    const { audit, fakeConsole } = createHarness();
    expect(audit.install()).toMatchObject({ ok: true, hookedConsoles: 1, errorsHooked: true, appId: "wxconsole" });

    (fakeConsole.error as (...args: unknown[]) => void)("boom", 500);
    const page = audit.drain(0, 10);
    expect(page.records).toHaveLength(1);
    expect(page.records[0].record).toMatchObject({
      type: "console",
      level: "error",
      text: "boom 500",
      appId: "wxconsole",
    });
    expect(page.records[0].seq).toBe(1);
    expect(page.nextSeq).toBe(1);
    expect(page.hasMore).toBe(false);
  });

  it("still calls the original console method", () => {
    const { audit, printed, fakeConsole } = createHarness();
    audit.install();
    (fakeConsole.log)("visible");
    expect(printed).toEqual(["log:visible"]);
  });

  it("installs only once: re-installing must not duplicate records", () => {
    const { audit, fakeConsole } = createHarness();
    audit.install();
    audit.install();
    (fakeConsole.log)("once");
    // The wrapper records before delegating, so a double wrap would show two
    // records for one call.
    expect(audit.drain(0, 10).records).toHaveLength(1);
  });

  it("drains with afterSeq/limit and reports hasMore", () => {
    const { audit, fakeConsole } = createHarness();
    audit.install();
    const log = fakeConsole.log;
    log("a");
    log("b");
    log("c");

    const first = audit.drain(0, 2);
    expect(first.records.map((entry) => entry.record.text)).toEqual(["a", "b"]);
    expect(first.nextSeq).toBe(2);
    expect(first.hasMore).toBe(true);

    const second = audit.drain(first.nextSeq, 2);
    expect(second.records.map((entry) => entry.record.text)).toEqual(["c"]);
    expect(second.hasMore).toBe(false);
  });

  it("clears the buffer without resetting the sequence", () => {
    const { audit, fakeConsole } = createHarness();
    audit.install();
    (fakeConsole.log)("gone");
    audit.clearHookedCalls();
    expect(audit.drain(0, 10).records).toEqual([]);
    (fakeConsole.log)("kept");
    const page = audit.drain(0, 10);
    expect(page.records.map((entry) => entry.record.text)).toEqual(["kept"]);
    expect(page.records[0].seq).toBe(2);
  });

  it("restores the original console methods on uninstall", () => {
    const { audit, fakeConsole } = createHarness();
    audit.install();
    const wrapped = fakeConsole.log;
    audit.uninstallHook();
    expect(fakeConsole.log).not.toBe(wrapped);
    (fakeConsole.log)("after uninstall");
    expect(audit.drain(0, 10).records).toEqual([]);
  });

  it("formats objects, errors and circular references without throwing", () => {
    const { audit, fakeConsole } = createHarness();
    audit.install();
    const circular: Record<string, unknown> = { name: "loop" };
    circular.self = circular;
    (fakeConsole.log)(
      { a: 1 },
      new Error("kaboom"),
      circular,
      undefined,
      null,
    );

    const text = String(audit.drain(0, 10).records[0].record.text);
    expect(text).toContain('{"a":1}');
    expect(text).toContain("Error: kaboom");
    expect(text).toContain("[无法序列化]");
    expect(text).toContain("undefined null");
  });

  it("captures uncaught errors and unhandled rejections as error records", () => {
    const { audit, errorHandlers, sandbox } = createHarness();
    audit.install();
    // Assigning through the sandbox reaches the same window object the hook
    // wrapped, so onerror behaves like the page's own handler.
    const onerror = sandbox.onerror as (message: string, source: string, line: number, column: number, error: Error) => void;
    onerror("uncaught!", "app.js", 12, 3, new Error("inner"));
    errorHandlers[0]?.({ reason: new Error("rejected") });

    const records = audit.drain(0, 10).records.map((entry) => entry.record);
    expect(records).toHaveLength(2);
    expect(records[0]).toMatchObject({ level: "error" });
    expect(String(records[0].text)).toContain("uncaught!");
    expect(records[0].extra).toMatchObject({ source: "app.js", line: "12" });
    expect(String(records[1].text)).toContain("Unhandled rejection");
    expect(String(records[1].text)).toContain("rejected");
  });

  it("bounds a burst of records to the drain buffer capacity", () => {
    const { audit, fakeConsole } = createHarness();
    audit.install();
    const log = fakeConsole.log;
    for (let i = 0; i < 1200; i += 1) {
      log(`line-${i}`);
    }
    const page = audit.drain(0, 2000);
    expect(page.records).toHaveLength(1000);
    // The newest record survives; the oldest is evicted.
    expect(page.records[999].record.text).toBe("line-1199");
    expect(page.records[0].record.text).toBe("line-200");
    // 与 wxapi.js 的 drain 契约一致：溢出必须报数，页内丢弃不能是无声缺口。
    expect(page.droppedRecords).toBe(200);
    // clear 随缓冲一起把计数归零（计数描述的是"缓冲里曾有什么"）。
    audit.clearHookedCalls();
    expect(audit.drain(0, 10).droppedRecords).toBe(0);
  });
  // vConsole 这类库会替换 console 方法：只认「对象标记」的实现会从此永久停采。
  it("re-wraps console methods another library replaced", () => {
    const { audit, fakeConsole } = createHarness();
    audit.install();

    const theirs = vi.fn();
    (fakeConsole as Record<string, unknown>).log = theirs;
    audit.install();

    (fakeConsole.log)("after replace");
    expect(audit.drain(0, 10).records.map((entry) => entry.record.text)).toEqual(["after replace"]);
    expect(theirs).toHaveBeenCalled();
  });

  // 真机现象：安装之后小程序运行时把 console.log 换成了自己的实现，采集静默停止。
  // 访问器方案必须在「对方替换、我们没有重新 install」时也能继续记录。
  it("keeps capturing when console methods are replaced after install", () => {
    const { audit, fakeConsole } = createHarness();
    audit.install();

    const theirs = vi.fn();
    (fakeConsole as Record<string, unknown>).log = theirs;
    (fakeConsole.log)("replaced later");

    expect(audit.drain(0, 10).records.map((entry) => entry.record.text)).toEqual(["replaced later"]);
    expect(theirs).toHaveBeenCalledWith("replaced later");
  });

  // 还原必须把普通属性放回去，而不是再包一层（否则会重复记录）。
  it("restores a plain method on uninstall and records nothing afterwards", () => {
    const { audit, fakeConsole } = createHarness();
    audit.install();
    audit.uninstallHook();

    const descriptor = Object.getOwnPropertyDescriptor(fakeConsole, "log");
    expect(typeof descriptor?.value).toBe("function");
    (fakeConsole.log)("after uninstall");
    expect(audit.drain(0, 10).records).toEqual([]);
  });

  // 真机形状：window.console 是访问器、console.log 也是访问器（形如 ()=>o.value），
  // 小程序用 Object.defineProperty 换掉它时不经过我们的 setter。必须在读取
  // window.console 时兜底重包，否则采集会静默停止。
  it("keeps capturing when the page re-defines console through accessors", () => {
    const printed: string[] = [];
    const inner: Record<string, unknown> = { log: (...args: unknown[]) => { printed.push(args.join(" ")); } };
    const consoleObject: Record<string, unknown> = {};
    Object.defineProperty(consoleObject, "log", { configurable: true, get: () => inner.log });

    const sandbox: Record<string, unknown> = {};
    sandbox.window = sandbox;
    Object.defineProperty(sandbox, "console", { configurable: true, get: () => consoleObject });
    sandbox.__wxConfig = { accountInfo: { appAccount: { appId: "wxconsole" } } };
    createContext(sandbox);
    runInContext(hookSource, sandbox);
    const audit = sandbox.consoleAudit as HookApi;

    expect(audit.install()).toMatchObject({ ok: true });
    (sandbox.console as { log: (value: string) => void }).log("first");

    // 对方重新定义（绕过我们的 setter）。
    Object.defineProperty(consoleObject, "log", { configurable: true, get: () => inner.log });
    (sandbox.console as { log: (value: string) => void }).log("second");

    const texts = audit.drain(0, 10).records.map((entry) => entry.record.text);
    expect(texts).toEqual(["first", "second"]);
    expect(printed).toEqual(["first", "second"]);
  });

  // 真机形状之二：console 是 configurable:false 的访问器 —— 既换不掉也包不上。
  // 这时必须如实回 ok:false（界面据此提示"没采到"），而不是谎报装好了。
  it("reports ok=false when the page locks its console behind a non-configurable accessor", () => {
    const printed: string[] = [];
    const inner = { log: (...args: unknown[]) => { printed.push(args.join(" ")); } };
    const consoleObject: Record<string, unknown> = {};
    Object.defineProperty(consoleObject, "log", {
      configurable: false,
      enumerable: true,
      get: () => inner.log,
      set: () => { /* 对方的 setter 吞掉赋值 */ },
    });
    Object.defineProperty(consoleObject, "info", { configurable: false, enumerable: true, get: () => () => {}, set: () => {} });
    Object.defineProperty(consoleObject, "warn", { configurable: false, enumerable: true, get: () => () => {}, set: () => {} });
    Object.defineProperty(consoleObject, "error", { configurable: false, enumerable: true, get: () => () => {}, set: () => {} });
    Object.defineProperty(consoleObject, "debug", { configurable: false, enumerable: true, get: () => () => {}, set: () => {} });

    const sandbox: Record<string, unknown> = {};
    sandbox.window = sandbox;
    sandbox.console = consoleObject;
    createContext(sandbox);
    runInContext(hookSource, sandbox);
    const audit = sandbox.consoleAudit as HookApi;

    const report = audit.install() as { ok: boolean; hookedConsoles: number; unwrappableConsoles?: number; errorsHooked?: boolean };
    expect(report.ok).toBe(false);
    expect(report.hookedConsoles).toBe(0);
    expect(report.unwrappableConsoles).toBe(1);
    // console 被锁只影响 console.*：错误出口照常挂上了，这个布尔是 CDP 事件源
    // 对异常事件去重的依据（ok:false + errorsHooked:true 是 WMPF 的常态形状）。
    expect(report.errorsHooked).toBe(true);
    // 页面自己的日志照常输出，只是我们收不到 —— 这正是要如实报出来的情况。
    (sandbox.console as { log: (value: string) => void }).log("invisible");
    expect(printed).toEqual(["invisible"]);
    expect(audit.drain(0, 10).records).toEqual([]);
  });

  it("can be installed again after uninstallHook", () => {
    const { audit, fakeConsole } = createHarness();
    audit.install();
    audit.uninstallHook();

    expect(audit.install()).toMatchObject({ ok: true, hookedConsoles: 1 });
    (fakeConsole.log)("after reinstall");
    expect(audit.drain(0, 10).records.map((entry) => entry.record.text)).toEqual(["after reinstall"]);
  });

  // install 必须报真实结果：页面里没有可包装的 console 时不能报成功，否则界面
  // 显示「捕获中」却永远零记录。
  it("reports ok=false when there is no console to wrap", () => {
    const sandbox: Record<string, unknown> = {};
    sandbox.window = sandbox;
    sandbox.console = {};
    sandbox.__wxConfig = { accountInfo: { appAccount: { appId: "wxconsole" } } };
    createContext(sandbox);
    runInContext(hookSource, sandbox);

    const audit = sandbox.consoleAudit as HookApi;
    expect(audit.install()).toMatchObject({ ok: false, hookedConsoles: 0 });
  });
});
