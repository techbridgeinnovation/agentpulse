// Whether a claim reaches only the call it was opened for, through the real AI SDK 7, under concurrency and nesting. Skipped where AI SDK 7 is not installed, unless AP_AI_SDK_REQUIRED says it must be.
//
// The middleware and the wrapped tools record to one reporter and telemetry to another, so each record says which side made it. Every invocation carries its own request on the scope, and its own token count, result size or least duration, so a record is matched to the invocation that made it rather than counted.

import assert from "node:assert/strict";
import { createServer, type IncomingMessage, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { after, before, test } from "node:test";

import { scope } from "../src/context.ts";
import { Recorder } from "../src/recorder.ts";
import { Reporter } from "../src/report.ts";
import { STATUS_FAILED, STATUS_OK, type Activity } from "../src/wire.ts";
import { MemorySink } from "./fakes.ts";
import { openaiSdk, sdk, telemetrySkip as skip } from "./sdk.ts";

const ROUNDS = 100;

// A provider that answers each request from what the request itself says, so concurrent requests never depend on the order they arrive in.
// An embedding input `n:<n>:d:<ms>` is answered after <ms> with <n> tokens, or with `:fail:<status>` fails with that status. A chat model id `<name>:d:<ms>` is answered after <ms>; `chat` decides the reply.
let chat: (model: string, toolTurn: boolean) => unknown = (model) => stop(model);
let server: Server;
let url = "";

const reply = (model: string, message: object, finish: string) => ({ id: "c", object: "chat.completion", created: 1, model, choices: [{ index: 0, finish_reason: finish, message: { role: "assistant", ...message } }], usage: { prompt_tokens: 3, completion_tokens: 2, total_tokens: 5 } });
const stop = (model: string) => reply(model, { content: "ok" }, "stop");
const toolCalls = (model: string, list: { id: string; name: string; n: number }[]) =>
  reply(model, { content: null, tool_calls: list.map((c) => ({ id: c.id, type: "function", function: { name: c.name, arguments: JSON.stringify({ n: c.n }) } })) }, "tool_calls");

async function body(req: IncomingMessage): Promise<Record<string, unknown>> {
  const chunks: Buffer[] = [];
  for await (const c of req) chunks.push(c as Buffer);
  return JSON.parse(Buffer.concat(chunks).toString() || "{}");
}

before(async () => {
  server = createServer(async (req, res) => {
    const b = await body(req);
    if (req.url?.endsWith("/embeddings")) {
      const inputs = (Array.isArray(b.input) ? b.input : [b.input]).map(String);
      const parsed = inputs.map((v) => ({ n: Number(/n:(\d+)/.exec(v)?.[1] ?? 1), d: Number(/d:(\d+)/.exec(v)?.[1] ?? 0), fail: Number(/fail:(\d+)/.exec(v)?.[1] ?? 0) }));
      await new Promise((r) => setTimeout(r, Math.max(...parsed.map((p) => p.d))));
      if (res.destroyed) return;
      const failure = parsed.find((p) => p.fail)?.fail;
      res.writeHead(failure || 200, { "content-type": "application/json" });
      if (failure) return void res.end(JSON.stringify({ error: { message: "failed", type: "server_error" } }));
      const tokens = parsed.reduce((sum, p) => sum + p.n, 0);
      return void res.end(JSON.stringify({ object: "list", data: parsed.map((_, index) => ({ object: "embedding", index, embedding: [0.1] })), model: String(b.model), usage: { prompt_tokens: tokens, total_tokens: tokens } }));
    }
    const model = String(b.model);
    await new Promise((r) => setTimeout(r, Number(/d:(\d+)/.exec(model)?.[1] ?? 0)));
    if (res.destroyed) return;
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify(chat(model, ((b.messages ?? []) as { role: string }[]).some((m) => m.role === "tool"))));
  }).listen(0, "127.0.0.1");
  await new Promise((r) => server.once("listening", r));
  url = `http://127.0.0.1:${(server.address() as AddressInfo).port}/v1`;
});
after(() => {
  server.closeAllConnections();
  server.close();
});

