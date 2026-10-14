---
title: AutoGen
summary: Record AutoGen's tool calls from its own spans and its model calls with OpenLLMetry.
section: frameworks
order: 17
---

# AutoGen

AutoGen emits tool spans of its own but no model call spans. [OpenLLMetry](openllmetry.md) adds the model calls, and both go through one exporter. Python.

## Turn tracing on

```bash
pip install traceloop-sdk
```

Once at startup, before `autogen_agentchat` is imported:

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

With `model_client_stream=True`, ask the client for usage, or streamed calls are not recorded:

```python
OpenAIChatCompletionClient(model="gpt-4o-mini", stream_options={"include_usage": True})
```

## What is recorded

- Each model call, with its model, provider and tokens, cached tokens included.
- Each tool call.

Checked with AutoGen AgentChat 0.7 and traceloop-sdk 0.62.

## Budgets

Spans arrive after the call, so spend counts against your budgets and raises alerts, but a budget cannot stop a call recorded this way.
