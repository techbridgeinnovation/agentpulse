// Recording, and a budget that can stop a call, for anything built on the Vercel AI SDK.
//
// One middleware, wrapped around a model where it is created, and nothing at the call sites:
//
//   const model = wrapLanguageModel({ model: openai("gpt-5"), middleware: reporter.aiSdkMiddleware() });
//
// Every generateText, streamText, generateObject and agent step made with that model is recorded. The SDK hands a middleware the provider's own usage as `usage.raw`, so a call to OpenAI, Anthropic or Gemini is recorded in that provider's convention, and any other provider in the SDK's own, `AI_SDK`. Nothing about a call's content is read: the middleware is handed the prompt and the reply, and this takes only the model, the counts, the finish and the failure.
//
// A call to Gemini on Vertex AI is labelled with the agent and component, so a line of the Cloud billing export can be tied back to the agent that made it.
//
// A governed reporter's middleware asks before each call and refuses one by throwing SpendDenied before the provider is called. A middleware cannot change which model its call uses, so a DOWNGRADE lets the call through as asked and is counted as not applied.

import { createRequire } from "node:module";

import { SpendDenied } from "./failure.ts";
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
} from "./usage.ts";

export const FRAMEWORK = "vercel/ai";

// The parts of a language model a middleware is handed that this reads. Declared here rather than imported, so the SDK is not a dependency of this library.
interface Model {
  readonly provider: string;
  readonly modelId: string;
}

// The call options a middleware can change. Only the provider options are read or replaced.
interface Params {
  providerOptions?: Record<string, unknown>;
}

interface FinishReason {
  unified?: string;
  raw?: string;
}

interface Result {
  usage?: unknown;
  finishReason?: FinishReason | string;
  response?: { modelId?: string };
}

interface StreamPart {
  type: string;
  usage?: unknown;
  finishReason?: FinishReason | string;
  modelId?: string;
  error?: unknown;
}

interface StreamResult {
  stream: ReadableStream<StreamPart>;
}

/** The middleware `wrapLanguageModel` takes. */
export interface Middleware {
  readonly specificationVersion: string;
  transformParams<P extends Params>(options: { type: string; params: P; model: Model }): Promise<P>;
  wrapGenerate(options: { doGenerate: () => PromiseLike<Result>; model: Model }): Promise<Result>;
  wrapStream(options: { doStream: () => PromiseLike<StreamResult>; model: Model }): Promise<StreamResult>;
}

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
function usageOf(usage: unknown, format: string): { reported: Record<string, number>; format: string } {
  const u = (usage ?? {}) as { raw?: unknown };
  if (format !== FORMAT_AI_SDK && u.raw && typeof u.raw === "object") {
    const reported = reportedFrom(u.raw);
    if (Object.keys(reported).length) return { reported, format };
  }
  const { raw: _raw, ...normalised } = u as Record<string, unknown>;
  return { reported: reportedFrom(normalised), format: FORMAT_AI_SDK };
}

/** A billing label value as Google accepts one, matching the Go recorder: lowercase, only [a-z0-9_-], at most 63 characters. */
export function labelValue(v: string): string {
  let out = "";
  for (const ch of v.trim().toLowerCase()) out += /^[a-z0-9_-]$/.test(ch) ? ch : "_";
  return out.replace(/^_+|_+$/g, "").slice(0, 63);
}

const isRecord = (v: unknown): v is Record<string, unknown> => typeof v === "object" && v !== null && !Array.isArray(v);

// AI SDK 5 reads Vertex options from `google` only, 6 from `vertex` and 7 from `googleVertex` then `vertex`, each falling back to `google` when its own is absent. Labels go into `google` and into whichever of the others the caller already set, so the namespace the provider reads always carries them and no namespace is added that would displace the caller's.
const VERTEX_NAMESPACES = ["google", "vertex", "googleVertex"];

/** The params with Agent Pulse labels added, for a Gemini call served by Vertex AI; otherwise the params as they were. A label the caller set is never replaced. */
export function withLabels<P extends Params>(params: P, provider: string, agent: string, component: string): P {
  // The Gemini API is not labelled: labels are a Vertex AI billing feature, and the Gemini API has refused the field.
  if (!provider.toLowerCase().startsWith("google.vertex.")) return params;
  const labels: Record<string, string> = {};
  const name = labelValue(agent.slice(agent.lastIndexOf("/") + 1));
  if (name) labels.ap_agent = name;
  const part = labelValue(component);
  if (part) labels.ap_component = part;
  if (!Object.keys(labels).length) return params;
  const given = params.providerOptions;
  if (given !== undefined && !isRecord(given)) return params;
  const providerOptions: Record<string, unknown> = { ...given };
  for (const ns of VERTEX_NAMESPACES) {
    const current = providerOptions[ns];
    if (current === undefined && ns !== "google") continue;
    if (current !== undefined && !isRecord(current)) continue;
    const existing = current?.labels;
    if (existing !== undefined && !isRecord(existing)) continue;
    providerOptions[ns] = { ...current, labels: { ...labels, ...existing } };
  }
  return { ...params, providerOptions };
}

