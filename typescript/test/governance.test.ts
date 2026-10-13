import assert from "node:assert/strict";
import { beforeEach, test } from "node:test";

import { ask, CachingDecider, type Decider, forgetRefusals, GovernanceUnavailable } from "../src/governance.ts";
import { DECISION_ALLOW, DECISION_DENY, DECISION_DOWNGRADE, type DecideResponse, LEVEL_EXHAUSTED, LEVEL_PASSED_95_PERCENT, SCOPE_RESERVE, SCOPE_WORKSPACE } from "../src/wire.ts";

beforeEach(() => forgetRefusals());

const Q = { parent: "organisations/acme", agent: "organisations/acme/agents/a", requestedModel: "gpt-5" };

function answering(decision: number, extra: Partial<DecideResponse> = {}): Decider & { asked: number } {
  return {
    asked: 0,
    async decide() {
      this.asked++;
      return { decision, reason: "", policyVersion: "v1", replacementProvider: "", replacementModel: "", ...extra };
    },
  };
}

test("only a genuine DENY stops a call, and a DOWNGRADE names its replacement", async () => {
  assert.equal((await ask(answering(DECISION_ALLOW), Q, 100)).proceed, true);
  assert.equal((await ask(answering(DECISION_DENY), Q, 100)).proceed, false);
  const v = await ask(answering(DECISION_DOWNGRADE, { replacementModel: "gpt-5-mini" }), Q, 100);
  assert.deepEqual([v.decision, v.proceed, v.replacementModel], ["DOWNGRADE", true, "gpt-5-mini"]);
});

test("a verdict is reused within its lifetime, and concurrent callers share one question", async () => {
  const inner = answering(DECISION_ALLOW);
  const cache = new CachingDecider(inner, 60_000);
  await Promise.all([ask(cache, Q, 100), ask(cache, Q, 100), ask(cache, Q, 100)]);
  await ask(cache, Q, 100);
  assert.equal(inner.asked, 1);
  await ask(cache, { ...Q, user: "someone-else" }, 100);
  assert.equal(inner.asked, 2);
});

test("a governance that failed is not asked again during the backoff", async () => {
  let asked = 0;
  const down: Decider = { decide: async () => (asked++, Promise.reject(new Error("down"))) };
  const cache = new CachingDecider(down, 60_000, { failureBackoffMs: 60_000 });
  await assert.rejects(ask(cache, Q, 100));
  await assert.rejects(ask(cache, Q, 100), GovernanceUnavailable);
  assert.equal(asked, 1);
});

test("a governance too slow to answer is given up on at the timeout", async () => {
  const slow: Decider = { decide: () => new Promise((r) => setTimeout(() => r({ decision: DECISION_ALLOW, reason: "", policyVersion: "", replacementProvider: "", replacementModel: "" }), 1000)) };
  const started = performance.now();
  await assert.rejects(ask(slow, Q, 50));
  assert.ok(performance.now() - started < 500);
});

test("a refusal is repeated when governance cannot answer within the hour after it", async () => {
  await ask(answering(DECISION_DENY), Q, 100);
  const down: Decider = { decide: () => Promise.reject(new Error("down")) };
  const v = await ask(down, Q, 100);
  assert.equal(v.proceed, false);
  await assert.rejects(ask(down, { ...Q, requestedModel: "other" }, 100));
});

const STATUSES = [
  { budget: "organisations/acme/budgets/eng", displayName: "Engineering", scope: SCOPE_WORKSPACE, workspace: "organisations/acme/workspaces/w1", spentMicros: 9_600_000, limitMicros: 10_000_000, periodEndTimeNs: 1793491200_000000000n, warnOnly: false, level: LEVEL_PASSED_95_PERCENT },
  { budget: "organisations/acme/budgets/org", displayName: "", scope: SCOPE_RESERVE, workspace: "", spentMicros: 0, limitMicros: 0, periodEndTimeNs: 0n, warnOnly: false, level: LEVEL_EXHAUSTED },
  { budget: "organisations/acme/budgets/later", displayName: "", scope: 9, workspace: "", spentMicros: 0, limitMicros: 0, periodEndTimeNs: 0n, warnOnly: true, level: 7 },
];

test("a verdict carries every budget status and when governance worked them out", async () => {
  const v = await ask(answering(DECISION_ALLOW, { budgetStatuses: STATUSES, evaluateTimeNs: 1791892800_123456789n }), Q, 100);
  assert.equal(v.evaluateTime?.toISOString(), "2026-10-13T12:00:00.123Z");
  const [workspace, reserve, later] = v.budgetStatuses;
  assert.deepEqual(workspace, {
    budget: "organisations/acme/budgets/eng",
    displayName: "Engineering",
    scope: "WORKSPACE",
    workspace: "organisations/acme/workspaces/w1",
    spentMicros: 9_600_000,
    limitMicros: 10_000_000,
    periodEndTime: new Date("2026-11-01T00:00:00Z"),
    warnOnly: false,
    level: "PASSED_95_PERCENT",
  });
  // A reserve with nothing in it is exhausted at a limit of zero, and nothing here divides by it.
  assert.deepEqual([reserve?.scope, reserve?.level, reserve?.limitMicros, reserve?.periodEndTime], ["RESERVE", "EXHAUSTED", 0, null]);
  // A scope or level this version does not know is read as unspecified rather than refused.
  assert.deepEqual([later?.scope, later?.level, later?.warnOnly], ["SCOPE_UNSPECIFIED", "LEVEL_UNSPECIFIED", true]);
  assert.ok(Object.isFrozen(v.budgetStatuses) && Object.isFrozen(workspace));
});

test("a governance that sends no statuses, or a decider written before them, leaves the verdict without any", async () => {
  const v = await ask(answering(DECISION_ALLOW), Q, 100);
  assert.deepEqual([v.budgetStatuses, v.evaluateTime], [[], null]);
});

test("a cached verdict keeps the statuses of the answer it reuses", async () => {
  const inner = answering(DECISION_ALLOW, { budgetStatuses: STATUSES, evaluateTimeNs: 1791892800_123456789n });
  const cache = new CachingDecider(inner, 60_000);
  const first = await ask(cache, Q, 100);
  const second = await ask(cache, Q, 100);
  assert.equal(inner.asked, 1);
  assert.deepEqual(second.budgetStatuses, first.budgetStatuses);
  assert.deepEqual(second.evaluateTime, first.evaluateTime);
});
