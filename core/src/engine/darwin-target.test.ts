import { describe, expect, it } from "vitest";

import {
  findMacWmpfTarget,
  MAC_WMPF_INFO_PLIST,
  parseMacWmpfVersion,
  type ProcessInfo,
} from "./darwin-target.js";

function process(pid: number, name: string, ppid?: number, path?: string): ProcessInfo {
  const parameters: Record<string, unknown> = {};
  if (path !== undefined) parameters.path = path;
  if (ppid !== undefined) parameters.ppid = ppid;
  return { pid, name, parameters };
}

/** A plist that lists CFBundleShortVersionString first, the way real ones do. */
function infoPlist(version: string): string {
  return [
    '<?xml version="1.0" encoding="UTF-8"?>',
    '<plist version="1.0"><dict>',
    "<key>CFBundleShortVersionString</key>",
    "<string>4.1.0</string>",
    "<key>CFBundleVersion</key>",
    `<string>${version}</string>`,
    "</dict></plist>",
  ].join("\n");
}

const HOST_PATH = "/Applications/WeChat.app/Contents/MacOS/WeChatAppEx.app/Contents/MacOS/WeChatAppEx";

/** The host (pid 10) with two of its helpers, as Frida reports them. */
function runningWeChat(): ProcessInfo[] {
  return [
    process(31, "WeChatAppEx Helper", 10, "/Applications/WeChat.app/Contents/MacOS/WeChatAppEx.app/Contents/Frameworks/WeChatAppEx Helper.app/Contents/MacOS/WeChatAppEx Helper"),
    process(32, "WeChatAppEx Helper", 10),
    process(10, "WeChatAppEx", 1, HOST_PATH),
  ];
}

