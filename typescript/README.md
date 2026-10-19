# recorder/typescript

## Service Overview

**Purpose.** The TypeScript recorder is a library that runs inside an agent's own Node process, the counterpart of `recorder/v1` and `recorder/python`. It observes what a service spent on models and tools and hands it to Agent Pulse through the gateway. It observes rather than carries: model traffic goes straight from the agent to its provider and never passes through here.

**The first rule.** It must never degrade its host. Recording never blocks, never throws and never waits on the network. A full queue drops records and counts them rather than waiting, and a destination that fails or hangs is isolated from the others and from the caller. That ranks above completeness of data. Dropped records are counted in `stats()`, and that counter cannot be turned off. The metering sink also carries the losses on the next batch it delivers, as counts of records dropped, records not delivered after every retry and recorder faults, so a loss shows where spend is read and not only in this process. A count is taken as reported only once that batch is accepted, so a failed batch passes it to the next.

**It has no dependencies.** It speaks gRPC-web to the gateway over the platform's own `fetch`, with the few protobuf messages it sends encoded by hand, so it never decides which version of a package somebody else's agent loads. Node 18.18 or newer.

## Ownership & Contact

| | |
| --- | --- |
| Owner | Moses Otieno — moses@techbridgeinnovation.co |
| Product | Agent Pulse (`techbridge.ap`) |
| Package | `techbridge-agentpulse` |

## Dependencies

**Calls out to** `gateway/v1`, which forwards to `metering/v1` for records and names, one call per batch and never one per record, and to `governance/v1` for spend decisions. Every call carries the api key and its secret.

**Called by** each Node service's own process, in line.

**Publishes and listens to** nothing.

## Wiring it in

Four settings, read from the environment and refused at startup when missing: `AP_GATEWAY`, `AP_API_KEY` (the key's name, `organisations/<id>/apiKeys/<id>`), `AP_API_SECRET`, and `AP_AGENT` (`organisations/<id>/agents/<name>`, under the key's organisation).

```ts
import { connect, scope } from "techbridge-agentpulse";

const reporter = connect("research-agent").governed();
```

Once per request, where the sign-in has been checked, say who the work is for. Everything awaited inside is filed under it.

```ts
await scope({ request: requestId, user: { id: claims.sub, name: claims.name }, workspace: "acme" }, () => handle(req));
```

A `component` or `skill` on the scope wins over the middleware's `component` and the reporter's `skill`; a tool record keeps `tool:<name>` as its component. A user identifier shaped like an email address is recorded as no user, never sent to the directory, and counted in `stats().emailUsersRefused`.

## The Vercel AI SDK

Two ways in, used alone or together: a telemetry integration for AI SDK 7, which records every call the SDK reports, and a middleware for AI SDK 7 and 6, which records the calls of the model it wraps and can refuse one. AI SDK 5 and earlier are not supported.

### Telemetry

One integration, registered once where the process starts:

```ts
import { registerTelemetry } from "ai";

registerTelemetry(reporter.aiSdkTelemetry());
```

Every `generateText`, `streamText`, `generateObject`, `embed`, `embedMany` and agent step is recorded once the SDK reports it finished, with each tool the model calls. An integration passed on a call, `telemetry: { integrations: [...] }`, takes the place of the registered ones for that call, as the SDK has it, and a call with `telemetry: { isEnabled: false }` is not recorded. The component is the scope's, then the call's `functionId`, then the integration's own `component`, then `ai-sdk`.

The SDK hands an integration the prompt, the reply, the tools' arguments and their results; this reads none of them, only the model, the counts, the timings, the finish, a failure's code and the size of a tool's result. Usage arrives in the SDK's own flattened shape and is restated in the shape the middleware sends, so the provider's own counts are recorded for OpenAI, Anthropic and Gemini, and `AI_SDK` for any other provider, exactly as the middleware records them. An embedding's usage reaches an integration only as the SDK's single count, so an embedding is recorded as `AI_SDK`.

Telemetry hears of a call as it happens and never asks a budget. A model call that fails before the provider answers is recorded per attempt, by its code. One that breaks part way through a stream is recorded as failed when the SDK reports the error, and one whose stream is aborted as truncated with `Canceled`. A stream nobody reads to its end and nobody aborts is never reported finished, so telemetry does not record it.

`flush()` and `shutdown()` on the integration are `recorder.flush()` and `recorder.close()`.

### Telemetry and the middleware together

