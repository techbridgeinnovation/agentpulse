// Recording, and a budget that can stop a call, for anything built on the Vercel AI SDK.
//
// One middleware, wrapped around a model where it is created, and nothing at the call sites:
//
//   const model = wrapLanguageModel({ model: openai("gpt-5"), middleware: reporter.aiSdkMiddleware() });
//
// Every generateText, streamText, generateObject and agent step made with that model is recorded. An embedding model is wrapped the same way, with `wrapEmbeddingModel` and its own middleware, and a set of tools with `reporter.aiSdkTools(tools)`. The SDK hands a middleware the provider's own usage as `usage.raw`, so a call to OpenAI, Anthropic or Gemini is recorded in that provider's convention, and any other provider in the SDK's own, `AI_SDK`. Nothing about a call's content is read: the middleware is handed the prompt and the reply, and this takes only the model, the counts, the finish and the failure.
//
// A governed reporter's middleware asks before each call and refuses one by throwing SpendDenied before the provider is called. A middleware cannot change which model its call uses, so a DOWNGRADE lets the call through as asked and is counted as not applied.

import { createRequire } from "node:module";

import { blockedFinish, Cancelled, errorCode, SpendDenied, truncatedFinish } from "./failure.ts";
import type { Framework, Reporter } from "./report.ts";
import {
  FORMAT_AI_SDK,
  FORMAT_ANTHROPIC,
  FORMAT_OPENAI_CHAT,
  FORMAT_OPENAI_RESPONSES,
  FORMAT_PERPLEXITY,
  FORMAT_VERTEX,
  PROVIDER_ANTHROPIC,
  PROVIDER_OPENAI,
  PROVIDER_PERPLEXITY,
  PROVIDER_VERTEX_AI,
  reportedFrom,
  resultSize,
} from "./usage.ts";

export const FRAMEWORK = "vercel/ai";

// The parts of a language model a middleware is handed that this reads. Declared here rather than imported, so the SDK is not a dependency of this library.
interface Model {
  readonly provider: string;
  readonly modelId: string;
}

interface FinishReason {
  unified?: string;
  raw?: string;
}

interface Result {
  usage?: unknown;
  finishReason?: FinishReason | string;
  providerMetadata?: unknown;
  response?: { modelId?: string };
}

interface StreamPart {
  type: string;
  usage?: unknown;
  finishReason?: FinishReason | string;
  providerMetadata?: unknown;
  modelId?: string;
  error?: unknown;
}

interface EmbedResult {
  usage?: { tokens?: number };
  response?: { body?: unknown };
}

interface StreamResult {
  stream: ReadableStream<StreamPart>;
}

/** The middleware `wrapLanguageModel` takes. */
export interface Middleware {
  readonly specificationVersion: string;
  wrapGenerate(options: { doGenerate: () => PromiseLike<Result>; model: Model }): Promise<Result>;
  wrapStream(options: { doStream: () => PromiseLike<StreamResult>; model: Model }): Promise<StreamResult>;
}

/** The middleware `wrapEmbeddingModel` takes, from AI SDK 6. */
export interface EmbeddingMiddleware {
  readonly specificationVersion: string;
  wrapEmbed(options: { doEmbed: () => PromiseLike<EmbedResult>; model: Model }): Promise<EmbedResult>;
}

/** A set of AI SDK tools, by name. Only each tool's `execute` is read. */
export type Tools = Record<string, object>;

export interface MiddlewareOptions {
  /** The middleware version the installed SDK expects: `v4` for AI SDK 7, `v3` for 6, `v2` for 5. */
  specificationVersion?: string;
  /** The part of the product the calls belong to, where the scope names none. */
  component?: string;
}

let sdkVersion: string | undefined;

function installedVersion(): string {
  if (sdkVersion === undefined) {
    try {
      sdkVersion = String(createRequire(import.meta.url)("ai/package.json").version ?? "");
    } catch {
      sdkVersion = "";
    }
  }
  return sdkVersion;
}

