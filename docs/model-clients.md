---
title: OpenAI, Anthropic and Gemini clients
summary: Record every call a model client makes by wrapping it once.
section: frameworks
order: 0
---

# OpenAI, Anthropic and Gemini clients

If your code calls a provider's SDK directly, wrap the client once where you create it. Every call through it is recorded from then on, including calls written later.

## Wrap the client

```python [Python]
from openai import OpenAI, AsyncOpenAI
from anthropic import Anthropic
from google import genai

gpt = OpenAI(http_client=reporter.openai_http_client())
gpt_async = AsyncOpenAI(http_client=reporter.openai_http_client(asynchronous=True))
claude = Anthropic(http_client=reporter.anthropic_http_client())
gemini = genai.Client(http_options=reporter.genai_http_options())
```

```go [Go]
gemini, err := genai.NewClient(ctx, &genai.ClientConfig{Backend: genai.BackendVertexAI})
reporter.InstrumentGenAI(gemini)

claude := anthropic.NewClient(option.WithMiddleware(reporter.AnthropicMiddleware()))

// Also records Perplexity and any other API that speaks OpenAI's, through option.WithBaseURL.
gpt := openai.NewClient(option.WithMiddleware(reporter.OpenAIMiddleware()))
```

In TypeScript, use the [Vercel AI SDK](vercel-ai-sdk.md) middleware, or record calls by hand as shown there.

Each call is recorded under the request, user and customer set with `scope`, and under the function in your code that made it.

## Name the part of your product

Where several functions do one job, or every call goes through one shared helper, name the part yourself:

```python [Python]
with agentpulse.scope(component="report_generation"):
    write_report()
```

```go [Go]
ctx = recorder.WithComponent(ctx, "report_generation")
```

## Things the client cannot see

- **Streamed OpenAI chats** report their token counts only when the request sets `stream_options={"include_usage": True}`. Without it, the call is recorded with no counts.
- **Claude through Vertex or Bedrock** is billed by Google or Amazon. In Go, set `BilledBy` on the attribution and register the middleware before the SDK's Vertex or Bedrock option. A streamed Bedrock answer is recorded without token counts.
- **Gemini's Live API** does not use the client's HTTP transport, so it is not recorded.
- **A client an ADK agent also uses** should not be wrapped: the agent's hooks already record its calls.

## A provider none of these reach

Record the call yourself, passing the provider's own usage numbers unchanged. Agent Pulse prices them on the server.

```python [Python]
reporter.model_call(agentpulse.ModelCall(
    model="gemini-2.5-pro",
    reported=agentpulse.reported_from_genai(response.usage_metadata),
    format=agentpulse.FORMAT_VERTEX,
    duration=time.monotonic() - started,
))
```

```go [Go]
reporter.ModelCall(ctx, recorder.ModelCall{
    Model:    "claude-sonnet-4-5",
    Duration: time.Since(started),
    Err:      err,
    Format:   recorder.FormatAnthropic,
    Reported: map[string]int64{"input_tokens": in, "output_tokens": out},
})
```

Pass a failure as the error. Only its code is recorded, never its message.
