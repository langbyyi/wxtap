export type RpcId = string | number;

export type RpcRequest = {
  id: RpcId;
  method: string;
  params: Record<string, unknown>;
};

type RpcError = {
  code: number;
  message: string;
  retryable: boolean;
};

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

export function parseRequest(line: string): RpcRequest {
  const value: unknown = JSON.parse(line);
  if (!isObject(value)) {
    throw new Error("request must be an object");
  }
  if (typeof value.id !== "string" && typeof value.id !== "number") {
    throw new Error("id must be a string or number");
  }
  if (typeof value.method !== "string" || value.method.length === 0) {
    throw new Error("method must be a non-empty string");
  }
  if (!isObject(value.params)) {
    throw new Error("params must be an object");
  }
  return { id: value.id, method: value.method, params: value.params };
}

export function serialiseResult(id: RpcId, result: unknown): string {
  return JSON.stringify({ id, result });
}

// An unparseable line has no id to correlate; the error carries null instead.
export function serialiseError(id: RpcId | null, code: number, message: string, retryable: boolean): string {
  const error: RpcError = { code, message, retryable };
  return JSON.stringify({ id, error });
}
