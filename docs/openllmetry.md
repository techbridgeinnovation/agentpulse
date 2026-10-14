---
title: OpenLLMetry
summary: Record any Python framework built on the OpenAI client with Traceloop's OpenLLMetry.
section: frameworks
order: 12
---

# OpenLLMetry

OpenLLMetry instruments the model client a framework calls, so it records frameworks that emit no GenAI spans of their own. Python.

## Turn tracing on

```bash
pip install traceloop-sdk
```

Once at startup, before the framework is imported:

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

`api_endpoint` is the gateway alone; `/v1/traces` is added for you. The three settings keep prompts off the spans and stop it sending metrics and usage data of its own.

## What is recorded

- Each OpenAI client call, with its model, provider and tokens, cached tokens included.
- A call that fails, as failed, with the client's error name.
- Not tool calls: OpenLLMetry has no tool span.

A streamed OpenAI call reports tokens only when the request asks for them with `stream_options={"include_usage": True}`. Without it the call is not recorded.

Checked with traceloop-sdk 0.62 on the OpenAI client, [CrewAI](crewai.md), [LlamaIndex](llamaindex.md), [Haystack](haystack.md) and [AutoGen](autogen.md).

## Budgets

Spans arrive after the call, so spend counts against your budgets and raises alerts, but a budget cannot stop a call recorded this way.
