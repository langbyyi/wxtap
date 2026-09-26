import WebSocket from "ws";
import { afterEach, describe, expect, it } from "vitest";

import { CdpBridge } from "./cdp-bridge.js";
import { WmpfSocketServers } from "./websocket-servers.js";
import { decodeCdpMessage, encodeCdpMessage } from "../protocol/wmpf-codec.js";

let servers: WmpfSocketServers | undefined;

afterEach(async () => {
  await servers?.stop();
  servers = undefined;
});

function open(url: string): Promise<WebSocket> {
  return new Promise((resolve, reject) => {
    const socket = new WebSocket(url);
    socket.once("open", () => resolve(socket));
    socket.once("error", reject);
  });
}

function nextMessage(socket: WebSocket): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    socket.once("message", (data) => resolve(data as Buffer));
    socket.once("error", reject);
  });
}

// Resolves "rejected" when the server refuses the upgrade instead of
// completing the WebSocket handshake.
function handshake(url: string, origin?: string): Promise<"open" | "rejected"> {
  return new Promise((resolve, reject) => {
    const socket = origin === undefined ? new WebSocket(url) : new WebSocket(url, { origin });
    socket.once("open", () => {
      socket.close();
      resolve("open");
    });
    socket.once("error", (error: Error) => {
      if (error.message.includes("403")) resolve("rejected");
      else reject(error);
    });
  });
}

describe("WmpfSocketServers", () => {
  it("bridges CDP text and WMPF binary data on loopback-only servers", async () => {
    servers = new WmpfSocketServers(new CdpBridge(), { debugPort: 0, cdpPort: 0 });
    const ports = await servers.start();
    const miniapp = await open(`ws://127.0.0.1:${ports.debugPort}`);
    const devtools = await open(`ws://127.0.0.1:${ports.cdpPort}`);

    const toMiniapp = nextMessage(miniapp);
    devtools.send('{"id":7,"method":"Runtime.enable"}');
    expect(decodeCdpMessage(await toMiniapp)).toMatchObject({
      category: "chromeDevtools",
      payload: '{"id":7,"method":"Runtime.enable"}',
    });

    const toDevtools = nextMessage(devtools);
    miniapp.send(encodeCdpMessage({
      sequence: 1,
      category: "chromeDevtoolsResult",
      operationId: 7,
      payload: '{"id":7,"result":{}}',
      jsContextId: "",
    }));
    expect((await toDevtools).toString("utf8")).toBe('{"id":7,"result":{}}');

    miniapp.close();
    devtools.close();
  });
});

describe("WmpfSocketServers malformed frames", () => {
  it("terminates a peer sending protocol-invalid frames instead of crashing", async () => {
    servers = new WmpfSocketServers(new CdpBridge(), { debugPort: 0, cdpPort: 0 });
    const ports = await servers.start();
    const miniapp = await open(`ws://127.0.0.1:${ports.debugPort}`);

    // RSV2 set on the client raw socket: ws emits a connection error that
    // must be swallowed by the server's error handler, not thrown. The
    // handler terminates the peer, which the client observes as a close.
    const closed = new Promise<void>((resolve) => miniapp.once("close", () => resolve()));
    const raw = (miniapp as unknown as { _socket: import("node:net").Socket })._socket;
    raw.write(Buffer.from([0xa2, 0x80, 0x00, 0x00, 0x00, 0x00]));
    await Promise.race([closed, new Promise((_, reject) => setTimeout(() => reject(new Error("peer not terminated and error not handled")), 1000))]);

    // The server process must still accept and serve a fresh connection.
    const second = await open(`ws://127.0.0.1:${ports.cdpPort}`);
    expect(second.readyState).toBe(WebSocket.OPEN);
    second.close();
    miniapp.close();
  });
});

describe("WmpfSocketServers origin policy", () => {
  // Browsers always attach Origin to a WebSocket handshake, so a page from a
  // public site could otherwise drive the miniapp debug and CDP channels.
  it("rejects non-loopback browser origins on the miniapp and CDP ports", async () => {
    servers = new WmpfSocketServers(new CdpBridge(), { debugPort: 0, cdpPort: 0 });
    const ports = await servers.start();
    const debugUrl = `ws://127.0.0.1:${ports.debugPort}`;
    const cdpUrl = `ws://127.0.0.1:${ports.cdpPort}`;

    for (const origin of ["https://evil.example", "http://evil.example", "chrome-extension://abcdef", "null"]) {
      await expect(handshake(cdpUrl, origin)).resolves.toBe("rejected");
    }
    await expect(handshake(debugUrl, "https://evil.example")).resolves.toBe("rejected");

    // A rejected handshake must not poison the listener for real clients.
    await expect(handshake(cdpUrl)).resolves.toBe("open");
  });

  it("accepts native clients, devtools:// and loopback pages", async () => {
    servers = new WmpfSocketServers(new CdpBridge(), { debugPort: 0, cdpPort: 0 });
    const ports = await servers.start();
    const cdpUrl = `ws://127.0.0.1:${ports.cdpPort}`;

    await expect(handshake(`ws://127.0.0.1:${ports.debugPort}`)).resolves.toBe("open");
    for (const origin of [
      "devtools://devtools",
      "http://127.0.0.1:9222",
      "http://localhost:9222",
      "https://[::1]:9222",
    ]) {
      await expect(handshake(cdpUrl, origin)).resolves.toBe("open");
    }
  });
});