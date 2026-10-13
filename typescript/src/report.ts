// The way in: what does not change is given once, in an Attribution, what does is named at the call, and the mapping onto a record happens here.
//
// Neither a model call nor a tool call states a cost. A service says what happened and the server prices it, which is what stops an agent asserting what its own work was worth.

import { currentScope, looksLikeEmail, workspaceName } from "./context.ts";
import { embeddingMiddleware, type EmbeddingMiddleware, middleware, type Middleware, type MiddlewareOptions, type Tools, tools } from "./aisdk.ts";
import { blockedFinish, errorCode, truncatedFinish } from "./failure.ts";
import { ConfigError, Gateway } from "./gateway.ts";
import { ask, type BudgetStatus, CachingDecider, type Decider, type Decision, DEFAULT_DECIDE_TIMEOUT_MS, DEFAULT_FAILURE_BACKOFF_MS, GatewayDecider, type Verdict, verdict } from "./governance.ts";
import { type Config, Recorder } from "./recorder.ts";
import { MeteringSink } from "./sinks.ts";
import { PROVIDER_VERTEX_AI, reportedQuantities } from "./usage.ts";
import { type Activity, type Charge, type DecideRequest, KIND_AGENT, KIND_SERVICE, STATUS_DENIED, STATUS_FAILED, STATUS_OK, STATUS_TRUNCATED } from "./wire.ts";

// How many events wait for the callback. A callback slower than decisions arrive loses the newest events rather than growing memory.
const BUDGET_STATUS_QUEUE = 64;

/** What every record from one service has in common. */
export interface Attribution {
  /** The registered agent this service records as. Format: organisations/{organisation}/agents/{agent} */
  agent: string;
  /** The service itself, e.g. "documents-service". */
  service: string;
  /** Who bills for the calls, e.g. "VERTEX_AI" or "OPENAI". Any string is sent as written. */
  billedBy?: string;
  /** The area of the product the work belongs to, where the product wants cost sliced that way. A skill set with `scope` wins over it. */
  skill?: string;
}

/** One finished call to a model. */
export interface ModelCall {
  /** As the provider names it, e.g. "gpt-5". */
  model: string;
  /** The part of the service that spent this. Defaults to the component on the current scope. */
  component?: string;
  /** What the provider said, under its own name for each count. Name the convention in `format`. */
  reported?: Record<string, number>;
  format?: string;
  /** Who served the call, where it is not the reporter's own `billedBy`. */
  billedBy?: string;
  durationMs?: number;
  region?: string;
  /** Read for its code only. The message is never recorded. */
  error?: unknown;
  /** What the provider said ended the call, e.g. "length" or "content-filter". */
  finishReason?: string;
  /** The call was cut short and did partial work, where no finish reason says so, such as a stream that ended without one. */
  truncated?: boolean;
  charges?: Charge[];
  framework?: string;
  frameworkVersion?: string;
}

/** One finished call to a tool, worth recording on its own because a search or a lookup is charged per use. */
export interface ToolCall {
  tool: string;
  durationMs?: number;
  error?: unknown;
  /** How large the result was, in bytes, never the result itself. */
  resultBytes?: number;
  /** The tool succeeded and returned nothing the agent could use. */
  emptyResult?: boolean;
  charges?: Charge[];
  framework?: string;
  frameworkVersion?: string;
}

/** What an agent framework knows about a call, used where the product set nothing on the scope. */
export interface Framework {
  request?: string;
  session?: string;
  user?: string;
  /** The agent inside the run that made the call. */
  agent?: string;
  name?: string;
  version?: string;
}

/**
 * What an `onBudgetStatus` callback is given: the budget statuses governance sent with its answer for one instrumented call, and what that call was.
 *
 * The statuses are a snapshot as of `evaluateTime`, from a verdict that may have been reused from the cache: they can trail the calls made since by the cache's lifetime plus the time spend takes to be recorded. They are for warning a person, never an exact live percentage or a hard cap.
 */
export interface BudgetStatusEvent {
  readonly budgetStatuses: readonly BudgetStatus[];
  readonly evaluateTime: Date | null;
  /** The decision the statuses came with, such as ALLOW or NOTIFY. */
  readonly decision: Decision;
  /** The model and provider the call asked for, and the part of the service making it. */
  readonly model: string;
  readonly provider: string;
  readonly component: string;
  /** Who and what the call was for, as the question to governance named them. The user is empty where the identifier looked like an email address, which is never sent. */
  readonly user: string;
  readonly workspace: string;
  readonly project: string;
  readonly request: string;
}