// Two reporters: `mw` for what the middleware and the wrapped tools record, `tel` for what telemetry records.
function sides() {
  const make = () => {
    const sink = new MemorySink();
    return { sink, rp: new Reporter(new Recorder({ sinks: [sink], exitTimeoutMs: 0, queueSize: 100_000, batchSize: 10_000 }), { agent: "organisations/o/agents/a", service: "s" }) };
  };
  const mw = make();
  const tel = make();
  const ai = sdk!;
  const openai = openaiSdk!.createOpenAI({ apiKey: "k", baseURL: url });
  return {
    ai,
    openai,
    mw: mw.rp,
    telemetry: { integrations: [tel.rp.aiSdkTelemetry()] },
    wrappedEmbedding: (model: Parameters<typeof ai.wrapEmbeddingModel>[0]["model"]) => ai.wrapEmbeddingModel({ model, middleware: mw.rp.aiSdkEmbeddingMiddleware() }),
    wrappedChat: (id: string) => ai.wrapLanguageModel({ model: openai.chat(id), middleware: mw.rp.aiSdkMiddleware() }),
    async records(): Promise<Records> {
      await Promise.all([mw.rp.recorder.flush(10_000), tel.rp.recorder.flush(10_000)]);
      return { mw: mw.sink.activities, tel: tel.sink.activities };
    },
  };
}

type Records = { mw: Activity[]; tel: Activity[] };
type Expected = { request: string; side: "mw" | "tel"; model?: string; tool?: string; tokens?: number; bytes?: number; atLeastMs?: number; status?: number; code?: string; times?: number };

const tokensOf = (a: Activity): number | undefined => a.reportedUsage?.find((q) => q.unit === "prompt_tokens" || q.unit === "tokens")?.quantity;

// Each expected invocation was recorded exactly once (or `times` times, for the steps of one generation), by its own side, carrying its own request and, where given, its own model or tool, tokens, result size, least duration, status and code; and nothing else was recorded.
function each(records: Records, expected: Expected[]): void {
  const key = (a: { request?: string; model?: string; tool?: string }) => `${a.request}|${a.tool || a.model}`;
  for (const e of expected) {
    const matches = (a: Activity) => a.request === e.request && (e.model === undefined || a.model === e.model) && (e.tool === undefined || a.tool === e.tool);
    const mine = records[e.side].filter(matches);
    const other = records[e.side === "mw" ? "tel" : "mw"].filter(matches);
    assert.equal(mine.length, e.times ?? 1, `${key(e)}: ${mine.length} records by ${e.side}`);
    assert.equal(other.length, 0, `${key(e)}: recorded by the other side too`);
    const a = mine[0]!;
    if (e.tokens !== undefined) assert.equal(tokensOf(a), e.tokens, `${key(e)}: carries another invocation's tokens`);
    if (e.bytes !== undefined) assert.equal(a.resultBytes, e.bytes, `${key(e)}: carries another invocation's result size`);
    if (e.atLeastMs !== undefined) assert.ok(a.durationMs! >= e.atLeastMs - 2, `${key(e)}: ${a.durationMs}ms is shorter than its own ${e.atLeastMs}ms`);
    if (e.status !== undefined) assert.equal(a.status, e.status, `${key(e)}: status`);
    if (e.code !== undefined) assert.equal(a.errorCode, e.code, `${key(e)}: code`);
  }
  assert.equal(records.mw.length + records.tel.length, expected.reduce((sum, e) => sum + (e.times ?? 1), 0), "a record no invocation accounts for");
}

// --- Embeddings ---

test(`same-model concurrent embeddings, wrapped and not, each recorded once by its own side with its own tokens and duration, both completion orders, ${ROUNDS} rounds`, { skip }, async () => {
  const { ai, openai, telemetry, wrappedEmbedding, records } = sides();
  const expected: Expected[] = [];
  for (let round = 0; round < ROUNDS; round++) {
    // Even rounds the wrapped calls finish first, odd rounds the unwrapped ones.
    await Promise.all(Array.from({ length: 8 }, (_, i) => {
      const wrapped = i % 2 === 0;
      const delay = (wrapped === (round % 2 === 0) ? 2 : 25) + i;
      const request = `r${round}-${i}`;
      const n = round * 8 + i + 1;
      expected.push({ request, side: wrapped ? "mw" : "tel", model: "embed-1", tokens: n, atLeastMs: delay, status: STATUS_OK });
      const model = wrapped ? wrappedEmbedding(openai.embedding("embed-1")) : openai.embedding("embed-1");
      return scope({ request }, () => ai.embed({ model, value: `n:${n}:d:${delay}`, telemetry }));
    }));
  }
  each(await records(), expected);
});

