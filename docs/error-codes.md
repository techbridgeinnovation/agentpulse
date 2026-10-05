---
title: Error codes
summary: Failure kinds on recorded calls, and the errors the API and recorders return.
section: more
order: 2
---

# Error codes

## Kinds of failure on a recorded call

Every failed call is recorded with the provider's own error code, and grouped into one kind, so the same failure reads the same across providers. Group by `error_class` over the API to count them.

| `error_class` | Shown as | Whose | What it means |
| --- | --- | --- | --- |
| `ERROR_CLASS_RATE_LIMITED` | Rate limit exceeded | Provider | Over a rate or quota. Retrying after the wait usually works. |
| `ERROR_CLASS_TIMEOUT` | Request timed out | Provider | No answer in time. Retrying may work. |
| `ERROR_CLASS_UNAVAILABLE` | Service unavailable | Provider | The provider or tool was down, overloaded or unreachable. |
| `ERROR_CLASS_SAFETY_BLOCKED` | Blocked by content filter | Provider | A safety filter stopped the prompt or the answer. |
| `ERROR_CLASS_INVALID_REQUEST` | Invalid request | Yours | The provider rejected the request as sent. It fails until the request changes. |
| `ERROR_CLASS_NOT_FOUND` | Resource not found | Yours | The model named does not exist, or this account cannot use it. |
| `ERROR_CLASS_PERMISSION_DENIED` | Access denied | Yours | The provider credential was missing, wrong or not allowed. |
| `ERROR_CLASS_CONTEXT_OVERFLOW` | Context window exceeded | Yours | The request held more tokens than the model accepts. |
| `ERROR_CLASS_BILLING` | Billing limit reached | Yours | The provider account cannot pay: a spend limit, an empty balance or billing off. |
| `ERROR_CLASS_TOOL_ERROR` | Tool execution failed | Yours | The tool ran and failed in its own code. |
| `ERROR_CLASS_CANCELLED` | Request cancelled | Unknown | The caller stopped waiting. Nothing was wrong with the call. |
| `ERROR_CLASS_UNKNOWN` | Unknown error | Unknown | A code not yet mapped. The provider's code is shown beside it. |

Two statuses are not failures of the call itself: `DENIED`, stopped by your budget before it was sent and never billed, and `TRUNCATED`, stopped at a token limit or deadline after doing part of the work.

## Errors from the API

| Status | Why | What to do |
| --- | --- | --- |
| `400` | The window is missing or ends before it starts, or a dimension or filter field is unknown. | Check the parameters against the method's page. |
| `401` | The key or secret is missing, unknown or revoked. | Send both headers. Create a new key if this one was revoked. |
| `403` | A key without **Read** was used, or the path names another organisation. | Use a read key belonging to the organisation in the path. |
| `429` | Over your organisation's rate limit. | Wait a moment and retry. |

## Errors from the recorders

| Error | When | What to do |
| --- | --- | --- |
| Settings error at startup | An `AP_*` setting is missing or malformed, for example an agent outside the key's organisation. | Fix the setting. The message names it. |
| `SpendDenied` | A budget stopped the call. Recognise it with `agentpulse.denied(err)`, `denied(err)` or `recorder.Denied(err)`. | Tell your user in your own words. |
| `403` from the LiteLLM proxy | A budget stopped the call. Marked not to be retried. | As above. |

Nothing else the recorder does raises into your code. Problems sending records show in its stats instead.
