---
title: Organisations, workspaces and requests
summary: How spend is grouped, from your organisation down to one end-user request.
section: concepts
order: 3
---

# Organisations, workspaces and requests

## Organisation

Your organisation holds your agents, API keys, budgets and team. Each organisation's data is separate. People get access through roles:

| Role | Can |
| --- | --- |
| Organisation admin | Everything, including keys, team and settings. |
| Budget admin | Read everything, and set budgets and alert channels. |
| Cost viewer | Read spend, agents, models and alerts. |

An agency can build inside a client's organisation, with access the client grants and can take back.

## Workspace

A workspace is one of your customers. Spend filed under it can be read for that customer alone, so you can show each customer their own usage. A product with no customers of its own never names one; everything goes to `default`.

The workspace is set once per request, where you check the sign-in, never at a model call. See [Track cost per customer and user](cost-per-customer.md).

## Request

A request is one thing an end user asked for. It often makes many model and tool calls, across agents. Tag each call with the same request identifier and the console can open the request and show every call in order, with what each cost. That is what makes cost per request possible.

## Outcome

An outcome is a result your product counts, such as a booking made or a report finished. It is attached to the request that produced it, not to a call, because one request makes many calls. With outcomes recorded, you can read cost per outcome rather than only cost per call. See List outcomes.
