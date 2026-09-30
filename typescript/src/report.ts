// The way in: what does not change is given once, in an Attribution, what does is named at the call, and the mapping onto a record happens here.
//
// Neither a model call nor a tool call states a cost. A service says what happened and the server prices it, which is what stops an agent asserting what its own work was worth.

import { currentScope, workspaceName } from "./context.ts";
import { middleware, type Middleware, type MiddlewareOptions } from "./aisdk.ts";
import { blockedFinish, errorCode, truncatedFinish } from "./failure.ts";
import { ConfigError, Gateway } from "./gateway.ts";
import { ask, CachingDecider, type Decider, DEFAULT_DECIDE_TIMEOUT_MS, DEFAULT_FAILURE_BACKOFF_MS, GatewayDecider, type Verdict, verdict } from "./governance.ts";
import { type Config, Recorder } from "./recorder.ts";
import { MeteringSink } from "./sinks.ts";
import { PROVIDER_VERTEX_AI, reportedQuantities } from "./usage.ts";
import { type Activity, type Charge, KIND_AGENT, KIND_SERVICE, STATUS_DENIED, STATUS_FAILED, STATUS_OK, STATUS_TRUNCATED } from "./wire.ts";

/** What every record from one service has in common. */
export interface Attribution {
  /** The registered agent this service records as. Format: organisations/{organisation}/agents/{agent} */
  agent: string;
  /** The service itself, e.g. "documents-service". */
  service: string;
  /** Who bills for the calls, e.g. "VERTEX_AI" or "OPENAI". Any string is sent as written. */
  billedBy?: string;
  /** The area of the product the work belongs to, where the product wants cost sliced that way. */
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
  charges?: Charge[];
  framework?: string;
  frameworkVersion?: string;
}

/** One finished call to a tool, worth recording on its own because a search or a lookup is charged per use. */
export interface ToolCall {
  tool: string;
  durationMs?: number;
  error?: unknown;
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

export interface GovernOptions {
  /** How long a verdict is reused. Zero asks every time. */
  cacheTtlMs?: number;
  timeoutMs?: number;
  failureBackoffMs?: number;
}

/** Records on behalf of one service. Cheap to hold and safe to share. */
export class Reporter {
  readonly recorder: Recorder;
  readonly attribution: Attribution;
  private readonly gateway: Gateway | undefined;
  private decider: Decider | undefined;
  private decideTimeoutMs = DEFAULT_DECIDE_TIMEOUT_MS;

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
    return governed;
  }

  /**
   * Asks whether a call to `model` may proceed, before it is made, and never rejects. Skip the call when `proceed` is false: the refusal is recorded here, with nothing spent. Only a genuine DENY refuses; governance unreachable or slow lets the call go ahead, and is counted.
   */
  decide(model = "", options: { provider?: string; component?: string } = {}): Promise<Verdict> {
    return this.decideFor(model, options.provider ?? "", options.component ?? "");
  }

  /** @internal */
  async decideFor(model: string, provider: string, component: string, framework?: Framework): Promise<Verdict> {
    if (!this.decider) return verdict();
    const billedBy = provider || this.attribution.billedBy || PROVIDER_VERTEX_AI;
    let answer: Verdict;
    try {
      const at = this.attribution.agent.indexOf("/agents/");
      const organisation = at > 0 ? this.attribution.agent.slice(0, at) : "";
      if (!organisation) {
        this.recorder.note("decisionErrors");
        return verdict();
      }
      const s = currentScope();
      answer = await ask(
        this.decider,
        {
          parent: organisation,
          agent: this.attribution.agent,
          user: s.user?.id || framework?.user || "",
          workspace: workspaceName(organisation, s.workspace),
          project: s.project ?? "",
          requestedProvider: billedBy,
          requestedModel: model,
        },
        this.decideTimeoutMs,
      );
    } catch {
      this.recorder.note("decisionErrors");
      return verdict();
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
        if (truncatedFinish(call.finishReason)) {
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

  /** Records a finished tool call and returns immediately. */
  toolCall(call: ToolCall, framework?: Framework): void {
    try {
      const activity = this.activity(`tool:${call.tool}`, call.durationMs ?? 0, call.error, framework, call.charges);
      activity.tool = call.tool;
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
      skill: this.attribution.skill ?? "",
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
