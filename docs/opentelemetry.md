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

- Model calls need the `gen_ai.*` attributes, with token usage.
- Tool calls need `execute_tool` spans.
- Prompts and answers on a span are never stored.

## Budgets

Spans arrive after the call has happened, so spend is counted against your budgets and alerts are raised, but a budget cannot stop or downgrade a call recorded this way.
