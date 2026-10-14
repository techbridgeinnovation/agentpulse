---
title: Microsoft Agent Framework
summary: Send Agent Framework's own OpenTelemetry spans to Agent Pulse.
section: frameworks
order: 10
---

# Microsoft Agent Framework

Agent Framework emits GenAI spans of its own and sets up the exporter from the standard variables. Python.

## Turn tracing on

```bash
pip install agent-framework
```

Set the exporter variables from [OpenTelemetry](opentelemetry.md). Use the `_TRACES_` variables, not `OTEL_EXPORTER_OTLP_ENDPOINT`, which would also send metrics and logs that Agent Pulse does not take. Then once at startup:

```python
from agent_framework.observability import configure_otel_providers

configure_otel_providers()
```

## What is recorded

- Each model call, with its model, provider and tokens, cached tokens included, streamed or not.
- Each tool call.
- The agent's own span carries the run's totals and is not counted again.

Checked with Agent Framework 1.19, on the chat completions client streamed and not, and on the Responses client not streamed.

## Budgets

Spans arrive after the call, so spend counts against your budgets and raises alerts, but a budget cannot stop a call recorded this way.
