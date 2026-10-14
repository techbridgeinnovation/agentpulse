---
title: OpenLIT
summary: Record any Python framework built on the OpenAI client with OpenLIT.
section: frameworks
order: 13
---

# OpenLIT

OpenLIT instruments the model client a framework calls, so it records frameworks that emit no GenAI spans of their own. Python.

## Turn tracing on

```bash
pip install openlit
```

Set the exporter variables from [OpenTelemetry](opentelemetry.md), and this one, without which OpenLIT prints spans instead of sending them:

```env
OTEL_TRACES_EXPORTER=otlp
```

Then once at startup:

```python
import openlit

openlit.init(disable_metrics=True, capture_message_content=False)
```

## What is recorded

- Each OpenAI client call, with its model, provider and tokens, cached tokens included.
- A call that fails, as failed, with the client's error name.
- Not tool calls: OpenLIT has no tool span.
- Spans OpenLIT adds for a framework's own steps carry totals and are not counted again.

A streamed OpenAI call made without `stream_options={"include_usage": True}` is recorded with OpenLIT's own estimate of its tokens, which can be far from what the provider bills. Ask for usage on streamed calls.

Checked with OpenLIT 1.45 on the OpenAI client, [CrewAI](crewai.md) and [LlamaIndex](llamaindex.md).

## Budgets

Spans arrive after the call, so spend counts against your budgets and raises alerts, but a budget cannot stop a call recorded this way.
