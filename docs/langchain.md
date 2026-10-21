---
title: LangChain and LangGraph
summary: One handler on the outermost call records every model and tool call in the run.
section: frameworks
order: 2
---

# LangChain and LangGraph

Pass one handler on the outermost call. Every model call and tool call the run makes is recorded, including agents called from inside another agent's tool. Python only.

## Install

```bash
pip install techbridge-agentpulse
```

The recorder has no dependencies of its own. It uses the `langchain` you already have.

## Set four settings

The recorder reads them from the environment and stops at startup if one is missing. See the [quickstart](/docs/quickstart) for what each one is.

```env
AP_GATEWAY=gateway.agentspulse.ai
AP_API_KEY=organisations/<organisation>/apiKeys/<key>
AP_API_SECRET=<secret>
AP_AGENT=organisations/<organisation>/agents/<agent>
```

## Add the handler

```python
import agentpulse
from langchain.agents import create_agent

reporter = agentpulse.connect(service="weather-service").governed()


def get_weather(city: str) -> str:
    """Returns the current weather for a city."""
    return f"The weather in {city} is sunny."


agent = create_agent(
    model="google_genai:gemini-flash-latest",
    tools=[get_weather],
    name="weather-agent",
)

agent.invoke(
    {"messages": [{"role": "user", "content": "What's the weather in Nairobi?"}]},
    config={
        "callbacks": [reporter.langchain_handler()],
        "configurable": {"thread_id": "conversation-1"},
    },
)
```

## What is recorded

- Each call is filed under the agent LangChain names for it, from `create_agent(name=...)`. A graph without agents is filed under the node the call ran in.
- The LangGraph thread is the session, and the outermost run is the request, unless you set them with `agentpulse.scope`.
- A stream that fails part way is recorded with the tokens it had used. A call its caller cancels is recorded as `TRUNCATED`.
- A tool's result size is recorded, never its content.

## Budgets

A governed handler checks your budgets before each model call, and a stopped call raises `SpendDenied` before it is sent; `agentpulse.denied(err)` recognises it. A warn-only budget past its limit lets the call through and alerts you.
