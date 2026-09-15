# Read your spend

Agent Pulse records what your agents spend. This is how you read it back, so you can put the figures on a dashboard of your own rather than ours.

Everything here is one HTTPS endpoint and one API key. There is no SDK to install, and the recorder is not involved — recording and reading are separate credentials on purpose.

## Get a reading key

On the console's **API keys** page, create a key and choose **Reads**.

A key does one thing or the other, never both. A key that records lives inside an agent's process, which is where a credential is most likely to leak, and one that could also read would hand over your whole organisation's spend. A reading key cannot write anything.

The secret is shown once. Keep it where you keep your other secrets; it is not recoverable.

## Ask a question

Send the key as `x-api-key`, over TLS, to the gateway address shown on the console's Connect page. Every request names your organisation as `parent`; a request naming any other is refused, whether or not that organisation exists.

The one method most dashboards need is `AggregateActivities`: it answers with one row per group, summed by Agent Pulse, so you never read individual calls.

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
      "p50DurationMs": "820",
      "p95DurationMs": "4100",
      "lastOccurredAt": "2026-09-15T12:41:08Z"
    }
  ]
}
```

## What you can group by

`agent` · `model` · `provider` · `user` · `caller_service` · `caller_component` · `skill` · `status` · `error_code` · `kind` · `date` · `hour`

`date` and `hour` are UTC. `kind` is `model`, `tool` or `call`. An empty `groupBy` returns a single total for the window.

Group by as many at once as you need — `["date", "model"]` is a stacked chart, `["user"]` is a cost-per-person table. Ask for what a panel shows, rather than reading rows and summing them yourself.

## Money

`estimatedCostMicros` and `billedCostMicros` are whole millionths of a US dollar, as strings, because JSON numbers cannot hold a 64-bit integer exactly. Divide by 1,000,000 for dollars, and do it at the point you render rather than on the way in.

The estimate is priced from tokens when the call was recorded, against the rate card in force at that moment. The billed figure is what the provider's invoice later confirmed, and it stays zero for a provider billed directly rather than through Google — that boundary is real, not a gap waiting to close.

## The other four methods

| Method | What it answers |
| --- | --- |
| `AggregateActivities` | spend grouped by anything above — what a dashboard reads |
| `ListActivities` | individual calls, newest first, inside a window — a drill-down, not a report |
| `ListAgents` | the agents registered under your organisation |
| `ListUsers` | the names you have given the identifiers your activities carry |
| `ListOutcomes` | units of business work counted against a request |

`ListPriceableUnits` reads the rate card, which belongs to no organisation and is the same for everyone.

## Names, not identifiers

An activity carries an identifier for the person it was done for — whatever your own sign-in issued — and never their name or email. Agent Pulse has no way to resolve one: it never sees your sign-in.

To show names on your dashboard, either join `ListUsers` against the identifiers your rows carry, or resolve them against your own user table. `ListUsers` holds only what your agents have sent, through the recorder's `recorder.User{ID, Name, Email}`, so an organisation that sends nothing sees identifiers, and that is correct rather than broken.

## Limits

Reads are held to your plan's rate limit, weighted one call per request. Going over answers `RESOURCE_EXHAUSTED`, which means try again shortly.

The monthly quota is not applied to reads. It counts what you recorded, which is what you pay for; reading those figures back is not a second charge for the same work.

## When something is refused

| Code | What it means |
| --- | --- |
| `UNAUTHENTICATED` | the key is missing, unknown or revoked — these are answered identically, so probing learns nothing |
| `PERMISSION_DENIED` | a recording key was used to read, or the request named an organisation the key does not belong to |
| `INVALID_ARGUMENT` | the window is missing or ends before it starts, or a `groupBy` name is not one of those listed |
| `RESOURCE_EXHAUSTED` | over the plan's rate limit |
