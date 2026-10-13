---
title: Set a budget that warns or stops calls
summary: Alert at a share of a limit, and either stop covered model calls at the limit or only warn.
section: guides
order: 2
---

# Set a budget that warns or stops calls

A budget is a spending limit over a period. As spend approaches it you get alerts. At the limit, an enforced budget stops covered model calls, and a warn-only budget lets them continue and alerts you. Every call is recorded either way.

## What a budget does at each point

| Spend | What happens to the next call |
| --- | --- |
| Under 75% of the limit | It goes ahead. |
| 75% | It goes ahead, and a notice is raised. |
| 95% | It goes ahead, and an urgent alert is raised. |
| At the limit, enforced | It is not sent, and an urgent alert is raised. Your code decides what to tell the user. |
| At the limit, warn only | It goes ahead, and an urgent alert is raised. Spend past the limit keeps counting. |

A notice is also raised when spend is on pace to pass the limit before the period ends. Every decision is written to a log that cannot be edited afterwards.

## Create a budget

On the **Budgets** page, choose **New budget**. Organisation admins and budget admins can create and change budgets; everyone else can read them.

- **Applies to:** the whole organisation, one agent, a set of users, or a set of workspaces. Choosing several users creates a separate budget for each person by default, each with the full limit; choose **One shared budget** instead for a team pool, where their spend is added together against one limit. Choosing several workspaces creates a budget for each.
- **Workspace:** optionally, narrow it to one workspace. A workspace is one tenant of your organisation, named by your code when it records: a customer of your product, or an internal team. A call that names no workspace counts as your default workspace's.
- **Period:** daily, weekly from Monday, monthly, yearly, or custom. A custom budget runs once between two dates, or repeats every so many days, up to 730. Periods begin at midnight in your organisation's time zone.
- **Limit:** the amount, in US dollars.
- **At the limit:** **Enforce** stops covered model calls once the limit is reached. **Warn only** lets them continue and alerts you.

When several budgets apply to one call, each counts all the spend it covers, and the strictest answer wins: an enforced budget that is exhausted stops the call even when a warn-only budget covering it would let it continue. A warn-only budget never stops or changes a call.

## Divide a budget among workspaces

An organisation-wide budget, one narrowed to nothing, can be divided into workspace allocations. On the **Budgets** page, choose **Allocate** beside it, add workspaces, and set an amount for each. Amounts need not be equal, and together they can be at most the budget's limit.

- **Allocation:** a workspace can always spend its own allocation. No other workspace can spend it.
- **Reserve:** whatever the allocations leave of the limit. A workspace past its allocation, and every workspace without one, draws on it, first come first served.
- **Period:** an allocation always has its budget's period. While a budget has allocations, its period cannot change and it cannot be narrowed.

An allocation counts as reached only once its own amount and the reserve are both used. From then on its own **At the limit** setting applies: an enforced allocation stops its workspace's calls, and a warn-only one lets them continue. A workspace without an allocation is held to the budget's own setting once the reserve is used.

Unused allocations are protected only while everything enforces. A warn-only allocation, or a warn-only budget, lets workspaces keep spending once the reserve is used, and that spend counts toward the budget's total, which can then reach its limit before other workspaces have spent their allocations.

Changing an allocation or the budget's limit does not reset spend. The reserve is worked out again from what has already been spent this period, so lowering an allocation or the limit can leave the reserve used at once. The Budgets page warns you before you save when that would happen.

Clearing an allocation keeps its budget as the workspace's own, with the same limit; budgets cannot be deleted. The workspace then shares the reserve like any workspace without an allocation.

## Let budgets stop calls

Recording alone counts spend against your budgets and raises alerts. For an enforced budget to stop a call, the recorder has to ask before each call. Turn that on where you connect:

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

## Where a budget can stop a call

An enforced budget stops a call only where the recorder asks before the call is sent. Wherever it can ask, a warn-only budget lets the call go ahead and alerts you once its limit is passed.

| Where you record | An enforced budget |
| --- | --- |
| OpenAI, Anthropic and Gemini clients in Python | Stops the call |
| Gemini, OpenAI and Anthropic clients in Go, and Claude through Vertex in Go | Stops the call |
| Google ADK, in Python and Go | Stops the call |
| LiteLLM proxy | Stops the call |
| LangChain, OpenAI Agents SDK, Vercel AI SDK | Stops the call |
| OpenTelemetry | Neither: records only, and alerts |

## How exact the limit is

A budget is not a precise hard cap. An enforced budget starts stopping calls shortly after its limit is reached, not at the exact cent:

- **Spend is counted after each call.** A call's cost reaches the budget only once it has been recorded and sent on, so calls already under way, and calls made before that spend arrives, can pass the limit.
- **Answers are reused for a short time.** Python and TypeScript recorders reuse a budget answer for 30 seconds by default, in every process that is running, and Go does when you set `CacheTTL`. Reaching the limit does not cut that short; changing a budget does, where the recorder follows changes.
- **Only recorders that ask are stopped.** An agent that records without `.governed()` is never stopped.

Set an enforced budget's limit with that margin in mind.

## If Agent Pulse does not answer

If a budget check gets no answer within 1.5 seconds, the call goes ahead, so Agent Pulse being slow or down never stops your product. One exception: a call stopped by a budget in the last hour stays stopped, so an outage does not lift a limit that was already reached.
