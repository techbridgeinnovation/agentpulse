import assert from "node:assert/strict";
import { after, before, beforeEach, test } from "node:test";

import { looksLikeEmail, scope } from "../src/context.ts";
import { blockedFinish, errorCode, truncatedFinish } from "../src/failure.ts";
import { ConfigError } from "../src/gateway.ts";
import { type Decider, forgetRefusals } from "../src/governance.ts";
import { connect } from "../src/report.ts";
import { Recorder } from "../src/recorder.ts";
import { type BudgetStatusEvent, Reporter } from "../src/report.ts";
import { resultSize } from "../src/usage.ts";
import { DECISION_ALLOW, DECISION_DENY, DECISION_NOTIFY, fields, LEVEL_PASSED_75_PERCENT, SCOPE_PERSONAL, STATUS_DENIED, STATUS_TRUNCATED } from "../src/wire.ts";
import { MemorySink } from "./fakes.ts";
import { FakeGateway } from "./fakes.ts";

let gw: FakeGateway;
before(async () => {
  gw = await new FakeGateway().start();
});
after(() => gw.close());
beforeEach(() => forgetRefusals());

const env = () => ({ AP_GATEWAY: gw.url, AP_API_KEY: "organisations/acme/apiKeys/k1", AP_API_SECRET: "s", AP_AGENT: "organisations/acme/agents/assistant" });

function strings(message: Uint8Array): string[] {
  const out: string[] = [];
  const walk = (m: Uint8Array, depth: number) => {
    for (const [, value] of fields(m)) {
      if (typeof value !== "bigint") {
        out.push(new TextDecoder().decode(value));
        if (depth < 2) {
          try {
            walk(value, depth + 1);
          } catch {}
        }
      }
    }
  };
  walk(message, 0);
  return out;
}

test("connect refuses an agent outside the key's organisation", () => {
  assert.throws(() => connect("svc", { env: { ...env(), AP_AGENT: "organisations/other/agents/a" } }), ConfigError);
  assert.throws(() => connect("svc", { env: { ...env(), AP_API_KEY: "a-bare-secret" } }), ConfigError);
});

test("a model call reaches metering under the workspace, request and person on the scope, and never with content", async () => {
  const rp = connect("documents-service", { billedBy: "OPENAI", env: env(), config: { exitTimeoutMs: 0 } });
  scope({ request: "req-1", workspace: "acme-corp", user: { id: "u-42", name: "Ada" } }, () =>
    rp.modelCall({ model: "gpt-5", reported: { input_tokens: 100, output_tokens: 20 }, format: "OPENAI_RESPONSES", finishReason: "length" }),
  );
  assert.equal(await rp.recorder.flush(2000), true);
  const batch = gw.calls.find((c) => c.path.endsWith("/BatchCreateActivities"))!;
  const text = strings(batch.message);
  for (const want of ["organisations/acme/workspaces/acme-corp", "req-1", "u-42", "gpt-5", "OPENAI", "OPENAI_RESPONSES", "input_tokens", "length", "documents-service"]) {
    assert.ok(text.includes(want), `batch lacks ${want}`);
  }
  assert.ok(!text.includes("Ada"), "a name travelled on a record");
  const users = gw.calls.find((c) => c.path.endsWith("/BatchUpsertUsers"))!;
  assert.ok(strings(users.message).includes("Ada"));
  await rp.recorder.close();
});

test("a governed reporter asks before a call and records a refusal with nothing spent", async () => {
  const start = gw.calls.length;
  const rp = connect("svc", { env: env(), config: { exitTimeoutMs: 0 } }).governed(undefined, { cacheTtlMs: 0, failureBackoffMs: 0 });
  gw.replies.push({ message: new Uint8Array([8, DECISION_DENY]) });
  const v = await scope({ workspace: "acme-corp" }, () => rp.decide("gpt-5", { provider: "OPENAI" }));
  assert.equal(v.proceed, false);
  assert.equal(rp.recorder.stats().denied, 1);
  const decide = gw.calls.at(-1)!;
  assert.equal(decide.path, "/techbridge.ap.governance.v1.DecisionsService/Decide");
  assert.ok(strings(decide.message).includes("organisations/acme/workspaces/acme-corp"));
  await rp.recorder.flush(2000);
  const batch = gw.calls.slice(start).find((c) => c.path.endsWith("/BatchCreateActivities"))!;
  assert.ok(batch);
  let status = 0n;
  for (const [n, v2] of fields(batch.message)) if (n === 2 && typeof v2 !== "bigint") for (const [f, x] of fields(v2)) if (f === 22) status = x as bigint;
  assert.equal(Number(status), STATUS_DENIED);
  await rp.recorder.close();
});