for (const reason of ["the default AbortError", "a custom error"]) {
  test(`same-model embeddings aborted by one signal with ${reason}, the one same error object, each fail once by their own side, both orders, ${ROUNDS} rounds`, { skip }, async () => {
    const { ai, openai, telemetry, wrappedEmbedding, records } = sides();
    const expected: Expected[] = [];
    let shared = 0;
    for (let round = 0; round < ROUNDS; round++) {
      const controller = new AbortController();
      const order = round % 2 === 0 ? [true, false] : [false, true];
      const errors: unknown[] = [];
      const running = order.map((wrapped) => {
        const request = `r${round}-${wrapped ? "w" : "p"}`;
        expected.push({ request, side: wrapped ? "mw" : "tel", model: "embed-1", status: STATUS_FAILED, code: reason === "a custom error" ? "Unknown" : "Canceled" });
        const model = wrapped ? wrappedEmbedding(openai.embedding("embed-1")) : openai.embedding("embed-1");
        return scope({ request }, () => ai.embed({ model, value: "n:1:d:300", maxRetries: 0, abortSignal: controller.signal, telemetry })).catch((e: unknown) => void errors.push(e));
      });
      setTimeout(() => (reason === "a custom error" ? controller.abort(new Error("stopped")) : controller.abort()), 5);
      await Promise.all(running);
      if (errors.length === 2 && errors[0] === errors[1]) shared++;
    }
    assert.equal(shared, ROUNDS, "every round's two calls failed with the one same error object");
    each(await records(), expected);
  });
}

test(`same-model embeddings failing with the same status, wrapped and not, each recorded once by their own side, both orders, ${ROUNDS} rounds`, { skip }, async () => {
  const { ai, openai, telemetry, wrappedEmbedding, records } = sides();
  const expected: Expected[] = [];
  for (let round = 0; round < ROUNDS; round++) {
    await Promise.all([true, false, true, false].map((wrapped, i) => {
      const request = `r${round}-${i}`;
      const delay = (wrapped === (round % 2 === 0) ? 1 : 15) + i;
      expected.push({ request, side: wrapped ? "mw" : "tel", model: "embed-1", status: STATUS_FAILED, code: "503", atLeastMs: delay });
      const model = wrapped ? wrappedEmbedding(openai.embedding("embed-1")) : openai.embedding("embed-1");
      return scope({ request }, () => ai.embed({ model, value: `n:1:d:${delay}:fail:503`, maxRetries: 0, telemetry })).catch(() => {});
    }));
  }
  each(await records(), expected);
});

test(`an embedMany the SDK splits into parallel calls is recorded call by call, by its own side, beside one of the same model, ${ROUNDS} rounds`, { skip }, async () => {
  const { ai, openai, telemetry, wrappedEmbedding, records } = sides();
  // Two values a call, made in parallel: one operation is three calls to the provider at once.
  const chunked = () => {
    const inner = openai.embedding("embed-1");
    return Object.assign(Object.create(inner) as typeof inner, { maxEmbeddingsPerCall: 2, supportsParallelCalls: true });
  };
  const expected: Expected[] = [];
  for (let round = 0; round < ROUNDS; round++) {
    await Promise.all([true, false].map((wrapped) => {
      const request = `r${round}-${wrapped ? "w" : "p"}`;
      const base = round * 100 + (wrapped ? 0 : 50);
      // Each call carries two values; its tokens are the sum of theirs, which no other call has.
      for (const k of [1, 3, 5]) expected.push({ request: `${request}#${k}`, side: wrapped ? "mw" : "tel", tokens: base + k + base + k + 1 });
      const values = [1, 2, 3, 4, 5, 6].map((k) => `n:${base + k}:d:${(7 - k) * 2}`);
      return scope({ request }, () => ai.embedMany({ model: wrapped ? wrappedEmbedding(chunked()) : chunked(), values, telemetry }));
    }));
  }
  const r = await records();
  // A call's request is the operation's; tell the three apart by their tokens.
  const relabel = (list: Activity[]) => list.map((a) => ({ ...a, request: `${a.request}#${(tokensOf(a)! - 1 - 2 * (Number(/^r(\d+)/.exec(a.request!)![1]) * 100 + (a.request!.endsWith("w") ? 0 : 50))) / 2}` }));
  each({ mw: relabel(r.mw), tel: relabel(r.tel) }, expected);
});

