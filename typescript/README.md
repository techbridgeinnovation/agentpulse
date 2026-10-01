# recorder/typescript

## Service Overview

**Purpose.** The TypeScript recorder is a library that runs inside an agent's own Node process, the counterpart of `recorder/v1` and `recorder/python`. It observes what a service spent on models and tools and hands it to Agent Pulse through the gateway. It observes rather than carries: model traffic goes straight from the agent to its provider and never passes through here.

**The first rule.** It must never degrade its host. Recording never blocks, never throws and never waits on the network. A full queue drops records and counts them rather than waiting, and a destination that fails or hangs is isolated from the others and from the caller. That ranks above completeness of data. Dropped records are counted in `stats()`, and that counter cannot be turned off.

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

One middleware, wrapped around a model where it is created, and nothing at the call sites:

```ts
import { wrapLanguageModel } from "ai";

const model = wrapLanguageModel({ model: openai("gpt-5"), middleware: reporter.aiSdkMiddleware() });
```

Every `generateText`, `streamText`, `generateObject` and agent step made with that model is recorded, a streamed call once when its stream ends or is cancelled. A stream that ends with no finish part is recorded as truncated, one cancelled before its finish as truncated with the code `Canceled`, as the Go and Python recorders record it, and one that nothing reads or cancels the same way once it is garbage collected, which can be late and, at exit, never. The SDK hands a middleware the provider's own usage, so a call to OpenAI, Anthropic or Gemini is recorded in that provider's convention, and billed by Google where it is served through Vertex. Any other provider is recorded in the SDK's own convention, `AI_SDK`, which `metering/v1` reads with a reader of its own.

A governed reporter's middleware refuses a call a budget has run out on by throwing `SpendDenied` before the provider is called; `denied(err)` recognises it. A middleware cannot change which model its call uses, so a DOWNGRADE lets the call through as asked and is counted in `stats()` as not applied. `specificationVersion` is `v4` for AI SDK 7, and can be set to `v3` or `v2` for an earlier major.

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

## Delivery and exit

Records leave on a background flush. A batch the gateway refuses for a reason that passes, such as being unavailable or out of time, is sent again up to three times in all, with a growing wait, under the same request id. What is still queued or waiting when the recorder closes is counted in `stats().dropped`.

On SIGTERM, SIGINT and the event loop emptying, the recorder waits up to `exitTimeoutMs` for what is queued. After a signal it then ends the process as the signal would have, unless the host has a listener of its own, which then decides. `process.exit`, and a serverless platform freezing the process once it answers, give it no chance: await `reporter.recorder.close()`, or `flush()`, before returning.

## Layout

```
recorder/typescript/
├── src/
│   ├── index.ts      the public names
│   ├── report.ts     Attribution, Reporter, connect
│   ├── aisdk.ts      the middleware for the Vercel AI SDK
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

The tests against the real AI SDK run where `ai`, `@ai-sdk/openai`, `@ai-sdk/anthropic` and `@ai-sdk/google` are installed and are skipped where they are not:

```bash
npm install --no-save ai @ai-sdk/openai @ai-sdk/anthropic @ai-sdk/google && npm test
```

`npm run build` writes the published JavaScript and its type declarations to `dist/`.
