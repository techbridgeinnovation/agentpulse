---
title: LiteLLM proxy
summary: One line in the proxy's config records every model and service behind it.
section: frameworks
order: 5
---

# LiteLLM proxy

If your services call models through a LiteLLM proxy, add one line to its config. Every model and every service behind the proxy is recorded, with no change to the services.

## Add the callback

Install the Python recorder where the proxy runs, set the four `AP_*` settings in its environment, and add:

```yaml
litellm_settings:
  callbacks: agentpulse.litellm_proxy.handler
```

`AP_SERVICE` sets the name the proxy records as; it is `litellm-proxy` if unset.

## Say who each call is for

The proxy cannot see your services' context, so pass it on each request: LiteLLM's own `user` field, and these keys in its `metadata`:

| Key | What it is |
| --- | --- |
| `agentpulse_request` | Your identifier for the end-user request. |
| `agentpulse_workspace` | Your customer. |
| `agentpulse_project` | A unit of work inside the customer. |
| `agentpulse_user` | The user's identifier, if not in `user`. |
| `agentpulse_user_name`, `agentpulse_user_email` | The user's name and email, stored once. Read only when `agentpulse_user` is set. |
| `agentpulse_session`, `agentpulse_component`, `agentpulse_skill` | Session and the part of your product. |

## Budgets

The proxy checks your budgets before forwarding each call. A stopped call is answered with a `403`, marked not to be retried. A budget set to downgrade sends the call to its cheaper model instead, when that model has the same provider. A warn-only budget past its limit lets the call through and alerts you. If Agent Pulse cannot answer, the proxy forwards the call.

## The LiteLLM SDK

A service using the LiteLLM SDK instead registers the callback once:

```python
litellm.callbacks = [reporter.litellm_callback()]
```

The SDK has no hook that can stop a call. To have a budget stop one, ask before the call, and name the provider unless the model runs on Vertex AI:

```python
verdict = reporter.decide("gpt-4o", provider="OPENAI")
if verdict.proceed:
    response = litellm.completion(model="gpt-4o", messages=messages)
```
