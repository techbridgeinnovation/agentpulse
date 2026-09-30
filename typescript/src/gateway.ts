// The connection an agent uses to reach Agent Pulse: one gRPC-web call over fetch, with an api key and its secret on every call.
//
// gRPC-web puts a call's outcome in a trailer frame at the end of the body, and a refusal before any reply puts it in the headers instead, still under HTTP 200. So a call succeeds only when a status of zero was actually read from one of the two, and HTTP 200 on its own means nothing.

import { VERSION } from "./version.ts";

// The canonical gRPC status codes, named as grpc-go names them, which is how the Go and Python recorders write a transport failure's code onto a record.
export const CODE_NAMES: Record<number, string> = {
  0: "OK",
  1: "Canceled",
  2: "Unknown",
  3: "InvalidArgument",
  4: "DeadlineExceeded",
  5: "NotFound",
  6: "AlreadyExists",
  7: "PermissionDenied",
  8: "ResourceExhausted",
  9: "FailedPrecondition",
  10: "Aborted",
  11: "OutOfRange",
  12: "Unimplemented",
  13: "Internal",
  14: "Unavailable",
  15: "DataLoss",
  16: "Unauthenticated",
};

const DEADLINE_EXCEEDED = 4;
const UNKNOWN = 2;
const INTERNAL = 13;
const UNAVAILABLE = 14;

// What an HTTP status means when the reply carried no gRPC status at all, as the gRPC-over-HTTP mapping has it.
const HTTP_TO_CODE: Record<number, number> = { 400: 13, 401: 16, 403: 7, 404: 12, 429: 14, 502: 14, 503: 14, 504: 14 };

// A reply is a small message and a trailer. Anything larger is not a reply from the gateway.
const MAX_REPLY = 4 << 20;

const LOOPBACK = new Set(["localhost", "127.0.0.1", "::1", "[::1]"]);

// Where the gateway reads the key's name and its secret. Two headers rather than HTTP basic authentication, because the gateway keeps the Authorization header for platform identity.
export const API_KEY_HEADER = "x-api-key";
export const API_SECRET_HEADER = "x-api-secret";

/** A call that did not succeed, by its gRPC code. The status message is never kept: a library that never holds error text cannot be the one that leaks it. */
export class RpcError extends Error {
  readonly code: number;
  constructor(code: number) {
    super(CODE_NAMES[code] ?? "Unknown");
    this.name = "RpcError";
    this.code = code;
  }
  get codeName(): string {
    return CODE_NAMES[this.code] ?? "Unknown";
  }
}

/** A setting the recorder cannot run without is missing or malformed. Thrown where the recorder is set up and nowhere later, so a wrong setting stops the process where a person is watching it start. */
export class ConfigError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ConfigError";
  }
}

/**
 * The base URL of the gateway, from whatever a person pasted.
 *
 * A bare host is taken as HTTPS, because the gateway serves on 443 and a bare host is what its URL shows. Plain HTTP is accepted only for this machine, so a test can reach a local gateway and a key can never travel in clear to anywhere else.
 */
