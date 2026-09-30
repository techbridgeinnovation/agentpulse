// Calls made through the real Vercel AI SDK and its providers, against a provider on this machine. Skipped where the SDK is not installed.

import assert from "node:assert/strict";
import { after, before, beforeEach, test } from "node:test";

import { scope } from "../src/context.ts";
import { denied, SpendDenied } from "../src/failure.ts";
import { type Decider, forgetRefusals } from "../src/governance.ts";
import { Recorder } from "../src/recorder.ts";
import { Reporter } from "../src/report.ts";
import { DECISION_DENY, DECISION_DOWNGRADE, STATUS_FAILED, STATUS_TRUNCATED } from "../src/wire.ts";
import { MemorySink, Provider } from "./fakes.ts";

const sdk = await import("ai").catch(() => undefined);
const openaiSdk = await import("@ai-sdk/openai").catch(() => undefined);
const anthropicSdk = await import("@ai-sdk/anthropic").catch(() => undefined);
const googleSdk = await import("@ai-sdk/google").catch(() => undefined);
const skip = !sdk || !openaiSdk ? "the Vercel AI SDK is not installed" : false;

const AGENT = "organisations/acme/agents/assistant";

let provider: Provider;
before(async () => {
  provider = await new Provider().start();
});
after(() => provider.close());
beforeEach(() => forgetRefusals());

function reporter(decider?: Decider): [Reporter, MemorySink] {
  const sink = new MemorySink();
  const rp = new Reporter(new Recorder({ sinks: [sink], exitTimeoutMs: 0 }), { agent: AGENT, service: "svc" });
  return [decider ? rp.governed(decider, { cacheTtlMs: 0, failureBackoffMs: 0 }) : rp, sink];
}

async function recorded(rp: Reporter, sink: MemorySink) {
  await rp.recorder.flush(2000);
  return sink.activities;
}

const usage = (a: { reportedUsage?: { unit: string; quantity: number }[] }) => Object.fromEntries((a.reportedUsage ?? []).map((q) => [q.unit, q.quantity]));

const CHAT = {
  id: "c1",
  object: "chat.completion",
  created: 1,
  model: "gpt-5-2026-08-01",
  choices: [{ index: 0, finish_reason: "length", message: { role: "assistant", content: "SECRET ANSWER" } }],
  usage: { prompt_tokens: 100, completion_tokens: 50, total_tokens: 150, prompt_tokens_details: { cached_tokens: 80 } },
};

function chatModel(rp: Reporter) {
  const openai = openaiSdk!.createOpenAI({ apiKey: "k", baseURL: provider.url + "/v1" });
  return sdk!.wrapLanguageModel({ model: openai.chat("gpt-5"), middleware: rp.aiSdkMiddleware() as any });
}

test("a call is recorded in the provider's own convention under the scope it was made in", { skip }, async () => {
  const [rp, sink] = reporter();
  provider.json(CHAT);
  const result = await scope({ request: "req-1", workspace: "acme-corp" }, () => sdk!.generateText({ model: chatModel(rp), prompt: "SECRET PROMPT" }));
  assert.equal(result.text, "SECRET ANSWER");
  const [a] = await recorded(rp, sink);
  assert.deepEqual([a!.model, a!.billedBy, a!.usageFormat, a!.request, a!.status, a!.errorCode], ["gpt-5-2026-08-01", "OPENAI", "OPENAI_CHAT", "req-1", STATUS_TRUNCATED, "length"]);
  assert.deepEqual(usage(a!), { prompt_tokens: 100, completion_tokens: 50, total_tokens: 150, "prompt_tokens_details.cached_tokens": 80 });
  assert.equal(a!.framework, "vercel/ai");
  assert.equal(sink.batches[0]![0], "acme-corp");
  assert.ok(!JSON.stringify(sink.batches, (_, v) => (typeof v === "bigint" ? String(v) : v)).includes("SECRET"));
});

test("Anthropic's own counts are recorded as Anthropic reports them", { skip: skip || (!anthropicSdk && "no Anthropic provider") }, async () => {
  const [rp, sink] = reporter();
  provider.json({
    id: "m1",
    type: "message",
    role: "assistant",
    model: "claude-sonnet-5-20260801",
    content: [{ type: "text", text: "SECRET" }],
    stop_reason: "end_turn",
    usage: { input_tokens: 10, output_tokens: 20, cache_read_input_tokens: 300, cache_creation_input_tokens: 50 },
  });
  const anthropic = anthropicSdk!.createAnthropic({ apiKey: "k", baseURL: provider.url + "/v1" });
  await sdk!.generateText({ model: sdk!.wrapLanguageModel({ model: anthropic("claude-sonnet-5"), middleware: rp.aiSdkMiddleware() as any }), prompt: "x" });
  const [a] = await recorded(rp, sink);
  assert.deepEqual([a!.billedBy, a!.usageFormat], ["ANTHROPIC", "ANTHROPIC"]);
  const u = usage(a!);
  assert.deepEqual([u.input_tokens, u.cache_read_input_tokens, u.cache_creation_input_tokens, u.output_tokens], [10, 300, 50, 20]);
});