export interface GovernOptions {
  /** How long a verdict is reused. Zero asks every time. */
  cacheTtlMs?: number;
  timeoutMs?: number;
  failureBackoffMs?: number;
  /**
   * Called for every instrumented call (the AI SDK middleware) whose decision came with at least one budget status, at any level including WITHIN, so the product can warn its own users; a call to `decide` returns the statuses on its verdict instead.
   *
   * Delivery is asynchronous and the model call never waits for it: the event goes on a queue of 64 and is delivered from a later turn of the event loop, one event at a time, in the order the decisions were obtained, at most once, so what is still queued at exit is lost. A full queue drops the event and counts it in `budgetStatusDropped`. A throw, and a returned promise that rejects, is counted in `budgetStatusFailed`; a returned promise is not awaited, so a slow one delays nothing behind it. A callback that runs long synchronously blocks the whole event loop, model calls included, and cannot be interrupted, so it must be quick or hand its work elsewhere. Not called when no decision was reached.
   */
  onBudgetStatus?: (event: BudgetStatusEvent) => unknown;
}

/** Records on behalf of one service. Cheap to hold and safe to share. */
export class Reporter {
  readonly recorder: Recorder;
  readonly attribution: Attribution;
  private readonly gateway: Gateway | undefined;
  private decider: Decider | undefined;
  private decideTimeoutMs = DEFAULT_DECIDE_TIMEOUT_MS;
  private onBudgetStatus: GovernOptions["onBudgetStatus"];
  private readonly budgetStatusQueue: BudgetStatusEvent[] = [];
  private budgetStatusScheduled = false;

  constructor(recorder: Recorder, attribution: Attribution, gateway?: Gateway) {
    this.recorder = recorder;
    this.attribution = attribution;
    this.gateway = gateway;
  }

  /** A reporter whose `decide` asks governance, so a budget that stops an agent stops this service spending against the same budget too. */
  governed(decider?: Decider, options: GovernOptions = {}): Reporter {
    if (!decider) {
      if (!this.gateway) throw new ConfigError("agentpulse: a reporter built without a gateway needs a decider to be governed");
      decider = new GatewayDecider(this.gateway);
    }
    const ttl = options.cacheTtlMs ?? 30_000;
    const backoff = options.failureBackoffMs ?? DEFAULT_FAILURE_BACKOFF_MS;
    const governed = new Reporter(this.recorder, this.attribution, this.gateway);
    governed.decider = ttl > 0 || backoff > 0 ? new CachingDecider(decider, ttl, { failureBackoffMs: backoff }) : decider;
    governed.decideTimeoutMs = options.timeoutMs && options.timeoutMs > 0 ? options.timeoutMs : DEFAULT_DECIDE_TIMEOUT_MS;
    governed.onBudgetStatus = options.onBudgetStatus;
    return governed;
  }

  /**
   * Asks whether a call to `model` may proceed, before it is made, and never rejects. Skip the call when `proceed` is false: the refusal is recorded here, with nothing spent. Only a genuine DENY refuses; governance unreachable or slow lets the call go ahead, and is counted.
   *
   * The verdict carries how each budget that applies stood, in `budgetStatuses`, as of `evaluateTime`. The reporter's `onBudgetStatus` is not called for it: the caller already holds the statuses.
   */
  decide(model = "", options: { provider?: string; component?: string } = {}): Promise<Verdict> {
    return this.decideFor(model, options.provider ?? "", options.component ?? "", undefined, false);
  }

