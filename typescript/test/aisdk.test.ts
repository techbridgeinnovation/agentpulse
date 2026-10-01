// Calls made through the real Vercel AI SDK and its providers, against a provider on this machine. Skipped where the SDK is not installed.

import assert from "node:assert/strict";
import { after, before, beforeEach, test } from "node:test";
import { setFlagsFromString } from "node:v8";
import { runInNewContext } from "node:vm";

import { scope } from "../src/context.ts";
import { denied, SpendDenied } from "../src/failure.ts";
import { type Decider, forgetRefusals } from "../src/governance.ts";
import { Recorder } from "../src/recorder.ts";
import { Reporter } from "../src/report.ts";
import { DECISION_DENY, DECISION_DOWNGRADE, STATUS_FAILED, STATUS_OK, STATUS_TRUNCATED } from "../src/wire.ts";
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

test("an embedding call is recorded with the provider's own counts", { skip: skip || (!("wrapEmbeddingModel" in sdk!) && "this AI SDK has no embedding middleware") }, async () => {
  const [rp, sink] = reporter();
  provider.json({ object: "list", data: [{ object: "embedding", index: 0, embedding: [0.1, 0.2] }], model: "text-embedding-3-small", usage: { prompt_tokens: 7, total_tokens: 7 } });
  const openai = openaiSdk!.createOpenAI({ apiKey: "k", baseURL: provider.url + "/v1" });
  const wrap = (sdk as unknown as { wrapEmbeddingModel: (o: { model: unknown; middleware: unknown }) => unknown }).wrapEmbeddingModel;
  await sdk!.embed({ model: wrap({ model: openai.embedding("text-embedding-3-small"), middleware: rp.aiSdkEmbeddingMiddleware() }) as any, value: "SECRET" });
  const [a] = await recorded(rp, sink);
  assert.deepEqual([a!.model, a!.billedBy, a!.usageFormat, a!.status], ["text-embedding-3-small", "OPENAI", "OPENAI_CHAT", STATUS_OK]);
  assert.deepEqual(usage(a!), { prompt_tokens: 7, total_tokens: 7 });
});

test("a tool the model calls is recorded with its duration and the size of what it returned, never the content", { skip }, async () => {
  const [rp, sink] = reporter();
  provider.json({
    ...CHAT,
    choices: [{ index: 0, finish_reason: "tool_calls", message: { role: "assistant", content: null, tool_calls: [{ id: "t1", type: "function", function: { name: "lookup", arguments: '{"q":"SECRET ARG"}' } }] } }],
  });
  provider.json({ ...CHAT, choices: [{ index: 0, finish_reason: "stop", message: { role: "assistant", content: "done" } }] });
  const tools = rp.aiSdkTools({ lookup: sdk!.tool({ inputSchema: sdk!.jsonSchema<{ q: string }>({ type: "object", properties: { q: { type: "string" } } }), execute: async () => ({ hits: ["SECRET RESULT"] }) }) });
  await sdk!.generateText({ model: chatModel(rp), prompt: "x", tools, stopWhen: sdk!.stepCountIs(2) });
  const tool = (await recorded(rp, sink)).find((a) => a.tool === "lookup")!;
  assert.deepEqual([tool.callerComponent, tool.status, tool.resultBytes, tool.framework], ["tool:lookup", STATUS_OK, JSON.stringify({ hits: ["SECRET RESULT"] }).length, "vercel/ai"]);
  assert.ok(!JSON.stringify(sink.batches, (_, v) => (typeof v === "bigint" ? String(v) : v)).includes("SECRET"));
});

// The middleware on its own, handed streams and replies in the shapes the SDK gives it, so how a call ended is read the same whether or not the SDK is installed.

const MODEL = { provider: "openai.chat", modelId: "gpt-5" };

function parts(list: object[], options: { end?: "close" | "hang" } = {}): ReadableStream<{ type: string }> {
  return new ReadableStream({
    start(controller) {
      for (const p of list) controller.enqueue(p as { type: string });
      if (options.end !== "hang") controller.close();
    },
  });
}

async function streamed(list: object[], read: (stream: ReadableStream<{ type: string }>) => Promise<void>, end?: "close" | "hang") {
  const [rp, sink] = reporter();
  const { stream } = await rp.aiSdkMiddleware().wrapStream({ doStream: async () => ({ stream: parts(list, { end }) }), model: MODEL });
  await read(stream);
  return (await recorded(rp, sink))[0];
}

const drain = async (stream: ReadableStream<{ type: string }>): Promise<void> => {
  for await (const _ of stream);
};

const USAGE = { inputTokens: { total: 10 }, outputTokens: { total: 5 }, raw: { prompt_tokens: 10, completion_tokens: 5 } };
const finish = (unified: string, raw?: string) => ({ type: "finish", usage: USAGE, finishReason: { unified, raw } });

