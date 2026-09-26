import { defineStore } from 'pinia';
import { backend, BackendCallError } from '../api/bridge';
import { DEFAULT_CDP_PORT } from '../ports';
import { messageOf } from '../utils/format';

// 后端 status 载荷里还有 kind/state/available/generation（engine/cloudapi/mcp
// 共用的状态信封），前端只读下面这些；要展示它们时再补字段，别预先声明。
type EngineStatus = {
  frida: boolean;
  miniapp: boolean;
  devtools: boolean;
  wechatRunning?: boolean;
  appInfo?: { appid?: string; name?: string; icon?: string } | null;
};

type EngineConfig = {
  cdp_port?: string | number;
};
export type TaskState = { id: string; kind: string; phase: string; current?: number; total?: number; message?: string; error?: string; startedAt?: string; finishedAt?: string };
export type LogEntry = { time?: string; level?: string; message?: string; seq?: number };
// One captured console line, as the page-side console hook reports it.
export type ConsoleRecord = { level?: string; text?: string; timestamp?: string; ts?: number; appId?: string; extra?: Record<string, string>; seq?: number };
export type VConsolePhase = '' | 'enable' | 'disable';

const stoppedStatus = (): EngineStatus => ({ frida: false, miniapp: false, devtools: false });
// 运行日志的界面保留上限；后端环形缓冲是 1000，回放最多取这么多条。
export const LOG_LIMIT = 500;
const LOG_FLUSH_MS = 120;
const LOG_PENDING_LIMIT = 1000;
// The shell keeps 1000 console records; the view renders fewer, and the trim
// marker below is what keeps a trim from resurrecting old rows.
export const CONSOLE_LIMIT = 500;
const CONSOLE_PAGE_SIZE = 200;
// hasMore 时最多连取这么多页，突发日志不必等好几秒才补齐。
const CONSOLE_MAX_PAGES = 5;
const VCONSOLE_SETTLE_MS = 15000;
// 任务面板只显示进行中的任务，已完成的不清理就会永久堆积在 store 里。
const FINISHED_TASK_LIMIT = 10;
const TERMINAL_TASK_PHASES = ['done', 'failed', 'cancelled'];

/** 完成后端认为值得重试的错误（端口占用、Core 未启动等）才让界面出现重试。 */
function retryableOf(error: unknown): boolean {
  return error instanceof BackendCallError && error.retryable;
}

/** 已完成任务只保留最近若干条。 */
function pruneFinishedTasks(tasks: Record<string, TaskState>) {
  const finished = Object.values(tasks).filter((task) => TERMINAL_TASK_PHASES.includes(task.phase));
  if (finished.length <= FINISHED_TASK_LIMIT) return;
  finished
    .sort((a, b) => (a.finishedAt ?? '').localeCompare(b.finishedAt ?? ''))
    .slice(0, finished.length - FINISHED_TASK_LIMIT)
    .forEach((task) => { delete tasks[task.id]; });
}

type LogBuffer = { pending: LogEntry[]; seq: number; seeded: boolean; timer?: ReturnType<typeof setTimeout> };
const logBuffers = new WeakMap<object, LogBuffer>();
// The vConsole toggle answers asynchronously through vconsole_result; this
// timer is the fallback for a result that never arrives (the engine can die
// mid-toggle), so the buttons never stay stuck on "开启中". Keyed by store
// instance like logBuffers: tests build several stores in one process.
const vconsoleTimers = new WeakMap<object, ReturnType<typeof setTimeout>>();

function clearVConsoleTimer(store: object) {
  const timer = vconsoleTimers.get(store);
  if (timer) clearTimeout(timer);
  vconsoleTimers.delete(store);
}

// console.list is a read, not a consume: two in-flight polls share the same
// afterSeq and would each append the same rows. One poll at a time, like the
// capture views do.
const consolePollsInFlight = new WeakSet<object>();

function normalizeCdpPort(value: unknown): number {
  const port = Number(value);
  return Number.isInteger(port) && port >= 1 && port <= 65535 ? port : DEFAULT_CDP_PORT;
}

