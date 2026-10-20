---
title: Read your spend over the API
summary: Put your organisation's figures in your own screens, a warehouse or a report.
section: guides
order: 6
---

# Read your spend over the API

Read your organisation's spend over HTTP and show it beside your own figures: in your product's console, a warehouse, a spreadsheet or a board report.

## Get a key

On the **API keys** page, create a key for **A dashboard**. Send its name as `X-Api-Key` and its secret as `X-Api-Secret` on every request.

Every path starts with `https://gateway.agentspulse.ai`, the address on the **Connect** page. The examples call it `$AP_GATEWAY`.

Keep reading and recording apart. A key used by your agents only needs to record, and a dashboard only needs to read. A read key that leaks shows everyone's spend in the organisation, so give one both only where a process does both.

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

The dimensions are `workspace`, `project`, `agent`, `request`, `model`, `provider`, `user`, `caller_service`, `caller_component`, `sub_agent`, `skill`, `framework`, `tool`, `status`, `error_code`, `error_class`, `kind`, `agent_kind`, `environment`, `date` and `hour`. Each also works in camelCase, such as `errorCode`, and a row's `dimensions` always come back keyed in snake_case, such as `error_code`.

- **Days and hours** are cut in the `timeZone` you send, such as `timeZone=Africa/Nairobi`, and in UTC if you send none. A day reads `2026-10-14` and an hour `2026-10-14T12:00`, with no offset.
- **Rows** come most expensive first. Sort them yourself for a chart by day.
- **Times** are RFC 3339. Send an offset or `Z`, such as `2026-10-01T00:00:00+03:00`. The window includes `startTime` and excludes `endTime`.
- **A blank value** groups the calls that carried none. A `user` of `""` is work done for no signed-in person. A call that did not fail has the `error_class` `ERROR_CLASS_UNSPECIFIED`.
- **A day with no calls** has no row. Fill the gaps in your chart yourself.
- **`readMask`** picks the figures to work out, such as `readMask=estimatedCostMicros,activityCount`, and a narrow one answers faster. The figures left out still appear, as `"0"`, and their times as `null`. Leave it out for all of them.

## Narrow it with a filter

A filter keeps only matching calls: terms of `field = value` joined by `AND`. The fields are `workspace`, `project`, `agent`, `request`, `model`, `provider`, `user`, `caller_service`, `caller_component`, `sub_agent`, `skill`, `framework`, `tool`, `status`, `error_code`, `error_class`, `kind` and `environment`, in either spelling. A `workspace` or `agent` can be its short id, such as `default` or `atlas`, or its full name.

| Filter | With | Gives |
| --- | --- | --- |
| `user = "8c21e0b4"` | `groupBy=date` | One person's spend by day |
| `agent = "organisations/acme/agents/atlas" AND status = FAILED` | `groupBy=error_class` | That agent's failures by kind |

Only `=` and `AND` are accepted. Anything else is refused rather than ignored, so a figure never silently leaves out a term you wrote. `status`, `error_class` and `kind` take a fixed set of values, matched in any case, and a value outside the set is refused.

## Read one customer

Use the workspace path to read one of your customers only, for example to show them their own usage:

```
/v1/organisations/<organisation>/workspaces/brightline/activities:aggregate
```

A filter can never widen a read past its path: filtering a workspace by another workspace returns nothing.

## Show money

Costs are whole millionths of a US dollar, sent as strings. Divide by 1,000,000 when you display them, not before you add them up.

`estimatedCostMicros` is priced from the call's tokens when it was recorded. `billedCostMicros` is the call's share of the Google bill you reported, filled only where its hour and model matched. See [check your bill](bill-check.md). See [how cost is calculated](how-cost-is-calculated.md).

## Show names

Rows carry user identifiers. Join them with List users, or with your own user table. A user is listed only once your code has sent their name or email, see [show names instead of identifiers](cost-per-customer.md#show-names-instead-of-identifiers).

## Read cost per outcome

1. List every outcome. The list has no window or filter, so page through it all and keep the ones whose `occurredAt` falls in the period you report on.
2. Ask Aggregate spend for the same period with `groupBy=request`, which gives every request's spend in one call.
3. Join the two on `request`, and divide each request's spend by its outcomes' `quantity`.

## Page through a list

A list returns 100 items unless you ask for up to 1,000 with `pageSize`. Pass `nextPageToken` back as `pageToken` until it comes back empty. Each item comes once, whatever the filter. An empty list is `[]` and a zero figure is `"0"`, so neither field is ever missing.

## When something is refused

A refusal has the HTTP status and a body naming what to change:

```json
{"error": {"code": "InvalidArgument", "reason": "UNKNOWN_DIMENSION", "message": "\"colour\" is not a dimension that can be grouped by; allowed: ...", "field": "groupBy", "requestId": "4f1c9e2a7b3d4e5f8a9b0c1d2e3f4a5b"}}
```

Check `reason` in your code, show `message` to a person, and quote `requestId` when you contact us. Every reply also carries the id as `X-Request-Id`. Every read your key is allowed to make also says where your rate limit stands:

| Header | Means |
| --- | --- |
| `RateLimit-Policy` | The limit and its window in seconds, such as `6000;w=60`: 6,000 reads a minute. |
| `RateLimit-Limit` | The reads the window holds. |
| `RateLimit-Remaining` | How many are left now. |
| `RateLimit-Reset` | Seconds until the full limit is back. |

A `429` says how many seconds to wait in `Retry-After`. A read refused before it is counted, a `401` or `403`, carries none of them. See [error codes](error-codes.md) for every reason.