test("a stream that ends with no finish part is recorded as cut short", async () => {
  const a = await streamed([{ type: "text-delta", delta: "x" }], drain);
  assert.deepEqual([a!.status, a!.errorCode], [STATUS_TRUNCATED, ""]);
});

test("a stream cancelled before it finished is recorded as cancelled, and one cancelled after keeps its finish and its usage", async () => {
  const early = await streamed([{ type: "text-delta", delta: "x" }], async (stream) => {
    const reader = stream.getReader();
    await reader.read();
    await reader.cancel();
  }, "hang");
  assert.deepEqual([early!.status, early!.errorCode], [STATUS_TRUNCATED, "Canceled"]);
  const late = await streamed([finish("stop", "stop")], async (stream) => {
    const reader = stream.getReader();
    await reader.read();
    await reader.cancel();
  }, "hang");
  assert.deepEqual([late!.status, usage(late!).prompt_tokens], [STATUS_OK, 10]);
});

test("an error part the provider got past does not fail a stream that then finished", async () => {
  const recovered = await streamed([{ type: "error", error: "SECRET" }, finish("stop", "stop")], drain);
  assert.deepEqual([recovered!.status, usage(recovered!).prompt_tokens], [STATUS_OK, 10]);
  const failed = await streamed([{ type: "error", error: { statusCode: 529 } }, finish("error")], drain);
  assert.deepEqual([failed!.status, failed!.errorCode, usage(failed!).prompt_tokens], [STATUS_FAILED, "529", 10]);
  const plain = await streamed([{ type: "error", error: "SECRET" }, finish("error")], drain);
  assert.deepEqual([plain!.status, plain!.errorCode], [STATUS_FAILED, "error"]);
});

test("a finish the SDK calls an error or other, with no reason from the provider, is a failure", async () => {
  for (const [reason, status, code] of [
    [{ unified: "error" }, STATUS_FAILED, "error"],
    [{ unified: "other" }, STATUS_FAILED, "other"],
    [{ unified: "other", raw: "compaction" }, STATUS_OK, ""],
    [{ unified: "content-filter", raw: "IMAGE_SAFETY" }, STATUS_FAILED, "IMAGE_SAFETY"],
    [{ unified: "length", raw: "model_context_window_exceeded" }, STATUS_TRUNCATED, "model_context_window_exceeded"],
    ["other", STATUS_FAILED, "other"],
  ] as const) {
    const [rp, sink] = reporter();
    await rp.aiSdkMiddleware().wrapGenerate({ doGenerate: async () => ({ usage: USAGE, finishReason: reason }), model: MODEL });
    const [a] = await recorded(rp, sink);
    assert.deepEqual([a!.status, a!.errorCode ?? ""], [status, code], JSON.stringify(reason));
  }
});

test("AI SDK 5's Anthropic cache writes are read from the provider's metadata", async () => {
  const [rp, sink] = reporter();
  await rp.aiSdkMiddleware({ specificationVersion: "v2" }).wrapGenerate({
    doGenerate: async () => ({
      usage: { inputTokens: 10, outputTokens: 20, cachedInputTokens: 300 },
      finishReason: "stop",
      providerMetadata: { anthropic: { usage: { input_tokens: 10, output_tokens: 20, cache_read_input_tokens: 300, cache_creation_input_tokens: 50 }, cacheCreationInputTokens: 50 } },
    }),
    model: { provider: "anthropic.messages", modelId: "claude-sonnet-5" },
  });
  const [a] = await recorded(rp, sink);
  assert.equal(a!.usageFormat, "ANTHROPIC");
  assert.equal(usage(a!).cache_creation_input_tokens, 50);
});

test("a stream nothing reads or cancels is recorded as cancelled once it is collected", async () => {
  setFlagsFromString("--expose-gc");
  const gc = runInNewContext("gc") as () => void;
  const [rp, sink] = reporter();
  await (async () => {
    await rp.aiSdkMiddleware().wrapStream({ doStream: async () => ({ stream: parts([{ type: "text-delta", delta: "x" }], { end: "hang" }) }), model: MODEL });
  })();
  const deadline = performance.now() + 2000;
  let activities = sink.activities;
  while (activities.length === 0 && performance.now() < deadline) {
    gc();
    await new Promise((r) => setTimeout(r, 10));
    activities = await recorded(rp, sink);
  }
  assert.deepEqual([activities[0]?.status, activities[0]?.errorCode], [STATUS_TRUNCATED, "Canceled"]);
});

