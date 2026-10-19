---
title: Check your bill
summary: Send your Google bill once a day and see each hour checked against what was recorded.
section: guides
order: 7
---

# Check your bill

Agent Pulse estimates every call from its tokens at published prices. Send it your Google bill and it checks each hour and model against those estimates, so you can see the estimates hold and catch what doesn't: calls that went unrecorded, or a price that's wrong.

Your own code reads the bill, with your own credentials, and sends it. Agent Pulse never gets access to your billing account.

## What you need

- **Your Google billing export in BigQuery.** The standard usage cost export is enough. Turn it on in the Google Cloud console under **Billing**, then **Billing export**.
- **Something that can read it.** A service account of yours with BigQuery read access to the export, and permission to run queries in a project of yours.
- **A key that records.** On the **API keys** page, create a key for **An agent**, covering the whole organisation rather than one workspace. The key your agents already use works.

Only traffic Google bills can be checked, which is Gemini and other models on Vertex AI. Calls to OpenAI or Anthropic directly stay estimates.

## Read a day of your bill

This query returns one day of Vertex AI charges for the projects your agents run in, in US dollars after credits. Replace the table and the project ids with yours.

```sql
SELECT
  project.id AS billingAccount,
  FORMAT_TIMESTAMP('%Y-%m-%dT%H:%M:%SZ', usage_start_time) AS startTime,
  FORMAT_TIMESTAMP('%Y-%m-%dT%H:%M:%SZ', usage_end_time) AS endTime,
  sku.description AS product,
  CAST(ROUND((cost + IFNULL((SELECT SUM(c.amount) FROM UNNEST(credits) c), 0))
    / NULLIF(currency_conversion_rate, 0) * 1e6) AS INT64) AS costMicros,
  usage.amount AS usageAmount
FROM `my-billing-project.billing_export.gcp_billing_export_v1_XXXXXX`
WHERE service.description = 'Vertex AI'
  AND project.id IN ('my-prod-project')
  AND usage_start_time >= TIMESTAMP('2026-10-13')
  AND usage_start_time < TIMESTAMP('2026-10-14')
```

Send every line, including those that cost nothing: the day you send is taken as the whole bill for that day.

- **Only your own projects.** An export often covers several projects, such as test and production. Send only the projects whose agents record to the gateway you send to, or nothing matches.
- **Skip a day with no lines.** Google may not have exported it yet, and sending none says the bill was zero. Send it on a later run instead.

## Send it

Post the lines with the window they cover:

```bash
curl "https://$AP_GATEWAY/v1/organisations/<organisation>/reconciliations:reportBill" \
  -H "X-Api-Key: $AP_API_KEY" \
  -H "X-Api-Secret: $AP_API_SECRET" \
  -H "Content-Type: application/json" \
  -d '{
    "billedBy": "VERTEX_AI",
    "startTime": "2026-10-13T00:00:00Z",
    "endTime": "2026-10-14T00:00:00Z",
    "lines": [
      {
        "billingAccount": "my-prod-project",
        "startTime": "2026-10-13T17:00:00Z",
        "endTime": "2026-10-13T18:00:00Z",
        "product": "Gemini 3 Flash Text Input - Predictions",
        "costMicros": "1310000",
        "usageAmount": 2620000
      }
    ]
  }'
```

```python
reply = requests.post(
    f"https://{os.environ['AP_GATEWAY']}/v1/organisations/<organisation>/reconciliations:reportBill",
    headers={"X-Api-Key": os.environ["AP_API_KEY"], "X-Api-Secret": os.environ["AP_API_SECRET"]},
    json={
        "billedBy": "VERTEX_AI",
        "startTime": "2026-10-13T00:00:00Z",
        "endTime": "2026-10-14T00:00:00Z",
        "lines": [
            {
                "billingAccount": "my-prod-project",
                "startTime": "2026-10-13T17:00:00Z",
                "endTime": "2026-10-13T18:00:00Z",
                "product": "Gemini 3 Flash Text Input - Predictions",
                "costMicros": "1310000",
                "usageAmount": 2620000,
            }
        ],
    },
    timeout=60,
)
reply.raise_for_status()
```

The reply lists each hour and model and how it compared.

- **The window** starts and ends on the hour and covers at most a day. Every line must start inside it.
- **At most 20,000 lines** go in one report. A day with more is sent an hour at a time.
- **`environment`** is optional. If your production and test agents bill to different projects, send each project's lines with its environment, `organisations/<organisation>/environments/<environment>`, so each is checked only against calls recorded there.

## Run it every day

Google's export lands within about two days. Run the query and the post once a day for the day two days back, for example from Cloud Scheduler or a cron job.

Sending a day again replaces what was sent for it, so a day Google corrects later is simply sent again. Running the same day twice is safe.

Here is the whole job in Python:

```python
import datetime, os, requests
from google.cloud import bigquery

day = datetime.date.today() - datetime.timedelta(days=2)
start, end = day.isoformat() + "T00:00:00Z", (day + datetime.timedelta(days=1)).isoformat() + "T00:00:00Z"

query = open("bill.sql").read()  # the query above, with @start and @end in place of the dates
rows = bigquery.Client().query(query, job_config=bigquery.QueryJobConfig(query_parameters=[
    bigquery.ScalarQueryParameter("start", "TIMESTAMP", start),
    bigquery.ScalarQueryParameter("end", "TIMESTAMP", end),
])).result()

reply = requests.post(
    f"https://{os.environ['AP_GATEWAY']}/v1/{os.environ['AP_ORGANISATION']}/reconciliations:reportBill",
    headers={"X-Api-Key": os.environ["AP_API_KEY"], "X-Api-Secret": os.environ["AP_API_SECRET"]},
    json={"billedBy": "VERTEX_AI", "startTime": start, "endTime": end,
          "lines": [{**dict(r), "costMicros": str(r["costMicros"])} for r in rows]},
    timeout=60,
)
reply.raise_for_status()
```

`AP_ORGANISATION` is `organisations/<organisation>`.

## Read the result

The **Bill check** page shows each day: Google's bill, what was recorded for the same calls, the difference, and how much of your spend could be checked. A line under your figures on **Home** says whether the bill matched.

| Result | Means |
| --- | --- |
| Matched | The bill came within 5% of what was recorded. Each call in that hour carries its share of the bill as its billed figure. |
| Bill higher | Google billed more than was recorded, which usually means calls went unrecorded. |
| Bill lower | Google billed less, which usually means our price for that model is too high. |
| Not recorded | Google billed for an hour and model Agent Pulse saw no calls for. |
| Waiting for bill | Calls were recorded and their bill hasn't been sent yet. |

Bill higher and Not recorded usually come from calls outside your recorded services, such as local tests against the same project. Run tests against a separate project to keep them out.

Recorded figures never change. The bill sits beside them, and budgets keep running on the estimate, which is live and covers every provider.

Agent Pulse keeps the result for each hour and model, not your bill lines, for 120 days.

To read the same results from your own code, list them with a key for **A dashboard**:

```bash
curl "https://$AP_GATEWAY/v1/organisations/<organisation>/reconciliations" \
  -H "X-Api-Key: $AP_API_KEY" \
  -H "X-Api-Secret: $AP_API_SECRET"
```
