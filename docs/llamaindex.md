---
title: LlamaIndex
summary: Record LlamaIndex's model calls with OpenLLMetry.
section: frameworks
order: 15
---

# LlamaIndex

LlamaIndex emits no GenAI spans of its own. [OpenLLMetry](openllmetry.md) records its model calls. Python.

## Turn tracing on

```bash
pip install traceloop-sdk
```

Once at startup, before `llama_index` is imported:

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

LlamaIndex agents stream, and a streamed OpenAI call reports tokens only when asked. Ask on the model:

```python
from llama_index.llms.openai import OpenAI

llm = OpenAI(model="gpt-4o-mini", additional_kwargs={"stream_options": {"include_usage": True}})
```

Without it no model call is recorded.

## What is recorded

- Each model call, with its model, provider and tokens, cached tokens included.
- Not tool calls.

[OpenLIT](openlit.md) records the same calls, and needs `include_usage` too.

Checked with LlamaIndex 0.14 and traceloop-sdk 0.62.

## Budgets

Spans arrive after the call, so spend counts against your budgets and raises alerts, but a budget cannot stop a call recorded this way.
