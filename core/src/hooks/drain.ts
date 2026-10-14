/**
 * Reference model for the page-side drain protocol.
 *
 * The miniapp hooks (`core/hooks/wxapi.js`, `core/hooks/cloud.js`,
 * `core/hooks/console.js`) ship as hand-written ES5 that cannot import this
 * module, so they re-implement the contract; `src/hooks/wxapi-hook.test.ts`,
 * `cloud-hook-vm.test.ts` and `console-hook-vm.test.ts` execute the shipped
 * files in a VM and assert they match this model.
 *
 * Records are buffered with monotonically increasing `seq` numbers inside a
 * bounded queue. `drain(afterSeq, limit)` returns at most `limit` records with
 * `seq > afterSeq` in sequence order; the caller acknowledges by passing the
 * returned `nextSeq` on the next drain, so acknowledged records are never
 * resent. When the buffer exceeds its capacity the oldest records are evicted
 * — memory stays bounded even if the consumer stops draining. Each drain
 * counts unread sequence gaps (`dropped`); evicting already acknowledged
 * entries is normal retention, not data loss. Retries do not recount gaps.
 *
 * `UpdateQueue` is the same protocol for the wxapi hook's second stream: a
 * record that settles *after* it was already drained (async success/fail
 * landing post-ack) appends an update frame instead of mutating a record the
 * consumer has already consumed. Updates carry their own sequence and ack, so
 * they can never be confused with the record stream.
 */

export type DrainedRecord<T> = {
  seq: number;
  item: T;
};

export type DrainPage<T> = {
  records: Array<DrainedRecord<T>>;
  nextSeq: number;
  hasMore: boolean;
};

export class DrainQueue<T> {
  private nextSeqValue = 0;
  private droppedValue = 0;
  private accountedSeq = 0;
  private readonly buffer: Array<DrainedRecord<T>> = [];

  constructor(private readonly capacity: number) {
    if (!Number.isInteger(capacity) || capacity < 1) {
      throw new Error("DrainQueue capacity must be a positive integer");
    }
  }

  /**
   * Cumulative unread gaps observed by drain(). The page hooks expose the
   * same number as `droppedRecords`.
   */
  get dropped(): number {
    return this.droppedValue;
  }

  push(item: T): number {
    this.nextSeqValue += 1;
    this.buffer.push({ seq: this.nextSeqValue, item });
    while (this.buffer.length > this.capacity) {
      this.buffer.shift();
    }
    return this.nextSeqValue;
  }

  drain(afterSeq: number, limit: number): DrainPage<T> {
    if (!Number.isInteger(afterSeq) || afterSeq < 0) {
      throw new Error("afterSeq must be a non-negative integer");
    }
    if (!Number.isInteger(limit) || limit < 1) {
      throw new Error("limit must be a positive integer");
    }
    const floor = Math.max(afterSeq, this.accountedSeq);
    const evictedThrough = this.buffer[0]?.seq === undefined ? this.nextSeqValue : this.buffer[0].seq - 1;
    this.droppedValue += Math.max(0, evictedThrough - floor);
    this.accountedSeq = Math.max(floor, evictedThrough);

    const records: Array<DrainedRecord<T>> = [];
    let nextSeq = afterSeq;
    let hasMore = false;
    for (const record of this.buffer) {
      if (record.seq <= afterSeq) {
        continue;
      }
      if (records.length === limit) {
        hasMore = true;
        break;
      }
      records.push(record);
      nextSeq = record.seq;
    }
    return { records, nextSeq, hasMore };
  }
}

export type DrainedUpdate<T> = {
  seq: number;
  update: T;
};

export type UpdateDrainPage<T> = {
  updates: Array<DrainedUpdate<T>>;
  nextUpdateSeq: number;
  hasMore: boolean;
};

/**
 * Same seq+ack protocol as `DrainQueue`, for the wxapi hook's update stream.
 * `drainUpdates` additionally accepts limit 0, meaning "read nothing and leave
 * the cursor where it is" — the shell's record-only callers pass it.
 */
export class UpdateQueue<T> {
  private nextSeqValue = 0;
  private droppedValue = 0;
  private accountedSeq = 0;
  private readonly buffer: Array<DrainedUpdate<T>> = [];

  constructor(private readonly capacity: number) {
    if (!Number.isInteger(capacity) || capacity < 1) {
      throw new Error("UpdateQueue capacity must be a positive integer");
    }
  }

  /**
   * Cumulative unread gaps observed by drainUpdates(). The page hooks expose
   * the same number as `droppedUpdates`.
   */
  get dropped(): number {
    return this.droppedValue;
  }

  pushUpdate(update: T): number {
    this.nextSeqValue += 1;
    this.buffer.push({ seq: this.nextSeqValue, update });
    while (this.buffer.length > this.capacity) {
      this.buffer.shift();
    }
    return this.nextSeqValue;
  }

  drainUpdates(afterUpdateSeq: number, limit: number): UpdateDrainPage<T> {
    if (!Number.isInteger(afterUpdateSeq) || afterUpdateSeq < 0) {
      throw new Error("afterUpdateSeq must be a non-negative integer");
    }
    if (!Number.isInteger(limit) || limit < 0) {
      throw new Error("limit must be a non-negative integer");
    }

    const updates: Array<DrainedUpdate<T>> = [];
    let nextUpdateSeq = afterUpdateSeq;
    let hasMore = false;
    // limit 0 means "do not read the update stream": the cursor does not move,
    // so a later call with a real limit still sees these updates.
    if (limit === 0) {
      return { updates, nextUpdateSeq, hasMore };
    }
    const floor = Math.max(afterUpdateSeq, this.accountedSeq);
    const evictedThrough = this.buffer[0]?.seq === undefined ? this.nextSeqValue : this.buffer[0].seq - 1;
    this.droppedValue += Math.max(0, evictedThrough - floor);
    this.accountedSeq = Math.max(floor, evictedThrough);
    for (const update of this.buffer) {
      if (update.seq <= afterUpdateSeq) {
        continue;
      }
      if (updates.length === limit) {
        hasMore = true;
        break;
      }
      updates.push(update);
      nextUpdateSeq = update.seq;
    }
    return { updates, nextUpdateSeq, hasMore };
  }
}