test("an ungoverned reporter asks nobody and lets every call through", async () => {
  const before = gw.calls.length;
  const rp = connect("svc", { env: env(), config: { exitTimeoutMs: 0 } });
  assert.equal((await rp.decide("gpt-5")).decision, "UNDECIDED");
  assert.equal(gw.calls.length, before);
});

test("a governance that cannot answer lets the call through and is counted", async () => {
  const rp = connect("svc", { env: env(), config: { exitTimeoutMs: 0 } }).governed(undefined, { cacheTtlMs: 0, failureBackoffMs: 0, timeoutMs: 50 });
  gw.replies.push({ delayMs: 400 });
  const v = await rp.decide("gpt-5");
  assert.equal(v.proceed, true);
  assert.equal(rp.recorder.stats().decisionErrors, 1);
});

test("a caller stopping a call is told apart from a deadline", () => {
  const abort = (cause?: unknown) => Object.assign(new Error("SECRET"), { name: "AbortError", cause });
  assert.equal(errorCode(abort()), "Canceled");
  assert.equal(errorCode(abort(new DOMException("t", "TimeoutError"))), "DeadlineExceeded");
  assert.equal(errorCode(new DOMException("t", "TimeoutError")), "DeadlineExceeded");
});

test("Gemini's image refusals fail a call and Anthropic's full context window cuts one short", () => {
  assert.ok(blockedFinish("IMAGE_SAFETY"));
  assert.ok(truncatedFinish("model_context_window_exceeded"));
  assert.ok(!blockedFinish("end_turn"));
});

test("a tool call by hand carries the size of its result and whether it was empty", async () => {
  const sink = new MemorySink();
  const rp = new Reporter(new Recorder({ sinks: [sink], exitTimeoutMs: 0 }), { agent: "organisations/acme/agents/a", service: "svc" });
  rp.toolCall({ tool: "search", resultBytes: resultSize({ hits: ["a"] }).bytes });
  rp.toolCall({ tool: "search", resultBytes: 0, emptyResult: resultSize([]).empty });
  rp.toolCall({ tool: "search", resultBytes: 10, error: new Error("x") });
  rp.modelCall({ model: "m", truncated: true });
  await rp.recorder.flush(1000);
  const [sized, empty, failed, cut] = sink.activities;
  assert.deepEqual([sized!.resultBytes, sized!.emptyResult], [JSON.stringify({ hits: ["a"] }).length, undefined]);
  assert.deepEqual([empty!.resultBytes, empty!.emptyResult], [undefined, true]);
  assert.equal(failed!.resultBytes, undefined);
  assert.equal(cut!.status, STATUS_TRUNCATED);
});

test("an error whose causes lead back to each other still reduces to a code", () => {
  const a: { name: string; cause?: unknown } = { name: "AbortError" };
  const b = { name: "Error", cause: a };
  a.cause = b;
  assert.equal(errorCode(a), "Canceled");
  assert.equal(errorCode(b), "Canceled");
});

test("a skill and a component set on the request win over the reporter's own, and a tool keeps its own component", async () => {
  const sink = new MemorySink();
  const rp = new Reporter(new Recorder({ sinks: [sink], exitTimeoutMs: 0 }), { agent: "organisations/acme/agents/a", service: "svc", skill: "summaries" });
  scope({ skill: "search", component: "triage" }, () => {
    rp.modelCall({ model: "m" });
    rp.modelCall({ model: "m", component: "asset_summary" });
    rp.toolCall({ tool: "lookup" });
  });
  rp.modelCall({ model: "m" });
  await rp.recorder.flush(1000);
  assert.deepEqual(sink.activities.map((a) => [a.callerComponent, a.skill]), [["triage", "search"], ["asset_summary", "search"], ["tool:lookup", "search"], ["", "summaries"]]);
});