export function gatewayUrl(address: string): string {
  let target = address.trim().replace(/\/+$/, "");
  if (!target) throw new ConfigError("agentpulse: the gateway address is empty");
  if (!/^https?:\/\//.test(target)) target = "https://" + target;
  let url: URL;
  try {
    url = new URL(target);
  } catch {
    throw new ConfigError("agentpulse: the gateway address is not a URL");
  }
  if (url.protocol === "http:" && !LOOPBACK.has(url.hostname)) {
    throw new ConfigError("agentpulse: plain http is only allowed to this machine; use https for the gateway");
  }
  return url.origin;
}

/** The organisation a key was issued for, e.g. `organisations/acme` from `organisations/acme/apiKeys/k1`, or an empty string when the value is not a key's name. */
export function organisationOfKey(key: string): string {
  const k = key.trim();
  const at = k.indexOf("/apiKeys/");
  if (at < 0) return "";
  const organisation = k.slice(0, at);
  if (!organisation.startsWith("organisations/") || organisation.split("/").length !== 2) return "";
  return organisation;
}

function codeOf(header: string | null): number {
  const n = Number.parseInt(header ?? "", 10);
  return Number.isNaN(n) ? UNKNOWN : n;
}

/** The reply message and the status the trailer frame carried, if it carried one. */
function frames(payload: Uint8Array): [Uint8Array, number | undefined] {
  let reply: Uint8Array = new Uint8Array();
  let status: number | undefined;
  let at = 0;
  while (at + 5 <= payload.length) {
    const flags = payload[at]!;
    const length = new DataView(payload.buffer, payload.byteOffset + at + 1, 4).getUint32(0, false);
    const start = at + 5;
    const end = start + length;
    if (end > payload.length) throw new RpcError(INTERNAL);
    const frame = payload.subarray(start, end);
    if (flags & 0x80) {
      for (const line of new TextDecoder("latin1").decode(frame).split("\r\n")) {
        const colon = line.indexOf(":");
        if (colon > 0 && line.slice(0, colon).trim().toLowerCase() === "grpc-status") status = codeOf(line.slice(colon + 1).trim());
      }
    } else {
      reply = frame;
    }
    at = end;
  }
  return [reply, status];
}

/** TLS to the gateway, presenting a key and its secret as a pair on every call. Lazy: nothing touches the network until the first call. */
export class Gateway {
  readonly key: string;
  readonly organisation: string;
  private readonly base: string;
  private readonly headers: Record<string, string>;

  constructor(address: string, key: string, secret: string) {
    if (!address?.trim()) throw new ConfigError("agentpulse: the gateway address is required");
    if (!key?.trim()) throw new ConfigError("agentpulse: the api key is required");
    if (!secret?.trim()) throw new ConfigError("agentpulse: the api secret is required");
    this.base = gatewayUrl(address);
    this.key = key.trim();
    this.organisation = organisationOfKey(this.key);
    this.headers = {
      [API_KEY_HEADER]: this.key,
      [API_SECRET_HEADER]: secret.trim(),
      "x-user-agent": `agentpulse-typescript/${VERSION}`,
      "content-type": "application/grpc-web+proto",
      accept: "application/grpc-web+proto",
      "x-grpc-web": "1",
    };
  }

  /** The gateway named by `AP_GATEWAY`, `AP_API_KEY` and `AP_API_SECRET`, all of which are required. */
  static fromEnv(env: Record<string, string | undefined> = process.env): Gateway {
    return new Gateway(env.AP_GATEWAY ?? "", env.AP_API_KEY ?? "", env.AP_API_SECRET ?? "");
  }

  /** One unary call, e.g. `/techbridge.ap.metering.v1.ActivitiesService/BatchCreateActivities`. Rejects with RpcError for every way a call can fail, a network failure included. */
  async call(method: string, message: Uint8Array, timeoutMs: number): Promise<Uint8Array> {
    const body = new Uint8Array(5 + message.length);
    new DataView(body.buffer).setUint32(1, message.length, false);
    body.set(message, 5);
    let response: Response;
    let payload: Uint8Array;
    try {
      response = await fetch(this.base + method, {
        method: "POST",
        headers: { ...this.headers, "grpc-timeout": `${Math.max(1, Math.floor(timeoutMs))}m` },
        body,
        signal: AbortSignal.timeout(timeoutMs),
        redirect: "error",
      });
      const buffer = await response.arrayBuffer();
      if (buffer.byteLength > MAX_REPLY) throw new RpcError(INTERNAL);
      payload = new Uint8Array(buffer);
    } catch (err) {
      if (err instanceof RpcError) throw err;
      if (err instanceof Error && (err.name === "TimeoutError" || err.name === "AbortError")) throw new RpcError(DEADLINE_EXCEEDED);
      throw new RpcError(UNAVAILABLE);
    }
    const statusHeader = response.headers.get("grpc-status");
    if (response.status !== 200) {
      throw new RpcError(statusHeader !== null ? codeOf(statusHeader) : HTTP_TO_CODE[response.status] ?? UNKNOWN);
    }
    if (statusHeader !== null && codeOf(statusHeader) !== 0) throw new RpcError(codeOf(statusHeader));
    const [reply, trailer] = frames(payload);
    const status = trailer ?? (statusHeader !== null ? codeOf(statusHeader) : undefined);
    if (status === undefined) throw new RpcError(INTERNAL);
    if (status !== 0) throw new RpcError(status);
    return reply;
  }
}
