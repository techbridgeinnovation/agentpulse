---
title: Get alerts in Slack or a webhook
summary: Which alerts are raised, and how to send them to Slack or your own endpoint.
section: guides
order: 3
---

# Get alerts in Slack or a webhook

Agent Pulse raises an alert when a budget nears its limit, spend jumps, or an agent starts failing. Every alert appears on the **Notifications** page, and can also go to Slack or a webhook of your own.

## Alerts raised

| Alert | When | Severity |
| --- | --- | --- |
| Budget near limit | Spend reaches 75% of a budget, then 95% | Notice at 75%, urgent at 95% |
| Budget reached | Spend reaches the limit | Urgent |
| Budget running out early | Spend is on pace to pass the limit before the period ends | Notice |
| Spend spike | The last 24 hours cost at least twice an ordinary day, and $5 more | Notice; urgent at four times |
| Failure spike | An agent fails far more of its calls in an hour than it usually does | Notice; urgent at half its calls |

An ordinary day is the middle day of the 14 before, once at least seven of them had spend. A spend spike names the agents and customers behind the increase and their share of it. A failure spike needs at least 20 calls and 5 failures in the hour, and a failure rate three times the agent's usual and at least 10 points above it. Calls stopped by a budget are not counted as failures.

Each alert is raised once. A spike stays one alert for 12 hours, and a failure spike for 6.

## Send alerts to Slack

1. In Slack, create an incoming webhook for the channel you want, and copy its URL.
2. In Agent Pulse, open **Notifications**, then **Alert channels**, and choose **Add a channel**.
3. Choose Slack, paste the URL, and give the channel a name.
4. Choose which alerts it takes: all, or urgent only, and optionally only some kinds.
5. Send a test alert to check it arrives.

Organisation admins and budget admins can add and change channels.

## Send alerts to your own endpoint

Add a channel as above, choose **Webhook**, and give an HTTPS address. Agent Pulse shows a signing secret once, starting `whsec_`. Keep it to check that deliveries came from us.

Each alert is a `POST` with a JSON body:

```json
{
  "type": "budget.threshold",
  "timestamp": "2026-10-04T09:12:00Z",
  "data": { "kind": "BUDGET_THRESHOLD", "severity": "URGENT", "title": "brightline budget at 95%" },
  "link": "https://www.agentspulse.ai/notifications"
}
```

`type` is one of `budget.threshold`, `budget.exhausted`, `budget.projected`, `spend.spike`, `failure.spike`, `model.retiring`, `model.price_changed`, or `test`. Ignore a type you do not know; more may be added. `data` is the alert as the Notifications page shows it.

## Check a delivery is ours

Deliveries follow the Standard Webhooks signing scheme, so a Standard Webhooks library can check them. Each carries three headers:

| Header | What it is |
| --- | --- |
| `webhook-id` | The delivery's ID, the same on every retry. |
| `webhook-timestamp` | When this attempt was sent, in seconds since 1970. |
| `webhook-signature` | `v1,` and the base64 HMAC-SHA256 of `{id}.{timestamp}.{body}`, keyed with the secret after `whsec_`, base64-decoded. |

```python [Python]
import base64, hashlib, hmac

def is_ours(secret: str, headers, body: bytes) -> bool:
    key = base64.b64decode(secret.removeprefix("whsec_"))
    signed = f"{headers['webhook-id']}.{headers['webhook-timestamp']}.".encode() + body
    expected = "v1," + base64.b64encode(hmac.new(key, signed, hashlib.sha256).digest()).decode()
    return any(hmac.compare_digest(expected, s) for s in headers["webhook-signature"].split())
```

```ts [TypeScript]
import { createHmac, timingSafeEqual } from "node:crypto";

function isOurs(secret: string, headers: Record<string, string>, body: string): boolean {
  const key = Buffer.from(secret.replace(/^whsec_/, ""), "base64");
  const signed = `${headers["webhook-id"]}.${headers["webhook-timestamp"]}.${body}`;
  const expected = Buffer.from("v1," + createHmac("sha256", key).update(signed).digest("base64"));
  return headers["webhook-signature"].split(" ").some((s) => {
    const given = Buffer.from(s);
    return given.length === expected.length && timingSafeEqual(given, expected);
  });
}
```

When you rotate the secret, deliveries carry a signature under each secret, space separated, until the old one stops being honoured. Reject a timestamp more than a few minutes old.

## Retries

Any 2xx response within ten seconds counts as delivered. Anything else, a redirect included, is retried for a day after the alert, with the same `webhook-id`, so you can tell a retry from a new alert. Pausing or deleting a channel stops its retries.
