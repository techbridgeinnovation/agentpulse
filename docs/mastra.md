---
title: Mastra
summary: Send Mastra's own OpenTelemetry spans to Agent Pulse.
section: frameworks
order: 11
---

# Mastra

Mastra emits GenAI spans of its own through its OpenTelemetry exporter. TypeScript.

## Turn tracing on

```bash
npm install @mastra/observability @mastra/otel-exporter @opentelemetry/exporter-trace-otlp-proto
```

```ts
import { Mastra } from "@mastra/core/mastra";
import { Observability } from "@mastra/observability";
import { OtelExporter } from "@mastra/otel-exporter";

export const mastra = new Mastra({
  agents: { researcher },
  observability: new Observability({
    configs: {
      otel: {
        serviceName: "research-agent",
        exporters: [new OtelExporter({
          provider: { custom: {
            endpoint: "https://<gateway>/v1/traces",
            protocol: "http/protobuf",
            headers: {
              "X-Api-Key": "organisations/<organisation>/apiKeys/<key>",
              "X-Api-Secret": process.env.AGENTPULSE_SECRET!,
              "X-Agentpulse-Agent": "organisations/<organisation>/agents/<agent>",
            },
          } },
          signals: { logs: false },
        })],
      },
    },
  }),
});
```

`signals: { logs: false }` stops the exporter sending logs to the same host, which Agent Pulse does not take.

## What is recorded

- Each model call, with its model and tokens, cached tokens included, streamed or not.
- Each tool call.

Mastra names the AI SDK provider, such as `openai.chat`, so the provider is told from the model name, as on the [Strands](strands-agents.md) page.

Checked with Mastra 1.77.

## Budgets

Spans arrive after the call, so spend counts against your budgets and raises alerts, but a budget cannot stop a call recorded this way.
