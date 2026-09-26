import { describe, expect, it, vi } from 'vitest';
import { BackendCallError, createBackendBridge } from './bridge';

function withWailsCall<T>(call: ReturnType<typeof vi.fn>, run: () => Promise<T>): Promise<T> {
  const previousGo = window.go;
  (window as typeof window & { go: unknown }).go = { main: { App: { Call: call } } };
  return run().finally(() => {
    window.go = previousGo;
  });
}

describe('createBackendBridge', () => {
  it('allows every GUI IPC method and rejects methods outside the explicit whitelist', async () => {
    const call = vi.fn().mockResolvedValue('{"result":{"ok":true}}');
    await withWailsCall(call, async () => {
      const bridge = createBackendBridge();
      const methods = [
        'ak.verify', 'assets.export', 'assets.list', 'assets.scan',
        'cloud.call', 'cloud.call_container', 'cloud.clear', 'cloud.export', 'cloud.export.progress', 'cloud.poll', 'cloud.scan', 'cloud.start', 'cloud.stats',
        'cloudapi.start', 'cloudapi.status', 'cloudapi.stop', 'code.expandDir', 'code.formatAll', 'code.formatFile', 'code.openDir', 'code.project', 'code.readFile', 'code.search',
        'config.load', 'config.save', 'console.clear', 'console.list', 'engine.start', 'engine.status', 'engine.stop', 'engine.vconsole', 'extract.browse', 'extract.builtinPatterns',
        'extract.candidateDirs', 'extract.clearOutput', 'extract.decompile', 'extract.decompileAll', 'extract.defaultDir', 'extract.delete', 'extract.openDir', 'extract.packages', 'extract.scan', 'fetch.md', 'hook.inject', 'hook.list', 'hook.setGlobal', 'mcp.start', 'mcp.status', 'mcp.stop',
        'navigator.autoVisit', 'navigator.autoVisitState', 'navigator.disableRedirectGuard',
        'navigator.enableRedirectGuard', 'navigator.getCurrentRoute', 'navigator.guardState', 'navigator.navigate', 'navigator.pages', 'navigator.stopAutoVisit',
        'node.detect', 'node.status',
        'sessionkey.detect', 'sessionkey.users', 'sessionkey.scan', 'sessionkey.scanDecompiled', 'sessionkey.scanTraffic', 'settings.getPaths', 'shell.openDevtoolsWindow', 'shell.openFolder', 'shell.openUrl', 'targets.list', 'traffic.list', 'traffic.curl', 'traffic.getBody', 'traffic.replay', 'traffic.exportHar', 'traffic.stats', 'traffic.clear', 'traffic.delete',
        'wxopen.call', 'wxopen.endpoints',
        'update.checkVersion', 'update.downloadRelease', 'update.restart', 'update.status', 'update.syncSkills', 'update.syncWMPF', 'wxapi.clear', 'wxapi.poll', 'wxapi.replay', 'wxapi.start', 'wxapi.stats', 'wxapi.stop',
      ];

      await Promise.all(methods.map((method) => bridge.call(method)));
      await expect(bridge.call('engine.restart')).rejects.toThrow('Unsupported backend method: engine.restart');
      expect(call).toHaveBeenCalledTimes(methods.length);
    });
  });

  it('dispatches a backend event to every active subscriber', () => {
    const bridge = createBackendBridge();
    const first = vi.fn();
    const second = vi.fn();
    bridge.on('status', first);
    bridge.on('status', second);

    window.__onBackendEvent?.('status', { frida: true });

    expect(first).toHaveBeenCalledWith({ frida: true });
    expect(second).toHaveBeenCalledWith({ frida: true });
  });

  it('accepts task lifecycle events', () => {
    const bridge = createBackendBridge();
    const listener = vi.fn();
    bridge.on('task', listener);
    window.__onBackendEvent?.('task', { id: 'extract-1', phase: 'running' });
    expect(listener).toHaveBeenCalledWith({ id: 'extract-1', phase: 'running' });
  });

  it('accepts wxapi settle updates and mocks capture stats without Wails', async () => {
    const bridge = createBackendBridge();
    const listener = vi.fn();
    bridge.on('wxapi_update', listener);
    const frame = { seq: 12, rid: 'wx.request-wxone-1758600000000-7', status: 'success', durationMs: 1830 };
    window.__onBackendEvent?.('wxapi_update', frame);
    expect(listener).toHaveBeenCalledWith(frame);

    const previousGo = window.go;
    delete window.go;
    try {
      await expect(bridge.call('wxapi.stats')).resolves.toMatchObject({ running: true, pending: 0, dropped: 0 });
    } finally {
      window.go = previousGo;
    }
  });

  it('mocks wxapi.poll as the array the poll contract promises', async () => {
    const previousGo = window.go;
    delete window.go;
    try {
      const bridge = createBackendBridge();
      // 没有记录就是空数组：返回 {ok:true} 会让页面插进一条假记录
      await expect(bridge.call('wxapi.poll', { limit: 500 })).resolves.toEqual([]);
    } finally {
      window.go = previousGo;
    }
  });

  it('accepts cloud settle updates and mocks the cloud stats and poll contracts without Wails', async () => {
    const bridge = createBackendBridge();
    const listener = vi.fn();
    bridge.on('cloud_update', listener);
    // 后端按批投递落定帧，也可能逐帧投递：两种形状都要能到订阅者手上
    const batch = [{ rid: 'function-wxone-1758600000000-7', status: 'success', durationMs: 1830 }];
    window.__onBackendEvent?.('cloud_update', batch);
    expect(listener).toHaveBeenCalledWith(batch);
    expect(() => bridge.on('cloud_update', vi.fn())).not.toThrow();

    const previousGo = window.go;
    delete window.go;
    try {
      await expect(bridge.call('cloud.stats')).resolves.toMatchObject({ running: true, pending: 0, dropped: 0 });
      // cloud.poll 与 wxapi.poll 同契约：响应是数组，不是 {ok:true}
      await expect(bridge.call('cloud.poll', { limit: 500 })).resolves.toEqual([]);
    } finally {
      window.go = previousGo;
    }
  });

  it('accepts the assets progress event', () => {
    const bridge = createBackendBridge();
    const assetsListener = vi.fn();
    bridge.on('assets_progress', assetsListener);
    window.__onBackendEvent?.('assets_progress', { status: 'done', current: 4, total: 4 });
    expect(assetsListener).toHaveBeenCalledWith({ status: 'done', current: 4, total: 4 });
  });

  // 预览样本的形状守卫：AssetItem 的关键字段缺一个，页面就会渲染成「未知」。
  // 这里钉住 mock 与契约不漂移（后端另行按契约实现）。
  it('mocks the assets contract with complete shapes', async () => {
    const previousGo = window.go;
    delete window.go;
    try {
      const bridge = createBackendBridge();
      const assets = await bridge.call<{ total: number; hosts: Array<{ host: string; count: number }>; items: Array<Record<string, unknown>> }>('assets.list', { offset: 0, limit: 100 });
      expect(assets.total).toBeGreaterThan(0);
      expect(assets.hosts.length).toBeGreaterThan(0);
      for (const item of assets.items) {
        expect(item).toEqual(expect.objectContaining({
          id: expect.any(String), kind: expect.any(String), url: expect.any(String), host: expect.any(String), path: expect.any(String),
          method: expect.any(String), sources: expect.any(Array), hits: expect.any(Number),
          firstSeen: expect.any(String), lastSeen: expect.any(String),
        }));
      }
      await expect(bridge.call('assets.list', { kind: 'api' })).resolves.toMatchObject({ total: 2 });
      await expect(bridge.call('traffic.curl', { id: 'seed-1' })).resolves.toMatchObject({ ok: true, bash: expect.any(String), cmd: expect.any(String) });
      await expect(bridge.call('traffic.replay', { id: 'seed-1' })).resolves.toMatchObject({ ok: true, status: expect.any(Number), elapsedMs: expect.any(Number), body: expect.any(String) });
    } finally {
      window.go = previousGo;
    }
  });

  it('stops dispatching to a subscriber after it is cancelled', () => {
    const bridge = createBackendBridge();
    const retained = vi.fn();
    const cancelled = vi.fn();
    bridge.on('log', retained);
    const unsubscribe = bridge.on('log', cancelled);

    unsubscribe();
    window.__onBackendEvent?.('log', { message: 'connected' });

    expect(retained).toHaveBeenCalledOnce();
    expect(cancelled).not.toHaveBeenCalled();
  });

  it('isolates subscriber exceptions while dispatching a backend event', () => {
    const bridge = createBackendBridge();
    const error = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const healthy = vi.fn();
    bridge.on('app_info', () => {
      throw new Error('subscriber failed');
    });
    bridge.on('app_info', healthy);

    window.__onBackendEvent?.('app_info', { appid: 'wx123' });

    expect(healthy).toHaveBeenCalledWith({ appid: 'wx123' });
    error.mockRestore();
  });

  it('rejects unknown backend events without registering a subscriber', () => {
    const bridge = createBackendBridge();

    expect(() => bridge.on('engine.restart', vi.fn())).toThrow('Unsupported backend event: engine.restart');
  });

  it('serializes a Wails request and returns its result', async () => {
    const call = vi.fn().mockResolvedValue('{"result":{"ok":true}}');
    await withWailsCall(call, async () => {
      const bridge = createBackendBridge();
      await expect(bridge.call('engine.start', { cdp_port: 62000 })).resolves.toEqual({ ok: true });
      expect(call).toHaveBeenCalledWith('engine.start', '{"cdp_port":62000}');
    });
  });

  it('throws the backend error message', async () => {
    const call = vi.fn().mockResolvedValue('{"error":"engine not running"}');
    await withWailsCall(call, async () => {
      const bridge = createBackendBridge();
      await expect(bridge.call('engine.stop')).rejects.toThrow('engine not running');
    });
  });

  it('preserves structured backend error metadata for actionable recovery', async () => {
    const call = vi.fn().mockResolvedValue('{"error":{"code":"PORT_IN_USE","message":"端口已占用","retryable":true,"details":{"port":31415}}}');
    await withWailsCall(call, async () => {
      const bridge = createBackendBridge();
      const error = await bridge.call('engine.start').catch((reason) => reason);
      expect(error).toBeInstanceOf(BackendCallError);
      expect(error).toMatchObject({ message: '端口已占用', code: 'PORT_IN_USE', retryable: true, details: { port: 31415 } });
    });
  });

  it('throws a useful error when the backend response is not JSON', async () => {
    const call = vi.fn().mockResolvedValue('not json');
    await withWailsCall(call, async () => {
      const bridge = createBackendBridge();
      await expect(bridge.call('engine.status')).rejects.toThrow('Invalid backend response JSON');
    });
  });

  it('uses browser-safe mock responses when Wails is unavailable', async () => {
    const previousGo = window.go;
    delete window.go;
    try {
      const bridge = createBackendBridge();
      // 预览按「已连接示例小程序」返回：目标跟随、目录 appid 对齐等链路在浏览器里可见。
      await expect(bridge.call('engine.status')).resolves.toEqual({
        frida: true,
        miniapp: true,
        devtools: false,
        appInfo: { appid: 'wxdeadbeefdeadbeef', name: '预览示例小程序' },
      });
      await expect(bridge.call('engine.start')).resolves.toEqual({ ok: true });
      await expect(bridge.call('config.load')).resolves.toEqual({});
      await expect(bridge.call('settings.getPaths')).resolves.toEqual({});
    } finally {
      window.go = previousGo;
    }
  });

  // 预览里的接口表是 Go 目录的快照，不是手写样本。手写的那份缩到过 4 条，看着像这个
  // 工具只认得几个接口——而快照由 desktop/wxopen_preview_contract_test.go 生成并校验，
  // 目录一变、对不上，那边就红。
  it('serves the whole endpoint catalog in the browser mock, not a small sample', async () => {
    const previousGo = window.go;
    delete window.go;
    try {
      const catalog = await createBackendBridge().call<{
        groups: Array<{ id: string }>;
        endpoints: Array<{ group: string }>;
      }>('wxopen.endpoints');
      expect(catalog.groups.map((group) => group.id)).toEqual(['mini', 'oa', 'work']);
      expect(catalog.endpoints.length).toBeGreaterThanOrEqual(40);
      for (const id of ['mini', 'oa', 'work']) {
        expect(catalog.endpoints.filter((item) => item.group === id).length).toBeGreaterThanOrEqual(8);
      }
    } finally {
      window.go = previousGo;
    }
  });

  it('dispatches completion events for event-driven methods in the browser mock', async () => {
    vi.useFakeTimers();
    const previousGo = window.go;
    delete window.go;
    const bridge = createBackendBridge();
    const received: Array<{ name: string; payload: unknown }> = [];
    const originalHandler = window.__onBackendEvent;
    window.__onBackendEvent = (name, payload) => { originalHandler?.(name, payload); received.push({ name, payload }); };
    bridge.on('vconsole_result', (payload) => received.push({ name: 'vconsole_result', payload }));
    bridge.on('export_progress', (payload) => received.push({ name: 'export_progress', payload }));

    try {
      await bridge.call('engine.vconsole', { enable: true });
      await bridge.call('cloud.export', { items: [{ name: 'x' }] });
      await vi.advanceTimersByTimeAsync(10);

      const names = received.map((entry) => entry.name);
      expect(names).toContain('vconsole_result');
      expect(names).toContain('export_progress');
    } finally {
      window.__onBackendEvent = originalHandler;
      window.go = previousGo;
      vi.useRealTimers();
    }
  });

  it('rejects unknown methods before contacting a real host', async () => {
    const call = vi.fn().mockResolvedValue('{"result":{"ok":true}}');
    await withWailsCall(call, async () => {
      const bridge = createBackendBridge();
      await expect(bridge.call('engine.restart')).rejects.toThrow('Unsupported backend method: engine.restart');
      expect(call).not.toHaveBeenCalled();
    });
  });

  it('rejects unknown methods in the browser mock', async () => {
    const previousGo = window.go;
    delete window.go;
    try {
      const bridge = createBackendBridge();
      await expect(bridge.call('engine.restart')).rejects.toThrow('Unsupported backend method: engine.restart');
    } finally {
      window.go = previousGo;
    }
  });

  it('routes calls through the Wails binding when available', async () => {
    const wailsCall = vi.fn().mockResolvedValue('{"result":{"route":"pages/index"}}');
    await withWailsCall(wailsCall, async () => {
      const bridge = createBackendBridge();
      await expect(bridge.call('navigator.pages')).resolves.toEqual({ route: 'pages/index' });
      expect(wailsCall).toHaveBeenCalledWith('navigator.pages', '{}');
    });
  });

  it('surfaces Wails binding errors through the envelope', async () => {
    const wailsCall = vi.fn().mockResolvedValue('{"error":"core process has exited"}');
    await withWailsCall(wailsCall, async () => {
      const bridge = createBackendBridge();
      await expect(bridge.call('engine.status')).rejects.toThrow('core process has exited');
    });
  });
});
