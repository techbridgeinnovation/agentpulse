---
title: Pydantic AI
summary: Send Pydantic AI's own OpenTelemetry spans to Agent Pulse.
section: frameworks
order: 7
---

# Pydantic AI

Pydantic AI emits GenAI spans of its own. Point a standard exporter at Agent Pulse and turn them on. Python.

## Turn tracing on

```bash
pip install pydantic-ai opentelemetry-sdk opentelemetry-exporter-otlp-proto-http
```

Set the exporter variables from [OpenTelemetry](opentelemetry.md), then once at startup:

```python
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from pydantic_ai import Agent

provider = TracerProvider()
provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))
trace.set_tracer_provider(provider)

Agent.instrument_all()
```

## What is recorded

- Each model call, with its model, provider and tokens, cached tokens included.
- Each tool call.
- A model call that fails, as failed.
- The agent's own span carries the run's totals and is not counted again.

Checked with Pydantic AI 2.56.

## Budgets

Spans arrive after the call, so spend counts against your budgets and raises alerts, but a budget cannot stop a call recorded this way.
