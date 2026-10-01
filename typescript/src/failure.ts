// A failure reduced to its code. The message is never kept: a provider's error text routinely quotes the prompt back.

import { RpcError } from "./gateway.ts";

// The reasons a provider gives for a call that produced nothing usable, recorded as a failure rather than a cheap success. The same set the Go and Python recorders hold.
const BLOCKED = new Set([
  "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT",
  "IMAGE_RECITATION", "LANGUAGE", "MALFORMED_FUNCTION_CALL", "UNEXPECTED_TOOL_CALL", "TOO_MANY_TOOL_CALLS",
  "NO_IMAGE", "CONTENT_FILTER", "CONTENT_FILTERED", "GUARDRAIL_INTERVENED", "REFUSAL", "ERROR_TOXIC",
  "MALFORMED_MODEL_OUTPUT", "MALFORMED_TOOL_USE", "OTHER", "IMAGE_OTHER", "MODEL_ARMOR", "JAILBREAK",
  "CONTENT_BLOCKED", "MALFORMED_TOOL_CALL", "MISSING_THOUGHT_SIGNATURE",
  // The AI SDK's own words, for a provider whose reason it did not pass on.
  "CONTENT-FILTER", "ERROR",
]);
// The reasons that mean a limit was reached.
const TRUNCATED = new Set(["MAX_TOKENS", "LENGTH", "MAX_OUTPUT_TOKENS", "MODEL_CONTEXT_WINDOW_EXCEEDED"]);

export function blockedFinish(reason: string | undefined): boolean {
  return BLOCKED.has((reason ?? "").trim().toUpperCase());
}

export function truncatedFinish(reason: string | undefined): boolean {
  return TRUNCATED.has((reason ?? "").trim().toUpperCase());
}

/** The one part of an error that is safe to keep: the status the layer that failed gave it, a network code, or a deadline. */
export function errorCode(err: unknown): string {
  return codeOf(err, 0);
}

// Causes are followed only so far, so a chain that leads back on itself ends.
const MAX_CAUSES = 8;

function codeOf(err: unknown, depth: number): string {
  if (depth > MAX_CAUSES) return "Unknown";
  if (err instanceof RpcError) return err.codeName;
  if (err === null || typeof err !== "object") return "Unknown";
  const e = err as { name?: unknown; statusCode?: unknown; status?: unknown; code?: unknown; cause?: unknown };
  if (e.name === "TimeoutError") return "DeadlineExceeded";
  // An abort is a deadline only where the reason it carries is one, as AbortSignal.timeout gives; otherwise somebody chose to stop.
  if (e.name === "AbortError") return codeOf(e.cause, depth + 1) === "DeadlineExceeded" ? "DeadlineExceeded" : "Canceled";
  for (const status of [e.statusCode, e.status]) {
    if (typeof status === "number" && status >= 100 && status < 600) return String(status);
  }
  // A network error's code, such as ECONNRESET, names the failure and nothing else.
  if (typeof e.code === "string" && /^[A-Z][A-Z0-9_]{2,40}$/.test(e.code)) return e.code;
  if (e.cause && e.cause !== err) {
    const inner = codeOf(e.cause, depth + 1);
    if (inner !== "Unknown") return inner;
  }
  return "Unknown";
}

/** A call its caller stopped before it finished. */
export class Cancelled extends Error {
  constructor() {
    super("agentpulse: the call was cancelled");
    this.name = "AbortError";
  }
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
