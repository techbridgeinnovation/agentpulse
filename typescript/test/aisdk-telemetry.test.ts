// The telemetry integration for AI SDK 7, through the real SDK and its providers against a provider on this machine, on its own and beside the middleware. Skipped where AI SDK 7 is not installed, unless AP_AI_SDK_REQUIRED says it must be.

import assert from "node:assert/strict";
import { after, before, beforeEach, test } from "node:test";

import { scope } from "../src/context.ts";
import { denied, SpendDenied } from "../src/failure.ts";
import { type Decider, forgetRefusals } from "../src/governance.ts";
import { Recorder } from "../src/recorder.ts";
import { Reporter } from "../src/report.ts";
import { batchCreateActivities, DECISION_DENY, STATUS_DENIED, STATUS_FAILED, STATUS_OK, STATUS_TRUNCATED, type Activity } from "../src/wire.ts";
import { MemorySink, Provider } from "./fakes.ts";
import { anthropicSdk, googleSdk, openaiSdk, sdk, telemetrySkip as skip } from "./sdk.ts";

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

async function recorded(rp: Reporter, sink: MemorySink): Promise<Activity[]> {
  await rp.recorder.flush(2000);
  return sink.activities;
}

const usage = (a: Activity) => Object.fromEntries((a.reportedUsage ?? []).map((q) => [q.unit, q.quantity]));

// What leaves the process: every batch as the bytes sent to the gateway, and as the records themselves.
function sent(sink: MemorySink): string {
  const bytes = sink.batches.map(([, batch]) => Buffer.from(batchCreateActivities("organisations/acme", batch)).toString("latin1")).join("");
  return bytes + JSON.stringify(sink.batches, (_, v) => (typeof v === "bigint" ? String(v) : v));
}

const CHAT = {
  id: "c1",
  object: "chat.completion",
  created: 1,
  model: "gpt-5-2026-08-01",
  choices: [{ index: 0, finish_reason: "length", message: { role: "assistant", content: "SECRET ANSWER" } }],
  usage: { prompt_tokens: 100, completion_tokens: 50, total_tokens: 150, prompt_tokens_details: { cached_tokens: 80 } },
};
const STOP = { ...CHAT, choices: [{ index: 0, finish_reason: "stop", message: { role: "assistant", content: "SECRET ANSWER" } }] };
const CHUNK = { id: "c1", object: "chat.completion.chunk", created: 1, model: "gpt-5-2026-08-01" };

const openai = () => openaiSdk!.createOpenAI({ apiKey: "k", baseURL: provider.url + "/v1" });
const wrapped = (rp: Reporter) => sdk!.wrapLanguageModel({ model: openai().chat("gpt-5"), middleware: rp.aiSdkMiddleware() });
const on = (rp: Reporter, functionId?: string) => ({ integrations: [rp.aiSdkTelemetry()], functionId });

const LOOKUP_CALL = {
  ...CHAT,
  choices: [{ index: 0, finish_reason: "tool_calls", message: { role: "assistant", content: null, tool_calls: [{ id: "t1", type: "function", function: { name: "lookup", arguments: '{"q":"SECRET ARG"}' } }] } }],
};
const lookup = (execute: () => Promise<unknown>) => sdk!.tool({ inputSchema: sdk!.jsonSchema<{ q: string }>({ type: "object", properties: { q: { type: "string" } } }), execute });

test("a call is recorded once it finishes, in the provider's own convention, under its scope and its functionId", { skip }, async () => {
  const [rp, sink] = reporter();
  provider.json(CHAT);
  const result = await scope({ request: "req-1", workspace: "acme-corp", user: { id: "u1" } }, () =>
    sdk!.generateText({ model: openai().chat("gpt-5"), prompt: "SECRET PROMPT", system: "SECRET SYSTEM", telemetry: on(rp, "summarise") }),
  );
  assert.equal(result.text, "SECRET ANSWER");
  const [a, ...rest] = await recorded(rp, sink);
  assert.equal(rest.length, 0);
  assert.deepEqual([a!.model, a!.billedBy, a!.usageFormat, a!.request, a!.user, a!.callerComponent, a!.status, a!.errorCode], ["gpt-5-2026-08-01", "OPENAI", "OPENAI_CHAT", "req-1", "u1", "summarise", STATUS_TRUNCATED, "length"]);
  assert.deepEqual(usage(a!), { prompt_tokens: 100, completion_tokens: 50, total_tokens: 150, "prompt_tokens_details.cached_tokens": 80 });
  assert.deepEqual([a!.framework, a!.frameworkVersion], ["vercel/ai", (await import("ai/package.json", { with: { type: "json" } })).default.version]);
  assert.equal(sink.batches[0]![0], "acme-corp");
  assert.ok(a!.durationMs! >= 0);
  assert.ok(!sent(sink).includes("SECRET"));
});

