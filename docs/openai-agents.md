---
title: OpenAI Agents SDK
summary: Run hooks record every model and tool call, under the agent that made it.
section: frameworks
order: 3
---

# OpenAI Agents SDK

Pass run hooks to the run, and to every agent you run as a tool. Python only.

## Add the hooks

```python
reporter = agentpulse.connect(service="research-agent").governed()
hooks = reporter.openai_agents_hooks()

# An agent run as a tool is a run of its own: give it the same hooks.
researcher_tool = researcher.as_tool(tool_name="ask_researcher", tool_description="...", hooks=hooks)
result = await Runner.run(agent, prompt, hooks=hooks)
```

If you set the model on a `RunConfig`, give the hooks the same config so they can read it: `reporter.openai_agents_hooks(run_config=config)`.

## What is recorded

- Each call is filed under the agent that made it. The run's trace is the request, and the trace's `group_id` is the session.
- A model routed through LiteLLM, `litellm/<provider>/<model>`, is billed under that provider.
- The SDK does not tell hooks when a model call fails. Such a call is recorded as `FAILED` with the code `Unknown` when the run's trace ends.
- A tool's result size is recorded, never its content.

## Budgets

A governed reporter's hooks check your budgets before each call, and a stopped call raises `SpendDenied` before it is sent. The hooks cannot change which model a call uses, so a downgrade lets the call through as asked.
