---
title: Find what is failing
summary: What failed calls and retries cost, which agents are failing, and why.
section: guides
order: 4
---

# Find what is failing

A failed call is usually still billed for the work the provider did before it failed. Agent Pulse keeps that spend apart from spend that worked, and shows where the failures are.

## What counts as failed

| Status | What it means | Billed |
| --- | --- | --- |
| `FAILED` | The call returned an error. | Usually, for the work done before the error. |
| `TRUNCATED` | The call hit a limit or was cancelled part way, and did part of the work. | Yes, for what it did. |
| `DENIED` | A budget stopped the call before it was sent. | No. Counted apart from failures. |

A call that succeeded but returned nothing the agent could use is counted as an empty result, apart from failures.

## How an agent reads

Every agent shows one status on the Agents page and on Home.

| Status | When |
| --- | --- |
| Healthy | Nothing below applies. |
| Degraded | Over 5% of its calls are failing and that is at least twice its usual rate, or it is failing in a way that is yours to fix. |
| Failing | Over 20% of its requests end in failure, so users are left without an answer. |
| Capped | A budget stopped its latest call. |
| Quiet | No calls in the last 24 hours. |

Failures are weighed by the request, not the call. A call that failed and then worked on retry still gave the user an answer.

## Find the cause

Open the agent. **What went wrong** groups its failures by kind, such as rate limits, timeouts or a request the provider rejected, with the provider's error codes, a one-line explanation, and whose problem it is: yours, the provider's, or unknown. It also shows the failure rate for each model the agent uses.

**Where the money goes** splits the agent's spend by part, model, tool or user, with the failure rate of each part beside it. One broken tool shows as that tool failing, not the whole agent.

Agent Pulse records the error code, never the error message, because a provider's message often quotes the prompt back.

## Get told when it happens

A failure spike alert is raised when an agent fails far more of its calls in an hour than it usually does. See [alerts](alerts.md) to send it to Slack or a webhook.

## Read it over the API

Aggregate spend returns `failedCount`, `failedCostMicros` and `retryCostMicros` on every row. Group by `error_class` to count failures by kind across providers: a rate limit is one class whether the provider calls it `RESOURCE_EXHAUSTED`, `rate_limit_error` or `HTTP_429`.

```bash
curl -G "https://$AP_GATEWAY/v1/organisations/<organisation>/activities:aggregate" \
  -H "X-Api-Key: $AP_API_KEY" \
  -H "X-Api-Secret: $AP_API_SECRET" \
  --data-urlencode 'groupBy=agent' \
  --data-urlencode 'groupBy=error_class' \
  --data-urlencode 'filter=status = FAILED' \
  --data-urlencode 'startTime=2026-09-01T00:00:00Z' \
  --data-urlencode 'endTime=2026-10-01T00:00:00Z'
```
