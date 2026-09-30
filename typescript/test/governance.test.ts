import assert from "node:assert/strict";
import { beforeEach, test } from "node:test";

import { ask, CachingDecider, type Decider, forgetRefusals, GovernanceUnavailable } from "../src/governance.ts";
import { DECISION_ALLOW, DECISION_DENY, DECISION_DOWNGRADE, type DecideResponse } from "../src/wire.ts";

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
