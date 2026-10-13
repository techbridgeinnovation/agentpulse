// Asking governance whether a call may proceed, before it is made.
//
// Only a genuine DENY stops a call. Every other answer, and no answer at all (governance unreachable, slow, refusing the question, or saying something this version does not know), lets it go ahead, because a spend decision that cannot be made must not be the reason the product stops working. Each of those is counted, so a degraded governance is visible rather than silent.

import type { Gateway } from "./gateway.ts";
import {
  type BudgetStatus as WireBudgetStatus,
  DECISION_ALLOW,
  DECISION_DENY,
  DECISION_DOWNGRADE,
  DECISION_NOTIFY,
  type DecideRequest,
  type DecideResponse,
  decodeDecideResponse,
  encodeDecideRequest,
  LEVEL_DRAWING_RESERVE,
  LEVEL_EXHAUSTED,
  LEVEL_PASSED_75_PERCENT,
  LEVEL_PASSED_95_PERCENT,
  LEVEL_WITHIN,
  SCOPE_ORGANISATION,
  SCOPE_OTHER,
  SCOPE_PERSONAL,
  SCOPE_RESERVE,
  SCOPE_SHARED_POOL,
  SCOPE_WORKSPACE,
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

/** What a budget is a limit on. SCOPE_UNSPECIFIED stands for one this version does not know. */
export type BudgetScope = "SCOPE_UNSPECIFIED" | "PERSONAL" | "SHARED_POOL" | "WORKSPACE" | "RESERVE" | "ORGANISATION" | "OTHER";

/** How close a budget is to its limit, least pressing first. LEVEL_UNSPECIFIED stands for one this version does not know. */
export type BudgetLevel = "LEVEL_UNSPECIFIED" | "WITHIN" | "PASSED_75_PERCENT" | "PASSED_95_PERCENT" | "DRAWING_RESERVE" | "EXHAUSTED";

const SCOPES: Record<number, BudgetScope> = {
  [SCOPE_PERSONAL]: "PERSONAL",
  [SCOPE_SHARED_POOL]: "SHARED_POOL",
  [SCOPE_WORKSPACE]: "WORKSPACE",
  [SCOPE_RESERVE]: "RESERVE",
  [SCOPE_ORGANISATION]: "ORGANISATION",
  [SCOPE_OTHER]: "OTHER",
};

const LEVELS: Record<number, BudgetLevel> = {
  [LEVEL_WITHIN]: "WITHIN",
  [LEVEL_PASSED_75_PERCENT]: "PASSED_75_PERCENT",
  [LEVEL_PASSED_95_PERCENT]: "PASSED_95_PERCENT",
  [LEVEL_DRAWING_RESERVE]: "DRAWING_RESERVE",
  [LEVEL_EXHAUSTED]: "EXHAUSTED",
};

/**
 * How one budget that applies to a call stood when governance decided it.
 *
 * A snapshot, never a live reading: spend reaches governance only after a call is recorded, and a cached verdict is reused, so this can trail the calls made since by the cache's lifetime and the recording delay. It is something to warn a person with, not a limit to enforce or a percentage to show as exact.
 */
export interface BudgetStatus {
  /** Format: organisations/{organisation}/budgets/{budget}. For a RESERVE, the budget whose reserve it is. */
  readonly budget: string;
  readonly displayName: string;
  readonly scope: BudgetScope;
  /** For a WORKSPACE status, the workspace. Format: organisations/{organisation}/workspaces/{workspace} */
  readonly workspace: string;
  /** In millionths of a US dollar, this period. The limit is zero for a reserve with nothing in it, so a share of it is never worked out by dividing. */
  readonly spentMicros: number;
  readonly limitMicros: number;
  /** When the current period ends, or null when governance did not say. */
  readonly periodEndTime: Date | null;
  /** True when the budget only warns and lets a call continue once it is exhausted. */
  readonly warnOnly: boolean;
  readonly level: BudgetLevel;
}

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
  /** How every budget that applies to the call stood, most pressing first, at every level including WITHIN. Empty from a governance that predates them, for a call no budget applies to, and when no answer was reached. A verdict reused from the cache carries the statuses of the answer it reuses. */
  budgetStatuses: readonly BudgetStatus[];
  /** When governance worked the answer out, which is how old the statuses are. Null when no answer was reached or governance did not say. */
  evaluateTime: Date | null;
}

const NONE: readonly BudgetStatus[] = Object.freeze([]);

export function verdict(decision: Decision = "UNDECIDED", fields: Partial<Omit<Verdict, "decision" | "proceed">> = {}): Verdict {
  return { decision, reason: "", replacementProvider: "", replacementModel: "", budgetStatuses: NONE, evaluateTime: null, ...fields, proceed: decision !== "DENY" };
}

/** A timestamp governance sent, or null where it sent none or one no Date can hold. */
function timeOf(nanoseconds: bigint): Date | null {
  if (!nanoseconds) return null;
  const at = new Date(Number(nanoseconds / 1_000_000n));
  return Number.isNaN(at.getTime()) ? null : at;
}

function statusOf(s: WireBudgetStatus): BudgetStatus {
  return Object.freeze({
    budget: s.budget,
    displayName: s.displayName,
    scope: SCOPES[s.scope] ?? "SCOPE_UNSPECIFIED",
    workspace: s.workspace,
    spentMicros: s.spentMicros,
    limitMicros: s.limitMicros,
    periodEndTime: timeOf(s.periodEndTimeNs),
    warnOnly: s.warnOnly,
    level: LEVELS[s.level] ?? "LEVEL_UNSPECIFIED",
  });
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
    // A response from a decider written before statuses existed may have no list at all.
    budgetStatuses: response.budgetStatuses?.length ? Object.freeze(response.budgetStatuses.map(statusOf)) : NONE,
    evaluateTime: timeOf(response.evaluateTimeNs ?? 0n),
  });
}
