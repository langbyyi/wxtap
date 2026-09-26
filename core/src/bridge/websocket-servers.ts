import WebSocket, { WebSocketServer, type RawData } from "ws";

import { CdpBridge, type Peer } from "./cdp-bridge.js";

// The miniapp and CDP sockets are local debug channels, and CORS does not
// apply to WebSocket: without an origin gate any page the user visits could
// open ws://127.0.0.1:<port> and drive them. Browsers always send Origin on
// the handshake, so only native clients (no Origin), the devtools:// scheme
// (the Electron DevTools window) and other loopback pages may connect.
const LOOPBACK_ORIGIN_HOSTS = new Set(["127.0.0.1", "localhost", "[::1]", "::1"]);
const reportedOrigins = new Set<string>();
const maxSocketPayload = 64 * 1024 * 1024;

export type SocketServerOptions = {
  debugPort: number;
  cdpPort: number;
};

export type SocketServerPorts = {
  debugPort: number;
  cdpPort: number;
};

export class WmpfSocketServers {
  private debugServer: WebSocketServer | undefined;
  private cdpServer: WebSocketServer | undefined;

  constructor(
    private readonly bridge: CdpBridge,
    private readonly options: SocketServerOptions,
  ) {}

  async start(): Promise<SocketServerPorts> {
    if (this.debugServer !== undefined || this.cdpServer !== undefined) {
      throw new Error("WMPF socket servers are already running");
    }
    const debugServer = new WebSocketServer({
      host: "127.0.0.1",
      port: this.options.debugPort,
      maxPayload: maxSocketPayload,
      verifyClient: (info, done) =>
        done(verifyHandshakeOrigin(info.origin, this.options.debugPort), 403, "Forbidden"),
    });
    const cdpServer = new WebSocketServer({
      host: "127.0.0.1",
      port: this.options.cdpPort,
      maxPayload: maxSocketPayload,
      verifyClient: (info, done) =>
        done(verifyHandshakeOrigin(info.origin, this.options.cdpPort), 403, "Forbidden"),
    });
    this.attachDebugServer(debugServer);
    this.attachCdpServer(cdpServer);
    try {
      await Promise.all([waitForListening(debugServer), waitForListening(cdpServer)]);
      this.debugServer = debugServer;
      this.cdpServer = cdpServer;
      return { debugPort: portOf(debugServer), cdpPort: portOf(cdpServer) };
    } catch (error) {
      await Promise.all([closeServer(debugServer), closeServer(cdpServer)]);
      throw new Error(describeListenFailure(error, this.options), { cause: error });
    }
  }

  async stop(): Promise<void> {
    const servers = [this.debugServer, this.cdpServer].filter((server): server is WebSocketServer => server !== undefined);
    this.debugServer = undefined;
    this.cdpServer = undefined;
    await Promise.all(servers.map(closeServer));
  }

  private attachDebugServer(server: WebSocketServer): void {
    server.on("connection", (socket) => {
      const peer = asPeer(socket);
      const clientId = this.bridge.addMiniapp(peer);
      // Without an error listener ws re-emits protocol errors as unhandled
      // 'error' events, which would take the whole core process down.
      socket.on("error", () => socket.terminate());
      socket.on("message", (data, isBinary) => {
        if (isBinary) {
          this.bridge.receiveMiniapp(toBuffer(data), clientId);
        }
      });
      socket.once("close", () => this.bridge.removeMiniapp(clientId));
    });
  }

  private attachCdpServer(server: WebSocketServer): void {
    server.on("connection", (socket) => {
      const peer = asPeer(socket);
      this.bridge.addDevtools(peer);
      socket.on("error", () => socket.terminate());
      socket.on("message", (data, isBinary) => {
        if (!isBinary) {
          this.bridge.forwardDevtools(toBuffer(data).toString("utf8"));
        }
      });
      socket.once("close", () => this.bridge.removeDevtools(peer));
    });
  }
}

function verifyHandshakeOrigin(origin: string | undefined, port: number): boolean {
  if (isAllowedOrigin(origin)) return true;
  // One line per distinct origin so a hostile page cannot flood the log.
  if (origin !== undefined && !reportedOrigins.has(origin)) {
    reportedOrigins.add(origin);
    console.error(`[core] 已拒绝 ${port} 端口上来自 ${origin} 的 WebSocket 握手：来源不是本机`);
  }
  return false;
}

function isAllowedOrigin(origin: string | undefined): boolean {
  if (origin === undefined) return true;
  if (origin.startsWith("devtools://")) return true;
  try {
    const url = new URL(origin);
    const loopback = LOOPBACK_ORIGIN_HOSTS.has(url.hostname);
    return (url.protocol === "http:" || url.protocol === "https:") && loopback;
  } catch {
    // Includes the opaque `null` origin sent by file:// pages and sandboxed
    // iframes.
    return false;
  }
}

function asPeer(socket: WebSocket): Peer {
  return { send: (message) => socket.send(message) };
}

function toBuffer(data: RawData): Buffer {
  if (Buffer.isBuffer(data)) return data;
  if (Array.isArray(data)) return Buffer.concat(data);
  return Buffer.from(data);
}

function waitForListening(server: WebSocketServer): Promise<void> {
  return new Promise((resolve, reject) => {
    server.once("listening", resolve);
    server.once("error", reject);
  });
}

/**
 * A listen failure surfaces verbatim in the 状态页 error banner, so the raw
 * Node EADDRINUSE string ("listen EADDRINUSE: address already in use …") is not
 * good enough: name the ports and say what to do about it.
 */
function describeListenFailure(error: unknown, options: SocketServerOptions): string {
  const code = (error as { code?: string } | undefined)?.code;
  const detail = error instanceof Error ? error.message : String(error);
  const ports = `CDP ${options.cdpPort} / 调试 ${options.debugPort}`;
  if (code === "EADDRINUSE") {
    // The two causes worth naming ahead of "some other program": another WxTap
    // window (the debug port is fixed by the miniapp's convention, so two
    // instances always collide on it), and a Windows port range reserved by
    // Hyper-V / WSL, where the bind fails with nothing listening at all.
    return `端口已被占用（${ports}）：最常见的是本程序已在运行——请改用那个窗口，或先退出它；` +
      "若不是，请关闭占用该端口的程序，或在「状态」页换一个 CDP 端口" +
      `（调试端口 ${options.debugPort} 是小程序约定的固定端口，不能更改）`;
  }
  if (code === "EACCES" || code === "EPERM") {
    return `无权监听调试端口（${ports}）：Windows 上常见原因是该端口落在 Hyper-V / WSL 保留的区间内` +
      "（`netsh int ipv4 show excludedportrange protocol=tcp` 可查看），请在「状态」页换一个 CDP 端口";
  }
  return `无法监听调试端口（${ports}）：${detail}`;
}

function portOf(server: WebSocketServer): number {
  const address = server.address();
  if (address === null || typeof address === "string") {
    throw new Error("socket server has no TCP address");
  }
  return address.port;
}

function closeServer(server: WebSocketServer): Promise<void> {
  for (const client of server.clients) {
    client.terminate();
  }
  return new Promise((resolve) => server.close(() => resolve()));
}