/** Who serves a call and the convention its raw usage is in, from the SDK's provider name, e.g. `openai.responses` or `google.vertex.chat`. */
export function providerOf(provider: string): { billedBy: string; format: string } {
  const p = provider.toLowerCase();
  if (p.includes("anthropic")) return { billedBy: p.includes("vertex") ? PROVIDER_VERTEX_AI : p.includes("bedrock") ? "BEDROCK" : PROVIDER_ANTHROPIC, format: FORMAT_ANTHROPIC };
  if (p.startsWith("google") || p.includes("vertex")) return { billedBy: PROVIDER_VERTEX_AI, format: FORMAT_VERTEX };
  if (p.startsWith("openai.responses")) return { billedBy: PROVIDER_OPENAI, format: FORMAT_OPENAI_RESPONSES };
  if (p.startsWith("openai")) return { billedBy: PROVIDER_OPENAI, format: FORMAT_OPENAI_CHAT };
  if (p.startsWith("perplexity")) return { billedBy: PROVIDER_PERPLEXITY, format: FORMAT_PERPLEXITY };
  return { billedBy: (p.split(".")[0] ?? "").toUpperCase(), format: FORMAT_AI_SDK };
}

/** The counts to record and their convention: the provider's own where the SDK passed them on and the provider is one the server reads, the SDK's otherwise. */
function usageOf(usage: unknown, format: string, metadata?: unknown): { reported: Record<string, number>; format: string } {
  const u = (usage ?? {}) as { raw?: unknown };
  // AI SDK 5 passes no raw usage, and its Anthropic provider leaves Anthropic's own in the metadata instead, which is the only place the cache writes are.
  const raw = u.raw ?? (format === FORMAT_ANTHROPIC ? (metadata as { anthropic?: { usage?: unknown } } | undefined)?.anthropic?.usage : undefined);
  if (format !== FORMAT_AI_SDK && raw && typeof raw === "object") {
    const reported = reportedFrom(raw);
    if (Object.keys(reported).length) return { reported, format };
  }
  const { raw: _raw, ...normalised } = u as Record<string, unknown>;
  return { reported: reportedFrom(normalised), format: FORMAT_AI_SDK };
}

const known = (reason: string): boolean => blockedFinish(reason) || truncatedFinish(reason);

// The provider's own reason, except where the SDK knows the call failed or was cut short and the provider's word is one no list here knows.
function finishOf(reason: FinishReason | string | undefined): string {
  if (typeof reason === "string") return reason;
  const raw = reason?.raw ?? "";
  const unified = reason?.unified ?? "";
  if (raw && !known(raw) && ["error", "content-filter", "length"].includes(unified)) return unified;
  return raw || unified;
}

// A stream that nothing read to its end and nothing cancelled is recorded when it is collected, as cancelled. Collection may come late or, at exit, never, and nothing earlier can tell an abandoned stream from a slow reader.
const abandoned = new FinalizationRegistry<() => void>((settle) => settle());

function frameworkOf(): Framework {
  return { name: FRAMEWORK, version: installedVersion() };
}

interface Outcome {
  usage?: unknown;
  metadata?: unknown;
  finish?: string;
  servedBy?: string;
  error?: unknown;
  truncated?: boolean;
}

type Call = { started: number; billedBy: string; format: string; component: string };

