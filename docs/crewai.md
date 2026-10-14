---
title: CrewAI
summary: Record CrewAI's model calls with OpenLLMetry or OpenLIT.
section: frameworks
order: 14
---

# CrewAI

CrewAI emits no GenAI spans of its own. Either [OpenLLMetry](openllmetry.md) or [OpenLIT](openlit.md) records its model calls. Python.

## Turn tracing on

```bash
pip install traceloop-sdk
```

Once at startup, before `crewai` is imported:

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

Or with OpenLIT, as on its [page](openlit.md).

Set `CREWAI_DISABLE_TELEMETRY=true` too, so CrewAI's own telemetry is not sent.

## What is recorded

- Each model call, with its model, provider and tokens, cached tokens included.
- Not tool calls: neither instrumentation emits a tool span for CrewAI.
- The crew, task and agent spans are not counted, including the totals OpenLIT puts on them.

Checked with CrewAI 1.15, traceloop-sdk 0.62 and OpenLIT 1.45.

## Budgets

Spans arrive after the call, so spend counts against your budgets and raises alerts, but a budget cannot stop a call recorded this way.
