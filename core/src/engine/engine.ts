import type { CdpBridge } from "../bridge/cdp-bridge.js";

export type EngineStatus = {
  frida: boolean;
  miniapp: boolean;
  devtools: boolean;
};

/** What the connection page can say about the desktop WeChat host before attach. */
export type WeChatHost = {
  running: boolean;
  pid?: number;
  version?: number;
  path?: string;
  addressTable?: boolean;
  note?: string;
};

export interface RuntimeAdapter {
  start(cdpPort: number): Promise<EngineStatus>;
  stop(): Promise<void>;
  status(): EngineStatus;
  isWeChatRunning?(): Promise<boolean>;
  describeWeChat?(): Promise<WeChatHost>;
  getBridge?(): CdpBridge | undefined;
}

export class Engine {
  constructor(private readonly runtime: RuntimeAdapter) {}

  async start(cdpPort: number): Promise<{ frida: boolean }> {
    const status = await this.runtime.start(cdpPort);
    return { frida: status.frida };
  }

  async stop(): Promise<void> {
    await this.runtime.stop();
  }

  status(): EngineStatus {
    return this.runtime.status();
  }

  async isWeChatRunning(): Promise<boolean> {
    return await this.runtime.isWeChatRunning?.() ?? false;
  }

  async describeWeChat(): Promise<WeChatHost> {
    if (this.runtime.describeWeChat) return await this.runtime.describeWeChat();
    return { running: await this.isWeChatRunning() };
  }

  getBridge(): CdpBridge | undefined {
    return this.runtime.getBridge?.();
  }
}