test("an integration registered with registerTelemetry records every call, and a call that turns telemetry off is not recorded", { skip }, async () => {
  const [rp, sink] = reporter();
  sdk!.registerTelemetry(rp.aiSdkTelemetry({ component: "drafts" }));
  try {
    provider.json(STOP);
    provider.json(STOP);
    await sdk!.generateText({ model: openai().chat("gpt-5"), prompt: "x" });
    await sdk!.generateText({ model: openai().chat("gpt-5"), prompt: "x", telemetry: { isEnabled: false } });
  } finally {
    (globalThis as { AI_SDK_TELEMETRY_INTEGRATIONS?: unknown[] }).AI_SDK_TELEMETRY_INTEGRATIONS = [];
  }
  const activities = await recorded(rp, sink);
  assert.deepEqual(activities.map((a) => [a.callerComponent, a.status]), [["drafts", STATUS_OK]]);
});

test("Anthropic's and Gemini's own counts are recorded in their own conventions", { skip: skip || ((!anthropicSdk || !googleSdk) && "no Anthropic or Google provider") }, async () => {
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
  provider.json({
    candidates: [{ content: { role: "model", parts: [{ text: "SECRET" }] }, finishReason: "STOP" }],
    modelVersion: "gemini-2.5-flash",
    usageMetadata: { promptTokenCount: 100, candidatesTokenCount: 20, cachedContentTokenCount: 30, thoughtsTokenCount: 7, totalTokenCount: 127 },
  });
  const anthropic = anthropicSdk!.createAnthropic({ apiKey: "k", baseURL: provider.url + "/v1" });
  const google = googleSdk!.createGoogleGenerativeAI({ apiKey: "k", baseURL: provider.url + "/v1beta" });
  await sdk!.generateText({ model: anthropic("claude-sonnet-5"), prompt: "x", telemetry: on(rp) });
  await sdk!.generateText({ model: google("gemini-2.5-flash"), prompt: "x", telemetry: on(rp) });
  const [claude, gemini] = await recorded(rp, sink);
  assert.deepEqual([claude!.billedBy, claude!.usageFormat], ["ANTHROPIC", "ANTHROPIC"]);
  const u = usage(claude!);
  assert.deepEqual([u.input_tokens, u.cache_read_input_tokens, u.cache_creation_input_tokens, u.output_tokens], [10, 300, 50, 20]);
  assert.deepEqual([gemini!.billedBy, gemini!.usageFormat], ["VERTEX_AI", "VERTEX"]);
  assert.deepEqual(usage(gemini!), { promptTokenCount: 100, candidatesTokenCount: 20, cachedContentTokenCount: 30, thoughtsTokenCount: 7, totalTokenCount: 127 });
});

test("a streamed call is recorded once, with its usage, when the stream finishes", { skip }, async () => {
  const [rp, sink] = reporter();
  provider.sse([
    { ...CHUNK, choices: [{ index: 0, delta: { role: "assistant", content: "SECRET" }, finish_reason: null }] },
    { ...CHUNK, choices: [{ index: 0, delta: {}, finish_reason: "stop" }] },
    { ...CHUNK, choices: [], usage: { prompt_tokens: 12, completion_tokens: 3, total_tokens: 15 } },
  ]);
  const result = sdk!.streamText({ model: openai().chat("gpt-5"), prompt: "SECRET", telemetry: on(rp) });
  let text = "";
  for await (const part of result.textStream) text += part;
  assert.equal(text, "SECRET");
  const activities = await recorded(rp, sink);
  assert.equal(activities.length, 1);
  assert.deepEqual([activities[0]!.model, activities[0]!.status, usage(activities[0]!)], ["gpt-5-2026-08-01", STATUS_OK, { prompt_tokens: 12, completion_tokens: 3, total_tokens: 15 }]);
  assert.ok(!sent(sink).includes("SECRET"));
});

