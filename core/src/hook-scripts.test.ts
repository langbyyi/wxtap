import { describe, expect, it } from "vitest";

import { hookScriptFileName } from "./hook-scripts.js";

describe("hookScriptFileName", () => {
  it("maps the navigator protocol hook to the shipped nav.js file", () => {
    expect(hookScriptFileName("navigator")).toBe("nav.js");
  });

  it("keeps hook names whose protocol and filename match", () => {
    expect(hookScriptFileName("wxapi")).toBe("wxapi.js");
  });
});
