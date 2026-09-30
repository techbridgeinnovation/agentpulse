// A failure reduced to its code. The message is never kept: a provider's error text routinely quotes the prompt back.

import { RpcError } from "./gateway.ts";

// The finish reasons that mean a call produced nothing usable, recorded as a failure rather than a cheap success, and those that mean a limit was reached.
const BLOCKED = new Set(["SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "CONTENT_FILTER", "CONTENT-FILTER", "CONTENT_FILTERED", "GUARDRAIL_INTERVENED", "REFUSAL", "MALFORMED_FUNCTION_CALL"]);
const TRUNCATED = new Set(["MAX_TOKENS", "LENGTH", "MAX_OUTPUT_TOKENS"]);

export function blockedFinish(reason: string | undefined): boolean {
  return BLOCKED.has((reason ?? "").trim().toUpperCase());
}

export function truncatedFinish(reason: string | undefined): boolean {
  return TRUNCATED.has((reason ?? "").trim().toUpperCase());
}

/** The one part of an error that is safe to keep: the status the layer that failed gave it, a network code, or a deadline. */
export function errorCode(err: unknown): string {
  if (err instanceof RpcError) return err.codeName;
  if (err === null || typeof err !== "object") return "Unknown";
  const e = err as { name?: unknown; statusCode?: unknown; status?: unknown; code?: unknown; cause?: unknown };
  if (e.name === "TimeoutError" || e.name === "AbortError") return "DeadlineExceeded";
  for (const status of [e.statusCode, e.status]) {
    if (typeof status === "number" && status >= 100 && status < 600) return String(status);
  }
  // A network error's code, such as ECONNRESET, names the failure and nothing else.
  if (typeof e.code === "string" && /^[A-Z][A-Z0-9_]{2,40}$/.test(e.code)) return e.code;
  if (e.cause && e.cause !== err) {
    const inner = errorCode(e.cause);
    if (inner !== "Unknown") return inner;
  }
  return "Unknown";
}

/** A call a spend limit refused before it was sent. */
export class SpendDenied extends Error {
  constructor() {
    super("agentpulse: the call was declined because it would exceed a spend limit");
    this.name = "SpendDenied";
  }
}

/** Whether an error is a call a spend limit refused before it was sent. */
export function denied(err: unknown): boolean {
  for (let e = err; e instanceof Error; e = (e as { cause?: unknown }).cause) {
    if (e instanceof SpendDenied) return true;
  }
  return false;
}
