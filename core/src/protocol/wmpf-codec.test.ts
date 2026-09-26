import { Buffer } from "node:buffer";
import { deflateSync } from "node:zlib";

import { Field, Type } from "protobufjs";
import { describe, expect, it } from "vitest";

import { decodeCdpMessage, encodeCdpMessage } from "./wmpf-codec.js";

const envelopeType = new Type("WARemoteDebug_DebugMessage")
  .add(new Field("seq", 1, "uint32"))
  .add(new Field("after", 2, "uint32"))
  .add(new Field("category", 3, "string"))
  .add(new Field("data", 4, "bytes"))
  .add(new Field("compressAlgo", 5, "uint32"))
  .add(new Field("originalSize", 6, "uint32"));

describe("WMPF CDP codec", () => {
  it("round-trips an uncompressed DevTools command", () => {
    const frame = encodeCdpMessage({
      sequence: 80001,
      category: "chromeDevtools",
      operationId: 12,
      payload: '{"id":1,"method":"Runtime.enable"}',
      jsContextId: "",
    });

    expect(decodeCdpMessage(frame)).toEqual({
      sequence: 80001,
      category: "chromeDevtools",
      operationId: 12,
      payload: '{"id":1,"method":"Runtime.enable"}',
      jsContextId: "",
    });
  });

  it("inflates a compressed DevTools result before decoding it", () => {
    const frame = encodeCdpMessage({
      sequence: 99,
      category: "chromeDevtoolsResult",
      operationId: 13,
      payload: '{"id":1,"result":{"value":true}}',
      jsContextId: "7",
      compress: true,
    });

    expect(decodeCdpMessage(frame)).toEqual({
      sequence: 99,
      category: "chromeDevtoolsResult",
      operationId: 13,
      payload: '{"id":1,"result":{"value":true}}',
      jsContextId: "7",
    });
  });

  it("recognises setupContext envelopes as an identity-probe trigger", () => {
    const frame = encodeCdpMessage({
      sequence: 5,
      category: "setupContext",
      operationId: 0,
      payload: "",
      jsContextId: "",
    });

    expect(decodeCdpMessage(frame)).toMatchObject({ sequence: 5, category: "setupContext" });
  });
});

// The envelope's originalSize field is written by the sender, so a hand-built
// frame can understate it: only enforcing the cap while inflating keeps a 1 KB
// frame from expanding into hundreds of megabytes outside the V8 heap.
function lyingBombFrame(expandedBytes: number, declaredSize: number): Buffer {
  const data = deflateSync(Buffer.alloc(expandedBytes));
  return Buffer.from(
    envelopeType
      .encode({
        seq: 1,
        category: "chromeDevtools",
        data,
        compressAlgo: 1,
        originalSize: declaredSize,
      })
      .finish(),
  );
}

describe("WMPF CDP codec decompression bomb", () => {
  it("bounds inflation while decoding instead of trusting originalSize", () => {
    const frame = lyingBombFrame(1 << 20, 1);

    expect(decodeCdpMessage(frame, { maxPayloadBytes: 4 << 10 })).toBeUndefined();
  });

  it("still inflates a payload that fits the cap", () => {
    const frame = encodeCdpMessage({
      sequence: 7,
      category: "chromeDevtools",
      operationId: 3,
      payload: '{"id":1,"method":"Runtime.enable"}',
      jsContextId: "",
      compress: true,
    });

    expect(decodeCdpMessage(frame, { maxPayloadBytes: 4 << 10 })).toMatchObject({
      sequence: 7,
      payload: '{"id":1,"method":"Runtime.enable"}',
    });
  });

  it("reports a corrupt compressed stream as a bad frame instead of throwing", () => {
    const frame = Buffer.from(
      envelopeType
        .encode({
          seq: 2,
          category: "chromeDevtools",
          data: Buffer.from([0xff, 0xff, 0xff]),
          compressAlgo: 1,
          originalSize: 4,
        })
        .finish(),
    );

    expect(decodeCdpMessage(frame, { maxPayloadBytes: 4 << 10 })).toBeUndefined();
  });

  it("refuses a compressed payload that expands beyond the size cap", () => {
    const bomb = Buffer.alloc(65 * 1024 * 1024, 0);
    const frame = encodeCdpMessage({
      sequence: 1,
      category: "chromeDevtools",
      operationId: 1,
      payload: bomb.toString("latin1"),
      jsContextId: "",
      compress: true,
    });

    expect(decodeCdpMessage(frame)).toBeUndefined();
  });
});