describe("findMacWmpfTarget", () => {
  it("takes the helper's parent as the host and the build number from Info.plist", async () => {
    const target = await findMacWmpfTarget(runningWeChat(), async (path) => {
      expect(path).toBe(MAC_WMPF_INFO_PLIST);
      return infoPlist("269136");
    });

    expect(target).toEqual({ pid: 10, version: 269136 });
  });

  it("throws when no WMPF host is running", async () => {
    // WeChat itself runs from WeChat.app/Contents/MacOS/WeChat; only the WMPF
    // runtime runs from the nested WeChatAppEx.app, which is what the host
    // match looks for.
    await expect(
      findMacWmpfTarget([process(1, "WeChat", 0, "/Applications/WeChat.app/Contents/MacOS/WeChat")]),
    ).rejects.toThrow("未找到微信 WMPF 宿主进程（WeChatAppEx / WeChatAppEx Helper）");
  });

  // The host is located by the bundle it runs from, the way a working macOS port
  // does it (`pgrep -f '/MacOS/WeChatAppEx.app/Contents/MacOS/WeChatAppEx'`).
  // Assuming the Helpers' parent IS the host was this project's own unverified
  // guess, so the main process wins whenever it is visible.
  it("prefers the main WeChatAppEx process over the Helpers' parent", async () => {
    const target = await findMacWmpfTarget(
      [
        process(31, "WeChatAppEx Helper", 99),
        // The Helpers' parent here is WeChat itself, not the WMPF host: taking
        // it would attach to a process with no WMPF framework to hook.
        process(99, "WeChat", 1, "/Applications/WeChat.app/Contents/MacOS/WeChat"),
        process(10, "WeChatAppEx", 1, HOST_PATH),
      ],
      async () => infoPlist("269136"),
    );

    expect(target).toEqual({ pid: 10, version: 269136 });
  });

  it("finds the host by its bundle with no Helper process listed", async () => {
    const target = await findMacWmpfTarget(
      [process(10, "WeChatAppEx", 1, HOST_PATH)],
      async () => infoPlist("269136"),
    );

    expect(target).toEqual({ pid: 10, version: 269136 });
  });

  it("falls back to the name when the host reports no path", async () => {
    const target = await findMacWmpfTarget([process(10, "WeChatAppEx")], async () => infoPlist("269136"));

    expect(target).toEqual({ pid: 10, version: 269136 });
  });

  it("reports when helpers expose no parent process id", async () => {
    await expect(findMacWmpfTarget([process(31, "WeChatAppEx Helper")])).rejects.toThrow(
      "微信 WMPF 宿主进程信息不完整（拿不到父进程）",
    );
  });

  it("refuses to guess when two WeChat instances are running", async () => {
    await expect(
      findMacWmpfTarget([
        process(31, "WeChatAppEx Helper", 10),
        process(41, "WeChatAppEx Helper", 20),
        process(10, "WeChatAppEx", 1, HOST_PATH),
        process(20, "WeChatAppEx", 1, HOST_PATH),
      ]),
    ).rejects.toThrow("发现多个微信 WMPF 宿主进程");
  });

  it("reports when the helper's parent is missing from the process list", async () => {
    await expect(findMacWmpfTarget([process(31, "WeChatAppEx Helper", 10)])).rejects.toThrow(
      "未能定位微信主进程",
    );
  });

  // A per-user WeChat in ~/Applications is a normal macOS layout, and reading
  // only the hardcoded /Applications path made the engine unstartable there even
  // though the host process had been found.
  it("reads the plist from a per-user install instead of only /Applications", async () => {
    const userHostPath =
      "/Users/someone/Applications/WeChat.app/Contents/MacOS/WeChatAppEx.app/Contents/MacOS/WeChatAppEx";
    const tried: string[] = [];
    const target = await findMacWmpfTarget([process(10, "WeChatAppEx", 1, userHostPath)], async (path) => {
      tried.push(path);
      if (!path.startsWith("/Users/someone/")) throw new Error("ENOENT: no such file or directory");
      return infoPlist("269136");
    });

    expect(target).toEqual({ pid: 10, version: 269136 });
    expect(tried).toEqual([
      MAC_WMPF_INFO_PLIST,
      "/Users/someone/Applications/WeChat.app/Contents/MacOS/WeChatAppEx.app/Contents/Info.plist",
    ]);
  });

  it("reports an unreadable Info.plist instead of leaking the fs error", async () => {
    const failure: unknown = await findMacWmpfTarget(runningWeChat(), async () => {
      throw new Error("ENOENT: no such file or directory");
    }).then(
      () => undefined,
      (reason: unknown) => reason,
    );
    const message = failure instanceof Error ? failure.message : String(failure);

    expect(message).toContain("读不到微信 WMPF 版本信息");
    expect(message).toContain(MAC_WMPF_INFO_PLIST);
    expect(message).toContain("ENOENT");
  });

  // Once a plist has been read its contents are the answer: a bad build number
  // is that plist's problem, not a reason to go looking for another bundle.
  it("surfaces a bad CFBundleVersion as itself, not as an unreadable plist", async () => {
    await expect(findMacWmpfTarget(runningWeChat(), async () => infoPlist("abc"))).rejects.toThrow(
      "CFBundleVersion=abc",
    );
  });
});

describe("parseMacWmpfVersion", () => {
  it("reads CFBundleVersion and not the shorter version string beside it", () => {
    expect(parseMacWmpfVersion(infoPlist("269136"))).toBe(269136);
  });

  it("reports a missing CFBundleVersion", () => {
    expect(() => parseMacWmpfVersion("<plist><dict></dict></plist>")).toThrow(
      "CFBundleVersion=缺失",
    );
  });

  // The macOS port this project's SIP note came from reads the build out of a
  // dotted CFBundleVersion ("4.18788.<patch>"), while upstream WMPFDebugger
  // hands the whole string to Number() and its own commit calls macOS support
  // "not working yet". Neither form can be assumed, so both have to parse.
  it("reads the build out of a dotted CFBundleVersion", () => {
    expect(parseMacWmpfVersion(infoPlist("4.269136.0"))).toBe(269136);
    expect(parseMacWmpfVersion(infoPlist("4.18788"))).toBe(18788);
    expect(parseMacWmpfVersion(infoPlist("269136.0"))).toBe(269136);
  });

  it("reports a CFBundleVersion carrying no build number", () => {
    expect(() => parseMacWmpfVersion(infoPlist("abc"))).toThrow("CFBundleVersion=abc");
    expect(() => parseMacWmpfVersion(infoPlist("0"))).toThrow("CFBundleVersion=0");
  });
});
