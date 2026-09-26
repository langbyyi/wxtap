import { createPinia, setActivePinia } from 'pinia';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { backend, BackendCallError } from '../api/bridge';
import { useEngineStore } from './engine';

const { on } = vi.hoisted(() => ({ on: vi.fn<(event: string, listener: (payload: unknown) => void) => () => void>(() => vi.fn()) }));
// BackendCallError 用真实实现：store 靠 instanceof 判断后端给的是否可重试。
vi.mock('../api/bridge', async () => {
  const actual = await vi.importActual<typeof import('../api/bridge')>('../api/bridge');
  return { backend: { call: vi.fn(), on }, BackendCallError: actual.BackendCallError };
});

const bridge = vi.mocked(backend);
// 与 store 里的 VCONSOLE_SETTLE_MS 对齐：兜底只要求「最终会清掉」，加上一点余量。
const VCONSOLE_SETTLE_MS_FOR_TEST = 15000;

describe('useEngineStore', () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    bridge.call.mockReset();
    on.mockReset();
    on.mockReturnValue(vi.fn());
  });

  it.each([null, '', 0, 'abc', -1])('falls back to the default port for invalid config cdp_port %j', async (invalid) => {
    bridge.call
      .mockResolvedValueOnce({ cdp_port: invalid })
      .mockResolvedValueOnce({ frida: false, miniapp: false, devtools: false });

    const store = useEngineStore();
    await store.load();

    expect(store.cdpPort).toBe(31415);
  });

  it('sends the default port when the input field was cleared to an empty string', async () => {
    bridge.call
      .mockResolvedValueOnce({ ok: true })
      .mockResolvedValueOnce({ ok: true })
      .mockResolvedValueOnce({ frida: true, miniapp: false, devtools: false });

    const store = useEngineStore();
    store.cdpPort = '' as unknown as number;
    await store.start();

    expect(bridge.call).toHaveBeenNthCalledWith(1, 'engine.start', { cdp_port: 31415 });
    expect(bridge.call).toHaveBeenNthCalledWith(2, 'config.save', { cdp_port: 31415 });
  });

  it('subscribes once to status events and keeps the store status in sync', async () => {
    bridge.call.mockResolvedValue({ frida: false, miniapp: false, devtools: false });
    const store = useEngineStore();
    store.listen();
    store.listen();

    expect(on).toHaveBeenCalledTimes(4);
    expect(on).toHaveBeenCalledWith('status', expect.any(Function));
    expect(on).toHaveBeenCalledWith('log', expect.any(Function));
    expect(on).toHaveBeenCalledWith('vconsole_result', expect.any(Function));
    const listener = on.mock.calls[0][1];
    listener({ frida: true, miniapp: true, devtools: true });
    expect(store.status).toEqual({ frida: true, miniapp: true, devtools: true });
  });

  it('loads the configured port before refreshing engine status', async () => {
    bridge.call
      .mockResolvedValueOnce({ cdp_port: '62001' })
      .mockResolvedValueOnce({ frida: true, miniapp: false, devtools: true });

    const store = useEngineStore();
    await store.load();

    expect(store.cdpPort).toBe(62001);
    expect(store.status).toEqual({ frida: true, miniapp: false, devtools: true });
    expect(bridge.call).toHaveBeenNthCalledWith(1, 'config.load');
    expect(bridge.call).toHaveBeenNthCalledWith(2, 'engine.status');
  });

  it('refreshes the live engine status without reloading configuration', async () => {
    bridge.call.mockResolvedValueOnce({ frida: true, miniapp: false, devtools: false });
    const store = useEngineStore();
    store.status = { frida: true, miniapp: true, devtools: false };

    await store.refreshStatus();

    expect(bridge.call).toHaveBeenCalledWith('engine.status');
    expect(store.status).toEqual({ frida: true, miniapp: false, devtools: false });
  });

  it('does not overlap WeChat process checks while a previous check is pending', async () => {
    let resolveCheck: ((value: { running: boolean }) => void) | undefined;
    bridge.call.mockImplementation(() => new Promise((resolve) => { resolveCheck = resolve; }));
    const store = useEngineStore();

    const first = store.loadWeChatStatus();
    const second = store.loadWeChatStatus();
    expect(bridge.call).toHaveBeenCalledTimes(1);
    resolveCheck?.({ running: true });
    await Promise.all([first, second]);

    expect(store.status.wechatRunning).toBe(true);
  });

  it('keeps the WeChat process state when a later engine status refresh arrives', async () => {
    bridge.call
      .mockResolvedValueOnce({ cdp_port: 31415 })
      .mockResolvedValueOnce({ frida: false, miniapp: false, devtools: false });
    const store = useEngineStore();
    store.status.wechatRunning = true;

    await store.load();

    expect(store.status.wechatRunning).toBe(true);
  });

  it('surfaces a failed WeChat process check instead of reporting it as not running', async () => {
    bridge.call.mockResolvedValueOnce({ running: false, error: 'Frida local device unavailable' });
    const store = useEngineStore();

    await store.loadWeChatStatus();

    expect(store.wechatStatusError).toBe('Frida local device unavailable');
  });

  it('keeps the verified WMPF build beside the WeChat running flag', async () => {
    bridge.call.mockResolvedValueOnce({
      running: true,
      pid: 10,
      version: 14161,
      path: 'C:\\Tencent\\WeChat\\14161\\WeChatAppEx.exe',
      addressTable: false,
      note: '没有这份构建的静态地址表，启动时会自动检测',
    });
    const store = useEngineStore();

    await store.loadWeChatStatus();

    expect(store.status.wechatRunning).toBe(true);
    expect(store.wechatHost).toEqual({
      pid: 10,
      version: 14161,
      path: 'C:\\Tencent\\WeChat\\14161\\WeChatAppEx.exe',
      addressTable: false,
      note: '没有这份构建的静态地址表，启动时会自动检测',
    });
  });

  it('preserves the current port and status when config loading fails', async () => {
    bridge.call.mockRejectedValueOnce(new Error('config unavailable'));

    const store = useEngineStore();
    store.cdpPort = 62004;
    store.status = { frida: true, miniapp: false, devtools: true };
    await store.load();

    expect(store.cdpPort).toBe(62004);
    expect(store.status).toEqual({ frida: true, miniapp: false, devtools: true });
    expect(store.error).toBe('config unavailable');
    expect(bridge.call).toHaveBeenCalledTimes(1);
  });

  it('preserves the current port and status when status loading fails', async () => {
    bridge.call
      .mockResolvedValueOnce({})
      .mockRejectedValueOnce(new Error('status unavailable'));

    const store = useEngineStore();
    store.cdpPort = 62005;
    store.status = { frida: false, miniapp: true, devtools: true };
    await store.load();

    expect(store.cdpPort).toBe(62005);
    expect(store.status).toEqual({ frida: false, miniapp: true, devtools: true });
    expect(store.error).toBe('status unavailable');
  });

  it('does not commit a configured port when status loading fails', async () => {
    bridge.call
      .mockResolvedValueOnce({ cdp_port: '62001' })
      .mockRejectedValueOnce(new Error('status unavailable'));

    const store = useEngineStore();
    store.cdpPort = 62000;
    store.status = { frida: true, miniapp: false, devtools: true };
    await store.load();

    expect(store.cdpPort).toBe(62000);
    expect(store.status).toEqual({ frida: true, miniapp: false, devtools: true });
    expect(store.error).toBe('status unavailable');
  });

  it('starts the engine with the entered port and refreshes its status', async () => {
    bridge.call
      .mockResolvedValueOnce({ ok: true })
      .mockResolvedValueOnce({ ok: true })
      .mockResolvedValueOnce({ frida: true, miniapp: true, devtools: false });

    const store = useEngineStore();
    store.cdpPort = 62002;
    await store.start();

    expect(bridge.call).toHaveBeenNthCalledWith(1, 'engine.start', { cdp_port: 62002 });
    expect(bridge.call).toHaveBeenNthCalledWith(2, 'config.save', { cdp_port: 62002 });
    expect(store.status).toEqual({ frida: true, miniapp: true, devtools: false });
    expect(store.starting).toBe(false);
  });

  it('keeps the engine running when the chosen port cannot be saved', async () => {
    bridge.call
      .mockResolvedValueOnce({ ok: true })
      .mockRejectedValueOnce(new Error('disk full'))
      .mockResolvedValueOnce({ frida: true, miniapp: false, devtools: false });

    const store = useEngineStore();
    store.cdpPort = 62006;
    await store.start();

    expect(store.status).toEqual({ frida: true, miniapp: false, devtools: false });
    expect(store.error).toContain('端口未能写入配置');
    expect(store.error).toContain('disk full');
    expect(store.starting).toBe(false);
  });

  it('keeps runtime logs across flushes, caps them, and drops a pending burst on clear', async () => {
    const store = useEngineStore();
    store.listen();
    const listener = on.mock.calls.find((call) => call[0] === 'log')?.[1];
    expect(listener).toBeTypeOf('function');

    vi.useFakeTimers();
    listener?.({ time: '10:00:00', level: 'info', message: '引擎已启动' });
    expect(store.logs).toEqual([]);
    await vi.advanceTimersByTimeAsync(200);
    expect(store.logs).toEqual([{ time: '10:00:00', level: 'info', message: '引擎已启动', seq: 1 }]);

    for (let index = 0; index < 1200; index += 1) {
      listener?.({ time: '10:00:01', level: 'info', message: `日志 ${index}` });
    }
    store.clearLogs();
    await vi.advanceTimersByTimeAsync(200);
    expect(store.logs).toEqual([]);

    for (let index = 700; index < 1200; index += 1) {
      listener?.({ time: '10:00:02', level: 'info', message: `留下 ${index}` });
    }
    await vi.advanceTimersByTimeAsync(200);
    expect(store.logs).toHaveLength(500);
    expect(store.logs[0]?.message).toBe('留下 700');
    expect(store.logs.at(-1)?.message).toBe('留下 1199');

    // 稳定 key：裁剪后 500 条的 seq 必须仍然两两不同，否则 :key 会退化
    const seqs = store.logs.map((entry) => entry.seq);
    expect(seqs.every((seq) => typeof seq === 'number')).toBe(true);
    expect(new Set(seqs).size).toBe(seqs.length);
    vi.useRealTimers();
  });

  it('retains the entered port and clears the busy state when start fails', async () => {
    bridge.call.mockRejectedValueOnce(new Error('port unavailable'));

    const store = useEngineStore();
    store.cdpPort = 62003;
    await store.start();

    expect(store.cdpPort).toBe(62003);
    expect(store.error).toBe('port unavailable');
    expect(store.starting).toBe(false);
  });

  it('stops the engine and marks every engine status as false', async () => {
    bridge.call.mockResolvedValueOnce({ ok: true });

    const store = useEngineStore();
    store.status = { frida: true, miniapp: true, devtools: true };
    store.cloudCapturing = true;
    store.wxapiCapturing = true;
    await store.stop();

    expect(bridge.call).toHaveBeenCalledWith('engine.stop');
    expect(store.status).toEqual({ frida: false, miniapp: false, devtools: false });
    // 引擎停止后捕获必然结束，UI 不应继续显示「捕获中」
    expect(store.cloudCapturing).toBe(false);
    expect(store.wxapiCapturing).toBe(false);
    expect(store.stopping).toBe(false);
  });

  it('retains the current status and clears the busy state when stop fails', async () => {
    bridge.call.mockRejectedValueOnce(new Error('engine unavailable'));

    const store = useEngineStore();
    store.status = { frida: true, miniapp: false, devtools: true };
    await store.stop();

    expect(store.status).toEqual({ frida: true, miniapp: false, devtools: true });
    expect(store.error).toBe('engine unavailable');
    expect(store.stopping).toBe(false);
  });

  // vConsole 只有后端回推的结果可信：状态放在 store 里，切页不丢，而且
  // 「进行中」的方向也来自 store，不再是靠按钮文案里找『开启』两个字猜。
  it('settles the vConsole state from the backend result event', async () => {
    bridge.call.mockResolvedValue({ ok: true, async: true });
    const store = useEngineStore();
    store.listen();
    const listener = on.mock.calls.find(([event]) => event === 'vconsole_result')?.[1] as (payload: unknown) => void;

    expect(store.vconsoleEnabled).toBeNull();
    // 结果只在「小程序仍连着」时才可信：断开后到达的结果属于已消失的 realm。
    store.status.miniapp = true;
    await store.setVConsole(true);
    expect(store.vconsolePhase).toBe('enable');
    expect(bridge.call).toHaveBeenCalledWith('engine.vconsole', { enable: true });

    listener({ ok: true, enable: true });
    expect(store.vconsolePhase).toBe('');
    expect(store.vconsoleEnabled).toBe(true);

    listener({ ok: false, error: 'no miniapp connected' });
    expect(store.vconsoleError).toBe('未连接小程序（请在微信里打开目标小程序后重试）');
  });

  it('clears the vConsole phase when the request itself fails', async () => {
    bridge.call.mockRejectedValueOnce(new Error('denied'));
    const store = useEngineStore();

    await store.setVConsole(false);

    expect(store.vconsolePhase).toBe('');
    expect(store.vconsoleError).toBe('denied');
  });

  // 按序号增量拉取：暂停期间后端照常缓冲，恢复时补齐且不重复。
  it('polls console records by sequence and appends only what is new', async () => {
    const store = useEngineStore();
    bridge.call.mockResolvedValueOnce({
      records: [{ seq: 1, record: { level: 'log', text: 'first' } }, { seq: 2, record: { level: 'error', text: 'second' } }],
      nextSeq: 2,
      hasMore: false,
    });
    await store.pollConsole();

    expect(bridge.call).toHaveBeenCalledWith('console.list', { afterSeq: 0, limit: 200 });
    expect(store.consoleRecords.map((record) => record.text)).toEqual(['first', 'second']);
    expect(store.consoleSeq).toBe(2);

    bridge.call.mockResolvedValueOnce({ records: [], nextSeq: 2, hasMore: false });
    await store.pollConsole();

    expect(bridge.call).toHaveBeenLastCalledWith('console.list', { afterSeq: 2, limit: 200 });
    expect(store.consoleRecords).toHaveLength(2);
  });

  // console.list 是读而不是消费：两次并发轮询会用同一个 afterSeq 各追加一遍。
  it('never overlaps console polls', async () => {
    const store = useEngineStore();
    let resolveFirst: ((value: unknown) => void) | undefined;
    bridge.call.mockImplementationOnce(() => new Promise((resolve) => { resolveFirst = resolve; }));

    const first = store.pollConsole();
    const second = store.pollConsole();
    expect(bridge.call).toHaveBeenCalledTimes(1);

    resolveFirst?.({ records: [{ seq: 1, record: { level: 'log', text: 'once' } }], nextSeq: 1 });
    await Promise.all([first, second]);

    expect(store.consoleRecords.map((record) => record.text)).toEqual(['once']);
    expect(store.consoleSeq).toBe(1);
  });

  // bridge.call 没有超时：兜底定时器是「开启中…」不会永远停在那里的唯一出路。
  it('clears the working phase when no vconsole result ever arrives', async () => {
    vi.useFakeTimers();
    bridge.call.mockResolvedValue({ ok: true, async: true });
    const store = useEngineStore();
    store.status.miniapp = true;

    await store.setVConsole(true);
    expect(store.vconsolePhase).toBe('enable');

    await vi.advanceTimersByTimeAsync(VCONSOLE_SETTLE_MS_FOR_TEST + 100);
    expect(store.vconsolePhase).toBe('');
    vi.useRealTimers();
  });

  it('ignores a vconsole result that belongs to a realm that is already gone', async () => {
    bridge.call.mockResolvedValue({ ok: true, async: true });
    const store = useEngineStore();
    store.listen();
    const listener = on.mock.calls.find(([event]) => event === 'vconsole_result')?.[1] as (payload: unknown) => void;

    store.status.miniapp = true;
    await store.setVConsole(true);
    store.status.miniapp = false;

    listener({ ok: true, enable: true });

    expect(store.vconsoleEnabled).toBeNull();
    expect(store.vconsolePhase).toBe('enable');
  });

  it('drops a stale vconsole failure note when the miniapp disconnects', async () => {
    const store = useEngineStore();
    store.listen();
    const statusListener = on.mock.calls.find(([event]) => event === 'status')?.[1] as (payload: unknown) => void;

    store.status.miniapp = true;
    store.vconsoleError = 'denied';
    statusListener({ frida: true, miniapp: false, devtools: false });

    expect(store.vconsoleError).toBe('');
    expect(store.vconsoleEnabled).toBeNull();
  });

  // 后端重发已读过的序号时不能出现重复行：客户端自己按标记过滤，而不是指望
  // 对方永远不重发。
  it('ignores records at or below the read marker', async () => {
    const store = useEngineStore();
    bridge.call.mockResolvedValueOnce({
      records: [{ seq: 1, record: { level: 'log', text: 'first' } }, { seq: 2, record: { level: 'log', text: 'second' } }],
      nextSeq: 2,
    });
    await store.pollConsole();

    // 同一批再次返回（例如后端没推进 nextSeq，或与清空竞态）。
    bridge.call.mockResolvedValueOnce({
      records: [{ seq: 1, record: { level: 'log', text: 'first' } }, { seq: 2, record: { level: 'log', text: 'second' } }],
      nextSeq: 2,
    });
    await store.pollConsole();

    expect(store.consoleRecords.map((record) => record.text)).toEqual(['first', 'second']);
  });

  // 同一原则：重发的那一批不该把缺口计数也一起重复计。
  it('counts the evicted gap only when the read actually advanced', async () => {
    const store = useEngineStore();
    bridge.call.mockResolvedValueOnce({
      records: [{ seq: 5, record: { level: 'log', text: 'new' } }],
      nextSeq: 5,
      dropped: 4,
    });
    await store.pollConsole();
    expect(store.consoleDropped).toBe(4);

    bridge.call.mockResolvedValueOnce({
      records: [{ seq: 5, record: { level: 'log', text: 'new' } }],
      nextSeq: 5,
      dropped: 4,
    });
    await store.pollConsole();
    expect(store.consoleDropped).toBe(4);
  });

  it('clears the console buffer and resets its read marker', async () => {
    const store = useEngineStore();
    store.consoleRecords = [{ level: 'log', text: 'old' }];
    store.consoleSeq = 7;
    bridge.call.mockResolvedValueOnce({ ok: true });

    await store.clearConsole();

    expect(bridge.call).toHaveBeenCalledWith('console.clear');
    expect(store.consoleRecords).toEqual([]);
    expect(store.consoleSeq).toBe(0);
  });

  it('reports a console read failure without dropping what is already rendered', async () => {
    const store = useEngineStore();
    store.consoleRecords = [{ level: 'log', text: 'kept' }];
    bridge.call.mockRejectedValueOnce(new Error('engine not running'));

    await store.pollConsole();

    expect(store.consoleError).toBe('engine not running');
    expect(store.consoleRecords.map((record) => record.text)).toEqual(['kept']);
  });
  it('flags retryable backend errors and retries the same action', async () => {
    const store = useEngineStore();
    bridge.call.mockRejectedValueOnce(new BackendCallError({ message: '端口已被占用', code: 'BACKEND_ERROR', retryable: true }));
    await store.start();

    expect(store.error).toBe('端口已被占用');
    expect(store.errorRetryable).toBe(true);
    expect(store.lastFailed).toBe('start');

    bridge.call.mockResolvedValue({ ok: true, frida: true });
    await store.retryFailed();

    expect(bridge.call).toHaveBeenCalledWith('engine.start', { cdp_port: 31415 });
    expect(store.errorRetryable).toBe(false);
  });

  it('does not offer a retry for a failure the backend called permanent', async () => {
    bridge.call.mockRejectedValueOnce(new Error('denied'));
    const store = useEngineStore();

    await store.stop();

    expect(store.errorRetryable).toBe(false);
    expect(store.lastFailed).toBe('stop');
  });

  // 任务面板只显示进行中的任务：已完成的不清理就会永久堆积在 store 里。
  it('prunes finished background tasks', async () => {
    const store = useEngineStore();
    store.listen();
    const listener = on.mock.calls.find(([event]) => event === 'task')?.[1] as (payload: unknown) => void;

    for (let i = 0; i < 14; i += 1) {
      listener({ id: `t${i}`, kind: 'extract.scan', phase: 'done', finishedAt: `2026-01-01T00:00:${String(i).padStart(2, '0')}Z` });
    }

    expect(Object.keys(store.tasks)).toHaveLength(10);
    expect(store.tasks.t13).toBeDefined();
    expect(store.tasks.t0).toBeUndefined();
  });

  // 订阅前发出的日志（启动、引擎早起输出）靠后端环形缓冲回补：回放快照覆盖
  // 所有已投递事件，pending 里与快照重叠的行按 seq 滤掉，先回放后实时。
  it('replays pre-subscription logs from log.list and dedupes the overlap', async () => {
    vi.useFakeTimers();
    bridge.call.mockResolvedValueOnce({
      records: [
        { seq: 1, time: '10:00:00', level: 'info', message: 'WxTap 启动' },
        { seq: 2, time: '10:00:01', level: 'error', message: '早期失败' },
      ],
      nextSeq: 2,
      dropped: 3,
    });
    const store = useEngineStore();
    store.listen();
    const listener = on.mock.calls.find((call) => call[0] === 'log')?.[1] as (entry: { time?: string; level?: string; message?: string; seq?: number }) => void;

    // 回放尚未落地：实时事件先进 pending；seq=2 的行已在快照里，合并时会被滤掉。
    listener({ time: '10:00:01', level: 'error', message: '早期失败', seq: 2 });
    listener({ time: '10:00:02', level: 'info', message: '实时行', seq: 3 });
    await vi.advanceTimersByTimeAsync(200);

    expect(bridge.call).toHaveBeenCalledWith('log.list', { tail: 500 });
    expect(store.logs.map((entry) => entry.message)).toEqual(['WxTap 启动', '早期失败', '实时行']);
    // 快照之前的淘汰数随回放浮出，否则那 3 条就是一段无人知晓的静默缺口。
    expect(store.logsDropped).toBe(3);

    listener({ time: '10:00:03', level: 'warn', message: '后续行', seq: 4 });
    await vi.advanceTimersByTimeAsync(200);
    expect(store.logs.at(-1)?.message).toBe('后续行');
    vi.useRealTimers();
  });

  it('counts rows trimmed by the local LOG_LIMIT into logsTrimmed', async () => {
    vi.useFakeTimers();
    const store = useEngineStore();
    store.listen();
    await vi.advanceTimersByTimeAsync(50); // 让回放落地（call 返回 undefined → 空快照）
    const listener = on.mock.calls.find((call) => call[0] === 'log')?.[1] as (entry: { time?: string; level?: string; message?: string }) => void;

    for (let index = 0; index < 505; index += 1) {
      listener({ time: '10:00:00', level: 'info', message: `行 ${index}` });
    }
    await vi.advanceTimersByTimeAsync(200);

    expect(store.logs).toHaveLength(500);
    expect(store.logs[0]?.message).toBe('行 5');
    expect(store.logsTrimmed).toBe(5);
    vi.useRealTimers();
  });

  it('discards an in-flight replay when the panel is cleared', async () => {
    let resolveList!: (value: unknown) => void;
    bridge.call.mockImplementation(() => new Promise((resolve) => { resolveList = resolve; }));
    const store = useEngineStore();
    store.listen();
    store.clearLogs(); // 递增代号，作废在途回放

    resolveList({ records: [{ seq: 1, time: '10:00:00', level: 'info', message: '迟到的回放' }], nextSeq: 1 });
    await vi.waitFor(() => expect(store.logs).toEqual([]));

    // 计数随清空归零：用户已经主动放弃这段历史。
    expect(store.logsDropped).toBe(0);
    expect(store.logsTrimmed).toBe(0);
    expect(bridge.call).toHaveBeenCalledWith('log.clear');
  });
});
