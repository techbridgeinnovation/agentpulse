---
title: Reading your spend
summary: Put your organisation's figures on a dashboard of your own.
order: 2
---

# Reading your spend

## Should you be reading this API at all

Agent Pulse ships data and an API. It does not ship your screens — this console exists so a person can check a figure, not so your product has a dashboard. If you are building anything your own team will look at daily, read the API and draw it yourself.

Read the API when:

- your product already has a console, and spend belongs next to everything else in it
- you want spend beside figures Agent Pulse has never heard of — revenue, seats, tickets closed
- you are exporting into a warehouse, a spreadsheet, or a board pack
- you want an alert of your own shape, on your own schedule

Use this console instead when someone needs a number once and nobody is building anything.

## What you cannot do here

Stated now rather than discovered as a refusal:

- **You cannot read another organisation.** A key belongs to one, and naming any other is refused whether or not it exists.
- **You cannot write anything.** A reading key records nothing, and a recording key reads nothing. They are separate credentials on purpose.
- **We cannot tell you who a person is.** An activity carries the identifier your own sign-in issued and never a name — we never see your sign-in. Names come from the directory your agents fill in, or from your own user table.

## Which method answers which question

This is the one thing worth getting right, because the wrong choice is slow rather than broken.

**`AggregateActivities` answers a panel.** One row per group, summed before it reaches you, for any window. Cost per agent, per model, per person, per day — ask for the shape you are about to draw.

**`ListActivities` answers "show me that one request".** Individual calls, newest first. It is a drill-down.

If you find yourself paging `ListActivities` and adding the rows up, stop — that is the query `AggregateActivities` exists to be. Our own console did it that way once and could not read past twenty thousand calls.

## Getting a key

On the **API keys** page, create a key and choose **Reads**.

A key does one thing or the other, never both. A key that records lives inside an agent's process, which is where a credential is most likely to leak, and one that could also read would hand over your whole organisation's spend.

The secret is shown once and is not recoverable. Keep it where you keep your others.

## Asking a question

Send the key as `x-api-key`, over TLS, to the gateway address shown on the Connect page. Every request names your organisation as `parent`.

```bash
curl -s https://<gateway>/v1/organisations/<id>/activities:aggregate \
  -H "x-api-key: $AP_API_KEY" \
  -H "content-type: application/json" \
  -d '{
        "groupBy": ["agent", "model"],
        "startTime": "2026-09-01T00:00:00Z",
        "endTime":   "2026-10-01T00:00:00Z"
      }'
```

```json
{
  "rows": [
    {
      "dimensions": { "agent": "organisations/acme/agents/atlas", "model": "gemini-2.5-pro" },
      "estimatedCostMicros": "41720000",
      "billedCostMicros": "0",
      "activityCount": "3184",
      "modelCalls": "2950",
      "toolCalls": "234",
      "failedCount": "12",
      "totalTokens": "8412330",
      "promptTokens": "7101220",
      "candidateTokens": "980110",
      "distinctRequests": "1204",
      "distinctUsers": "37",
      "lastOccurredAt": "2026-09-15T12:41:08Z"
    }
  ]
}
```

## What you can group by

`agent` · `model` · `provider` · `user` · `caller_service` · `caller_component` · `skill` · `status` · `error_code` · `kind` · `date` · `hour`

`date` and `hour` are UTC. `kind` is `model`, `tool` or `call`. An empty `groupBy` returns a single total for the window.

Group by as many at once as the panel needs: `["date", "model"]` is a stacked chart, `["user"]` is a cost-per-person table.

A tool call names no model, so a row grouped by `model` holds tool calls under an empty value rather than leaving them out. `modelCalls` and `toolCalls` need not sum to `activityCount` — a call that named no model, counted no tokens and ran no tool is neither.

## Money

`estimatedCostMicros` and `billedCostMicros` are whole millionths of a US dollar, as strings, because JSON numbers cannot hold a 64-bit integer exactly. Divide by 1,000,000 when you render, never on the way in.

The estimate is priced from tokens when the call was recorded, against the rate card in force at that moment. The billed figure is what the provider's invoice later confirmed, and stays zero for a provider billed directly rather than through Google. That boundary is real, not a gap waiting to close.

## The other methods

| Method | What it answers |
| --- | --- |
| `AggregateActivities` | spend grouped by anything above — what a dashboard reads |
| `ListActivities` | individual calls, newest first, inside a window |
| `ListAgents` | the agents recording under your organisation |
| `ListUsers` | the names you have given the identifiers your activities carry |
| `ListOutcomes` | units of business work counted against a request |
| `ListPriceableUnits` | the rate card, which belongs to no organisation |

## Showing names instead of identifiers

Join `ListUsers` against the identifiers your rows carry, or resolve them against your own user table.

`ListUsers` holds only what your agents have sent, through the recorder's `recorder.User{ID, Name, Email}`. An organisation that sends nothing sees identifiers, and that is correct rather than broken.

## Limits

Reads are held to your plan's rate limit, weighted one call per request. Over it answers `RESOURCE_EXHAUSTED`, which means try again shortly.

The monthly quota is not applied to reads. It counts what you recorded, which is what you pay for; reading those figures back is not a second charge for the same work.

## When something is refused

| Code | What it means |
| --- | --- |
| `UNAUTHENTICATED` | the key is missing, unknown or revoked — answered identically, so probing learns nothing |
| `PERMISSION_DENIED` | a recording key was used to read, or the request named an organisation the key does not belong to |
| `INVALID_ARGUMENT` | the window is missing or ends before it starts, or a `groupBy` name is not one of those listed |
| `RESOURCE_EXHAUSTED` | over the plan's rate limit |