test("a user that looks like an email is recorded as no user, never named and never asked about", async () => {
  const sink = new MemorySink();
  const asked: string[] = [];
  const decider = { decide: async (r: { user?: string }) => (asked.push(r.user ?? ""), { decision: 1, reason: "", policyVersion: "", replacementProvider: "", replacementModel: "" }) };
  const rp = new Reporter(new Recorder({ sinks: [sink], exitTimeoutMs: 0 }), { agent: "organisations/acme/agents/a", service: "svc" }).governed(decider, { cacheTtlMs: 0 });
  await scope({ user: { id: "ada@example.com", name: "Ada" } }, async () => {
    await rp.decide("m");
    rp.modelCall({ model: "m" });
  });
  rp.modelCall({ model: "m" }, { user: "bob@example.org" });
  rp.modelCall({ model: "m" }, { user: "bob@corp" });
  await rp.recorder.flush(1000);
  assert.deepEqual(asked, [""]);
  assert.deepEqual(sink.activities.map((a) => a.user), ["", "", "bob@corp"]);
  assert.deepEqual(sink.named, []);
  assert.deepEqual([rp.recorder.stats().emailUsersRefused, rp.recorder.stats().namesFailed], [2, 1]);
});

test("an identifier is taken for an email only with a dot after the at", () => {
  assert.ok(looksLikeEmail("ada@example.com") && looksLikeEmail("@.") && looksLikeEmail("a.b@c.d"));
  assert.ok(!looksLikeEmail("ada@corp") && !looksLikeEmail("ada.lovelace") && !looksLikeEmail("") && !looksLikeEmail(undefined));
});

function withStatuses(decision = DECISION_ALLOW): Decider {
  const budgetStatuses = [{ budget: "organisations/acme/budgets/b", displayName: "Ada", scope: SCOPE_PERSONAL, workspace: "", spentMicros: 8, limitMicros: 10, periodEndTimeNs: 0n, warnOnly: false, level: LEVEL_PASSED_75_PERCENT }];
  return { decide: async () => ({ decision, reason: "", policyVersion: "v1", replacementProvider: "", replacementModel: "", budgetStatuses, evaluateTimeNs: 1791892800_000000000n }) };
}

function statusReporter(decider: Decider, onBudgetStatus?: (event: BudgetStatusEvent) => unknown): Reporter {
  return new Reporter(new Recorder({ exitTimeoutMs: 0 }), { agent: "organisations/acme/agents/assistant", service: "svc", billedBy: "OPENAI" }).governed(decider, { cacheTtlMs: 0, failureBackoffMs: 0, onBudgetStatus });
}

// The AI SDK's own call, as the middleware is handed it.
const generate = (rp: Reporter) => rp.aiSdkMiddleware().wrapGenerate({ model: { provider: "openai.chat", modelId: "gpt-5" }, doGenerate: async () => ({ usage: {} }) });

const tick = () => new Promise((resolve) => setImmediate(resolve));

async function until(condition: () => boolean): Promise<boolean> {
  for (let i = 0; i < 200 && !condition(); i++) await tick();
  return condition();
}

test("an instrumented call hands its budget statuses to the callback, later, with who it was for", async () => {
  const told: BudgetStatusEvent[] = [];
  const rp = statusReporter(withStatuses(), (event) => told.push(event));
  await scope({ request: "req-1", workspace: "w1", project: "p1", user: { id: "u1" }, component: "chat" }, () => generate(rp));
  // Delivery is a later turn of the event loop: the call has returned and nothing has been told yet.
  assert.equal(told.length, 0);
  assert.ok(await until(() => told.length === 1));
  const [event] = told;
  assert.deepEqual([event?.decision, event?.model, event?.provider, event?.component], ["ALLOW", "gpt-5", "OPENAI", "chat"]);
  assert.deepEqual([event?.user, event?.workspace, event?.project, event?.request], ["u1", "organisations/acme/workspaces/w1", "p1", "req-1"]);
  assert.deepEqual([event?.budgetStatuses[0]?.level, event?.evaluateTime?.toISOString()], ["PASSED_75_PERCENT", "2026-10-13T12:00:00.000Z"]);
});

