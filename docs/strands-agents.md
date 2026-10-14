---
title: Strands Agents
summary: Send Strands' own OpenTelemetry spans to Agent Pulse.
section: frameworks
order: 8
---

# Strands Agents

Strands emits GenAI spans of its own and sets up the exporter for you. Python.

## Turn tracing on

```bash
pip install 'strands-agents[otel]'
```

Set the exporter variables from [OpenTelemetry](opentelemetry.md), then once at startup:

```python
from strands.telemetry import StrandsTelemetry

StrandsTelemetry().setup_otlp_exporter()
```

## What is recorded

- Each model call, with its model and tokens, cached tokens included.
- Each tool call.
- A model call that fails, as failed.
- The agent's own span carries the run's totals and is not counted again.

Strands names itself, not the provider, on its spans, so the provider is told from the model name: `gpt-`, `o1` to `o4`, `claude-`, `gemini-`, `sonar` and Bedrock ids such as `us.anthropic.claude-…`. A model named any other way is recorded unpriced.

Checked with Strands Agents 1.60.

## Budgets

Spans arrive after the call, so spend counts against your budgets and raises alerts, but a budget cannot stop a call recorded this way.