export const useEngineStore = defineStore('engine', {
  state: () => ({
    status: stoppedStatus(),
    cdpPort: DEFAULT_CDP_PORT as number | string,
    starting: false,
    stopping: false,
    wechatStatusLoading: false,
    wechatStatusError: '',
    wechatHost: { pid: 0, version: 0, path: '', addressTable: false, note: '' },
    error: '',
    logs: [] as LogEntry[],
    // 回放与容量提示：logsDropped 是订阅前就被后端环形缓冲淘汰的条数，
    // logsTrimmed 是本地缓冲（pending/500 上限）裁掉的条数。不提示的话
    // 用户只会看到一段静默缺口。
    logsDropped: 0,
    logsTrimmed: 0,
    // 本地代号：清空时递增，作废在途回放（否则刚清掉的行会被回放拉回来）。
    logsEpoch: 0,
    listening: false,
    // 捕获状态放在 store 里，切换页面后仍然保持，与后端实际状态一致
    cloudCapturing: false,
    wxapiCapturing: false,
    // vConsole 的状态同样属于「页面 realm」，但只有后端回推的最近一次结果可
    // 信；放在 store 里，切走再回来不会退回「状态未知」。
    vconsoleEnabled: null as boolean | null,
    vconsolePhase: '' as VConsolePhase,
    vconsoleError: '',
    // 后端说这次失败可重试时才为 true；lastFailed 记住是哪个动作失败的，供重试复用。
    errorRetryable: false,
    lastFailed: '' as '' | 'load' | 'start' | 'stop',
    // 小程序 console 输出：shell 侧按序号环形缓冲，这里按 seq 增量拉取。
    consoleRecords: [] as ConsoleRecord[],
    consoleSeq: 0,
    consoleError: '',
    // 被后端环形缓冲淘汰的条数：不提示的话用户只会看到一段静默缺口。
    consoleDropped: 0,
    // 本地代号：清空时递增，用来丢弃在途轮询的响应（否则旧行会以新序号复活）。
    consoleEpoch: 0,
    tasks: {} as Record<string, TaskState>,
  }),

  actions: {
    listen() {
      if (this.listening) return;
      this.listening = true;
      const buffer: LogBuffer = { pending: [], seq: 0, seeded: false };
      logBuffers.set(this, buffer);
      const flushLogs = () => {
        buffer.timer = undefined;
        if (!buffer.pending.length) return;
        const next = this.logs.concat(buffer.pending);
        buffer.pending = [];
        if (next.length > LOG_LIMIT) {
          this.logsTrimmed += next.length - LOG_LIMIT;
          next.splice(0, next.length - LOG_LIMIT);
        }
        this.logs = next;
      };
      backend.on<EngineStatus>('status', (next) => {
        // 小程序断开意味着页面 realm 没了：vConsole 的已知状态随之失效。
        if (this.status.miniapp && !next.miniapp) {
          clearVConsoleTimer(this);
          this.vconsoleEnabled = null;
          this.vconsolePhase = '';
          // 上一次操作的失败提示同样过期了，留着会和「状态未知」自相矛盾。
          this.vconsoleError = '';
        }
        this.status = { ...next, wechatRunning: this.status.wechatRunning };
      });
      backend.on<TaskState>('task', (task) => {
        this.tasks[task.id] = task;
        pruneFinishedTasks(this.tasks);
      });
      backend.on<{ ok?: boolean; enable?: boolean; error?: string }>('vconsole_result', (event) => {
        if (!event) return;
        // 小程序断开后到达的结果描述的是已经消失的 realm，不能拿它写状态。
        if (!this.status.miniapp) return;
        clearVConsoleTimer(this);
        this.vconsolePhase = '';
        if (event.ok !== false && typeof event.enable === 'boolean') {
          this.vconsoleEnabled = event.enable;
          this.vconsoleError = '';
          return;
        }
        // 载荷里是 Core 的原文（no miniapp connected 一类）：显示前走一遍共用译文，
        // 与请求失败的 catch 路径同口径。
        this.vconsoleError = event.error ? messageOf(event.error) : '操作失败';
      });
      backend.on<LogEntry>('log', (entry) => {
        // logs 从头部裁剪、往尾部追加，用 index 作 :key 会让每次 flush 全表重渲染，
        // 所以补一个单调 seq 作稳定 key：后端事件自带环形缓冲序号，缺了（测试/
        // 旧载荷）才退回本地计数。
        buffer.seq += 1;
        buffer.pending.push({ ...entry, seq: entry.seq ?? buffer.seq });
        if (buffer.pending.length > LOG_PENDING_LIMIT) {
          this.logsTrimmed += buffer.pending.length - LOG_PENDING_LIMIT;
          buffer.pending.splice(0, buffer.pending.length - LOG_PENDING_LIMIT);
        }
        // 回放未落地前先攒着：回放要与快照合并，中途 flush 会把已入快照的行
        // 同时留在视图里，靠 seq 也得再 dedupe 一轮，不如统一等回放完成。
        if (!buffer.seeded) return;
        if (!buffer.timer) buffer.timer = setTimeout(flushLogs, LOG_FLUSH_MS);
      });
      // 订阅前发出的日志（应用启动、引擎早起输出）在后端环形缓冲留有副本，
      // 订阅完成后按 tail 回放一次，否则那些行永远不可见。
      void this.seedLogs();
    },

    // 回放订阅前的日志。emitLog 先入环再广播，所以回放快照必然覆盖所有已
    // 投递的事件：按快照 nextSeq 滤掉 pending 里重叠的部分，先回放后实时，
    // 既不丢行也不重复。回放失败不阻塞实时流——放弃补历史，继续走事件。
    async seedLogs() {
      const buffer = logBuffers.get(this);
      if (!buffer) return;
      const epoch = this.logsEpoch;
      try {
        const result = await backend.call<{
          records?: LogEntry[];
          nextSeq?: number;
          dropped?: number;
        }>('log.list', { tail: LOG_LIMIT });
        if (epoch !== this.logsEpoch) return;
        const rows = result?.records ?? [];
        const through = result?.nextSeq ?? 0;
        const dropped = result?.dropped ?? 0;
        const fresh = buffer.pending.filter((entry) => typeof entry.seq !== 'number' || entry.seq > through);
        const next = rows.concat(fresh);
        if (next.length > LOG_LIMIT) {
          this.logsTrimmed += next.length - LOG_LIMIT;
          next.splice(0, next.length - LOG_LIMIT);
        }
        buffer.pending = [];
        this.logs = next;
        if (dropped > 0) this.logsDropped = dropped;
      } catch {
        // 回放失败不阻塞实时流。
      } finally {
        buffer.seeded = true;
      }
    },

    clearLogs() {
      // 递增代号作废在途回放；后端环形缓冲一并清掉，否则重新订阅（刷新页面）
      // 时清掉的行会被回放拉回来。
      this.logsEpoch += 1;
      const buffer = logBuffers.get(this);
      if (buffer) {
        buffer.pending = [];
        if (buffer.timer) clearTimeout(buffer.timer);
        buffer.timer = undefined;
      }
      this.logs = [];
      this.logsDropped = 0;
      this.logsTrimmed = 0;
      Promise.resolve()
        .then(() => backend.call('log.clear'))
        .catch(() => {
          // 后端清理失败不影响界面清空；下次回放最多把已看过的行带回来。
        });
    },

    // vConsole 开关：后端异步回推结果（vconsole_result），这里只负责发出请求
    // 与标记进行中的方向，结果由 listen() 里的监听器落定。
    async setVConsole(enable: boolean) {
      this.vconsolePhase = enable ? 'enable' : 'disable';
      this.vconsoleError = '';
      // 兜底定时器必须在请求之前装好：bridge.call 没有超时，RPC 悬挂时它是让
      // 按钮不永远停在「开启中…」的唯一出路。
      clearVConsoleTimer(this);
      vconsoleTimers.set(this, setTimeout(() => { this.vconsolePhase = ''; }, VCONSOLE_SETTLE_MS));
      try {
        await backend.call('engine.vconsole', { enable });
      } catch (error) {
        clearVConsoleTimer(this);
        this.vconsolePhase = '';
        this.vconsoleError = messageOf(error);
      }
    },

    // 按序号增量拉取小程序 console：只取上次之后的新行，不合并事件流，所以
    // 视图里不会出现重复行。
    async pollConsole() {
      if (consolePollsInFlight.has(this)) return;
      consolePollsInFlight.add(this);
      const epoch = this.consoleEpoch;
      try {
        for (let page = 0; page < CONSOLE_MAX_PAGES; page += 1) {
          const result = await backend.call<{
            records?: Array<{ seq: number; record: ConsoleRecord }>;
            nextSeq?: number;
            hasMore?: boolean;
            dropped?: number;
          }>('console.list', { afterSeq: this.consoleSeq, limit: CONSOLE_PAGE_SIZE });
          // 期间用户点了清空：这一批属于被清掉的那段，丢弃（序号也不能回退）。
          if (epoch !== this.consoleEpoch) return;
          this.consoleError = '';
          // 只收序号在标记之后的记录：后端理论上不会重发已读过的序号，但客户端
          // 不能把「不重复」当成对方的承诺 —— 一旦重发（或与清空竞态），界面就会
          // 出现整段重复行。
          const records = (result.records ?? [])
            .filter((entry) => typeof entry.seq === 'number' && entry.seq > this.consoleSeq)
            .map((entry): ConsoleRecord => ({ ...entry.record, seq: entry.seq }))
            .filter((record) => record != null);
          if (records.length > 0) {
            const next = this.consoleRecords.concat(records);
            if (next.length > CONSOLE_LIMIT) next.splice(0, next.length - CONSOLE_LIMIT);
            this.consoleRecords = next;
          }
          // 缺口只在「本轮真的读到新记录」时计入：与上面同一原则 —— 后端重发同
          // 一批（未推进 nextSeq）时也会重报同一个缺口，累加会虚增。
          if (records.length > 0 && typeof result.dropped === 'number' && result.dropped > 0) {
            this.consoleDropped += result.dropped;
          }
          if (typeof result.nextSeq === 'number' && result.nextSeq > this.consoleSeq) {
            this.consoleSeq = result.nextSeq;
          }
          if (result.hasMore !== true) break;
        }
      } catch (error) {
        this.consoleError = messageOf(error);
      } finally {
        consolePollsInFlight.delete(this);
      }
    },

    // 清空后端缓冲与页面侧缓冲，并把本地标记归零：环形缓冲清空后旧序号不会
    // 再出现，因此归零不会把已删的行拉回来。
    async clearConsole() {
      await backend.call('console.clear');
      // 递增代号让在途轮询的响应作废：否则刚清掉的行会被 concat 回来。
      this.consoleEpoch += 1;
      this.consoleRecords = [];
      this.consoleSeq = 0;
      this.consoleDropped = 0;
      this.consoleError = '';
    },

    async load() {
      try {
        this.error = '';
        const config = await backend.call<EngineConfig>('config.load');
        const cdpPort = config.cdp_port === undefined ? DEFAULT_CDP_PORT : normalizeCdpPort(config.cdp_port);
        const status = await backend.call<EngineStatus>('engine.status');
        this.cdpPort = cdpPort;
        this.status = { ...status, wechatRunning: this.status.wechatRunning };
      } catch (error) {
        this.error = messageOf(error);
        this.errorRetryable = retryableOf(error);
        this.lastFailed = 'load';
      }
    },

    async refreshStatus() {
      try {
        this.error = '';
        const status = await backend.call<EngineStatus>('engine.status');
        this.status = { ...status, wechatRunning: this.status.wechatRunning };
      } catch (error) {
        this.error = messageOf(error);
      }
    },

    // 重试上一次失败的动作：错误提示上的「重试」应当重放同一个动作，而不是
    // 一律当成「启动引擎」。
    async retryFailed() {
      const action = this.lastFailed;
      this.error = '';
      this.errorRetryable = false;
      this.lastFailed = '';
      switch (action) {
        case 'load':
          return this.load();
        case 'stop':
          return this.stop();
        case 'start':
          return this.start();
        default:
          return;
      }
    },

    async loadWeChatStatus() {
      if (this.wechatStatusLoading) return;
      this.wechatStatusLoading = true;
      try {
        const result = await backend.call<{ running?: boolean; error?: string; pid?: number; version?: number; path?: string; addressTable?: boolean; note?: string }>('wechat.status');
        this.status.wechatRunning = result.running === true;
        this.wechatStatusError = typeof result.error === 'string' ? result.error : '';
        this.wechatHost = result.running === true
          ? {
            pid: result.pid ?? 0,
            version: result.version ?? 0,
            path: result.path ?? '',
            addressTable: result.addressTable === true,
            note: result.note ?? '',
          }
          : { pid: 0, version: 0, path: '', addressTable: false, note: '' };
      } catch (error) {
        this.status.wechatRunning = false;
        this.wechatHost = { pid: 0, version: 0, path: '', addressTable: false, note: '' };
        this.wechatStatusError = messageOf(error);
      } finally {
        this.wechatStatusLoading = false;
      }
    },

    async start() {
      this.starting = true;
      this.error = '';
      try {
        const port = normalizeCdpPort(this.cdpPort);
        // 只发端口：Core 的 engine.start 只接受 cdpPort（debug_main/debug_frida
        // 参数没有任何一层读取）。
        await backend.call('engine.start', { cdp_port: port });
        try {
          await backend.call('config.save', { cdp_port: port });
        } catch (error) {
          this.error = `引擎已启动，端口未能写入配置：${messageOf(error)}`;
        }
        this.status = { ...await backend.call<EngineStatus>('engine.status'), wechatRunning: this.status.wechatRunning };
      } catch (error) {
        this.error = messageOf(error);
        this.errorRetryable = retryableOf(error);
        this.lastFailed = 'start';
      } finally {
        this.starting = false;
      }
    },

    async stop() {
      this.stopping = true;
      this.error = '';
      try {
        await backend.call('engine.stop');
        this.status = { ...stoppedStatus(), wechatRunning: this.status.wechatRunning };
        // 引擎停止后捕获必然结束，避免 UI 停留在「捕获中」
        this.cloudCapturing = false;
        this.wxapiCapturing = false;
        // 停止后页面 realm 一并消失，vConsole 的真实状态不再可知。
        clearVConsoleTimer(this);
        this.vconsoleEnabled = null;
        this.vconsolePhase = '';
        this.vconsoleError = '';
      } catch (error) {
        this.error = messageOf(error);
        this.errorRetryable = retryableOf(error);
        this.lastFailed = 'stop';
      } finally {
        this.stopping = false;
      }
    },
  },
});