test("Gemini's own counts are recorded as the Vertex convention names them, billed by Google", { skip: skip || (!googleSdk && "no Google provider") }, async () => {
  const [rp, sink] = reporter();
  provider.json({
    candidates: [{ content: { role: "model", parts: [{ text: "SECRET" }] }, finishReason: "STOP" }],
    modelVersion: "gemini-2.5-flash",
    usageMetadata: { promptTokenCount: 100, candidatesTokenCount: 20, cachedContentTokenCount: 30, thoughtsTokenCount: 7, totalTokenCount: 127 },
  });
  const google = googleSdk!.createGoogleGenerativeAI({ apiKey: "k", baseURL: provider.url + "/v1beta" });
  await sdk!.generateText({ model: sdk!.wrapLanguageModel({ model: google("gemini-2.5-flash"), middleware: rp.aiSdkMiddleware() as any }), prompt: "x" });
  const [a] = await recorded(rp, sink);
  assert.deepEqual([a!.billedBy, a!.usageFormat], ["VERTEX_AI", "VERTEX"]);
  assert.deepEqual(usage(a!), { promptTokenCount: 100, candidatesTokenCount: 20, cachedContentTokenCount: 30, thoughtsTokenCount: 7, totalTokenCount: 127 });
});

test("a streamed call is recorded once, with its usage, when the stream ends", { skip }, async () => {
  const [rp, sink] = reporter();
  const chunk = { id: "c1", object: "chat.completion.chunk", created: 1, model: "gpt-5-2026-08-01" };
  provider.sse([
    { ...chunk, choices: [{ index: 0, delta: { role: "assistant", content: "SECRET" }, finish_reason: null }] },
    { ...chunk, choices: [{ index: 0, delta: {}, finish_reason: "stop" }] },
    { ...chunk, choices: [], usage: { prompt_tokens: 12, completion_tokens: 3, total_tokens: 15 } },
  ]);
  const result = sdk!.streamText({ model: chatModel(rp), prompt: "x" });
  let text = "";
  for await (const part of result.textStream) text += part;
  assert.equal(text, "SECRET");
  const activities = await recorded(rp, sink);
  assert.equal(activities.length, 1);
  assert.deepEqual([activities[0]!.model, usage(activities[0]!).prompt_tokens], ["gpt-5-2026-08-01", 12]);
});

test("a budget that has run out stops the call before it is sent", { skip }, async () => {
  const decider: Decider = { decide: async () => ({ decision: DECISION_DENY, reason: "", policyVersion: "", replacementProvider: "", replacementModel: "" }) };
  const [rp, sink] = reporter(decider);
  const before = provider.received.length;
  await assert.rejects(sdk!.generateText({ model: chatModel(rp), prompt: "SECRET" }), (err: unknown) => denied(err) || err instanceof SpendDenied);
  assert.equal(provider.received.length, before);
  const activities = await recorded(rp, sink);
  assert.equal(activities.length, 1);
  assert.equal(rp.recorder.stats().denied, 1);
});

test("a downgrade lets the call through as asked and is counted as not applied", { skip }, async () => {
  const decider: Decider = { decide: async () => ({ decision: DECISION_DOWNGRADE, reason: "", policyVersion: "", replacementProvider: "OPENAI", replacementModel: "gpt-5-mini" }) };
  const [rp] = reporter(decider);
  provider.json(CHAT);
  await sdk!.generateText({ model: chatModel(rp), prompt: "x" });
  assert.equal((provider.received.at(-1)!.body as { model: string }).model, "gpt-5");
  assert.equal(rp.recorder.stats().downgradeNotApplied, 1);
});

test("a failed call is recorded by its code and the error reaches the caller unchanged", { skip }, async () => {
  const [rp, sink] = reporter();
  provider.json({ error: { message: "SECRET QUOTED PROMPT", type: "invalid_request_error", code: "context_length_exceeded" } }, 400);
  await assert.rejects(sdk!.generateText({ model: chatModel(rp), prompt: "x", maxRetries: 0 }));
  const [a] = await recorded(rp, sink);
  assert.deepEqual([a!.status, a!.errorCode], [STATUS_FAILED, "400"]);
  assert.ok(!JSON.stringify(sink.batches, (_, v) => (typeof v === "bigint" ? String(v) : v)).includes("SECRET"));
});

