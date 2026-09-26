import { describe, expect, it, vi } from 'vitest';
import { createBatchQueue } from './batch';

describe('createBatchQueue', () => {
  it('coalesces a burst into one flush', () => {
    vi.useFakeTimers();
    const applied: number[][] = [];
    const queue = createBatchQueue<number>((records) => applied.push(records), { delayMs: 50 });

    queue.push(1);
    queue.pushMany([2, 3]);
    expect(applied).toEqual([]);

    vi.advanceTimersByTime(50);
    expect(applied).toEqual([[1, 2, 3]]);
    vi.useRealTimers();
  });

  it('keeps only the newest pending records when the queue is saturated', () => {
    vi.useFakeTimers();
    const applied: number[][] = [];
    const queue = createBatchQueue<number>((records) => applied.push(records), { delayMs: 50, pendingLimit: 2 });

    queue.pushMany([1, 2, 3, 4]);
    vi.advanceTimersByTime(50);

    expect(applied).toEqual([[3, 4]]);
    vi.useRealTimers();
  });

  it('drops pending records on clear', () => {
    vi.useFakeTimers();
    const applied: number[][] = [];
    const queue = createBatchQueue<number>((records) => applied.push(records), { delayMs: 50 });

    queue.push(1);
    queue.clear();
    vi.advanceTimersByTime(50);

    expect(applied).toEqual([]);
    vi.useRealTimers();
  });

  it('ignores records queued after dispose', () => {
    vi.useFakeTimers();
    const applied: number[][] = [];
    const queue = createBatchQueue<number>((records) => applied.push(records), { delayMs: 50 });

    queue.dispose();
    queue.push(1);
    vi.advanceTimersByTime(50);

    expect(applied).toEqual([]);
    vi.useRealTimers();
  });

  it('reports how many records it dropped when the queue is saturated', () => {
    vi.useFakeTimers();
    const applied: number[][] = [];
    const dropped: number[] = [];
    const queue = createBatchQueue<number>((records) => applied.push(records), {
      delayMs: 50,
      pendingLimit: 2,
      onDrop: (count) => dropped.push(count),
    });

    queue.pushMany([1, 2, 3, 4]);
    vi.advanceTimersByTime(50);

    expect(applied).toEqual([[3, 4]]);
    expect(dropped).toEqual([2]);
    vi.useRealTimers();
  });

  it('applies an oversized batch one frame at a time', async () => {
    vi.useFakeTimers();
    const applied: number[][] = [];
    const queue = createBatchQueue<number>((records) => applied.push(records), { delayMs: 50, applyLimit: 2 });

    queue.pushMany([1, 2, 3, 4, 5]);
    // flush 是同步的：第一帧只应用 applyLimit 条，其余排到后续帧
    queue.flush();
    expect(applied).toEqual([[1, 2]]);

    await vi.advanceTimersByTimeAsync(10);
    expect(applied).toEqual([[1, 2], [3, 4], [5]]);
    vi.useRealTimers();
  });

  it('drops the queued frames along with the pending records on clear', async () => {
    vi.useFakeTimers();
    const applied: number[][] = [];
    const queue = createBatchQueue<number>((records) => applied.push(records), { delayMs: 50, applyLimit: 2 });

    queue.pushMany([1, 2, 3, 4, 5]);
    queue.flush();
    expect(applied).toEqual([[1, 2]]);

    queue.clear();
    await vi.advanceTimersByTimeAsync(100);
    expect(applied).toEqual([[1, 2]]);
    vi.useRealTimers();
  });
});
