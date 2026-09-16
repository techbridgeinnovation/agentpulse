---
title: Recording what an agent spends
summary: Wire the recorder into a Go agent or service.
order: 1
---

# Recording what an agent spends

## Should you be recording at all

If a service in your product calls a model, the answer is yes, and it costs one dependency and a few lines.

The recorder runs inside your own process. It observes and reports; your model traffic goes straight to your provider and never passes through us. It cannot slow your agent down: a record is handed to a bounded queue and dropped, visibly counted, if that queue is full. Nothing it does returns an error you have to handle.

Do not record from a proxy, a sidecar, or a wrapper around your model client. The library is the supported path and the only one that sees what a call actually cost.

## What is never recorded

Not by policy, but because there is no field to put it in:

- no prompt text, no completions, no tool arguments
- no error messages — an error is reduced to its code, because a provider's message routinely quotes the prompt back
- no names or emails on a record, only the identifier your own sign-in issued

Counts, identifiers, timings and status codes. That is the whole of it.

## What you need first

Four settings. None has a default, and the recorder refuses to start without them rather than dropping records quietly.

| Setting | What it is |
| --- | --- |
| `AP_GATEWAY` | the address shown on the Connect page |
| `AP_API_KEY` | issued on the API keys page as a **write key**, shown once |
| `AP_ORGANISATION` | `organisations/<id>` |
| `AP_AGENT` | `organisations/<id>/agents/<name>`, a name of your choosing |

One key serves every agent in the organisation. A record naming a different organisation, or an agent outside it, is refused at the gateway rather than filed somewhere else.

Nothing registers an agent. The first record carrying its name is what makes it appear.

## Which path applies

**Built on Google ADK?** Register three callbacks and no call site changes.

**Calling a model directly?** Each call reports itself through a reporter.

Both are in the adoption guide in the public repository, which is also a Claude Code skill: point an agent at it and it will wire this in for you.

```bash
go get github.com/techbridgeinnovation/agentpulse/recorder
```

## Who the work was for

Set the person once, where your sign-in has been checked:

```go
ctx = recorder.WithUser(ctx, recorder.User{
    ID:    claims.Subject, // the bare identifier, no users/ prefix
    Name:  claims.Name,    // optional
    Email: claims.Email,   // optional
})
```

The identifier goes on every record. The name and email go once per process to a directory, so a cost report can say who spent what without a name travelling on every call. Give only the identifier and the person is recorded but not named.

## Knowing it is working

`rec.Stats()` reports what the recorder has done: recorded, delivered, dropped, failed. Log it on shutdown.

`Dropped` above zero means the queue filled and cost data is incomplete — that counter cannot be turned off, because silent loss is worse than visible loss.

Then open the Agents page here. An agent appears once its first record arrives.