The middleware is what asks before a call. Where both are set up, the middleware records the calls of the models it wraps and the wrapped tools their own runs, and telemetry records everything else, so each call is recorded once. Which calls those are is said through claims, never through what a call returned or how it failed: two calls can share a result or an error, as two aborted by one signal share its reason.

- **A model attempt** runs inside the SDK's `executeLanguageModelCall`, where the integration opens an `AsyncLocalStorage` claim for that attempt alone, and the middleware marks it.
- **A tool call** runs inside `executeTool`, in a claim carrying the call's `toolCallId`. A wrapped tool marks it only when the id the SDK handed its `execute` is the same, so a wrapped tool run inside another tool, by a generation without this integration, never claims the outer one. The integration knows each tool call by the SDK's own object for it, the same at its start, its run and its end, so calls sharing an id, which the SDK allows, are never mixed up.
- **An embedding** has no hook of its own, but the SDK runs every `embed` and `embedMany` inside its tracing channel, `ai:telemetry`. The integration binds a claim to that channel for each such operation, and none to the steps and tools it runs, and the embedding middleware marks it. Every call to the provider inside one operation is made with the same model, so the middleware wraps all of them or none. Binding the store makes the SDK run every operation in the process inside the channel; an operation with telemetry turned off is not run inside it.

A claim lives in the async context of the one call it was opened for, so no concurrent call sees it. A model or embedding model whose own call is made through a wrapped one, with no operation of its own in between, is claimed by it and recorded once, by the middleware. A refusal the middleware raises is recorded once, as denied.

### The middleware

One middleware, wrapped around a model where it is created, and nothing at the call sites:

```ts
import { wrapLanguageModel } from "ai";

const model = wrapLanguageModel({ model: openai("gpt-5"), middleware: reporter.aiSdkMiddleware() });
```

Every `generateText`, `streamText`, `generateObject` and agent step made with that model is recorded, a streamed call once when its stream ends or is cancelled. A stream that ends with no finish part is recorded as truncated, one cancelled before its finish as truncated with the code `Canceled`, as the Go and Python recorders record it, and one that nothing reads or cancels the same way once it is garbage collected, which can be late and, at exit, never. The SDK hands a middleware the provider's own usage, so a call to OpenAI, Anthropic or Gemini is recorded in that provider's convention, and billed by Google where it is served through Vertex. Any other provider is recorded in the SDK's own convention, `AI_SDK`, which `metering/v1` reads with a reader of its own.

A governed reporter's middleware refuses a call a budget has run out on by throwing `SpendDenied` before the provider is called; `denied(err)` recognises it. A middleware cannot change which model its call uses, so a DOWNGRADE lets the call through as asked and is counted in `stats()` as not applied. `specificationVersion` is `v4` for AI SDK 7, and `v3` for AI SDK 6. It is typed as the literal given, and each method as returning the SDK's own result type, so the middleware fits the SDK's middleware type without a cast.

Embedding models are wrapped the same way, from AI SDK 6, and tools are wrapped where they are declared:

```ts
const embedder = wrapEmbeddingModel({ model: openai.embedding("text-embedding-3-small"), middleware: reporter.aiSdkEmbeddingMiddleware() });

const tools = reporter.aiSdkTools({ search, lookup });
```

Each tool call is recorded with its duration, its failure code and the size of its result as JSON, never its arguments or its result. A result that cannot be serialised is recorded with no size and is not called empty.

## Recording by hand

Code with no framework to observe reports for itself, naming the provider's counts under the provider's own names:

```ts
reporter.modelCall({ model: "gpt-5", reported: reportedFrom(response.usage), format: FORMAT_OPENAI_RESPONSES, durationMs });

const verdict = await reporter.decide("gpt-5", { provider: "OPENAI" });
if (!verdict.proceed) return refuse();
```

A tool call by hand states the size of its result, never the result:

```ts
const { bytes, empty } = resultSize(result);
reporter.toolCall({ tool: "search", durationMs, resultBytes: bytes, emptyResult: empty });
```

Only a genuine DENY refuses. Governance unreachable or slower than a second and a half lets the call go ahead and is counted, unless it refused the same question within the hour, which it then does again.

## Budget statuses

A verdict from governance carries how every budget that applies to the call stood, most pressing first and at every level including `WITHIN`, in `verdict.budgetStatuses`, with `verdict.evaluateTime` saying when governance worked them out. Each `BudgetStatus` names its `budget`, `displayName`, `scope`, `workspace`, `spentMicros`, `limitMicros`, `periodEndTime`, `warnOnly` and `level`. A scope or level this version does not know reads as `SCOPE_UNSPECIFIED` or `LEVEL_UNSPECIFIED`. A reserve can have a `limitMicros` of zero, so a share is never worked out by dividing without checking it. A governance that predates statuses sends none, and a call with no answer has none.