test("a stream that breaks part way is recorded as failed, and one the caller aborts as cancelled", { skip }, async () => {
  const [rp, sink] = reporter();
  provider.sse([{ ...CHUNK, choices: [{ index: 0, delta: { role: "assistant", content: "SECRET" }, finish_reason: null }] }], { broken: true });
  const broken = sdk!.streamText({ model: openai().chat("gpt-5"), prompt: "x", telemetry: on(rp), onError: () => {} });
  await assert.rejects(async () => {
    for await (const _ of broken.fullStream) if (_.type === "error") throw _.error;
  });
  provider.sse(
    [
      { ...CHUNK, choices: [{ index: 0, delta: { role: "assistant", content: "one" }, finish_reason: null }] },
      { ...CHUNK, choices: [{ index: 0, delta: { content: " two" }, finish_reason: null }] },
      { ...CHUNK, choices: [{ index: 0, delta: {}, finish_reason: "stop" }] },
    ],
    { delayMs: 200 },
  );
  const stop = new AbortController();
  const aborted = sdk!.streamText({ model: openai().chat("gpt-5"), prompt: "x", telemetry: on(rp), abortSignal: stop.signal, onError: () => {} });
  for await (const _ of aborted.textStream) {
    stop.abort();
    break;
  }
  await new Promise((r) => setTimeout(r, 50));
  const [failed, cancelled, ...rest] = await recorded(rp, sink);
  assert.equal(rest.length, 0);
  assert.deepEqual([failed!.status, failed!.model], [STATUS_FAILED, "gpt-5"]);
  assert.deepEqual([cancelled!.status, cancelled!.errorCode], [STATUS_TRUNCATED, "Canceled"]);
});

test("a call that fails is recorded by its code, and each attempt the SDK retries is recorded", { skip }, async () => {
  const [rp, sink] = reporter();
  provider.json({ error: { message: "SECRET QUOTED PROMPT", type: "invalid_request_error", code: "context_length_exceeded" } }, 400);
  await assert.rejects(sdk!.generateText({ model: openai().chat("gpt-5"), prompt: "x", maxRetries: 0, telemetry: on(rp) }));
  provider.json({ error: { message: "SECRET", type: "server_error" } }, 503, { "retry-after-ms": "1" });
  provider.json(STOP);
  const result = await sdk!.generateText({ model: openai().chat("gpt-5"), prompt: "x", maxRetries: 1, telemetry: on(rp) });
  assert.equal(result.text, "SECRET ANSWER");
  const activities = await recorded(rp, sink);
  assert.deepEqual(activities.map((a) => [a.status, a.errorCode ?? ""]), [[STATUS_FAILED, "400"], [STATUS_FAILED, "503"], [STATUS_OK, ""]]);
  assert.ok(!sent(sink).includes("SECRET"));
});

test("a tool the model calls is recorded by its duration and the size of what it returned, and one that throws by its code", { skip }, async () => {
  const [rp, sink] = reporter();
  provider.json(LOOKUP_CALL);
  provider.json(STOP);
  await sdk!.generateText({ model: openai().chat("gpt-5"), prompt: "x", tools: { lookup: lookup(async () => ({ hits: ["SECRET RESULT"] })) }, stopWhen: sdk!.stepCountIs(2), telemetry: on(rp) });
  provider.json(LOOKUP_CALL);
  provider.json(STOP);
  await sdk!.generateText({
    model: openai().chat("gpt-5"),
    prompt: "x",
    tools: { lookup: lookup(async () => Promise.reject(Object.assign(new Error("SECRET"), { code: "ECONNRESET" }))) },
    stopWhen: sdk!.stepCountIs(2),
    telemetry: on(rp),
  });
  const activities = await recorded(rp, sink);
  const tools = activities.filter((a) => a.tool === "lookup");
  assert.equal(activities.length - tools.length, 4);
  assert.deepEqual(tools.map((t) => [t.callerComponent, t.status, t.errorCode ?? "", t.resultBytes, t.framework]), [
    ["tool:lookup", STATUS_OK, "", JSON.stringify({ hits: ["SECRET RESULT"] }).length, "vercel/ai"],
    ["tool:lookup", STATUS_FAILED, "ECONNRESET", undefined, "vercel/ai"],
  ]);
  assert.ok(!sent(sink).includes("SECRET"));
});

