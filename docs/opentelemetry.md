---
title: OpenTelemetry
summary: Point your OTLP exporter at Agent Pulse and send the GenAI spans you already emit.
section: frameworks
order: 6
---

# OpenTelemetry

If your agent already emits OpenTelemetry GenAI spans, point its trace exporter at Agent Pulse. Any language, no library.

## Point the exporter here

```env
OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=https://<gateway>/v1/traces
OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=http/protobuf
OTEL_EXPORTER_OTLP_TRACES_HEADERS=X-Api-Key=organisations/<organisation>/apiKeys/<key>,X-Api-Secret=<secret>,X-Agentpulse-Agent=organisations/<organisation>/agents/<agent>
```

To file the spend under one of your customers, add `X-Agentpulse-Workspace=<customer>` to the headers.

## What it reads

- Model calls need the `gen_ai.*` attributes, with token usage. A model call that fails is recorded as failed.
- Tool calls need `execute_tool` spans.
- A span for an agent, a run or a step is not counted, even when it carries the totals of the calls beneath it.
- When a span names a framework instead of the provider, the provider is told from the model name.
- Prompts and answers on a span are never stored.

## Frameworks checked

Each was checked against a real export:

- [Pydantic AI](pydantic-ai.md)
- [Strands Agents](strands-agents.md)
- [Semantic Kernel](semantic-kernel.md)
- [Microsoft Agent Framework](microsoft-agent-framework.md)
- [Mastra](mastra.md)
- [CrewAI](crewai.md), [LlamaIndex](llamaindex.md), [Haystack](haystack.md) and [AutoGen](autogen.md), through [OpenLLMetry](openllmetry.md) or [OpenLIT](openlit.md)
- The OpenTelemetry project's own `opentelemetry-instrumentation-openai-v2`, without cached tokens

Not recorded:

- OpenInference instrumentation, from Arize Phoenix, which smolagents and others use: it names usage `llm.token_count.*`, not `gen_ai.*`.
- Haystack's own tracer, which names nothing in GenAI terms.

Google ADK's own spans are recorded, but ADK writes each model call on two spans, and when they arrive in different exports the call is counted twice. Use the [ADK recorder](google-adk.md) instead.

## Budgets

Spans arrive after the call has happened, so spend is counted against your budgets and alerts are raised, but a budget cannot stop a call recorded this way.