test("nested embeddings: inside an unwrapped tool, inside a wrapped tool, without telemetry, inside another embedding model, and inside a tool inside one", { skip }, async () => {
  const { ai, openai, telemetry, wrappedEmbedding, mw, records } = sides();
  const once = new Set<string>();
  chat = (model, toolTurn) => (toolTurn || once.has(model) ? stop(model) : (once.add(model), toolCalls(model, [{ id: "t", name: "search", n: 1 }])));
  const tool = (execute: () => Promise<unknown>) => ({ search: ai.tool({ inputSchema: ai.jsonSchema<{ n: number }>({ type: "object", properties: { n: { type: "number" } } }), execute }) });

  // A wrapped embedding inside an unwrapped tool, both under telemetry.
  await scope({ request: "a" }, () =>
    ai.generateText({ model: openai.chat("outer-a"), prompt: "x", stopWhen: ai.stepCountIs(2), telemetry, tools: tool(async () => (await ai.embed({ model: wrappedEmbedding(openai.embedding("inner-a")), value: "n:11", telemetry })).embedding.length) }),
  );
  // An unwrapped embedding inside a wrapped tool.
  await scope({ request: "b" }, () =>
    ai.generateText({ model: openai.chat("outer-b"), prompt: "x", stopWhen: ai.stepCountIs(2), telemetry, tools: mw.aiSdkTools(tool(async () => (await ai.embed({ model: openai.embedding("inner-b"), value: "n:12", telemetry })).embedding.length)) }),
  );
  // A wrapped embedding with telemetry turned off, inside an unwrapped tool under telemetry.
  await scope({ request: "c" }, () =>
    ai.generateText({ model: openai.chat("outer-c"), prompt: "x", stopWhen: ai.stepCountIs(2), telemetry, tools: tool(async () => (await ai.embed({ model: wrappedEmbedding(openai.embedding("inner-c")), value: "n:13", telemetry: { isEnabled: false } })).embedding.length) }),
  );
  // An unwrapped embedding model whose own call makes a wrapped embedding: the inner operation is claimed, the outer one is not.
  const inner = wrappedEmbedding(openai.embedding("inner-d"));
  const outer = Object.assign(Object.create(openai.embedding("outer-d")) as ReturnType<typeof openai.embedding>, {
    modelId: "outer-d",
    doEmbed: async (options: { values: string[] }) => {
      const nested = await ai.embed({ model: inner, value: "n:14", telemetry });
      return { embeddings: options.values.map(() => nested.embedding), usage: { tokens: 15 } };
    },
  });
  await scope({ request: "d" }, () => ai.embed({ model: outer, value: "x", telemetry }));
  // An unwrapped embedding model whose own call runs a generation, whose tool makes a wrapped embedding with telemetry turned off: the tool runs outside the embedding operation's claim, so the outer embedding is still recorded.
  const deep = Object.assign(Object.create(openai.embedding("outer-e")) as ReturnType<typeof openai.embedding>, {
    modelId: "outer-e",
    doEmbed: async (options: { values: string[] }) => {
      await ai.generateText({
        model: openai.chat("middle-e"),
        prompt: "x",
        stopWhen: ai.stepCountIs(2),
        telemetry,
        tools: tool(async () => (await ai.embed({ model: wrappedEmbedding(openai.embedding("inner-e")), value: "n:16", telemetry: { isEnabled: false } })).embedding.length),
      });
      return { embeddings: options.values.map(() => [0.1]), usage: { tokens: 17 } };
    },
  });
  await scope({ request: "e" }, () => ai.embed({ model: deep, value: "x", telemetry }));

  // Each outer generation is two model calls: the step that asks for the tool, and the one after it.
  each(await records(), [
    { request: "a", side: "tel", model: "outer-a", times: 2 },
    { request: "a", side: "tel", tool: "search" },
    { request: "a", side: "mw", model: "inner-a", tokens: 11 },
    { request: "b", side: "tel", model: "outer-b", times: 2 },
    { request: "b", side: "mw", tool: "search" },
    { request: "b", side: "tel", model: "inner-b", tokens: 12 },
    { request: "c", side: "tel", model: "outer-c", times: 2 },
    { request: "c", side: "tel", tool: "search" },
    { request: "c", side: "mw", model: "inner-c", tokens: 13 },
    { request: "d", side: "tel", model: "outer-d", tokens: 15 },
    { request: "d", side: "mw", model: "inner-d", tokens: 14 },
    { request: "e", side: "tel", model: "outer-e", tokens: 17 },
    { request: "e", side: "tel", model: "middle-e", times: 2 },
    { request: "e", side: "tel", tool: "search" },
    { request: "e", side: "mw", model: "inner-e", tokens: 16 },
  ]);
});

// --- Tools ---

