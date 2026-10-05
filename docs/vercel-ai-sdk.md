---
title: Vercel AI SDK
summary: One middleware on the model records every generate, stream and agent step.
section: frameworks
order: 4
---

# Vercel AI SDK

Wrap the model once where you create it. Every `generateText`, `streamText`, `generateObject` and agent step made with it is recorded. TypeScript only.

## Wrap the model

```ts
import { connect, scope } from "techbridge-agentpulse";
import { wrapLanguageModel } from "ai";
import { openai } from "@ai-sdk/openai";

const reporter = connect("research-agent").governed();
const model = wrapLanguageModel({ model: openai("gpt-5"), middleware: reporter.aiSdkMiddleware() });

await scope({ request: requestId, user: { id: userId }, workspace: customerId }, () => handle(req));
```

Embedding models and tools are wrapped the same way:

```ts
import { wrapEmbeddingModel } from "ai";

const embedder = wrapEmbeddingModel({ model: openai.embedding("text-embedding-3-small"), middleware: reporter.aiSdkEmbeddingMiddleware() });
const tools = reporter.aiSdkTools({ search, lookup });
```

The middleware is for AI SDK 7. For an earlier major, set `specificationVersion` to `v3` or `v2`.

## What is recorded

- A streamed call is recorded once, when its stream ends or is cancelled.
- OpenAI, Anthropic and Gemini calls are recorded in the provider's own terms; other providers in the SDK's.
- A tool call's duration, failure code and result size are recorded, never its arguments or result.

## Budgets

A governed reporter's middleware checks your budgets before each call, and a stopped call throws `SpendDenied` before it is sent; `denied(err)` recognises it. A middleware cannot change which model a call uses, so a downgrade lets the call through as asked.

## Recording by hand

For code that does not use the AI SDK, check and record each call yourself:

```ts
import { denied, FORMAT_OPENAI_RESPONSES, reportedFrom } from "techbridge-agentpulse";

const verdict = await reporter.decide("gpt-5", { provider: "OPENAI" });
if (!verdict.proceed) return refuse();

reporter.modelCall({ model: "gpt-5", reported: reportedFrom(response.usage), format: FORMAT_OPENAI_RESPONSES, durationMs });
```
