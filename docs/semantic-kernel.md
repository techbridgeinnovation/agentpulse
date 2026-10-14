---
title: Semantic Kernel
summary: Send Semantic Kernel's own OpenTelemetry spans to Agent Pulse.
section: frameworks
order: 9
---

# Semantic Kernel

Semantic Kernel emits GenAI spans of its own once one setting is on. Python.

## Turn tracing on

```bash
pip install semantic-kernel opentelemetry-sdk opentelemetry-exporter-otlp-proto-http
```

Set the exporter variables from [OpenTelemetry](opentelemetry.md), and this one:

```env
SEMANTICKERNEL_EXPERIMENTAL_GENAI_ENABLE_OTEL_DIAGNOSTICS=true
```

Then once at startup:

```python
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter

provider = TracerProvider()
provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))
trace.set_tracer_provider(provider)
```

## What is recorded

- Each model call, with its model, provider and input and output tokens.
- Each tool call, named `<plugin>-<function>`.
- A model call that fails, as failed.

Semantic Kernel does not report cached tokens, so a call that read from the cache is priced as if it had not.

Checked with Semantic Kernel 1.36.

## Budgets

Spans arrive after the call, so spend counts against your budgets and raises alerts, but a budget cannot stop a call recorded this way.
