---
title: Google ADK
summary: Register the recorder once on an ADK agent; no call site changes.
section: frameworks
order: 1
---

# Google ADK

On an agent built with Google's Agent Development Kit, register the recorder once. Every model call and tool call is recorded, under the sub-agent that made it, and nothing at your call sites changes.

## Install

```bash [Python]
pip install techbridge-agentpulse
```

```bash [Go]
go get github.com/techbridgeinnovation/agentpulse/recorder
go get github.com/techbridgeinnovation/agentpulse/recorder/adkv2hooks
```

The recorder has no dependencies of its own. It uses the ADK you already have.

## Set four settings

The recorder reads them from the environment. See the [quickstart](/docs/quickstart) for what each one is.

```env
AP_GATEWAY=gateway.agentspulse.ai
AP_API_KEY=organisations/<organisation>/apiKeys/<key>
AP_API_SECRET=<secret>
AP_AGENT=organisations/<organisation>/agents/<agent>
```

## Register it

```python [Python]
import agentpulse
from google.adk.agents import Agent
from google.adk.apps import App

reporter = agentpulse.connect(service="research-agent").governed()

root_agent = Agent(
    name="research",
    model="gemini-2.5-pro",
    instruction="Answer research questions using the tools.",
    tools=tools,
)

app = App(
    name="research",
    root_agent=root_agent,
    plugins=[reporter.adk_plugin(denied_message="You have used this month's research credits.")],
)
```

```go [Go]
import (
    "github.com/techbridgeinnovation/agentpulse/recorder"
    "github.com/techbridgeinnovation/agentpulse/recorder/adkv2hooks"
    "google.golang.org/adk/v2/agent"
    "google.golang.org/adk/v2/agent/llmagent"
)

key := os.Getenv("AP_API_KEY")
conn, err := recorder.DialWithKey(os.Getenv("AP_GATEWAY"), key, os.Getenv("AP_API_SECRET"))
if err != nil {
    log.Fatal(err)
}
rec := recorder.New(recorder.Config{
    Sinks: []recorder.Sink{recorder.NewGRPCSink(conn, recorder.OrganisationOfKey(key))},
})
defer rec.Close(ctx)

recording := adkv2hooks.Options{
    Agent:   os.Getenv("AP_AGENT"),
    Service: "research-agent",
    Model:   "gemini-2.5-pro",
    // Lets budgets stop calls. Leave it out and budgets only count.
    Decider: recorder.NewGRPCDecider(conn),
}

rootAgent, err := llmagent.New(llmagent.Config{
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
})
```

In Go, `adkv2hooks` is for ADK 2 and `adkhooks` for ADK 1. They take the same callbacks; only `adkv2hooks` takes `Model`. `adkhooks` comes with the recorder, so ADK 1 needs only the first `go get`. Put the recorder's callbacks ahead of your own: an error callback after one that handles the error never runs, and the failed call would not be recorded.

## What is recorded

- The user, session, agent and turn come from ADK's own context. A request or user you set with `scope` or `recorder.WithUser` takes priority.
- A streamed call is recorded once, when it finishes.
- A call cancelled part way is recorded as `TRUNCATED`, with the tokens it had used.
- A tool's result size is recorded, never its content.

In Python, an agent run as a tool with `include_plugins=False` runs without the plugin, so its calls are not recorded.

## Budgets on ADK

With a governed reporter in Python, or a `Decider` in Go, each model call checks your budgets first. A stopped call is never sent: in Python the user reads your `denied_message` as the reply. A warn-only budget past its limit lets the call through and alerts you.