export function middleware(reporter: Reporter, options: MiddlewareOptions = {}): Middleware {
  const before = asker(reporter, options);
  const record = recorder(reporter);

  return {
    specificationVersion: options.specificationVersion ?? "v4",

    async wrapGenerate({ doGenerate, model }) {
      const call = await before(model);
      let result: Result;
      try {
        result = await doGenerate();
      } catch (err) {
        record(model, call, { error: err });
        throw err;
      }
      record(model, call, { usage: result.usage, metadata: result.providerMetadata, finish: finishOf(result.finishReason), servedBy: result.response?.modelId });
      return result;
    },

    async wrapStream({ doStream, model }) {
      const call = await before(model);
      let result: StreamResult;
      try {
        result = await doStream();
      } catch (err) {
        record(model, call, { error: err });
        throw err;
      }
      const seen: Outcome & { finished: boolean; errorPart: unknown; recorded: boolean } = { finished: false, errorPart: undefined, recorded: false };
      // How the stream ended. A finish part decides it, so an error part before one is something the provider got past, unless the finish says the call failed as well. With no finish part the stream stopped early, on an error, a cancel, or by ending with nothing said.
      // A stream its caller stopped, by a cancel or a deadline, was cut short rather than failed, as the Go and Python recorders record it.
      const settle = (stopped: unknown): void => {
        try {
          if (seen.recorded) return;
          seen.recorded = true;
          abandoned.unregister(seen);
          if (seen.finished) {
            const failed = (seen.finish ?? "").toLowerCase() === "error" && errorCode(seen.errorPart) !== "Unknown";
            record(model, call, { ...seen, error: failed ? seen.errorPart : undefined });
            return;
          }
          const error = seen.errorPart ?? stopped;
          const code = error === undefined ? "" : errorCode(error);
          if (error === undefined || code === "Canceled" || code === "DeadlineExceeded") record(model, call, { ...seen, finish: code, truncated: true });
          else record(model, call, { ...seen, error });
        } catch {
          reporter.recorder.note("panicked");
        }
      };
      const observe = (part: StreamPart): void => {
        try {
          if (part.type === "finish") {
            seen.finished = true;
            seen.usage = part.usage;
            seen.metadata = part.providerMetadata;
            seen.finish = finishOf(part.finishReason);
          } else if (part.type === "response-metadata" && part.modelId) {
            seen.servedBy = part.modelId;
          } else if (part.type === "error") {
            seen.errorPart = part.error ?? new Error("stream error");
          }
        } catch {
          reporter.recorder.note("panicked");
        }
      };
      // Read by hand rather than piped, so a caller that stops reading early still has the call recorded, as cancelled.
      const reader = result.stream.getReader();
      const observed = new ReadableStream<StreamPart>({
        async pull(controller) {
          let next: ReadableStreamReadResult<StreamPart>;
          try {
            next = await reader.read();
          } catch (err) {
            settle(err);
            controller.error(err);
            return;
          }
          if (next.done) {
            settle(undefined);
            controller.close();
            return;
          }
          observe(next.value);
          controller.enqueue(next.value);
        },
        async cancel(reason: unknown) {
          settle(stoppedBy(reason));
          await reader.cancel(reason);
        },
      });
      abandoned.register(observed, () => settle(new Cancelled()), seen);
      return { ...result, stream: observed };
    },
  };
}

/** The middleware for `wrapEmbeddingModel`. Embedding models arrived in the SDK's middleware with AI SDK 6, so `specificationVersion` is `v4` for 7 and `v3` for 6. */
export function embeddingMiddleware(reporter: Reporter, options: MiddlewareOptions = {}): EmbeddingMiddleware {
  const before = asker(reporter, options);
  const record = recorder(reporter);
  return {
    specificationVersion: options.specificationVersion ?? "v4",
    async wrapEmbed({ doEmbed, model }) {
      const call = await before(model);
      let result: EmbedResult;
      try {
        result = await doEmbed();
      } catch (err) {
        record(model, call, { error: err });
        throw err;
      }
      // The SDK restates an embedding's usage as one count, and the provider's own, where there is one, is on the reply body. Only its counts are read.
      const body = result.response?.body;
      const raw = body !== null && typeof body === "object" ? (body as { usage?: unknown }).usage : undefined;
      record(model, call, { usage: { ...result.usage, raw } });
      return result;
    },
  };
}