  /** @internal */
  async decideFor(model: string, provider: string, component: string, framework?: Framework, instrumented = true): Promise<Verdict> {
    if (!this.decider) return verdict();
    const billedBy = provider || this.attribution.billedBy || PROVIDER_VERTEX_AI;
    let answer: Verdict;
    let asked: DecideRequest;
    try {
      const at = this.attribution.agent.indexOf("/agents/");
      const organisation = at > 0 ? this.attribution.agent.slice(0, at) : "";
      if (!organisation) {
        this.recorder.note("decisionErrors");
        return verdict();
      }
      const s = currentScope();
      // A record has an address removed where it is queued; a spend decision removes its own.
      const user = s.user?.id || framework?.user || "";
      asked = {
        parent: organisation,
        agent: this.attribution.agent,
        user: looksLikeEmail(user) ? "" : user,
        workspace: workspaceName(organisation, s.workspace),
        project: s.project ?? "",
        requestedProvider: billedBy,
        requestedModel: model,
      };
      answer = await ask(this.decider, asked, this.decideTimeoutMs);
    } catch {
      this.recorder.note("decisionErrors");
      return verdict();
    }
    if (instrumented && answer.budgetStatuses.length && this.onBudgetStatus) {
      this.tellBudgetStatus(answer, asked, component || currentScope().component || "", framework);
    }
    if (answer.decision === "NOTIFY") this.recorder.note("notified");
    else if (answer.decision === "DOWNGRADE") this.recorder.note("downgraded");
    else if (answer.decision === "DENY") {
      this.recorder.note("denied");
      try {
        const activity = this.activity(component || currentScope().component || "", 0, undefined, framework);
        activity.model = model;
        activity.billedBy = billedBy;
        activity.status = STATUS_DENIED;
        this.recorder.record(activity);
      } catch {
        this.recorder.note("panicked");
      }
    }
    return answer;
  }

  /** Queues the statuses for the product's callback and returns at once. The event is built here because the scope is only this call's here. */
  private tellBudgetStatus(answer: Verdict, asked: DecideRequest, component: string, framework?: Framework): void {
    try {
      if (this.budgetStatusQueue.length >= BUDGET_STATUS_QUEUE) {
        this.recorder.note("budgetStatusDropped");
        return;
      }
      this.budgetStatusQueue.push({
        budgetStatuses: answer.budgetStatuses,
        evaluateTime: answer.evaluateTime,
        decision: answer.decision,
        model: asked.requestedModel ?? "",
        provider: asked.requestedProvider ?? "",
        component,
        user: asked.user ?? "",
        workspace: asked.workspace ?? "",
        project: asked.project ?? "",
        request: currentScope().request || framework?.request || "",
      });
      this.scheduleBudgetStatus();
    } catch {
      this.recorder.note("panicked");
    }
  }

  // One event a turn of the event loop, so a long queue never holds the loop for longer than one callback, and unreferenced so a pending delivery never keeps the process alive.
  private scheduleBudgetStatus(): void {
    if (this.budgetStatusScheduled) return;
    this.budgetStatusScheduled = true;
    setImmediate(() => {
      this.budgetStatusScheduled = false;
      const event = this.budgetStatusQueue.shift();
      if (event) this.deliverBudgetStatus(event);
      if (this.budgetStatusQueue.length) this.scheduleBudgetStatus();
    }).unref();
  }

  private deliverBudgetStatus(event: BudgetStatusEvent): void {
    const failed = () => this.recorder.note("budgetStatusFailed");
    try {
      const result = this.onBudgetStatus?.(event);
      if (result !== null && typeof result === "object" && typeof (result as PromiseLike<unknown>).then === "function") {
        (result as PromiseLike<unknown>).then(undefined, failed);
      }
    } catch {
      failed();
    }
  }

  /** Records a finished model call and returns immediately. Never throws: recording is not allowed to be the reason a request failed. */
  modelCall(call: ModelCall, framework?: Framework): void {
    try {
      const activity = this.activity(call.component || currentScope().component || "", call.durationMs ?? 0, call.error, framework, call.charges);
      activity.model = call.model;
      if (call.billedBy) activity.billedBy = call.billedBy;
      activity.usageFormat = call.reported && Object.keys(call.reported).length ? call.format : undefined;
      activity.reportedUsage = reportedQuantities(call.reported);
      activity.region = call.region;
      if (call.framework) activity.framework = call.framework;
      if (call.frameworkVersion) activity.frameworkVersion = call.frameworkVersion;
      if (call.error === undefined) {
        if (call.truncated || truncatedFinish(call.finishReason)) {
          activity.status = STATUS_TRUNCATED;
          activity.errorCode = call.finishReason;
        } else if (blockedFinish(call.finishReason)) {
          activity.status = STATUS_FAILED;
          activity.errorCode = call.finishReason;
        }
      }
      this.recorder.record(activity);
    } catch {
      this.recorder.note("panicked");
    }
  }