function finishOf(reason: FinishReason | string | undefined): string {
  if (typeof reason === "string") return reason;
  return reason?.raw || reason?.unified || "";
}

export function middleware(reporter: Reporter, options: MiddlewareOptions = {}): Middleware {
  const framework = (): Framework => ({ name: FRAMEWORK, version: installedVersion() });

  const component = options.component || "ai-sdk";

  // Only a refusal leaves here as an error: anything else going wrong inside is counted and the call goes ahead as if the middleware were not there.
  const before = async (model: Model): Promise<{ started: number; billedBy: string; format: string; component: string }> => {
    let call = { started: performance.now(), billedBy: "", format: FORMAT_AI_SDK, component };
    let refused = false;
    try {
      const { billedBy, format } = providerOf(String(model.provider ?? ""));
      call = { ...call, billedBy, format };
      const answer = await reporter.decideFor(String(model.modelId ?? ""), billedBy || reporter.attribution.billedBy || "", component, framework());
      refused = !answer.proceed;
      if (answer.replacementModel) reporter.recorder.note("downgradeNotApplied");
    } catch {
      reporter.recorder.note("panicked");
    }
    if (refused) throw new SpendDenied();
    return { ...call, started: performance.now() };
  };

  const record = (model: Model, call: { started: number; billedBy: string; format: string; component: string }, usage: unknown, finish: string, servedBy: string | undefined, error: unknown): void => {
    try {
      recordUnguarded(model, call, usage, finish, servedBy, error);
    } catch {
      reporter.recorder.note("panicked");
    }
  };

  const recordUnguarded = (model: Model, call: { started: number; billedBy: string; format: string; component: string }, usage: unknown, finish: string, servedBy: string | undefined, error: unknown): void => {
    const { reported, format } = usageOf(usage, call.format);
    reporter.modelCall(
      {
        model: servedBy || model.modelId,
        component: call.component,
        reported,
        format,
        billedBy: call.billedBy,
        durationMs: performance.now() - call.started,
        error,
        finishReason: finish,
      },
      framework(),
    );
  };

  return {
    specificationVersion: options.specificationVersion ?? "v4",

    async transformParams({ params, model }) {
      try {
        return withLabels(params, String(model.provider ?? ""), String(reporter.attribution.agent ?? ""), component);
      } catch {
        reporter.recorder.note("panicked");
        return params;
      }
    },

    async wrapGenerate({ doGenerate, model }) {
      const call = await before(model);
      let result: Result;
      try {
        result = await doGenerate();
      } catch (err) {
        record(model, call, undefined, "", undefined, err);
        throw err;
      }
      record(model, call, result.usage, finishOf(result.finishReason), result.response?.modelId, undefined);
      return result;
    },

    async wrapStream({ doStream, model }) {
      const call = await before(model);
      let result: StreamResult;
      try {
        result = await doStream();
      } catch (err) {
        record(model, call, undefined, "", undefined, err);
        throw err;
      }
      let usage: unknown;
      let finish = "";
      let servedBy: string | undefined;
      let failure: unknown;
      let recorded = false;
      const once = () => {
        if (!recorded) {
          recorded = true;
          record(model, call, usage, finish, servedBy, failure);
        }
      };
      const observe = (part: StreamPart): void => {
        try {
          if (part.type === "finish") {
            usage = part.usage;
            finish = finishOf(part.finishReason);
          } else if (part.type === "response-metadata" && part.modelId) {
            servedBy = part.modelId;
          } else if (part.type === "error") {
            failure = part.error ?? new Error("stream error");
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
            failure = failure ?? err;
            once();
            controller.error(err);
            return;
          }
          if (next.done) {
            once();
            controller.close();
            return;
          }
          observe(next.value);
          controller.enqueue(next.value);
        },
        async cancel(reason: unknown) {
          failure = failure ?? reason ?? new Error("cancelled");
          once();
          await reader.cancel(reason);
        },
      });
      return { ...result, stream: observed };
    },
  };
}
