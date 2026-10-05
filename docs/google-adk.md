---
title: Google ADK
summary: Register the recorder once on an ADK agent; no call site changes.
section: frameworks
order: 1
---

# Google ADK

On an agent built with Google's Agent Development Kit, register the recorder once. Every model call and tool call is recorded, under the sub-agent that made it, and nothing at your call sites changes.

## Register it

```python [Python]
from google.adk.apps import App

reporter = agentpulse.connect(service="research-agent").governed()

app = App(
    name="research",
    root_agent=agent,
    plugins=[reporter.adk_plugin(denied_message="You have used this month's research credits.")],
)
```

```go [Go]
import "github.com/techbridgeinnovation/agentpulse/recorder/adkv2hooks"

recording := adkv2hooks.Options{
    Agent:   agent,
    Service: "research-agent",
    Model:   "gemini-2.5-pro",
    // Lets budgets stop and downgrade calls. Leave it out and budgets only count.
    Decider: recorder.NewGRPCDecider(conn),
}

agentConfig := llmagent.Config{
    Name:  "research",
    Model: model,
    Tools: tools,

    BeforeModelCallbacks:  []llmagent.BeforeModelCallback{adkv2hooks.BeforeModel(rec, recording)},
    AfterModelCallbacks:   []llmagent.AfterModelCallback{adkv2hooks.AfterModel(rec, recording)},
    OnModelErrorCallbacks: []llmagent.OnModelErrorCallback{adkv2hooks.OnModelError(rec, recording)},
    BeforeToolCallbacks:   []llmagent.BeforeToolCallback{adkv2hooks.BeforeTool(rec, recording)},
    AfterToolCallbacks:    []llmagent.AfterToolCallback{adkv2hooks.AfterTool(rec, recording)},
    OnToolErrorCallbacks:  []llmagent.OnToolErrorCallback{adkv2hooks.OnToolError(rec, recording)},
    AfterAgentCallbacks:   []agent.AfterAgentCallback{adkv2hooks.AfterAgent(rec, recording)},
}
```

In Go, `adkv2hooks` is for ADK 2 and `adkhooks` for ADK 1. They take the same callbacks; only `adkv2hooks` takes `Model`. Put the recorder's callbacks ahead of your own: an error callback after one that handles the error never runs, and the failed call would not be recorded.

## What is recorded

- The user, session, agent and turn come from ADK's own context. A request or user you set with `scope` or `recorder.WithUser` takes priority.
- A streamed call is recorded once, when it finishes.
- A call cancelled part way is recorded as `TRUNCATED`, with the tokens it had used.
- A tool's result size is recorded, never its content.

In Python, an agent run as a tool with `include_plugins=False` runs without the plugin, so its calls are not recorded.

## Budgets on ADK

With a governed reporter in Python, or a `Decider` in Go, each model call checks your budgets first. A stopped call is never sent: in Python the user reads your `denied_message` as the reply. A downgrade switches the model the agent sends, when the replacement is from the same provider.