test("embeddings are recorded with the SDK's count, one record per call to the provider, and a failed one by its code", { skip }, async () => {
  const [rp, sink] = reporter();
  provider.json({ object: "list", data: [{ object: "embedding", index: 0, embedding: [0.1, 0.2] }], model: "text-embedding-3-small", usage: { prompt_tokens: 7, total_tokens: 7 } });
  await sdk!.embed({ model: openai().embedding("text-embedding-3-small"), value: "SECRET", telemetry: on(rp) });
  provider.json({ object: "list", data: [0, 1].map((index) => ({ object: "embedding", index, embedding: [0.1] })), model: "text-embedding-3-small", usage: { prompt_tokens: 9, total_tokens: 9 } });
  await sdk!.embedMany({ model: openai().embedding("text-embedding-3-small"), values: ["SECRET A", "SECRET B"], telemetry: on(rp) });
  provider.json({ error: { message: "SECRET", type: "rate_limit" } }, 429);
  await assert.rejects(sdk!.embed({ model: openai().embedding("text-embedding-3-small"), value: "SECRET", maxRetries: 0, telemetry: on(rp) }));
  const activities = await recorded(rp, sink);
  assert.deepEqual(activities.map((a) => [a.model, a.billedBy, a.usageFormat ?? "", usage(a), a.status, a.errorCode ?? ""]), [
    ["text-embedding-3-small", "OPENAI", "AI_SDK", { tokens: 7 }, STATUS_OK, ""],
    ["text-embedding-3-small", "OPENAI", "AI_SDK", { tokens: 9 }, STATUS_OK, ""],
    ["text-embedding-3-small", "OPENAI", "", {}, STATUS_FAILED, "429"],
  ]);
  assert.ok(!sent(sink).includes("SECRET"));
});

// Telemetry and the middleware together: the middleware asks first and records, telemetry records only what the middleware did not.

test("with the middleware and telemetry both set up, a call is recorded once, by the middleware", { skip }, async () => {
  const [rp, sink] = reporter();
  provider.json(CHAT);
  await sdk!.generateText({ model: wrapped(rp), prompt: "x", telemetry: on(rp) });
  provider.sse([
    { ...CHUNK, choices: [{ index: 0, delta: { role: "assistant", content: "x" }, finish_reason: null }] },
    { ...CHUNK, choices: [{ index: 0, delta: {}, finish_reason: "stop" }] },
    { ...CHUNK, choices: [], usage: { prompt_tokens: 12, completion_tokens: 3, total_tokens: 15 } },
  ]);
  for await (const _ of sdk!.streamText({ model: wrapped(rp), prompt: "x", telemetry: on(rp) }).textStream);
  provider.json({ error: { message: "SECRET", type: "invalid_request_error" } }, 400);
  await assert.rejects(sdk!.generateText({ model: wrapped(rp), prompt: "x", maxRetries: 0, telemetry: on(rp) }));
  const activities = await recorded(rp, sink);
  assert.deepEqual(activities.map((a) => [a.status, a.errorCode ?? "", usage(a).prompt_tokens]), [
    [STATUS_TRUNCATED, "length", 100],
    [STATUS_OK, "", 12],
    [STATUS_FAILED, "400", undefined],
  ]);
});

test("with both set up, a call a budget refuses is recorded once, as refused, and never sent", { skip }, async () => {
  const decider: Decider = { decide: async () => ({ decision: DECISION_DENY, reason: "", policyVersion: "", replacementProvider: "", replacementModel: "" }) };
  const [rp, sink] = reporter(decider);
  const before = provider.received.length;
  await assert.rejects(sdk!.generateText({ model: wrapped(rp), prompt: "x", telemetry: on(rp) }), (err: unknown) => denied(err) || err instanceof SpendDenied);
  const embedder = sdk!.wrapEmbeddingModel({ model: openai().embedding("text-embedding-3-small"), middleware: rp.aiSdkEmbeddingMiddleware() });
  await assert.rejects(sdk!.embed({ model: embedder, value: "x", telemetry: on(rp) }), (err: unknown) => denied(err));
  assert.equal(provider.received.length, before);
  const activities = await recorded(rp, sink);
  assert.deepEqual(activities.map((a) => a.status), [STATUS_DENIED, STATUS_DENIED]);
});

