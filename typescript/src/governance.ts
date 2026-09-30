// Asking governance whether a call may proceed, before it is made.
//
// Only a genuine DENY stops a call. Every other answer, and no answer at all (governance unreachable, slow, refusing the question, or saying something this version does not know), lets it go ahead, because a spend decision that cannot be made must not be the reason the product stops working. Each of those is counted, so a degraded governance is visible rather than silent.

import type { Gateway } from "./gateway.ts";
import {
  DECISION_ALLOW,
  DECISION_DENY,
  DECISION_DOWNGRADE,
  DECISION_NOTIFY,
  type DecideRequest,
  type DecideResponse,
  decodeDecideResponse,
  encodeDecideRequest,
} from "./wire.ts";

const DECIDE = "/techbridge.ap.governance.v1.DecisionsService/Decide";

/** How long a call waits for an answer before it goes ahead without one. */
export const DEFAULT_DECIDE_TIMEOUT_MS = 1500;

/** How long a refusal is still honoured when governance does not answer. A budget that ran out rarely comes back within the hour, and a check slowed by governance waking from idle must not let the call through. */
export const REFUSAL_WINDOW_MS = 3_600_000;

/** How long a question that just failed is not asked again, so an outage costs one timeout per question per interval rather than every call the whole timeout. */
export const DEFAULT_FAILURE_BACKOFF_MS = 5000;

const MAX_CACHED_VERDICTS = 4096;

export type Decision = "ALLOW" | "NOTIFY" | "DOWNGRADE" | "DENY" | "UNDECIDED";

const NAMES: Record<number, Decision> = {
  [DECISION_ALLOW]: "ALLOW",
  [DECISION_NOTIFY]: "NOTIFY",
  [DECISION_DOWNGRADE]: "DOWNGRADE",
  [DECISION_DENY]: "DENY",
};

/** What governance decided about one call. */
export interface Verdict {
  decision: Decision;
  /** Why, as governance put it. Its own words, never a provider's. */
  reason: string;
  /** For a DOWNGRADE, the provider and model to call instead. */
  replacementProvider: string;
  replacementModel: string;
  /** False only for a genuine DENY. */
  proceed: boolean;
}

export function verdict(decision: Decision = "UNDECIDED", fields: Partial<Omit<Verdict, "decision" | "proceed">> = {}): Verdict {
  return { decision, reason: "", replacementProvider: "", replacementModel: "", ...fields, proceed: decision !== "DENY" };
}

/** Asks governance for a verdict. Rejects on any failure; the caller treats a failure as no answer. */
export interface Decider {
  decide(request: DecideRequest, timeoutMs: number): Promise<DecideResponse>;
}

/** Asks the governance service through the gateway, on the same connection the records travel on. */
export class GatewayDecider implements Decider {
  private readonly gateway: Gateway;
  constructor(gateway: Gateway) {
    this.gateway = gateway;
  }
  async decide(request: DecideRequest, timeoutMs: number): Promise<DecideResponse> {
    return decodeDecideResponse(await this.gateway.call(DECIDE, encodeDecideRequest(request), timeoutMs));
  }
}

/** Every part of the question is part of the key: a budget can be narrowed to a tenant, a project, a person or an agent, and a cached DOWNGRADE names a replacement for the model it was asked about. */
function keyOf(r: DecideRequest): string {
  return [r.parent, r.workspace, r.project, r.user, r.agent, r.requestedProvider, r.requestedModel].map((p) => p ?? "").join("\u0000");
}

/** A question governance failed to answer a moment ago, not asked again yet. */
export class GovernanceUnavailable extends Error {}

/**
 * A decider that reuses a verdict for `ttlMs`, so a warm call answers from memory. Only an answer is cached; a failure is remembered for the backoff and the same question fails at once in that time. Concurrent calls asking the same question share one request.
 */
export class CachingDecider implements Decider {
  private readonly inner: Decider;
  private readonly ttlMs: number;
  private readonly backoffMs: number;
  private readonly now: () => number;
  private readonly entries = new Map<string, [DecideResponse, number]>();
  private readonly failed = new Map<string, number>();
  private readonly inflight = new Map<string, Promise<DecideResponse>>();

