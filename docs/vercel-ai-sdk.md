---
title: Vercel AI SDK
summary: A telemetry integration records every call; the middleware lets a budget stop one.
section: frameworks
order: 4
---

# Vercel AI SDK

Two ways in, used alone or together. TypeScript only.

- **Telemetry**, registered once with `registerTelemetry`, records every model, embedding and tool call the AI SDK reports. It hears of a call once it has happened, so it cannot stop one.
- **Middleware**, wrapped around a model where you create it, records that model's calls and checks your budgets before each one, so a budget can stop a call before it is sent.

## Supported versions

| AI SDK | Telemetry | Middleware and wrapped tools |
| --- | --- | --- |
| 7 | Yes | Yes |
| 6 | No, it needs AI SDK 7 | Yes, with `specificationVersion: "v3"` |
| 5 and earlier | No | No |

Checked against `ai` 7.0.143 and 6.0.309, with the OpenAI, Anthropic and Google providers. Node 18.18 or newer.

## Install

```bash
npm install techbridge-agentpulse
```

The recorder has no dependencies of its own. It uses the `ai` you already have.

## Set four settings

The recorder reads them from the environment and stops at startup if one is missing. See the [quickstart](quickstart.md) for what each one is.

```env
AP_GATEWAY=gateway.agentspulse.ai
AP_API_KEY=organisations/<organisation>/apiKeys/<key>
AP_API_SECRET=<shown once when the key is created>
AP_AGENT=organisations/<organisation>/agents/<agent>
```

## Record with telemetry

Once, where your process starts:

```ts
import { registerTelemetry } from "ai";
import { connect } from "techbridge-agentpulse";

const reporter = connect("research-agent");
registerTelemetry(reporter.aiSdkTelemetry());
```

Every `generateText`, `streamText`, `generateObject`, `embed`, `embedMany` and agent step is then recorded, with the tools the model calls. Nothing changes at your call sites.

To record only some calls, pass the integration on the call instead. A call's `integrations` take the place of the registered ones, so list every integration you want for that call:

```ts
const telemetry = reporter.aiSdkTelemetry();

await generateText({ model, prompt, telemetry: { integrations: [telemetry], functionId: "summarise" } });
```

A call with `telemetry: { isEnabled: false }` is not recorded.

## Add budget enforcement

To have a budget stop a call, make the reporter governed and wrap each model with the middleware. Keep telemetry registered for everything you do not wrap:

```ts
import { registerTelemetry, wrapEmbeddingModel, wrapLanguageModel } from "ai";
import { openai } from "@ai-sdk/openai";
import { connect, denied } from "techbridge-agentpulse";

const reporter = connect("research-agent").governed();
registerTelemetry(reporter.aiSdkTelemetry());

const model = wrapLanguageModel({ model: openai("gpt-5"), middleware: reporter.aiSdkMiddleware() });
const embedder = wrapEmbeddingModel({ model: openai.embedding("text-embedding-3-small"), middleware: reporter.aiSdkEmbeddingMiddleware() });
const tools = reporter.aiSdkTools({ search, lookup });
```

Each call is recorded once. The middleware records the calls of the models it wraps, and the wrapped tools record their own runs. Telemetry records everything else.

Before each call made with a wrapped model, the middleware checks your budgets. A stopped call throws `SpendDenied` before anything is sent to the provider; `denied(err)` recognises it. A warn-only budget past its limit lets the call through and alerts you. The middleware cannot change which model a call uses, so a downgrade lets the call go ahead as asked and is counted in `stats().downgradeNotApplied`.

On AI SDK 6, wrap the models and leave telemetry out:

```ts
const model = wrapLanguageModel({ model: openai("gpt-5"), middleware: reporter.aiSdkMiddleware({ specificationVersion: "v3" }) });
```

## Say who the work is for

Once per request, where you have checked the sign-in:

