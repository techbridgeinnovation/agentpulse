// Recording for anything built on Vercel AI SDK 7, through the SDK's own telemetry interface.
//
// One integration, registered once where the process starts, and nothing at the call sites:
//
//   registerTelemetry(reporter.aiSdkTelemetry());
//
// Every generateText, streamText, generateObject, embed, embedMany and agent step is then recorded once the SDK reports it finished, with the tools the model asked for. An integration passed on a single call, `telemetry: { integrations: [...] }`, takes the place of the registered ones for that call, as the SDK has it, and a call with `telemetry: { isEnabled: false }` is not recorded.
//
// Telemetry hears of a call as it happens and cannot stop one, so it never asks a budget. The middleware in aisdk.ts is what asks before a call and refuses one. Where both are set up, the middleware records the calls of the models it wraps and the wrapped tools their own, and this records everything else, so each call is recorded once.
//
// A call is left to the middleware only where the middleware claimed that very call. Each model attempt runs inside `executeLanguageModelCall` and each tool inside `executeTool`, in a claim of its own; a wrapped tool claims only the claim carrying its own toolCallId. The SDK gives no such hook around an embedding call, but runs each `embed` and `embedMany` inside its tracing channel, so a claim is bound to that channel for each operation; every call inside one operation is made with the same model, so the middleware wraps all of them or none. A claim lives in the async context of the one call it was opened for, so no concurrent call can see it, and a call nested inside another opens its own or, without telemetry, finds one of another kind or another id.
//
// Nothing about a call's content is read. The SDK hands an integration the prompt, the reply, the tools' arguments and their results; this reads the model, the counts, the timings, the finish, a failure's code and the size of a tool's result as JSON.
//
// What the SDK does not tell an integration is not recorded. A model call the SDK retries is seen once per attempt only where it fails before the provider answers. A stream nobody reads to its end and nobody aborts is never reported finished, so it is not recorded; the middleware records one as cancelled.

import { tracingChannel } from "node:diagnostics_channel";

import { claims, type Claim, embeddingClaims, finishOf, frameworkOf, providerOf, stoppedBy, usageOf } from "./aisdk.ts";
import { currentScope, type Scope, scope } from "./context.ts";
import { errorCode } from "./failure.ts";
import type { Reporter } from "./report.ts";
import { FORMAT_AI_SDK, resultSize } from "./usage.ts";

// The SDK's tracing channel, inside which it runs every operation.
const TRACING_CHANNEL = "ai:telemetry";

// How many calls, steps and tools in flight are remembered at once. One the SDK never reports finished, such as a stream nobody read, is forgotten once this many newer ones have started.
const IN_FLIGHT = 4096;

// The parts of the SDK's events this reads. Declared here rather than imported, so the SDK is not a dependency of this library.
interface ModelEvent {
  readonly callId?: string;
  readonly provider?: string;
  readonly modelId?: string;
  readonly functionId?: string;
}

interface ModelEndEvent extends ModelEvent {
  readonly finishReason?: string;
  readonly usage?: unknown;
  readonly providerMetadata?: unknown;
  readonly performance?: { readonly responseTimeMs?: number };
}

interface ToolEvent {
  readonly callId?: string;
  readonly functionId?: string;
  readonly toolCall?: { readonly toolCallId?: string; readonly toolName?: string };
}

interface ToolEndEvent extends ToolEvent {
  readonly toolExecutionMs?: number;
  readonly toolOutput?: { readonly type?: string; readonly output?: unknown; readonly error?: unknown };
}

interface EmbedEvent extends ModelEvent {
  readonly embedCallId?: string;
}

interface EmbedEndEvent extends EmbedEvent {
  readonly embeddings?: unknown;
  readonly usage?: { readonly tokens?: number };
}

interface StoppedEvent {
  readonly callId?: string;
  readonly error?: unknown;
  readonly reason?: unknown;
}

export interface AiSdkTelemetryOptions {
  /** The part of the product the calls belong to, where neither the scope nor the call's `functionId` names one. */
  component?: string;
}