  constructor(inner: Decider, ttlMs: number, options: { failureBackoffMs?: number; now?: () => number } = {}) {
    this.inner = inner;
    this.ttlMs = ttlMs;
    this.backoffMs = Math.max(0, options.failureBackoffMs ?? DEFAULT_FAILURE_BACKOFF_MS);
    this.now = options.now ?? (() => performance.now());
  }

  /** The live verdict for exactly this question, if one is cached. */
  cached(request: DecideRequest): DecideResponse | undefined {
    const key = keyOf(request);
    const entry = this.entries.get(key);
    if (!entry) return undefined;
    if (this.now() < entry[1]) return entry[0];
    this.entries.delete(key);
    return undefined;
  }

  decide(request: DecideRequest, timeoutMs: number): Promise<DecideResponse> {
    const live = this.cached(request);
    if (live) return Promise.resolve(live);
    const key = keyOf(request);
    const until = this.failed.get(key);
    if (until !== undefined) {
      if (this.now() < until) return Promise.reject(new GovernanceUnavailable());
      this.failed.delete(key);
    }
    const shared = this.inflight.get(key);
    if (shared) return shared;
    const call = this.inner.decide(request, timeoutMs).then(
      (response) => {
        this.store(key, response);
        return response;
      },
      (err: unknown) => {
        this.noteFailureKey(key);
        throw err;
      },
    );
    this.inflight.set(key, call);
    const clear = () => this.inflight.delete(key);
    call.then(clear, clear);
    return call;
  }

  /** Records that a caller stopped waiting for this question: a governance too slow to answer is as unavailable to the caller as one that refused. */
  noteFailure(request: DecideRequest): void {
    this.noteFailureKey(keyOf(request));
  }

  private noteFailureKey(key: string): void {
    if (this.backoffMs <= 0) return;
    if (this.failed.size >= MAX_CACHED_VERDICTS) this.failed.clear();
    this.failed.set(key, this.now() + this.backoffMs);
  }

  private store(key: string, response: DecideResponse): void {
    if (this.ttlMs <= 0) return;
    if (this.entries.size >= MAX_CACHED_VERDICTS && !this.entries.has(key)) {
      // The oldest eighth goes, which is cheaper than finding the single oldest on every store.
      const drop = [...this.entries.keys()].slice(0, Math.max(1, MAX_CACHED_VERDICTS / 8));
      for (const k of drop) this.entries.delete(k);
    }
    this.entries.set(key, [response, this.now() + this.ttlMs]);
  }
}

// Refusals governance gave in this process, by question, for answering when it cannot.
const refused = new Map<string, number>();

/** @internal Forgets every remembered refusal, for tests. */
export function forgetRefusals(): void {
  refused.clear();
}

function bounded(call: Promise<DecideResponse>, timeoutMs: number): Promise<DecideResponse> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const late = new Promise<never>((_, reject) => {
    timer = setTimeout(() => reject(new Error("governance did not answer in time")), timeoutMs);
    timer.unref?.();
  });
  return Promise.race([call, late]).finally(() => clearTimeout(timer));
}

/** Asks `decider`, waiting no longer than `timeoutMs`. Rejects on any failure, a timeout included, unless governance refused the same question within the hour, which is then refused again. */
export async function ask(decider: Decider, request: DecideRequest, timeoutMs: number): Promise<Verdict> {
  const key = keyOf(request);
  let response: DecideResponse;
  try {
    const caching = decider instanceof CachingDecider ? decider : undefined;
    response = caching?.cached(request) ?? (await bounded(decider.decide(request, timeoutMs), timeoutMs).catch((err: unknown) => {
      caching?.noteFailure(request);
      throw err;
    }));
  } catch (err) {
    const at = refused.get(key);
    if (at !== undefined && performance.now() - at < REFUSAL_WINDOW_MS) {
      return verdict("DENY", { reason: "refused within the hour and governance did not answer in time" });
    }
    throw err;
  }
  if (response.decision === DECISION_DENY) {
    if (refused.size >= MAX_CACHED_VERDICTS) refused.clear();
    refused.set(key, performance.now());
  } else {
    refused.delete(key);
  }
  return verdict(NAMES[response.decision] ?? "UNDECIDED", {
    reason: response.reason,
    replacementProvider: response.replacementProvider,
    replacementModel: response.replacementModel,
  });
}