A call made through the AI SDK middleware hands its statuses to a callback when one is set:

```ts
const reporter = connect("research-agent").governed(undefined, {
  onBudgetStatus: (event) => {
    for (const status of event.budgetStatuses) {
      if (status.level === "PASSED_95_PERCENT" || status.level === "EXHAUSTED") notices.add(event.user, status.displayName, status.level);
    }
  },
});
```

It is called for every instrumented call whose decision came with at least one status, with the statuses, `evaluateTime`, the decision, the model, provider and component, and the user, workspace, project and request the question named. It is not called for `decide`, whose verdict already holds the statuses, nor when no decision was reached, nor when a decision has no statuses.

Delivery is asynchronous and the model call never waits for it. The event goes on a queue of 64 and is delivered from a later turn of the event loop, one event at a time, in the order the decisions were obtained, at most once: what is still queued when the process exits is lost, and a pending delivery never keeps the process alive. A full queue drops the event and counts it in `stats().budgetStatusDropped`. A throw, and a returned promise that rejects, is counted in `stats().budgetStatusFailed`; a returned promise is not awaited, so a slow one delays nothing behind it. A callback that runs long synchronously blocks the whole event loop, model calls included, and cannot be interrupted, so it must be quick or hand its work elsewhere. Unset, nothing is called.

Statuses are a delayed snapshot, not a live reading. Spend reaches governance only after a call is recorded, and a verdict is reused for up to `cacheTtlMs`, so a status can trail the calls made since. They are not a hard cap and not an exact percentage. Agent Pulse shows them to no one: a product's users see a warning only where the product's own code reads the statuses and shows one, so wiring this in changes no product's interface by itself.

## Delivery and exit

Records leave on a background flush. A batch the gateway refuses for a reason that passes, such as being unavailable or out of time, is sent again up to three times in all, with a growing wait, under the same request id. What is still queued or waiting when the recorder closes is counted in `stats().dropped`.

On SIGTERM, SIGINT and the event loop emptying, the recorder waits up to `exitTimeoutMs` for what is queued. After a signal it then ends the process as the signal would have, unless the host has a listener of its own, which then decides. `process.exit`, and a serverless platform freezing the process once it answers, give it no chance: await `reporter.recorder.close()`, or `flush()`, before returning.

## Layout

```
recorder/typescript/
├── src/
│   ├── index.ts      the public names
│   ├── report.ts     Attribution, Reporter, connect
│   ├── aisdk.ts      the middleware and tool wrapper for the Vercel AI SDK
│   ├── aisdk-telemetry.ts the telemetry integration for AI SDK 7
│   ├── recorder.ts   the queue, the flush and the counters
│   ├── context.ts    scope, and who a piece of work is for
│   ├── governance.ts asking whether a call may proceed, verdicts reused from memory, refusals repeated
│   ├── sinks.ts      Sink, Discard, MeteringSink
│   ├── gateway.ts    one gRPC-web call over fetch, with the key and the secret
│   ├── usage.ts      a provider's counts under its own names
│   ├── failure.ts    an error reduced to its code, and SpendDenied
│   ├── wire.ts       the protobuf encoding of what is sent and read
│   └── version.ts
└── test/
```

## Running it

The tests run on the TypeScript sources directly, with Node 24's own type stripping, and read the wire fixtures `recorder/python` writes from the contract, so both recorders are checked against the same bytes.

```bash
cd recorder/typescript
npm install
npm test
npm run typecheck
```

AI SDK 7 and its OpenAI, Anthropic and Google providers are development dependencies, so `npm install` brings them and the tests against the real SDK run. Where they are missing those tests are skipped, unless `AP_AI_SDK_REQUIRED` names the major that must be installed, which makes a run without it fail. CI sets it:

```bash
AP_AI_SDK_REQUIRED=7 npm test
```

AI SDK 6 is checked the same way, with its own packages in place of 7's. The telemetry tests need AI SDK 7 and are skipped there:

```bash
npm install --no-save ai@ai-v6 @ai-sdk/openai@ai-v6 @ai-sdk/anthropic@ai-v6 @ai-sdk/google@ai-v6
AP_AI_SDK_REQUIRED=6 node --test test/aisdk.test.ts test/aisdk-telemetry.test.ts
```

`npm run build` writes the published JavaScript and its type declarations to `dist/`.
