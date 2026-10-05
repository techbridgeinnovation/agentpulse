---
title: LangChain and LangGraph
summary: One handler on the outermost call records every model and tool call in the run.
section: frameworks
order: 2
---

# LangChain and LangGraph

Pass one handler on the outermost call. Every model call and tool call the run makes is recorded, including agents called from inside another agent's tool. Python only.

## Add the handler

```python
reporter = agentpulse.connect(service="research-agent").governed()

agent.invoke(inputs, config={
    "callbacks": [reporter.langchain_handler()],
    "configurable": {"thread_id": conversation_id},
})
```

## What is recorded

- Each call is filed under the agent LangChain names for it, from `create_agent(name=...)`. A graph without agents is filed under the node the call ran in.
- The LangGraph thread is the session, and the outermost run is the request, unless you set them with `agentpulse.scope`.
- A stream that fails part way is recorded with the tokens it had used. A call its caller cancels is recorded as `TRUNCATED`.
- A tool's result size is recorded, never its content.

## Budgets

A governed handler checks your budgets before each model call, and a stopped call raises `SpendDenied` before it is sent; `agentpulse.denied(err)` recognises it. A handler cannot change which model a call uses, so a downgrade lets the call through as asked.
