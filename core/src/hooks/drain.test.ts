import { describe, expect, it } from "vitest";

import { DrainQueue } from "./drain.js";

type Sample = { name: string };

describe("DrainQueue", () => {
  it("assigns monotonically increasing sequence numbers", () => {
    const queue = new DrainQueue<Sample>(10);
    queue.push({ name: "a" });
    queue.push({ name: "b" });

    const page = queue.drain(0, 10);
    expect(page.records.map((record) => record.seq)).toEqual([1, 2]);
    expect(page.records.map((record) => record.item.name)).toEqual(["a", "b"]);
    expect(page.nextSeq).toBe(2);
    expect(page.hasMore).toBe(false);
  });

  it("returns at most limit records in sequence order and reports more", () => {
    const queue = new DrainQueue<Sample>(10);
    for (let i = 0; i < 5; i += 1) {
      queue.push({ name: `n${i}` });
    }

    const first = queue.drain(0, 2);
    expect(first.records.map((record) => record.seq)).toEqual([1, 2]);
    expect(first.hasMore).toBe(true);

    const second = queue.drain(first.nextSeq, 2);
    expect(second.records.map((record) => record.seq)).toEqual([3, 4]);

    const third = queue.drain(second.nextSeq, 2);
    expect(third.records.map((record) => record.seq)).toEqual([5]);
    expect(third.hasMore).toBe(false);
  });

  it("never resends records at or before the acknowledged sequence", () => {
    const queue = new DrainQueue<Sample>(10);
    queue.push({ name: "a" });
    queue.push({ name: "b" });
    const first = queue.drain(0, 10);
    expect(first.records).toHaveLength(2);

    queue.push({ name: "c" });
    const second = queue.drain(first.nextSeq, 10);
    expect(second.records.map((record) => record.item.name)).toEqual(["c"]);
    // Once the consumer acknowledges up to seq 3, nothing is resent.
    expect(queue.drain(second.nextSeq, 10).records).toEqual([]);
  });

  it("keeps memory bounded by evicting the oldest buffered records", () => {
    const queue = new DrainQueue<Sample>(3);
    for (let i = 0; i < 6; i += 1) {
      queue.push({ name: `n${i}` });
    }

    // Sequences keep counting even though the buffer only holds the newest 3.
    const page = queue.drain(0, 10);
    expect(page.records.map((record) => record.item.name)).toEqual(["n3", "n4", "n5"]);
    expect(page.records.map((record) => record.seq)).toEqual([4, 5, 6]);
  });

  it("reports the acknowledged sequence when nothing new is available", () => {
    const queue = new DrainQueue<Sample>(10);
    queue.push({ name: "a" });

    expect(queue.drain(1, 10)).toEqual({ records: [], nextSeq: 1, hasMore: false });
  });
  it("rejects an unusable capacity at construction time", () => {
    expect(() => new DrainQueue<Sample>(0)).toThrow("DrainQueue capacity must be a positive integer");
    expect(() => new DrainQueue<Sample>(2.5)).toThrow("DrainQueue capacity must be a positive integer");
  });

  it("rejects drain arguments the RPC layer could pass through", () => {
    const queue = new DrainQueue<Sample>(4);
    expect(() => queue.drain(-1, 10)).toThrow("afterSeq must be a non-negative integer");
    expect(() => queue.drain(1.5, 10)).toThrow("afterSeq must be a non-negative integer");
    expect(() => queue.drain(0, 0)).toThrow("limit must be a positive integer");
  });
});