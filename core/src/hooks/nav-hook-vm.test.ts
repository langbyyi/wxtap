import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { createContext, runInContext } from "node:vm";
import { describe, expect, it } from "vitest";

// hooks/nav.js ships verbatim into the miniapp page; run the shipped file in a
// VM so the page/config contract the Core reads is verified, not assumed.
const hookSource = readFileSync(
  fileURLToPath(new URL("../../hooks/nav.js", import.meta.url)),
  "utf8",
);

type NavFrame = {
  allPages: string[];
  tabBarPages: string[];
  config: Record<string, unknown> | null;
  wxFrame: unknown;
  current(): string;
  pageStack(): Array<{ route: string; params: Record<string, string> }>;
  getBlockedRedirects(): unknown[];
  isRedirectGuardOn(): boolean;
};

type Config = {
  pages?: string[];
  subPackages?: Array<{ root: string; pages: string[] }>;
  tabBar?: { list: Array<{ pagePath: string }> };
  accountInfo?: { appId: string };
};

type StackPage = { route?: string; __route__?: string; options?: unknown };
// 页内导航动作的结果形状：Go 侧用 awaitPromise 取它。
type NavOutcome = { ok: boolean; err: string; route?: string };

function runNav(
  config?: Config,
  options: { withEnvironment?: boolean; failMethods?: string[]; stack?: StackPage[] } = {},
) {
  const withEnvironment = options.withEnvironment ?? true;
  const sandbox: Record<string, unknown> = {};
  const calls: string[] = [];
  const failMethods = new Set(options.failMethods ?? []);
  const respond = (name: string, opts?: { url?: string; delta?: number; success?: () => void; fail?: () => void }) => {
    calls.push(`${name}:${opts?.url ?? opts?.delta ?? ""}`);
    if (failMethods.has(name)) opts?.fail?.();
    else opts?.success?.();
    return undefined;
  };
  const wx = {
    navigateTo: (opts: { url?: string; success?: () => void; fail?: () => void }) => respond("navigateTo", opts),
    redirectTo: (opts: { url?: string; success?: () => void; fail?: () => void }) => respond("redirectTo", opts),
    reLaunch: (opts: { url?: string; success?: () => void; fail?: () => void }) => respond("reLaunch", opts),
    switchTab: (opts: { url?: string; success?: () => void; fail?: () => void }) => respond("switchTab", opts),
    navigateBack: (opts: { delta?: number; success?: () => void; fail?: () => void }) => respond("navigateBack", opts),
  };
  sandbox.window = sandbox;
  sandbox.console = { error: () => undefined, log: () => undefined, warn: () => undefined };
  if (config) sandbox.__wxConfig = config;
  if (withEnvironment) {
    sandbox.wx = wx;
    const stack = options.stack ?? [{ route: "pages/index" }];
    sandbox.getCurrentPages = () => stack;
  }
  createContext(sandbox);
  runInContext(hookSource, sandbox);
  return { nav: sandbox.nav as NavFrame, wx, calls };
}