/** What `registerTelemetry` takes from AI SDK 7, and a way to deliver what it recorded. */
export interface AiSdkTelemetry {
  onLanguageModelCallStart(event: ModelEvent): void;
  onLanguageModelCallEnd(event: ModelEndEvent): void;
  onToolExecutionStart(event: ToolEvent): void;
  onToolExecutionEnd(event: ToolEndEvent): void;
  onEmbedStart(event: EmbedEvent): void;
  onEmbedEnd(event: EmbedEndEvent): void;
  onEnd(event: StoppedEvent): void;
  onAbort(event: StoppedEvent): void;
  onError(event: unknown): void;
  executeLanguageModelCall<T>(options: ModelEvent & { execute: () => PromiseLike<T> }): PromiseLike<T>;
  executeTool<T>(options: { callId?: string; toolCallId?: string; toolCall?: ToolEvent["toolCall"]; execute: () => PromiseLike<T> }): PromiseLike<T>;
  /** Delivers what has been recorded so far and resolves whether that finished within `timeoutMs`. The same as `reporter.recorder.flush`. */
  flush(timeoutMs?: number): Promise<boolean>;
  /** Delivers what has been recorded, then stops the reporter's recorder; anything recorded afterwards is dropped and counted. The same as `reporter.recorder.close`. */
  shutdown(timeoutMs?: number): Promise<boolean>;
}

// A model call, a tool or an embedding call that has started: when, under what scope, and what the start said. The scope is held because the SDK may report the end from wherever its stream is read.
interface Started {
  callId: string;
  at: number;
  scope: Scope;
  component: string;
  provider: string;
  model: string;
  claim?: Claim;
  // The attempt failed and was recorded, so nothing later records it again.
  settled?: boolean;
}

const text = (value: unknown): string => (typeof value === "string" ? value : "");

function remember<K, V>(map: Map<K, V>, key: K, value: V): void {
  map.delete(key);
  if (map.size >= IN_FLIGHT) {
    const oldest = map.keys().next();
    if (!oldest.done) map.delete(oldest.value);
  }
  map.set(key, value);
}

// The SDK states a call's usage to an integration in a shape of its own, flatter than the one it hands a middleware. The middleware's shape is the one the server reads, so a call is recorded the same whichever saw it.
function restated(usage: unknown): unknown {
  if (usage === null || typeof usage !== "object") return usage;
  const u = usage as {
    inputTokens?: unknown;
    outputTokens?: unknown;
    inputTokenDetails?: { noCacheTokens?: unknown; cacheReadTokens?: unknown; cacheWriteTokens?: unknown };
    outputTokenDetails?: { textTokens?: unknown; reasoningTokens?: unknown };
    raw?: unknown;
  };
  return {
    inputTokens: { total: u.inputTokens, noCache: u.inputTokenDetails?.noCacheTokens, cacheRead: u.inputTokenDetails?.cacheReadTokens, cacheWrite: u.inputTokenDetails?.cacheWriteTokens },
    outputTokens: { total: u.outputTokens, text: u.outputTokenDetails?.textTokens, reasoning: u.outputTokenDetails?.reasoningTokens },
    raw: u.raw,
  };
}

let bound = false;

// Once for the process: every `embed` and `embedMany` runs with a claim of its own, and anything else the channel runs, such as a step or a tool inside which an embedding is made, with none. Binding a store is what makes the SDK run its operations inside the channel; an operation with telemetry turned off is not run inside it, and is not reported either.
function bindEmbeddingClaims(): void {
  if (bound) return;
  bound = true;
  tracingChannel(TRACING_CHANNEL).start.bindStore(embeddingClaims, (message: unknown) => {
    const m = message as { type?: unknown; event?: { callId?: unknown } } | null;
    return m?.type === "embed" || m?.type === "embedMany" ? { kind: "embedding", callId: text(m.event?.callId), claimed: false } : undefined;
  });
}

// The claim of the embedding operation running here, where it is the operation named.
function embeddingClaim(callId: string): Claim | undefined {
  const held = embeddingClaims.getStore();
  return held?.kind === "embedding" && held.callId === callId ? held : undefined;
}

// A tool call is known by the SDK's own object for it, the same object at its start, its run and its end, so two calls that share an id are never mistaken for each other.
const toolKey = (toolCall: unknown, id: string): object | string => (toolCall !== null && typeof toolCall === "object" ? toolCall : id);