test("a call never waits for a callback whose promise never settles, and deliveries behind it go on", async () => {
  const told: string[] = [];
  const rp = statusReporter(withStatuses(), (event) => {
    told.push(event.model);
    if (told.length === 1) return new Promise(() => {});
  });
  assert.deepEqual(await generate(rp), { usage: {} });
  await generate(rp);
  assert.ok(await until(() => told.length === 2));
});

test("events are delivered one at a time in the order the decisions were obtained", async () => {
  const told: string[] = [];
  let running = 0;
  let overlapped = false;
  const rp = statusReporter(withStatuses(), (event) => {
    overlapped ||= ++running > 1;
    told.push(event.model);
    running--;
  });
  const models = Array.from({ length: 20 }, (_, i) => `m${i}`);
  for (const model of models) await rp.decideFor(model, "OPENAI", "chat");
  assert.ok(await until(() => told.length === 20));
  assert.deepEqual(told, models);
  assert.equal(overlapped, false);
});

test("a cached verdict's statuses are delivered for every call", async () => {
  const told: BudgetStatusEvent[] = [];
  let asked = 0;
  const inner = withStatuses();
  const counting: Decider = { decide: (...args) => (asked++, inner.decide(...args)) };
  const rp = new Reporter(new Recorder({ exitTimeoutMs: 0 }), { agent: "organisations/acme/agents/assistant", service: "svc" }).governed(counting, { cacheTtlMs: 60_000, onBudgetStatus: (event) => told.push(event) });
  await rp.decideFor("gpt-5", "OPENAI", "chat");
  await rp.decideFor("gpt-5", "OPENAI", "chat");
  assert.ok(await until(() => told.length === 2));
  assert.equal(asked, 1);
  assert.deepEqual(told[1]?.budgetStatuses, told[0]?.budgetStatuses);
});

test("a callback that throws or rejects does not stop the call and is counted failed", async () => {
  for (const callback of [
    () => {
      throw new Error("the product's own bug");
    },
    () => Promise.reject(new Error("the product's own bug, later")),
  ]) {
    const rp = statusReporter(withStatuses(DECISION_NOTIFY), callback);
    assert.deepEqual(await generate(rp), { usage: {} });
    assert.ok(await until(() => rp.recorder.stats().budgetStatusFailed === 1));
    const stats = rp.recorder.stats();
    assert.deepEqual([stats.budgetStatusDropped, stats.notified, stats.panicked, stats.decisionErrors], [0, 1, 0, 0]);
  }
});

test("a full queue drops events and counts them without ever holding a call", async () => {
  const told: string[] = [];
  const rp = statusReporter(withStatuses(), (event) => void told.push(event.model));
  // Every decision settles on microtasks, so no delivery turn comes between these and the queue fills.
  const started = performance.now();
  for (let i = 0; i < 70; i++) assert.ok((await rp.decideFor(`m${i}`, "OPENAI", "chat")).proceed);
  assert.ok(performance.now() - started < 2000);
  assert.equal(rp.recorder.stats().budgetStatusDropped, 6);
  assert.ok(await until(() => told.length === 64));
  assert.deepEqual(told, Array.from({ length: 64 }, (_, i) => `m${i}`));
});

test("the callback is not called without statuses, without a decision, for decide, or when none is set", async () => {
  let told = 0;
  const count = () => told++;
  const none: Decider = { decide: async () => ({ decision: DECISION_ALLOW, reason: "", policyVersion: "", replacementProvider: "", replacementModel: "" }) };
  await generate(statusReporter(none, count));
  const down = statusReporter({ decide: () => Promise.reject(new Error("down")) }, count);
  await generate(down);
  assert.equal(down.recorder.stats().decisionErrors, 1);
  const direct = statusReporter(withStatuses(), count);
  assert.equal((await direct.decide("gpt-5")).budgetStatuses.length, 1);
  const unset = statusReporter(withStatuses());
  await generate(unset);
  await until(() => false);
  assert.equal(told, 0);
  assert.deepEqual([unset.recorder.stats().budgetStatusDropped, unset.recorder.stats().budgetStatusFailed], [0, 0]);
});