  /** A middleware for the Vercel AI SDK's `wrapLanguageModel`, that records every call made with the wrapped model, and refuses one a budget has run out on by throwing SpendDenied when the reporter is governed. */
  aiSdkMiddleware(options?: MiddlewareOptions): Middleware {
    return middleware(this, options);
  }

  /** A middleware for the Vercel AI SDK's `wrapEmbeddingModel`, from AI SDK 6, that records every embedding call made with the wrapped model. */
  aiSdkEmbeddingMiddleware(options?: MiddlewareOptions): EmbeddingMiddleware {
    return embeddingMiddleware(this, options);
  }

  /** The same AI SDK tools, each recorded when it runs: how long it took, how it failed and how large its result was, never what it was given or returned. */
  aiSdkTools<T extends Tools>(set: T): T {
    return tools(this, set);
  }

  /** Records a finished tool call and returns immediately. */
  toolCall(call: ToolCall, framework?: Framework): void {
    try {
      const activity = this.activity(`tool:${call.tool}`, call.durationMs ?? 0, call.error, framework, call.charges);
      activity.tool = call.tool;
      if (call.error === undefined) {
        activity.resultBytes = call.resultBytes && call.resultBytes > 0 ? Math.round(call.resultBytes) : undefined;
        activity.emptyResult = call.emptyResult || undefined;
      }
      if (call.framework) activity.framework = call.framework;
      if (call.frameworkVersion) activity.frameworkVersion = call.frameworkVersion;
      this.recorder.record(activity);
    } catch {
      this.recorder.note("panicked");
    }
  }

  private activity(component: string, durationMs: number, error: unknown, framework?: Framework, charges?: Charge[]): Activity {
    const s = currentScope();
    this.recorder.noteUser(s.user);
    return {
      agent: this.attribution.agent,
      request: s.request || framework?.request || "",
      session: s.session || framework?.session || "",
      user: s.user?.id || framework?.user || "",
      project: s.project ?? "",
      callerService: this.attribution.service,
      callerComponent: component,
      subAgent: framework?.agent ?? "",
      // Seen through an agent framework that names the agent it is running, or made directly from the service's own code.
      observedAs: framework?.agent ? KIND_AGENT : KIND_SERVICE,
      framework: framework?.name ?? "",
      frameworkVersion: framework?.version ?? "",
      skill: s.skill || this.attribution.skill || "",
      billedBy: this.attribution.billedBy || PROVIDER_VERTEX_AI,
      durationMs: Math.max(0, Math.round(durationMs)),
      status: error === undefined ? STATUS_OK : STATUS_FAILED,
      errorCode: error === undefined ? "" : errorCode(error),
      occurredAtNs: BigInt(Date.now()) * 1_000_000n,
      charges: charges ?? [],
    };
  }
}

/**
 * A reporter that records to Agent Pulse through its gateway, set up from the environment.
 *
 * Four settings are read, and each is refused here if missing, so a wrong setting stops the process where a person is watching it start: `AP_GATEWAY`; `AP_API_KEY`, the key's name, `organisations/<id>/apiKeys/<id>`; `AP_API_SECRET`; and `AP_AGENT`, `organisations/<id>/agents/<name>`.
 */
export function connect(service: string, options: { billedBy?: string; skill?: string; config?: Config; env?: Record<string, string | undefined> } = {}): Reporter {
  const env = options.env ?? process.env;
  const gateway = Gateway.fromEnv(env);
  if (!gateway.organisation) throw new ConfigError("agentpulse: AP_API_KEY must be a key's name, organisations/<id>/apiKeys/<id>");
  const agent = (env.AP_AGENT ?? "").trim();
  if (!agent.startsWith(gateway.organisation + "/agents/") || agent.split("/").length !== 4) {
    throw new ConfigError(`agentpulse: AP_AGENT must be ${gateway.organisation}/agents/<name>, under the key's organisation`);
  }
  if (!service.trim()) throw new ConfigError("agentpulse: the service name is required");
  const config = { ...options.config, sinks: [new MeteringSink(gateway), ...(options.config?.sinks ?? [])] };
  return new Reporter(new Recorder(config), { agent, service: service.trim(), billedBy: options.billedBy, skill: options.skill }, gateway);
}
