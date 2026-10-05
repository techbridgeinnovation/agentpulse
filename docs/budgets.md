---
title: Set a budget that downgrades or stops calls
summary: Alert at a share of a limit, switch to a cheaper model, and stop calls at the limit.
section: guides
order: 2
---

# Set a budget that downgrades or stops calls

A budget is a spending limit over a period. As spend approaches it you get alerts; past a threshold you choose, calls can switch to a cheaper model; at the limit, calls stop.

## What a budget does at each point

| Spend | What happens to the next call |
| --- | --- |
| Under 75% of the limit | It goes ahead. |
| 75% | It goes ahead, and a notice is raised. |
| 95%, and the limit itself | It goes ahead, and an urgent alert is raised. |
| Past the downgrade threshold, if you set one | It is sent to the cheaper model you chose. |
| At the limit | It is not sent. Your code decides what to tell the user. |

A notice is also raised when spend is on pace to pass the limit before the period ends. Every decision is written to a log that cannot be edited afterwards.

## Create a budget

On the **Budgets** page, choose **New budget**. Organisation admins and budget admins can create and change budgets; everyone else can read them.

- **Applies to:** the whole organisation, one agent, or one user.
- **Customer:** optionally, narrow it to one customer's workspace.
- **Period:** daily, weekly from Monday, monthly, yearly, or custom. A custom budget runs once between two dates, or repeats every so many days, up to 730. Periods begin at midnight in your organisation's time zone.
- **Limit:** the amount, in US dollars.
- **Model downgrade:** optionally, a threshold below the limit, and the provider and model to switch to.

When several budgets apply to one call, the strictest answer wins: a stop beats a downgrade, and a downgrade beats going ahead.

## Let budgets stop calls

Recording alone counts spend against your budgets and raises alerts. For a budget to downgrade or stop a call, the recorder has to ask before each call. Turn that on where you connect:

```python [Python]
reporter = agentpulse.connect(service="support-triage").governed()
gpt = OpenAI(http_client=reporter.openai_http_client())
```

```ts [TypeScript]
const reporter = connect("support-triage").governed();
```

```go [Go]
reporter = reporter.Governed(recorder.Governance{Decider: recorder.NewGRPCDecider(conn)})
```

On Google ADK in Go, pass `Decider: recorder.NewGRPCDecider(conn)` in the hook options instead. In Python, build your model clients from the governed reporter.

## Handle a stopped call

A stopped call is never sent to the provider, and your code gets an error you can recognise:

```python [Python]
try:
    reply = gpt.chat.completions.create(model="gpt-4.1", messages=messages)
except Exception as err:
    if agentpulse.denied(err):
        return "You've reached this month's limit."
    raise
```

```ts [TypeScript]
import { denied } from "techbridge-agentpulse";

try {
  return await generateText({ model, prompt });
} catch (err) {
  if (denied(err)) return "You've reached this month's limit.";
  throw err;
}
```

```go [Go]
if recorder.Denied(err) {
    return "You've reached this month's limit."
}
```

On Google ADK in Python, give the plugin the words to reply with instead: `reporter.governed().adk_plugin(denied_message="You've reached this month's limit.")`. The model is never called and the user reads your message.

## Where a downgrade applies

The recorder switches the model only where it can change the call before it is sent, and only to a model from the same provider. Otherwise the call goes ahead as asked, and is counted as a downgrade not applied.

| Where you record | Downgrade |
| --- | --- |
| OpenAI, Anthropic and Gemini clients in Python | Applied |
| Gemini client in Go, and Claude through Vertex in Go | Applied |
| Google ADK, in Python and Go | Applied |
| LiteLLM proxy | Applied |
| OpenAI client in Go, and Anthropic called directly from Go | Not applied; stop still works |
| LangChain, OpenAI Agents SDK, Vercel AI SDK | Not applied; stop still works |
| OpenTelemetry | Neither: records only |

## If Agent Pulse does not answer

If a budget check gets no answer within 1.5 seconds, the call goes ahead, so Agent Pulse being slow or down never stops your product. One exception: a call stopped by a budget in the last hour stays stopped, so an outage does not lift a limit that was already reached.
