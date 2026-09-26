import { deflateSync, inflateSync } from "node:zlib";

import { Field, Type } from "protobufjs";

const debugMessageType = new Type("WARemoteDebug_DebugMessage")
  .add(new Field("seq", 1, "uint32"))
  .add(new Field("after", 2, "uint32"))
  .add(new Field("category", 3, "string"))
  .add(new Field("data", 4, "bytes"))
  .add(new Field("compressAlgo", 5, "uint32"))
  .add(new Field("originalSize", 6, "uint32"));

const chromeDevtoolsType = new Type("WARemoteDebug_ChromeDevtools")
  .add(new Field("opId", 1, "uint64"))
  .add(new Field("payload", 2, "string"))
  .add(new Field("jscontextId", 3, "string"));

const chromeDevtoolsResultType = new Type("WARemoteDebug_ChromeDevtoolsResult")
  .add(new Field("opId", 1, "uint64"))
  .add(new Field("payload", 2, "string"))
  .add(new Field("jscontextId", 3, "string"));

// Cap on the decompressed payload size; real DevTools traffic stays far
// below this, a bomb like a 1 MB frame inflating to 1 GB is rejected.
const maxDecodedPayload = 64 * 1024 * 1024;

type CdpCategory = "chromeDevtools" | "chromeDevtoolsResult" | "setupContext";

export type CdpMessage = {
  sequence: number;
  category: CdpCategory;
  operationId: number;
  payload: string;
  jsContextId: string;
  compress?: boolean;
};

type DecodedMessage = {
  seq?: number;
  category?: string;
  data?: Uint8Array;
  compressAlgo?: number;
  originalSize?: number;
};

type DecodedCdpPayload = {
  opId?: number | { toString(): string };
  payload?: string;
  jscontextId?: string;
};

export function encodeCdpMessage(message: CdpMessage): Buffer {
  const payloadType = message.category === "chromeDevtools" ? chromeDevtoolsType : chromeDevtoolsResultType;
  const payload = Buffer.from(payloadType.encode({
    opId: message.operationId,
    payload: message.payload,
    jscontextId: message.jsContextId,
  }).finish());
  const data = message.compress ? deflateSync(payload) : payload;
  return Buffer.from(debugMessageType.encode({
    seq: message.sequence,
    category: message.category,
    data,
    compressAlgo: message.compress ? 1 : 0,
    originalSize: message.compress ? payload.length : 0,
  }).finish());
}

export type DecodeOptions = {
  /** Inflated-payload cap; tests lower it so a bomb stays small. */
  maxPayloadBytes?: number;
};

export function decodeCdpMessage(
  frame: Uint8Array,
  options: DecodeOptions = {},
): Omit<CdpMessage, "compress"> | undefined {
  const maxPayload = options.maxPayloadBytes ?? maxDecodedPayload;
  const envelope = debugMessageType.decode(frame) as DecodedMessage;
  if (envelope.category === "setupContext") {
    // The miniapp announces its context with this category; only the
    // category itself matters to the bridge (identity probing), the
    // encapsulated payload is not a ChromeDevtools structure.
    return {
      sequence: envelope.seq ?? 0,
      category: "setupContext",
      operationId: 0,
      payload: "",
      jsContextId: "",
    };
  }
  if (envelope.category !== "chromeDevtools" && envelope.category !== "chromeDevtoolsResult") {
    return undefined;
  }
  const encodedPayload = Buffer.from(envelope.data ?? []);
  const compressed = ((envelope.compressAlgo ?? 0) & 1) === 1;
  // Decompression bombs: a small frame can expand to gigabytes (inflateSync
  // grows Buffers outside the V8 heap). The announced size is only a hint
  // from the sender, so the cap is enforced while inflating.
  if (compressed && (envelope.originalSize ?? 0) > maxPayload) {
    return undefined;
  }
  const payload = compressed ? inflateCapped(encodedPayload, maxPayload) : encodedPayload;
  if (payload === undefined || payload.length > maxPayload) {
    return undefined;
  }
  const payloadType = envelope.category === "chromeDevtools" ? chromeDevtoolsType : chromeDevtoolsResultType;
  const decoded = payloadType.decode(payload) as DecodedCdpPayload;
  return {
    sequence: envelope.seq ?? 0,
    category: envelope.category,
    operationId: toNumber(decoded.opId),
    payload: decoded.payload ?? "",
    jsContextId: decoded.jscontextId ?? "",
  };
}

/**
 * Inflates a compressed payload with the limit applied as it is written, so a
 * sender that understates originalSize cannot make this process allocate an
 * unbounded buffer. Corrupt streams decode to undefined like any other bad
 * frame instead of throwing at the caller.
 */
function inflateCapped(data: Buffer, maxPayload: number): Buffer | undefined {
  try {
    return inflateSync(data, { maxOutputLength: maxPayload });
  } catch {
    return undefined;
  }
}
function toNumber(value: DecodedCdpPayload["opId"]): number {
  if (typeof value === "number") {
    return value;
  }
  return value === undefined ? 0 : Number.parseInt(value.toString(), 10);
}
