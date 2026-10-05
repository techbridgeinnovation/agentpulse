---
title: Read your spend over the API
summary: Put your organisation's figures in your own screens, a warehouse or a report.
section: guides
order: 6
---

# Read your spend over the API

Read your organisation's spend over HTTP and show it beside your own figures: in your product's console, a warehouse, a spreadsheet or a board report.

## Get a key

On the **API keys** page, create a key and tick **Read**. Send its name as `X-Api-Key` and its secret as `X-Api-Secret` on every request.

Keep reading and recording apart. A key used by your agents only needs **Write**, and a dashboard only needs **Read**. A read key that leaks shows everyone's spend in the organisation, so give one both only where a process does both.

## Pick the right method

| You want | Use |
| --- | --- |
| A total, a chart or a table: spend by agent, customer, model or day | Aggregate spend |
| The individual calls behind a figure, such as one request | List activities |
| Names for the user identifiers in your rows | List users |
| Your agents and how they are listed | List agents |
| Business results counted against requests | List outcomes |
| The prices calls are charged at | List prices |

Aggregate spend adds up on the server, over any window. If you find yourself paging through List activities and adding rows up, ask Aggregate spend for the same figure instead.

## Ask for what a panel draws

Group by as many dimensions as the panel needs. `groupBy=date&groupBy=model` is a stacked daily chart; `groupBy=user` is a cost-per-person table; no `groupBy` is one total.

```bash
curl -G "https://$AP_GATEWAY/v1/organisations/<organisation>/activities:aggregate" \
  -H "X-Api-Key: $AP_API_KEY" \
  -H "X-Api-Secret: $AP_API_SECRET" \
  --data-urlencode 'groupBy=date' \
  --data-urlencode 'groupBy=model' \
  --data-urlencode 'startTime=2026-09-01T00:00:00Z' \
  --data-urlencode 'endTime=2026-10-01T00:00:00Z'
```

The dimensions are `workspace`, `project`, `agent`, `model`, `provider`, `user`, `caller_service`, `caller_component`, `skill`, `tool`, `status`, `error_code`, `error_class`, `kind`, `agent_kind`, `date` and `hour`. `date` and `hour` are in UTC.

## Narrow it with a filter

A filter keeps only matching calls: terms of `field = value` joined by `AND`, over the same names you can group by.

| Filter | With | Gives |
| --- | --- | --- |
| `user = "8c21e0b4"` | `groupBy=date` | One person's spend by day |
| `agent = "organisations/acme/agents/atlas" AND status = FAILED` | `groupBy=error_class` | That agent's failures by kind |

Only `=` and `AND` are accepted. Anything else is refused rather than ignored, so a figure never silently leaves out a term you wrote.

## Read one customer

Use the workspace path to read one of your customers only, for example to show them their own usage:

```
/v1/organisations/<organisation>/workspaces/brightline/activities:aggregate
```

A filter can never widen a read past its path: filtering a workspace by another workspace returns nothing.

## Show money

Costs are whole millionths of a US dollar, sent as strings. Divide by 1,000,000 when you display them, not before you add them up.

`estimatedCostMicros` is priced from the call's tokens when it was recorded. `billedCostMicros` is what the provider's billing export says, and is filled only for traffic billed through Google. See [how cost is calculated](how-cost-is-calculated.md).

## Show names

Rows carry user identifiers. Join them with List users, which holds the names your agents sent, or with your own user table.
