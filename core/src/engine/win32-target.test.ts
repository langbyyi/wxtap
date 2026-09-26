import { describe, expect, it } from "vitest";

import { findWindowsWmpfTarget } from "./win32-target.js";

describe("findWindowsWmpfTarget", () => {
  it("selects the current xwechat RadiumWMPF host under Weixin.exe", () => {
    expect(findWindowsWmpfTarget([
      { pid: 27264, name: "WeChatAppEx.exe", parameters: { ppid: 39484 } },
      {
        pid: 39484,
        name: "WeChatAppEx.exe",
        parameters: { ppid: 3320, path: "C:\\Users\\Lang\\AppData\\Roaming\\Tencent\\xwechat\\xplugin\\plugins\\RadiumWMPF\\25560\\extracted\\runtime\\WeChatAppEx.exe" },
      },
      { pid: 3320, name: "Weixin.exe", parameters: { path: "D:\\Tencent\\Weixin\\Weixin.exe" } },
    ])).toEqual({ pid: 39484, version: 25560 });
  });

  it("rejects an xwechat host whose Weixin.exe ancestor is outside Tencent's Weixin install path", () => {
    expect(() => findWindowsWmpfTarget([
      { pid: 31, name: "WeChatAppEx.exe", parameters: { ppid: 10 } },
      { pid: 10, name: "WeChatAppEx.exe", parameters: { ppid: 1, path: "C:\\Users\\Lang\\AppData\\Roaming\\Tencent\\xwechat\\xplugin\\plugins\\RadiumWMPF\\25560\\runtime\\WeChatAppEx.exe" } },
      { pid: 1, name: "Weixin.exe", parameters: { path: "D:\\OtherProduct\\Weixin.exe" } },
    ])).toThrow("未找到可验证的微信 WMPF 宿主");
  });

  it("selects the parent shared by WeChatAppEx workers and reads its WMPF version", () => {
    const target = findWindowsWmpfTarget([
      { pid: 31, name: "WeChatAppEx.exe", parameters: { ppid: 10 } },
      { pid: 32, name: "WeChatAppEx.exe", parameters: { ppid: 10 } },
      { pid: 10, name: "WeChatAppEx.exe", parameters: { ppid: 1, path: "C:\\WeChat\\WeChatAppEx\\14161\\WeChatAppEx.exe" } },
      { pid: 1, name: "WeChat.exe", parameters: { path: "C:\\WeChat\\WeChat.exe" } },
      { pid: 99, name: "not-wechat.exe", parameters: {} },
    ]);

    expect(target).toEqual({ pid: 10, version: 14161 });
  });

  it("rejects QQ Music's WMPF runtime even though it uses the WeChatAppEx executable name", () => {
    expect(() => findWindowsWmpfTarget([
      { pid: 17252, name: "WeChatAppEx.exe", parameters: { ppid: 22976 } },
      {
        pid: 22976,
        name: "WeChatAppEx.exe",
        parameters: {
          ppid: 1,
          path: "C:\\Users\\Lang\\AppData\\Roaming\\Tencent\\QQMusic\\wmpf\\2.2.3.465\\runtime\\WeChatAppEx.exe",
        },
      },
      { pid: 1, name: "QQMusic.exe", parameters: {} },
    ])).toThrow("未找到可验证的微信 WMPF 宿主");
  });

  it("rejects a WeChat-looking runtime when it is not descended from WeChat.exe", () => {
    expect(() => findWindowsWmpfTarget([
      { pid: 31, name: "WeChatAppEx.exe", parameters: { ppid: 10 } },
      { pid: 10, name: "WeChatAppEx.exe", parameters: { ppid: 1, path: "C:\\Tencent\\WeChat\\14161\\WeChatAppEx.exe" } },
      { pid: 1, name: "QQ.exe", parameters: { path: "C:\\Tencent\\QQ\\QQ.exe" } },
    ])).toThrow("未找到可验证的微信 WMPF 宿主");
  });

  it("selects WeChat when another Tencent WMPF family has more workers", () => {
    expect(findWindowsWmpfTarget([
      { pid: 31, name: "WeChatAppEx.exe", parameters: { ppid: 10 } },
      { pid: 32, name: "WeChatAppEx.exe", parameters: { ppid: 10 } },
      { pid: 10, name: "WeChatAppEx.exe", parameters: { ppid: 1, path: "C:\\Tencent\\QQMusic\\wmpf\\465\\WeChatAppEx.exe" } },
      { pid: 1, name: "QQMusic.exe", parameters: {} },
      { pid: 41, name: "WeChatAppEx.exe", parameters: { ppid: 20 } },
      { pid: 20, name: "WeChatAppEx.exe", parameters: { ppid: 2, path: "C:\\Tencent\\WeChat\\14161\\WeChatAppEx.exe" } },
      { pid: 2, name: "WeChat.exe", parameters: { path: "C:\\Tencent\\WeChat\\WeChat.exe" } },
    ])).toEqual({ pid: 20, version: 14161 });
  });

  it("rejects a host whose WeChat.exe ancestor belongs to another install root", () => {
    expect(() => findWindowsWmpfTarget([
      { pid: 31, name: "WeChatAppEx.exe", parameters: { ppid: 10 } },
      { pid: 10, name: "WeChatAppEx.exe", parameters: { ppid: 1, path: "C:\\Tencent\\WeChat\\14161\\WeChatAppEx.exe" } },
      { pid: 1, name: "WeChat.exe", parameters: { path: "D:\\OtherProduct\\WeChat.exe" } },
    ])).toThrow("未找到可验证的微信 WMPF 宿主");
  });

  it("ignores an orphaned third-party WMPF family when a verified WeChat family exists", () => {
    expect(findWindowsWmpfTarget([
      { pid: 31, name: "WeChatAppEx.exe", parameters: { ppid: 999 } },
      { pid: 41, name: "WeChatAppEx.exe", parameters: { ppid: 20 } },
      { pid: 20, name: "WeChatAppEx.exe", parameters: { ppid: 2, path: "C:\\Tencent\\WeChat\\14161\\WeChatAppEx.exe" } },
      { pid: 2, name: "WeChat.exe", parameters: { path: "C:\\Tencent\\WeChat\\WeChat.exe" } },
    ])).toEqual({ pid: 20, version: 14161 });
  });

  it("reports when workers expose no parent process id", () => {
    expect(() => findWindowsWmpfTarget([{ pid: 31, name: "WeChatAppEx.exe", parameters: {} }])).toThrow(
      "微信 WMPF 宿主进程信息不完整（拿不到父进程）：请重开小程序后重试",
    );
  });

  it("reports when the worker parent is missing from the process list", () => {
    expect(() => findWindowsWmpfTarget([{ pid: 31, name: "WeChatAppEx.exe", parameters: { ppid: 10 } }])).toThrow(
      "未能定位微信主进程：请确认微信已登录且未被安全软件隔离",
    );
  });

  it("reports when the host path carries no WMPF build number", () => {
    expect(() => findWindowsWmpfTarget([
      { pid: 31, name: "WeChatAppEx.exe", parameters: { ppid: 10 } },
      { pid: 10, name: "WeChatAppEx.exe", parameters: { ppid: 1, path: "C:\\WeChat\\WeChatAppEx.exe" } },
      { pid: 1, name: "WeChat.exe", parameters: { path: "C:\\WeChat\\WeChat.exe" } },
    ])).toThrow("无法从微信路径推断 WMPF 构建号");
  });
  it("reports when no WeChatAppEx process is available", () => {
    expect(() => findWindowsWmpfTarget([])).toThrow("未找到微信 WMPF 宿主进程（WeChatAppEx.exe）");
  });
});
