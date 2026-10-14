---
title: Haystack
summary: Record Haystack's model calls with OpenLLMetry.
section: frameworks
order: 16
---

# Haystack

Haystack's own tracer names its spans in terms of its own, not the GenAI conventions, so it records nothing here. [OpenLLMetry](openllmetry.md) records its model calls instead. Python.

## Turn tracing on

```bash
pip install traceloop-sdk
```

Once at startup, before `haystack` is imported:

```python
import os

os.environ["TRACELOOP_TRACE_CONTENT"] = "false"
os.environ["TRACELOOP_TELEMETRY"] = "false"
os.environ["TRACELOOP_METRICS_ENABLED"] = "false"

from traceloop.sdk import Traceloop

Traceloop.init(
    app_name="research-agent",
    api_endpoint="https://<gateway>",
    headers={
        "X-Api-Key": "organisations/<organisation>/apiKeys/<key>",
        "X-Api-Secret": os.environ["AGENTPULSE_SECRET"],
        "X-Agentpulse-Agent": "organisations/<organisation>/agents/<agent>",
    },
)
```

## What is recorded

- Each OpenAI model call, with its model, provider and tokens, cached tokens included.
- Not tool calls.

Checked with Haystack 3.3 and traceloop-sdk 0.62.

## Budgets

Spans arrive after the call, so spend counts against your budgets and raises alerts, but a budget cannot stop a call recorded this way.