const sized = (ai: NonNullable<typeof sdk>, delayOf: (n: number) => number = () => 0) =>
  ai.tool({
    inputSchema: ai.jsonSchema<{ n: number }>({ type: "object", properties: { n: { type: "number" } } }),
    execute: async ({ n }: { n: number }) => {
      await new Promise((r) => setTimeout(r, delayOf(n)));
      return "x".repeat(n);
    },
  });

test(`parallel tool calls in one step, wrapped and not, each recorded once by its own side with its own result size, ${ROUNDS} rounds`, { skip }, async () => {
  const { ai, openai, telemetry, mw, records } = sides();
  const plan = new Map<string, { id: string; name: string; n: number }[]>();
  chat = (model, toolTurn) => (toolTurn ? stop(model) : toolCalls(model, plan.get(model.split(":")[0]!)!));
  const tools = { plain: sized(ai, (n) => n % 13), ...mw.aiSdkTools({ wrapped: sized(ai, (n) => 13 - (n % 13)) }) };
  const expected: Expected[] = [];
  for (let round = 0; round < ROUNDS; round++) {
    const list = Array.from({ length: 6 }, (_, i) => ({ id: `call-${i}`, name: i % 2 ? "plain" : "wrapped", n: round * 6 + i + 1 }));
    plan.set(`m${round}`, list);
    const request = `r${round}`;
    for (const c of list) expected.push({ request, side: c.name === "wrapped" ? "mw" : "tel", tool: c.name, bytes: JSON.stringify("x".repeat(c.n)).length, status: STATUS_OK });
  }
  await Promise.all(Array.from({ length: ROUNDS }, (_, round) => scope({ request: `r${round}` }, () => ai.generateText({ model: openai.chat(`m${round}`), prompt: "x", tools, stopWhen: ai.stepCountIs(2), telemetry }))));
  const r = await records();
  // Two calls of one tool in one request are told apart by their result sizes.
  for (const e of expected) {
    const side = r[e.side].filter((a) => a.request === e.request && a.tool === e.tool && a.resultBytes === e.bytes);
    assert.equal(side.length, 1, `${e.request} ${e.tool} ${e.bytes}B by ${e.side}`);
  }
  assert.equal(r.mw.filter((a) => a.tool).length + r.tel.filter((a) => a.tool).length, ROUNDS * 6);
  assert.equal(r.mw.filter((a) => !a.tool).length, 0);
  assert.equal(r.tel.filter((a) => !a.tool).length, ROUNDS * 2);
});

test("tool calls sharing one id in one step, wrapped and not, are each recorded once by their own side with their own result size", { skip }, async () => {
  const { ai, openai, telemetry, mw, records } = sides();
  const list = [
    { id: "dup", name: "wrapped", n: 3 },
    { id: "dup", name: "plain", n: 5 },
    { id: "dup", name: "wrapped", n: 7 },
    { id: "dup", name: "plain", n: 9 },
  ];
  chat = (model, toolTurn) => (toolTurn ? stop(model) : toolCalls(model, list));
  // The calls finish in the reverse of the order they started.
  const tools = { plain: sized(ai, (n) => 40 - 4 * n), ...mw.aiSdkTools({ wrapped: sized(ai, (n) => 40 - 4 * n) }) };
  await scope({ request: "dup" }, () => ai.generateText({ model: openai.chat("m"), prompt: "x", tools, stopWhen: ai.stepCountIs(2), telemetry }));
  const r = await records();
  for (const c of list) {
    const side = c.name === "wrapped" ? "mw" : "tel";
    const other = side === "mw" ? "tel" : "mw";
    const bytes = JSON.stringify("x".repeat(c.n)).length;
    assert.equal(r[side].filter((a) => a.tool === c.name && a.resultBytes === bytes).length, 1, `${c.name} n=${c.n} by ${side}`);
    assert.equal(r[other].filter((a) => a.tool === c.name && a.resultBytes === bytes).length, 0, `${c.name} n=${c.n} by ${other} too`);
  }
  assert.equal(r.mw.length + r.tel.filter((a) => a.tool).length, 4);
});