test("middleware that fails inside is counted and the call goes on", { skip }, async () => {
  const [rp] = reporter();
  (rp as any).modelCall = () => {
    throw new Error("broken");
  };
  (rp as any).decideFor = () => Promise.reject(new Error("broken"));
  provider.json(CHAT);
  const result = await sdk!.generateText({ model: chatModel(rp), prompt: "x" });
  assert.equal(result.text, "SECRET ANSWER");
  assert.equal(rp.recorder.stats().panicked, 2);
});

test("a stream the caller stops reading is still recorded, as cancelled", { skip }, async () => {
  const [rp, sink] = reporter();
  const chunk = { id: "c1", object: "chat.completion.chunk", created: 1, model: "gpt-5-2026-08-01" };
  provider.sse([
    { ...chunk, choices: [{ index: 0, delta: { role: "assistant", content: "one" }, finish_reason: null }] },
    { ...chunk, choices: [{ index: 0, delta: { content: " two" }, finish_reason: null }] },
    { ...chunk, choices: [{ index: 0, delta: {}, finish_reason: "stop" }] },
  ]);
  const result = sdk!.streamText({ model: chatModel(rp), prompt: "x" });
  for await (const _ of result.textStream) break;
  // The SDK may go on reading the model's stream after its caller stops, so the call is recorded when the stream ends or is cancelled, whichever comes first.
  const deadline = performance.now() + 1000;
  let activities = await recorded(rp, sink);
  while (activities.length === 0 && performance.now() < deadline) {
    await new Promise((r) => setTimeout(r, 20));
    activities = await recorded(rp, sink);
  }
  assert.equal(activities.length, 1);
});

// Labels are added to the params before the provider builds its request, so these call the middleware's transform directly and need no SDK.
const VERTEX = { provider: "google.vertex.chat", modelId: "gemini-2.5-flash" };

async function transformed(params: Record<string, unknown>, model: { provider: string; modelId: string }, options: { component?: string; agent?: string } = {}) {
  const sink = new MemorySink();
  const rp = new Reporter(new Recorder({ sinks: [sink], exitTimeoutMs: 0 }), { agent: options.agent ?? AGENT, service: "svc" });
  const out = await rp.aiSdkMiddleware({ component: options.component }).transformParams({ type: "generate", params, model });
  return [out, rp] as const;
}

test("a call to Gemini on Vertex is labelled with the agent and component in every namespace the SDK reads", async () => {
  const [out] = await transformed({ prompt: [] }, VERTEX, { component: "shortlist" });
  assert.deepEqual(out.providerOptions, { google: { labels: { ap_agent: "assistant", ap_component: "shortlist" } } });
  const [v6] = await transformed({ providerOptions: { vertex: { cachedContent: "c" } } }, VERTEX);
  assert.deepEqual(v6.providerOptions, {
    google: { labels: { ap_agent: "assistant", ap_component: "ai-sdk" } },
    vertex: { cachedContent: "c", labels: { ap_agent: "assistant", ap_component: "ai-sdk" } },
  });
  const [v7] = await transformed({ providerOptions: { googleVertex: {} } }, VERTEX);
  assert.deepEqual((v7.providerOptions as any).googleVertex.labels, { ap_agent: "assistant", ap_component: "ai-sdk" });
});

test("the agent label is sanitised as Google requires", async () => {
  const [out] = await transformed({}, VERTEX, { agent: "organisations/acme/agents/__Deal Scout.v2!__", component: "x".repeat(80) });
  const labels = (out.providerOptions as any).google.labels;
  assert.equal(labels.ap_agent, "deal_scout_v2");
  assert.equal(labels.ap_component, "x".repeat(63));
});

test("a label the caller set is kept, and other provider options are left alone", async () => {
  const params = { prompt: [], providerOptions: { google: { labels: { ap_agent: "mine", team: "growth" }, thinkingConfig: { thinkingBudget: 0 } }, openai: { user: "u" } } };
  const before = structuredClone(params);
  const [out] = await transformed(params, VERTEX);
  assert.deepEqual(out.providerOptions, {
    google: { labels: { ap_agent: "mine", ap_component: "ai-sdk", team: "growth" }, thinkingConfig: { thinkingBudget: 0 } },
    openai: { user: "u" },
  });
  assert.deepEqual(out.prompt, []);
  assert.deepEqual(params, before);
});

test("no other provider is labelled, the Gemini API included", async () => {
  for (const provider of ["openai.chat", "openai.responses", "anthropic.messages", "google.generative-ai", "vertex.anthropic.messages"]) {
    const params = { providerOptions: { google: { labels: { team: "growth" } } } };
    const [out] = await transformed(params, { provider, modelId: "m" });
    assert.equal(out, params, provider);
  }
});

test("a failure while labelling leaves the params exactly as they were", async () => {
  const params = { providerOptions: { google: { get labels(): never { throw new Error("broken"); } } } };
  const [out, rp] = await transformed(params, VERTEX);
  assert.equal(out, params);
  assert.equal(rp.recorder.stats().panicked, 1);
});