```ts
import { scope } from "techbridge-agentpulse";

await scope({ request: requestId, user: { id: userId }, workspace: customerId }, () => handle(req));
```

## What is recorded

- **Model calls:** the model that answered, the provider, the token counts, the duration, how the call finished, and a failure's code. A streamed call is recorded once, when it finishes.
- **Token counts:** OpenAI, Anthropic and Gemini calls are recorded in the provider's own terms, cached and reasoning tokens included. Other providers are recorded in the AI SDK's terms.
- **Embedding calls:** recorded with the AI SDK's token count. A wrapped embedding model records the provider's own count where its reply has one.
- **Tool calls:** the duration, a failure's code, and the size of the result as JSON.
- **Who it was for:** the request, user and workspace from `scope`.
- **Component:** the `component` on the scope wins. Otherwise telemetry uses the call's `functionId`, then the `component` given to `aiSdkTelemetry`. The middleware uses the `component` given to `aiSdkMiddleware`. With none of these, it is `ai-sdk`.

## What is never recorded

- Prompts, system instructions and messages.
- Model replies.
- Tool arguments and tool results. A result is measured for its size and the content is discarded.
- Embedded values and their vectors.
- Error messages, which often quote the prompt. Only a failure's code is kept.

The AI SDK hands all of this to every telemetry integration. Agent Pulse's does not read it. The SDK's `recordInputs` and `recordOutputs` settings change nothing here, since none of it is recorded either way.

## Where records go

To Agent Pulse, through the gateway in `AP_GATEWAY`, filed under the agent in `AP_AGENT`. Records are sent in batches from the background, every two seconds or every 100 records. This is not OpenTelemetry and needs no `@ai-sdk/otel`. It can be registered beside other telemetry integrations, and each one hears every call.

## Flush and shutdown

At exit the recorder waits up to two seconds to send what is queued, on `SIGTERM`, `SIGINT` and the event loop emptying. Where a process can be frozen as soon as it answers, as on a serverless platform, or ends with `process.exit`, flush before returning:

```ts
const telemetry = reporter.aiSdkTelemetry();
registerTelemetry(telemetry);

await telemetry.flush();    // send what is queued; true when it finished within five seconds
await telemetry.shutdown(); // flush, then stop recording
```

These are the same as `reporter.recorder.flush()` and `reporter.recorder.close()`. After a shutdown, anything recorded is dropped and counted in `stats().dropped`.

## When something goes wrong

- **Never in your way:** recording never blocks a call and never throws into your code. A fault inside the integration or the middleware is counted in `stats().panicked` and the call carries on.
- **Agent Pulse unreachable:** records are retried, then dropped and counted. Your model calls go straight to the provider and are not affected. See [When Agent Pulse is unavailable](when-agent-pulse-is-unavailable.md).
- **No budget answer:** if a budget check gets no answer within 1.5 seconds, the call goes ahead.
- **Missing settings:** a missing or malformed setting stops `connect` at startup.

## What telemetry cannot see

- **Retries:** a call the AI SDK retries is recorded per attempt. An attempt that fails before the provider answers is recorded as failed, by its code.
- **Unfinished streams:** a stream that nobody reads to its end and nobody aborts is never reported finished, so telemetry does not record it. The middleware records one as cancelled.
- **Aborted streams:** an aborted stream is recorded as cut short, with the code `Canceled`.
- **Finish reasons:** telemetry is given the AI SDK's own word for how a call finished, such as `stop` or `length`. The middleware is given the provider's.

## Recording by hand

For code that does not use the AI SDK, check and record each call yourself:

```ts
import { denied, FORMAT_OPENAI_RESPONSES, reportedFrom } from "techbridge-agentpulse";

const verdict = await reporter.decide("gpt-5", { provider: "OPENAI" });
if (!verdict.proceed) return refuse();

reporter.modelCall({ model: "gpt-5", reported: reportedFrom(response.usage), format: FORMAT_OPENAI_RESPONSES, durationMs });
```
