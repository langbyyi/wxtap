import { describe, expect, it } from "vitest";

import { Engine, type RuntimeAdapter } from "./engine.js";

describe("Engine", () => {
  it("starts the runtime with the requested CDP port", async () => {
    const ports: number[] = [];
    const runtime: RuntimeAdapter = {
      start: async (cdpPort) => {
        ports.push(cdpPort);
        return { frida: true, miniapp: false, devtools: false };
      },
      stop: async () => {},
      status: () => ({ frida: true, miniapp: false, devtools: false }),
    };

    const engine = new Engine(runtime);

    await expect(engine.start(62000)).resolves.toEqual({ frida: true });
    expect(ports).toEqual([62000]);
  });

  it("returns the runtime connection state without changing it", () => {
    const runtime: RuntimeAdapter = {
      start: async () => ({ frida: false, miniapp: false, devtools: false }),
      stop: async () => {},
      status: () => ({ frida: true, miniapp: true, devtools: false }),
    };

    expect(new Engine(runtime).status()).toEqual({ frida: true, miniapp: true, devtools: false });
  });
});