test("with both set up, an embedding and a wrapped tool are each recorded once", { skip }, async () => {
  const [rp, sink] = reporter();
  const embedder = sdk!.wrapEmbeddingModel({ model: openai().embedding("text-embedding-3-small"), middleware: rp.aiSdkEmbeddingMiddleware() });
  provider.json({ object: "list", data: [{ object: "embedding", index: 0, embedding: [0.1] }], model: "text-embedding-3-small", usage: { prompt_tokens: 7, total_tokens: 7 } });
  await sdk!.embed({ model: embedder, value: "x", telemetry: on(rp) });
  provider.json({ error: { message: "SECRET", type: "rate_limit" } }, 429);
  await assert.rejects(sdk!.embed({ model: embedder, value: "x", maxRetries: 0, telemetry: on(rp) }));
  provider.json(LOOKUP_CALL);
  provider.json(STOP);
  const tools = rp.aiSdkTools({ lookup: lookup(async () => ({ hits: [1] })) });
  await sdk!.generateText({ model: wrapped(rp), prompt: "x", tools, stopWhen: sdk!.stepCountIs(2), telemetry: on(rp) });
  const activities = await recorded(rp, sink);
  assert.deepEqual(activities.map((a) => [a.tool || a.model, a.status, a.usageFormat ?? ""]), [
    ["text-embedding-3-small", STATUS_OK, "OPENAI_CHAT"],
    ["text-embedding-3-small", STATUS_FAILED, ""],
    ["gpt-5-2026-08-01", STATUS_OK, "OPENAI_CHAT"],
    ["lookup", STATUS_OK, ""],
    ["gpt-5-2026-08-01", STATUS_OK, "OPENAI_CHAT"],
  ]);
});

test("a model that is not wrapped is still recorded by telemetry beside one that is", { skip }, async () => {
  const [rp, sink] = reporter();
  provider.json(STOP);
  provider.json(STOP);
  await sdk!.generateText({ model: wrapped(rp), prompt: "x", telemetry: on(rp) });
  await sdk!.generateText({ model: openai().chat("gpt-5"), prompt: "x", telemetry: on(rp, "plain") });
  const activities = await recorded(rp, sink);
  assert.deepEqual(activities.map((a) => a.callerComponent), ["ai-sdk", "plain"]);
});

test("flush delivers what was recorded, and shutdown stops the recorder so what follows is dropped and counted", { skip }, async () => {
  const [rp, sink] = reporter();
  const integration = rp.aiSdkTelemetry();
  provider.json(STOP);
  await sdk!.generateText({ model: openai().chat("gpt-5"), prompt: "x", telemetry: { integrations: [integration] } });
  assert.equal(await integration.flush(2000), true);
  assert.equal(sink.activities.length, 1);
  assert.equal(await integration.shutdown(2000), true);
  provider.json(STOP);
  await sdk!.generateText({ model: openai().chat("gpt-5"), prompt: "x", telemetry: { integrations: [integration] } });
  assert.equal(sink.activities.length, 1);
  assert.equal(rp.recorder.stats().dropped, 1);
});

test("an integration that fails inside is counted and the call goes on", { skip }, async () => {
  const [rp] = reporter();
  (rp as unknown as { modelCall: () => void }).modelCall = () => {
    throw new Error("broken");
  };
  provider.json(STOP);
  const result = await sdk!.generateText({ model: openai().chat("gpt-5"), prompt: "x", telemetry: on(rp) });
  assert.equal(result.text, "SECRET ANSWER");
  assert.equal(rp.recorder.stats().panicked, 1);
});

// The integration on its own, handed events in the shapes the SDK gives it, so what it reads is checked whether or not the SDK is installed.

