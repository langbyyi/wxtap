export type BatchQueue<T> = {
  push: (record: T) => void;
  pushMany: (records: T[]) => void;
  flush: () => void;
  clear: () => void;
  dispose: () => void;
};

export type BatchQueueOptions = {
  delayMs?: number;
  pendingLimit?: number;
  // 单次 apply 最多消费的条数：超出的部分排到后续帧（setTimeout 0）继续，
  // 保证任何单帧的 DOM 工作量有界（F5）。缺省不限制，apply 一次吃完整批。
  applyLimit?: number;
  // 队列饱和（pending 超过 pendingLimit）时丢最旧，通过它把丢弃数告诉调用方。
  onDrop?: (dropped: number) => void;
};

// Coalesces bursts of backend events so each UI list renders at most once per
// interval instead of once per event. The pending queue is bounded to keep a
// noisy backend from growing memory without bound.
export function createBatchQueue<T>(
  apply: (records: T[]) => void,
  options: BatchQueueOptions = {},
): BatchQueue<T> {
  const delayMs = options.delayMs ?? 120;
  const pendingLimit = options.pendingLimit ?? 1000;
  const applyLimit = options.applyLimit ?? Number.POSITIVE_INFINITY;
  const onDrop = options.onDrop;
  let pending: T[] = [];
  // 已出队、等待分帧应用的余量。它一定早于之后入队的记录，所以补进来的
  // 新记录只能挂在它后面，顺序不会被分帧打乱。
  let backlog: T[] = [];
  let timer: ReturnType<typeof setTimeout> | undefined;
  let drainTimer: ReturnType<typeof setTimeout> | undefined;
  let disposed = false;

  function cancelTimer() {
    if (!timer) return;
    clearTimeout(timer);
    timer = undefined;
  }

  function cancelDrain() {
    if (!drainTimer) return;
    clearTimeout(drainTimer);
    drainTimer = undefined;
  }

  function drain() {
    drainTimer = undefined;
    if (disposed || !backlog.length) return;
    const chunk = backlog.length > applyLimit ? backlog.slice(0, applyLimit) : backlog;
    backlog = chunk.length === backlog.length ? [] : backlog.slice(chunk.length);
    apply(chunk);
    if (backlog.length) scheduleDrain();
  }

  function scheduleDrain() {
    if (drainTimer) return;
    drainTimer = setTimeout(drain, 0);
  }

  function flush() {
    cancelTimer();
    if (disposed || !pending.length) return;
    backlog = backlog.length ? backlog.concat(pending) : pending;
    pending = [];
    // 已有排队中的分帧消费时交给它继续，避免同一帧里叠加两次 apply。
    if (drainTimer) return;
    drain();
  }

  function enqueue(records: T[]) {
    if (disposed || !records.length) return;
    pending.push(...records);
    if (pending.length > pendingLimit) {
      const dropped = pending.length - pendingLimit;
      pending.splice(0, dropped);
      onDrop?.(dropped);
    }
    if (!timer) timer = setTimeout(flush, delayMs);
  }

  return {
    push(record) { enqueue([record]); },
    pushMany(records) { enqueue(records); },
    flush,
    clear() {
      pending = [];
      backlog = [];
      cancelTimer();
      cancelDrain();
    },
    dispose() {
      disposed = true;
      pending = [];
      backlog = [];
      cancelTimer();
      cancelDrain();
    },
  };
}