test("an embedding's usage is the provider's own where the reply has it, the SDK's count otherwise", async () => {
  const [rp, sink] = reporter();
  const mw = rp.aiSdkEmbeddingMiddleware();
  await mw.wrapEmbed({ doEmbed: async () => ({ usage: { tokens: 7 }, response: { body: { usage: { prompt_tokens: 7, total_tokens: 7 } } } }), model: { provider: "openai.embedding", modelId: "text-embedding-3-small" } });
  await mw.wrapEmbed({ doEmbed: async () => ({ usage: { tokens: 4 } }), model: { provider: "mistral.embedding", modelId: "mistral-embed" } });
  await assert.rejects(mw.wrapEmbed({ doEmbed: async () => Promise.reject(Object.assign(new Error("SECRET"), { statusCode: 429 })), model: { provider: "openai.embedding", modelId: "m" } }));
  const [openai, mistral, failed] = await recorded(rp, sink);
  assert.deepEqual([openai!.usageFormat, usage(openai!)], ["OPENAI_CHAT", { prompt_tokens: 7, total_tokens: 7 }]);
  assert.deepEqual([mistral!.usageFormat, usage(mistral!)], ["AI_SDK", { tokens: 4 }]);
  assert.deepEqual([failed!.status, failed!.errorCode], [STATUS_FAILED, "429"]);
});

test("wrapped tools are recorded by size and code, and hand back exactly what the tool did", async () => {
  const [rp, sink] = reporter();
  const circular: Record<string, unknown> = {};
  circular.self = circular;
  const set = rp.aiSdkTools({
    found: { description: "d", execute: async (input: { q: string }) => ({ q: input.q, hits: [1, 2] }) },
    empty: { execute: async () => [] },
    broken: { execute: async () => Promise.reject(Object.assign(new Error("SECRET"), { code: "ECONNRESET" })) },
    unmeasurable: { execute: () => circular },
    streaming: {
      execute: async function* () {
        yield { status: "working" };
        yield { status: "done", rows: 3 };
      },
    },
    provided: { type: "provider-defined" },
  });
  assert.deepEqual(await set.found.execute({ q: "SECRET" }), { q: "SECRET", hits: [1, 2] });
  assert.equal(set.found.description, "d");
  assert.deepEqual(await set.empty.execute(), []);
  await assert.rejects(set.broken.execute(), /SECRET/);
  assert.equal(set.unmeasurable.execute(), circular);
  const yielded = [];
  for await (const v of set.streaming.execute()) yielded.push(v);
  assert.equal(yielded.length, 2);
  assert.deepEqual(set.provided, { type: "provider-defined" });
  const byTool = Object.fromEntries((await recorded(rp, sink)).map((a) => [a.tool, a]));
  assert.deepEqual([byTool.found!.resultBytes, byTool.found!.emptyResult], [JSON.stringify({ q: "SECRET", hits: [1, 2] }).length, undefined]);
  assert.deepEqual([byTool.empty!.resultBytes, byTool.empty!.emptyResult], [undefined, true]);
  assert.deepEqual([byTool.broken!.status, byTool.broken!.errorCode, byTool.broken!.resultBytes], [STATUS_FAILED, "ECONNRESET", undefined]);
  assert.deepEqual([byTool.unmeasurable!.resultBytes, byTool.unmeasurable!.emptyResult], [undefined, undefined]);
  assert.equal(byTool.streaming!.resultBytes, JSON.stringify({ status: "done", rows: 3 }).length);
  assert.equal(byTool.provided, undefined);
});

test("a wrapped tool keeps its prototype, its getters and its own this", async () => {
  const [rp] = reporter();
  class Lookup {
    readonly prefix = "p:";
    get description(): string {
      return "from a getter";
    }
    execute(input: string): string {
      return this.prefix + input;
    }
  }
  const original = new Lookup();
  const set = rp.aiSdkTools({ lookup: original });
  assert.ok(set.lookup instanceof Lookup);
  assert.equal(set.lookup.description, "from a getter");
  assert.equal(set.lookup.execute("x"), "p:x");
  assert.notEqual(set.lookup, original);
});

test("a component and skill set on the request win over the middleware's own", { skip }, async () => {
  const [rp, sink] = reporter();
  provider.json(CHAT);
  const openai = openaiSdk!.createOpenAI({ apiKey: "k", baseURL: provider.url + "/v1" });
  const model = sdk!.wrapLanguageModel({ model: openai.chat("gpt-5"), middleware: rp.aiSdkMiddleware({ component: "drafts" }) as any });
  await scope({ component: "triage", skill: "search" }, () => sdk!.generateText({ model, prompt: "x" }));
  const [a] = await recorded(rp, sink);
  assert.deepEqual([a!.callerComponent, a!.skill], ["triage", "search"]);
});
