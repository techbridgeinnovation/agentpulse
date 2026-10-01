import assert from "node:assert/strict";
import { after, before, beforeEach, test } from "node:test";

import { scope } from "../src/context.ts";
import { blockedFinish, errorCode, truncatedFinish } from "../src/failure.ts";
import { ConfigError } from "../src/gateway.ts";
import { forgetRefusals } from "../src/governance.ts";
import { connect } from "../src/report.ts";
import { Recorder } from "../src/recorder.ts";
import { Reporter } from "../src/report.ts";
import { resultSize } from "../src/usage.ts";
import { DECISION_DENY, fields, STATUS_DENIED, STATUS_TRUNCATED } from "../src/wire.ts";
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