describe("hooks/nav.js page configuration", () => {
  it("exposes main, sub-package and tab bar pages from __wxConfig", () => {
    const { nav } = runNav({
      pages: ["pages/index", "pages/detail", "pages/index"],
      subPackages: [{ root: "pkgA", pages: ["list", "pages/list"] }],
      tabBar: { list: [{ pagePath: "pages/index" }, { pagePath: "pages/user.html" }] },
      accountInfo: { appId: "wxone" },
    });

    expect(nav.allPages).toEqual([
      "pages/index",
      "pages/detail",
      "pkgA/list",
      "pkgA/pages/list",
    ]);
    expect(nav.tabBarPages).toEqual(["pages/index", "pages/user"]);
  });

  it("reports the current route from getCurrentPages", () => {
    const { nav } = runNav({ pages: ["pages/index"], accountInfo: { appId: "wxone" } });

    expect(nav.current()).toBe("pages/index");
  });

  it("reports the whole runtime page stack with each page's query params", () => {
    const { nav } = runNav(
      { pages: ["pages/index"], accountInfo: { appId: "wxone" } },
      {
        stack: [
          { route: "pages/index", options: { from: "tab" } },
          { route: "pages/detail", options: { id: 42, nested: { deep: true }, list: [1, 2] } },
        ],
      },
    );

    // Only scalars survive: a nested object or array has no place in a
    // drained payload, so it is dropped rather than stringified.
    expect(nav.pageStack()).toEqual([
      { route: "pages/index", params: { from: "tab" } },
      { route: "pages/detail", params: { id: "42" } },
    ]);
  });

  it("falls back to __route__ and an empty params object on sparse entries", () => {
    const { nav } = runNav(
      { pages: ["pages/index"] },
      { stack: [{}, { __route__: "pages/profile", options: "not-an-object" }] },
    );

    expect(nav.pageStack()).toEqual([
      { route: "", params: {} },
      { route: "pages/profile", params: {} },
    ]);
  });

  // 没有小程序 frame 时初始化直接失败：留下一个 wxFrame=null 的 window.nav，
  // 之后每个导航调用都会在页面里抛难懂的 TypeError，而 Go 侧还以为 hook 装好了。
  it("refuses to install without a mini program frame", () => {
    expect(() => runNav({ pages: ["pages/index"] }, { withEnvironment: false }))
      .toThrow("未检测到小程序环境");
  });

  it("does not leave a half-built navigator behind when installation fails", () => {
    const sandbox: Record<string, unknown> = {};
    sandbox.window = sandbox;
    sandbox.console = { error: () => undefined, log: () => undefined, warn: () => undefined };
    createContext(sandbox);

    expect(() => runInContext(hookSource, sandbox)).toThrow("未检测到小程序环境");
    expect(sandbox.nav).toBeUndefined();
  });

  it("falls back through switchTab and redirectTo when reLaunch fails", async () => {
    const first = runNav({ pages: ["pages/a"] }, { failMethods: ["reLaunch"] });
    const safe = first.nav as unknown as { _safeNavigate(url: string): Promise<NavOutcome> };

    await expect(safe._safeNavigate("pages/b")).resolves.toEqual({ ok: true, err: "" });
    expect(first.calls).toEqual(["reLaunch:/pages/b", "switchTab:/pages/b"]);

    const second = runNav({ pages: ["pages/a"] }, { failMethods: ["reLaunch", "switchTab"] });
    const safeSecond = second.nav as unknown as { _safeNavigate(url: string): Promise<NavOutcome> };
    await expect(safeSecond._safeNavigate("/pages/c")).resolves.toMatchObject({ ok: true });
    expect(second.calls).toEqual(["reLaunch:/pages/c", "switchTab:/pages/c", "redirectTo:/pages/c"]);
  });

  // 失败必须能被 Go 看到：以前 resolve(false) 没人读，界面照样提示「已跳转」，
  // 自动遍历也永远计不到失败页。
  it("reports the failure when every navigation strategy fails", async () => {
    const { nav, calls } = runNav({ pages: ["pages/a"] }, { failMethods: ["reLaunch", "switchTab", "redirectTo"] });
    const safe = nav as unknown as { _safeNavigate(url: string): Promise<NavOutcome> };

    const outcome = await safe._safeNavigate("pages/nowhere");
    expect(outcome.ok).toBe(false);
    expect(outcome.err).toContain("全部失败");
    expect(calls).toEqual(["reLaunch:/pages/nowhere", "switchTab:/pages/nowhere", "redirectTo:/pages/nowhere"]);
  });

  it("sends tab bar pages through switchTab and normal pages through navigateTo", async () => {
    const { nav, calls } = runNav({
      pages: ["pages/index", "pages/detail"],
      tabBar: { list: [{ pagePath: "pages/index" }] },
    });
    const navApi = nav as unknown as { goTo(url: string): Promise<NavOutcome> };

    await expect(navApi.goTo("/pages/index")).resolves.toMatchObject({ ok: true });
    await expect(navApi.goTo("pages/detail")).resolves.toMatchObject({ ok: true });

    expect(calls).toEqual(["switchTab:/pages/index", "navigateTo:/pages/detail"]);
  });

  // 守卫拦的是页面自己的跳转，不是工具自己的。goTo 以前对 switchTab 直连
  // wx.switchTab，守卫一开就把工具自己的切 tab 吃掉、还替它回了 success ——
  // Go 侧于是看到 ok 而页面根本没动。
  it("routes a tab bar goTo through the original switchTab while the guard is on", async () => {
    const { nav, calls } = runNav({
      pages: ["pages/index", "pages/detail"],
      tabBar: { list: [{ pagePath: "pages/index" }] },
    });
    const guard = nav as unknown as { enableRedirectGuard(): { ok: boolean } };
    const navApi = nav as unknown as { goTo(url: string): Promise<NavOutcome> };
    guard.enableRedirectGuard();

    await expect(navApi.goTo("/pages/index")).resolves.toMatchObject({ ok: true });
    expect(calls).toEqual(["switchTab:/pages/index"]);
  });

  it("switchTo always asks for switchTab and surfaces its failure", async () => {
    const ok = runNav({ pages: ["pages/index"] });
    const okApi = ok.nav as unknown as { switchTo(route: string): Promise<NavOutcome> };

    await expect(okApi.switchTo("pages/index")).resolves.toMatchObject({ ok: true });
    expect(ok.calls).toEqual(["switchTab:/pages/index"]);

    // 目标不是 tab 页时 wx 会回 fail：必须如实报错，不能退化成 navigateTo。
    const bad = runNav({ pages: ["pages/index"] }, { failMethods: ["switchTab"] });
    const badApi = bad.nav as unknown as { switchTo(route: string): Promise<NavOutcome> };

    const outcome = await badApi.switchTo("pages/detail");
    expect(outcome.ok).toBe(false);
    expect(outcome.err).toContain("switchTab");
  });

  it("reports a failed jump instead of claiming success", async () => {
    const { nav } = runNav(
      { pages: ["pages/index", "pages/detail"] },
      { failMethods: ["navigateTo"] },
    );
    const navApi = nav as unknown as { goTo(url: string): Promise<NavOutcome> };

    const outcome = await navApi.goTo("pages/detail");
    expect(outcome.ok).toBe(false);
    expect(outcome.err).toContain("navigateTo");
  });

  it("refreshes the current page with its query string and reports failure honestly", async () => {
    const { nav, calls } = runNav(
      { pages: ["pages/detail"] },
      { stack: [{ route: "pages/detail", options: { id: "42" } }], failMethods: ["reLaunch"] },
    );
    const navApi = nav as unknown as { refreshPage(): Promise<NavOutcome> };

    const outcome = await navApi.refreshPage();
    expect(outcome).toMatchObject({ ok: true, route: "pages/detail" });
    expect(calls).toEqual(["reLaunch:/pages/detail?id=42", "redirectTo:/pages/detail?id=42"]);

    const failing = runNav(
      { pages: ["pages/detail"] },
      { stack: [{ route: "pages/detail" }], failMethods: ["reLaunch", "redirectTo"] },
    );
    const failingApi = failing.nav as unknown as { refreshPage(): Promise<NavOutcome> };
    const failed = await failingApi.refreshPage();
    expect(failed.ok).toBe(false);
    expect(failed.err).toContain("reLaunch/redirectTo");
  });

  it("routes redirectTo through the original method while the guard is on", async () => {
    const { nav, calls } = runNav({ pages: ["pages/a"] });
    const guard = nav as unknown as { enableRedirectGuard(): { ok: boolean } };
    const navApi = nav as unknown as { redirectTo(route: string): Promise<NavOutcome> };
    guard.enableRedirectGuard();

    // 防跳转只拦页面自己的调用：我们自己的导航走原始方法，不该被拦掉。
    await expect(navApi.redirectTo("pages/b")).resolves.toMatchObject({ ok: true });
    expect(calls).toEqual(["redirectTo:/pages/b"]);
  });

  it("navigates back with the default delta", async () => {
    const { nav, calls } = runNav({ pages: ["pages/a"] });
    const navApi = nav as unknown as { back(delta?: number): Promise<NavOutcome> };

    await expect(navApi.back()).resolves.toMatchObject({ ok: true });
    await expect(navApi.back(3)).resolves.toMatchObject({ ok: true });

    expect(calls).toEqual(["navigateBack:1", "navigateBack:3"]);
  });
  it("blocks forced redirects while the guard is on", () => {
    const { nav, wx, calls } = runNav({ pages: ["pages/index"] });
    const guard = nav as unknown as { enableRedirectGuard(): { ok: boolean; already?: boolean } };

    expect(guard.enableRedirectGuard()).toEqual({ ok: true });
    expect(nav.isRedirectGuardOn()).toBe(true);

    let succeeded = false;
    (wx.redirectTo as unknown as (options: { url: string; success: () => void }) => void)({
      url: "/pages/forced",
      success: () => {
        succeeded = true;
      },
    });

    // The forced navigation is swallowed, the caller still settles (no hang).
    expect(calls).toEqual([]);
    expect(succeeded).toBe(true);
    expect(nav.getBlockedRedirects()).toEqual([
      { type: "redirectTo", url: "/pages/forced", time: expect.any(String) },
    ]);
  });

  it("does not double-wrap the guard when enabled twice", () => {
    const { nav, wx, calls } = runNav({ pages: ["pages/index"] });
    const guard = nav as unknown as { enableRedirectGuard(): { ok: boolean; already?: boolean } };

    guard.enableRedirectGuard();
    expect(guard.enableRedirectGuard()).toEqual({ ok: true, already: true });

    (wx.redirectTo as unknown as (options: { url: string }) => void)({ url: "/pages/forced" });

    expect(nav.getBlockedRedirects()).toHaveLength(1);
    expect(calls).toEqual([]);
  });

  it("restores the original navigation once the guard is disabled", () => {
    const { nav, wx, calls } = runNav({ pages: ["pages/index"] });
    const guard = nav as unknown as { enableRedirectGuard(): { ok: boolean }; disableRedirectGuard(): void };

    guard.enableRedirectGuard();
    guard.disableRedirectGuard();

    expect(nav.isRedirectGuardOn()).toBe(false);
    (wx.redirectTo as unknown as (options: { url: string }) => void)({ url: "/pages/allowed" });
    expect(calls).toEqual(["redirectTo:/pages/allowed"]);
  });
  it("starts with the redirect guard off", () => {
    const { nav } = runNav({ pages: ["pages/index"] });

    expect(nav.isRedirectGuardOn()).toBe(false);
    expect(nav.getBlockedRedirects()).toEqual([]);
  });
});