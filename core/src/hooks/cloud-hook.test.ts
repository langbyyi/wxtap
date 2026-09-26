import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import vm from "node:vm";

import { describe, expect, it } from "vitest";

describe("cloud hook static scanner", () => {
  it("preserves the appId field on discovered references", () => {
    const frame: Record<string, unknown> = {
      wx: { cloud: {} },
      __wxConfig: { accountInfo: { appId: "wxscan123" } },
      __wxAppCode__: {
        "app.js": `wx.cloud.callFunction({name:"hello",data:{id:1}});wx.cloud.database().collection("users").get()`,
      },
      frames: [],
    };
    frame.window = frame;
    frame.parent = frame;
    const context = {
      window: frame,
      globalThis: frame,
      setInterval: () => 1,
      clearInterval: () => {},
      setTimeout,
      console,
    };
    const scriptPath = resolve(dirname(fileURLToPath(import.meta.url)), "../../hooks/cloud.js");
    vm.runInNewContext(readFileSync(scriptPath, "utf8"), context);

    const audit = frame.cloudAudit as { scanCloudFunctions(): Array<Record<string, unknown>> };
    const result = audit.scanCloudFunctions();
    expect(result).toEqual(expect.arrayContaining([
      expect.objectContaining({ name: "hello", type: "function", appId: "wxscan123" }),
      expect.objectContaining({ name: "users", type: "database", appId: "wxscan123" }),
    ]));
  });
});