/** The same tools, each one's `execute` recorded as a tool call. A tool without `execute` is passed through as it is. */
export function tools<T extends Tools>(reporter: Reporter, set: T): T {
  const out: Record<string, object> = {};
  for (const [name, tool] of Object.entries(set)) {
    const execute = (tool as { execute?: unknown }).execute;
    if (typeof execute !== "function") {
      out[name] = tool;
      continue;
    }
    // A copy with the same prototype and every property, getters and hidden ones included, so only `execute` differs.
    const wrapped = Object.create(Object.getPrototypeOf(tool) as object | null, Object.getOwnPropertyDescriptors(tool)) as object;
    Object.defineProperty(wrapped, "execute", {
      value: (...args: unknown[]): unknown => run(reporter, name, () => (execute as (...a: unknown[]) => unknown).apply(tool, args)),
      writable: true,
      enumerable: true,
      configurable: true,
    });
    out[name] = wrapped;
  }
  return out as T;
}

// The tool's own result and error reach the SDK unchanged; only the time, the code and the size are kept.
function run(reporter: Reporter, name: string, execute: () => unknown): unknown {
  const started = performance.now();
  const done = (error: unknown, result?: unknown): void => {
    try {
      const size = error === undefined ? resultSize(result) : undefined;
      reporter.toolCall({ tool: name, durationMs: performance.now() - started, error, resultBytes: size?.bytes, emptyResult: size?.empty }, frameworkOf());
    } catch {
      reporter.recorder.note("panicked");
    }
  };
  let out: unknown;
  try {
    out = execute();
  } catch (err) {
    done(err);
    throw err;
  }
  if (out !== null && typeof out === "object" && typeof (out as PromiseLike<unknown>).then === "function") {
    return Promise.resolve(out as PromiseLike<unknown>).then(
      (result) => {
        done(undefined, result);
        return result;
      },
      (err: unknown) => {
        done(err);
        throw err;
      },
    );
  }
  if (out !== null && typeof out === "object" && Symbol.asyncIterator in out) return relay(out as AsyncIterable<unknown>, done);
  done(undefined, out);
  return out;
}

// A tool that streams its progress yields as it goes, and the last thing it yields is its result.
async function* relay(source: AsyncIterable<unknown>, done: (error: unknown, result?: unknown) => void): AsyncGenerator<unknown> {
  let last: unknown;
  let failed = false;
  try {
    for await (const value of source) {
      last = value;
      yield value;
    }
  } catch (err) {
    failed = true;
    done(err);
    throw err;
  } finally {
    if (!failed) done(undefined, last);
  }
}

// What a cancel was given, where it names a deadline, or a cancel otherwise: the reason is whatever the caller passed, and only its code would be kept.
function stoppedBy(reason: unknown): unknown {
  try {
    return errorCode(reason) === "DeadlineExceeded" ? reason : new Cancelled();
  } catch {
    return new Cancelled();
  }
}

// Only a refusal leaves here as an error: anything else going wrong inside is counted and the call goes ahead as if the middleware were not there.
function asker(reporter: Reporter, options: MiddlewareOptions): (model: Model) => Promise<Call> {
  return async (model) => {
    const component = options.component || "ai-sdk";
    let call: Call = { started: performance.now(), billedBy: "", format: FORMAT_AI_SDK, component };
    let refused = false;
    try {
      const { billedBy, format } = providerOf(String(model.provider ?? ""));
      call = { ...call, billedBy, format };
      const answer = await reporter.decideFor(String(model.modelId ?? ""), billedBy || reporter.attribution.billedBy || "", component, frameworkOf());
      refused = !answer.proceed;
      if (answer.replacementModel) reporter.recorder.note("downgradeNotApplied");
    } catch {
      reporter.recorder.note("panicked");
    }
    if (refused) throw new SpendDenied();
    return { ...call, started: performance.now() };
  };
}

function recorder(reporter: Reporter): (model: Model, call: Call, outcome: Outcome) => void {
  return (model, call, outcome) => {
    try {
      const { reported, format } = usageOf(outcome.usage, call.format, outcome.metadata);
      reporter.modelCall(
        {
          model: outcome.servedBy || model.modelId,
          component: call.component,
          reported,
          format,
          billedBy: call.billedBy,
          durationMs: performance.now() - call.started,
          error: outcome.error,
          finishReason: outcome.finish ?? "",
          truncated: outcome.truncated,
        },
        frameworkOf(),
      );
    } catch {
      reporter.recorder.note("panicked");
    }
  };
}