export function telemetry(reporter: Reporter, options: AiSdkTelemetryOptions = {}): AiSdkTelemetry {
  const models = new Map<string, Started>();
  const tools = new Map<object | string, Started & { tool: string }>();
  const embeds = new Map<string, Started>();

  try {
    bindEmbeddingClaims();
  } catch {
    reporter.recorder.note("panicked");
  }

  const panicked = (): void => reporter.recorder.note("panicked");

  // A component on the scope wins, then the call's own functionId, then the integration's.
  const componentOf = (functionId: unknown): string => currentScope().component || text(functionId) || options.component || "ai-sdk";

  const started = (event: ModelEvent): Started => ({
    callId: text(event.callId),
    at: performance.now(),
    scope: currentScope(),
    component: componentOf(event.functionId),
    provider: text(event.provider),
    model: text(event.modelId),
  });

  // Records under the scope the call started in, where it was seen starting.
  const within = (from: Started | undefined, fn: () => void): void => {
    if (from) scope(from.scope, fn);
    else fn();
  };

  const modelFailed = (from: Started, error: unknown): void => {
    from.settled = true;
    within(from, () =>
      reporter.modelCall(
        { model: from.model, component: from.component, billedBy: providerOf(from.provider).billedBy, durationMs: performance.now() - from.at, error },
        frameworkOf(),
      ),
    );
  };

  const modelStopped = (from: Started, reason: unknown): void => {
    from.settled = true;
    const code = errorCode(stoppedBy(reason));
    within(from, () =>
      reporter.modelCall(
        { model: from.model, component: from.component, billedBy: providerOf(from.provider).billedBy, durationMs: performance.now() - from.at, finishReason: code, truncated: true },
        frameworkOf(),
      ),
    );
  };

  const embedFailed = (from: Started, error: unknown): void => {
    within(from, () =>
      reporter.modelCall(
        { model: from.model, component: from.component, billedBy: providerOf(from.provider).billedBy, durationMs: performance.now() - from.at, error },
        frameworkOf(),
      ),
    );
  };

  // Everything still in flight for an operation that has ended, however it ended, is forgotten.
  const forget = (callId: string): void => {
    models.delete(callId);
    for (const [id, from] of embeds) if (from.callId === callId) embeds.delete(id);
    for (const [id, from] of tools) if (from.callId === callId) tools.delete(id);
  };

  const unclaimed = (from: Started | undefined): from is Started => !!from && !from.settled && !from.claim?.claimed;

  return {
    onLanguageModelCallStart(event) {
      try {
        remember(models, text(event.callId), started(event));
      } catch {
        panicked();
      }
    },

    onLanguageModelCallEnd(event) {
      try {
        const callId = text(event.callId);
        const from = models.get(callId);
        models.delete(callId);
        if (from?.claim?.claimed) return;
        const { billedBy, format } = providerOf(text(event.provider) || from?.provider || "");
        const { reported, format: convention } = usageOf(restated(event.usage), format, event.providerMetadata);
        const responseTimeMs = event.performance?.responseTimeMs;
        within(from, () =>
          reporter.modelCall(
            {
              model: text(event.modelId) || from?.model || "",
              component: from?.component ?? componentOf(event.functionId),
              reported,
              format: convention,
              billedBy,
              durationMs: typeof responseTimeMs === "number" ? responseTimeMs : from ? performance.now() - from.at : 0,
              finishReason: finishOf(event.finishReason),
            },
            frameworkOf(),
          ),
        );
      } catch {
        panicked();
      }
    },

    // Each attempt runs here, so the middleware can claim it, and one that fails before the provider answers is recorded as it fails.
    executeLanguageModelCall(call) {
      let from: Started | undefined;
      let held: Claim | undefined;
      try {
        const callId = text(call.callId);
        from = models.get(callId);
        if (!from) {
          from = started(call);
          remember(models, callId, from);
        }
        held = { kind: "model", claimed: false };
        from.claim = held;
        from.settled = false;
        from.at = performance.now();
      } catch {
        panicked();
      }
      if (!from || !held) return call.execute();
      const attempt = from;
      const claim = held;
      return claims.run(claim, () =>
        Promise.resolve(call.execute()).then(undefined, (err: unknown) => {
          try {
            if (claim.claimed) attempt.settled = true;
            else modelFailed(attempt, err);
          } catch {
            panicked();
          }
          throw err;
        }),
      );
    },

    onToolExecutionStart(event) {
      try {
        const id = text(event.toolCall?.toolCallId);
        if (!id) return;
        const key = toolKey(event.toolCall, id);
        const existing = tools.get(key);
        const from = { ...started({ callId: event.callId, functionId: event.functionId }), tool: text(event.toolCall?.toolName), claim: existing?.claim };
        remember(tools, key, from);
      } catch {
        panicked();
      }
    },

    executeTool(call) {
      let held: Claim | undefined;
      try {
        const id = text(call.toolCallId) || text(call.toolCall?.toolCallId);
        if (id) {
          held = { kind: "tool", toolCallId: id, claimed: false };
          const key = toolKey(call.toolCall, id);
          const from = tools.get(key) ?? { ...started({ callId: call.callId }), tool: text(call.toolCall?.toolName) };
          from.claim = held;
          remember(tools, key, from);
        }
      } catch {
        panicked();
      }
      return held ? claims.run(held, call.execute) : call.execute();
    },

    onToolExecutionEnd(event) {
      try {
        const key = toolKey(event.toolCall, text(event.toolCall?.toolCallId));
        const from = tools.get(key);
        tools.delete(key);
        if (from?.claim?.claimed) return;
        const output = event.toolOutput;
        const failed = output?.type === "tool-error";
        // Only the size of the result is kept, measured as the JSON it reaches the model in.
        const size = failed ? undefined : resultSize(output?.output);
        within(from, () =>
          reporter.toolCall(
            {
              tool: from?.tool || text(event.toolCall?.toolName),
              durationMs: typeof event.toolExecutionMs === "number" ? event.toolExecutionMs : from ? performance.now() - from.at : 0,
              error: failed ? (output?.error ?? null) : undefined,
              resultBytes: size?.bytes,
              emptyResult: size?.empty,
            },
            frameworkOf(),
          ),
        );
      } catch {
        panicked();
      }
    },

    onEmbedStart(event) {
      try {
        const id = text(event.embedCallId);
        if (id) remember(embeds, id, { ...started(event), claim: embeddingClaim(text(event.callId)) });
      } catch {
        panicked();
      }
    },

    // The SDK hands an integration an embedding's usage as its own single count and not the provider's reply, so an embedding is recorded in the SDK's convention.
    onEmbedEnd(event) {
      try {
        const id = text(event.embedCallId);
        const from = embeds.get(id);
        embeds.delete(id);
        if ((from?.claim ?? embeddingClaim(text(event.callId)))?.claimed) return;
        const tokens = event.usage?.tokens;
        within(from, () =>
          reporter.modelCall(
            {
              model: text(event.modelId) || from?.model || "",
              component: from?.component ?? componentOf(event.functionId),
              reported: typeof tokens === "number" && Number.isInteger(tokens) && tokens > 0 ? { tokens } : undefined,
              format: FORMAT_AI_SDK,
              billedBy: providerOf(text(event.provider) || from?.provider || "").billedBy,
              durationMs: from ? performance.now() - from.at : 0,
            },
            frameworkOf(),
          ),
        );
      } catch {
        panicked();
      }
    },

    onEnd(event) {
      try {
        forget(text(event?.callId));
      } catch {
        panicked();
      }
    },

    // A stream the caller aborted: the model call it was in the middle of was cut short, as the middleware records it.
    onAbort(event) {
      try {
        const callId = text(event?.callId);
        const from = models.get(callId);
        if (unclaimed(from)) modelStopped(from, event.reason);
        forget(callId);
      } catch {
        panicked();
      }
    },

    // The operation failed. A model call still in flight failed with it, as a stream that broke part way does; an attempt that failed before the provider answered was recorded as it failed. Embedding calls are only seen failing here.
    onError(event) {
      try {
        const { callId: id, error } = (event ?? {}) as StoppedEvent;
        const callId = text(id);
        const from = models.get(callId);
        if (unclaimed(from)) modelFailed(from, error);
        const operation = embeddingClaim(callId);
        for (const embed of embeds.values()) {
          if (embed.callId === callId && !(embed.claim ?? operation)?.claimed) embedFailed(embed, error);
        }
        forget(callId);
      } catch {
        panicked();
      }
    },

    flush(timeoutMs) {
      return reporter.recorder.flush(timeoutMs);
    },

    shutdown(timeoutMs) {
      return reporter.recorder.close(timeoutMs);
    },
  };
}