test("a provider the server has no reader for is recorded in the middleware's shape of the SDK's counts", async () => {
  const [rp, sink] = reporter();
  const t = rp.aiSdkTelemetry();
  t.onLanguageModelCallStart({ callId: "c", provider: "mistral.chat", modelId: "mistral-large" });
  t.onLanguageModelCallEnd({
    callId: "c",
    provider: "mistral.chat",
    modelId: "mistral-large-2511",
    finishReason: "stop",
    usage: { inputTokens: 100, inputTokenDetails: { noCacheTokens: 70, cacheReadTokens: 30, cacheWriteTokens: undefined }, outputTokens: 20, outputTokenDetails: { textTokens: 15, reasoningTokens: 5 }, totalTokens: 120 },
    performance: { responseTimeMs: 42 },
  });
  const [a] = await recorded(rp, sink);
  assert.deepEqual([a!.model, a!.billedBy, a!.usageFormat, a!.durationMs], ["mistral-large-2511", "MISTRAL", "AI_SDK", 42]);
  assert.deepEqual(usage(a!), { "inputTokens.total": 100, "inputTokens.noCache": 70, "inputTokens.cacheRead": 30, "outputTokens.total": 20, "outputTokens.text": 15, "outputTokens.reasoning": 5 });
});

// Every property read is noted by its path. What a call said and what a tool was given or returned are never read, apart from a tool's result, measured for its size.
function watched<T extends object>(value: T, path: string, reads: Set<string>): T {
  return new Proxy(value, {
    get(target, key, receiver) {
      const name = typeof key === "symbol" ? key.toString() : `${path}.${key}`;
      reads.add(name);
      const v = Reflect.get(target, key, receiver);
      return v !== null && typeof v === "object" && !Array.isArray(v) ? watched(v, name, reads) : v;
    },
  });
}

test("nothing a call said, nor what a tool was given, is read", async () => {
  const [rp] = reporter();
  const t = rp.aiSdkTelemetry();
  const reads = new Set<string>();
  const content = { prompt: "SECRET", messages: [{ role: "user", content: "SECRET" }], instructions: "SECRET", system: "SECRET", tools: [{ name: "lookup" }], runtimeContext: { secret: "SECRET" } };
  t.onLanguageModelCallStart(watched({ callId: "c", provider: "openai.chat", modelId: "gpt-5", ...content }, "start", reads));
  await t.executeLanguageModelCall(watched({ callId: "c", provider: "openai.chat", modelId: "gpt-5", ...content, execute: async () => "ok" }, "execute", reads));
  t.onLanguageModelCallEnd(
    watched({ callId: "c", provider: "openai.chat", modelId: "gpt-5", finishReason: "stop", usage: { inputTokens: 1, outputTokens: 1 }, content: [{ type: "text", text: "SECRET" }], responseId: "r", performance: { responseTimeMs: 1 } }, "end", reads),
  );
  const toolCall = { toolCallId: "t1", toolName: "lookup", input: { q: "SECRET" } };
  t.onToolExecutionStart(watched({ callId: "c", toolCall, messages: content.messages, toolContext: { secret: 1 } }, "toolStart", reads));
  await t.executeTool(watched({ callId: "c", toolCallId: "t1", toolCall, messages: content.messages, execute: async () => "ok" }, "toolExecute", reads));
  t.onToolExecutionEnd(watched({ callId: "c", toolCall, messages: content.messages, toolExecutionMs: 3, toolOutput: { type: "tool-result", input: { q: "SECRET" }, output: { rows: 3 } } }, "toolEnd", reads));
  t.onEmbedStart(watched({ callId: "e", embedCallId: "e1", provider: "openai.embedding", modelId: "m", values: ["SECRET"] }, "embedStart", reads));
  t.onEmbedEnd(watched({ callId: "e", embedCallId: "e1", provider: "openai.embedding", modelId: "m", values: ["SECRET"], embeddings: [[0.1]], usage: { tokens: 3 } }, "embedEnd", reads));
  t.onEnd(watched({ callId: "c", steps: [{ text: "SECRET" }], text: "SECRET" }, "operationEnd", reads));
  const allowed = /^(start|execute|end|toolStart|toolExecute|toolEnd|embedStart|embedEnd|operationEnd)\.(callId|provider|modelId|functionId|finishReason|usage(\..*)?|providerMetadata|performance(\.responseTimeMs)?|execute|toolCallId|toolCall(\.toolCallId|\.toolName)?|toolExecutionMs|toolOutput(\.type|\.output(\..*)?)?|embedCallId|embeddings)$/;
  const forbidden = [...reads].filter((path) => !allowed.test(path));
  assert.deepEqual(forbidden, []);
  assert.ok(reads.has("toolEnd.toolOutput.output"));
});