test("nested tools: a wrapped tool inside an unwrapped one, an unwrapped inside a wrapped one, with and without telemetry on the inner call", { skip }, async () => {
  const { ai, openai, telemetry, mw, records } = sides();
  chat = (model, toolTurn) => (toolTurn ? stop(model) : toolCalls(model, [{ id: model.startsWith("outer") ? "outer-call" : "inner-call", name: model.startsWith("outer") ? "outer" : "inner", n: model.startsWith("outer") ? 2 : 4 }]));
  const runInner = (innerTools: object, innerTelemetry: object) => async () => (await ai.generateText({ model: openai.chat("inner"), prompt: "x", tools: innerTools as never, stopWhen: ai.stepCountIs(2), telemetry: innerTelemetry })).text;
  const outerTool = (execute: () => Promise<unknown>) => ({ outer: ai.tool({ inputSchema: ai.jsonSchema<{ n: number }>({ type: "object", properties: { n: { type: "number" } } }), execute }) });
  const wrappedInner = mw.aiSdkTools({ inner: sized(ai) });
  const plainInner = { inner: sized(ai) };
  // Each case: its name, the outer tools, and which side records the outer and the inner tool.
  const cases: [string, object, "mw" | "tel", "mw" | "tel" | "none"][] = [
    ["wrapped inside unwrapped, inner without telemetry", outerTool(runInner(wrappedInner, { isEnabled: false })), "tel", "mw"],
    ["wrapped inside unwrapped, inner with telemetry", outerTool(runInner(wrappedInner, telemetry)), "tel", "mw"],
    ["unwrapped inside wrapped, inner with telemetry", mw.aiSdkTools(outerTool(runInner(plainInner, telemetry))), "mw", "tel"],
    ["unwrapped inside wrapped, inner without telemetry", mw.aiSdkTools(outerTool(runInner(plainInner, { isEnabled: false }))), "mw", "none"],
    ["wrapped inside wrapped, inner without telemetry", mw.aiSdkTools(outerTool(runInner(wrappedInner, { isEnabled: false }))), "mw", "mw"],
  ];
  for (const [name, tools] of cases) await scope({ request: name }, () => ai.generateText({ model: openai.chat("outer"), prompt: "x", tools: tools as never, stopWhen: ai.stepCountIs(2), telemetry }));
  const r = await records();
  for (const [name, , outerSide, innerSide] of cases) {
    const count = (side: "mw" | "tel", tool: string) => r[side].filter((a) => a.request === name && a.tool === tool).length;
    assert.deepEqual([count("mw", "outer"), count("tel", "outer")], outerSide === "mw" ? [1, 0] : [0, 1], `${name}: outer tool`);
    assert.deepEqual([count("mw", "inner"), count("tel", "inner")], innerSide === "mw" ? [1, 0] : innerSide === "tel" ? [0, 1] : [0, 0], `${name}: inner tool`);
  }
});

// --- Model calls ---

test(`concurrent model calls, wrapped and not, each recorded once by its own side with its own duration, both completion orders, ${ROUNDS} rounds`, { skip }, async () => {
  const { ai, openai, telemetry, wrappedChat, records } = sides();
  chat = (model) => stop(model);
  const expected: Expected[] = [];
  for (let round = 0; round < ROUNDS; round++) {
    await Promise.all(Array.from({ length: 6 }, (_, i) => {
      const wrapped = i % 2 === 0;
      const delay = (wrapped === (round % 2 === 0) ? 1 : 20) + i;
      const request = `r${round}-${i}`;
      const id = `chat:d:${delay}`;
      expected.push({ request, side: wrapped ? "mw" : "tel", model: id, atLeastMs: delay, status: STATUS_OK });
      return scope({ request }, () => ai.generateText({ model: wrapped ? wrappedChat(id) : openai.chat(id), prompt: "x", telemetry }));
    }));
  }
  each(await records(), expected);
});

test(`model calls aborted by one signal, wrapped and not, each fail once by their own side, both orders, ${ROUNDS} rounds`, { skip }, async () => {
  const { ai, openai, telemetry, wrappedChat, records } = sides();
  chat = (model) => stop(model);
  const expected: Expected[] = [];
  for (let round = 0; round < ROUNDS; round++) {
    const controller = new AbortController();
    const order = round % 2 === 0 ? [true, false] : [false, true];
    const running = order.map((wrapped) => {
      const request = `r${round}-${wrapped ? "w" : "p"}`;
      expected.push({ request, side: wrapped ? "mw" : "tel", status: STATUS_FAILED });
      return scope({ request }, () => ai.generateText({ model: wrapped ? wrappedChat("chat:d:300") : openai.chat("chat:d:300"), prompt: "x", maxRetries: 0, abortSignal: controller.signal, telemetry })).catch(() => {});
    });
    setTimeout(() => (round % 4 < 2 ? controller.abort() : controller.abort(new Error("stopped"))), 5);
    await Promise.all(running);
  }
  each(await records(), expected);
});
