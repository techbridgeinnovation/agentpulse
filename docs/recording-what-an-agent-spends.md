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
| `AP_API_KEY` | the key's name, `organisations/<id>/apiKeys/<id>`, shown on the API keys page |
| `AP_API_SECRET` | issued with the key, with **Write** ticked, shown once |
| `AP_AGENT` | `organisations/<id>/agents/<name>`, a name of your choosing |

The key is public and names the organisation it belongs to, so `recorder.OrganisationOfKey` reads the organisation off it and it is not a separate setting. The secret is the part that authenticates. The two are presented together and resolve only together.

One key serves every agent in the organisation. A record naming a different organisation, or an agent outside it, is refused at the gateway rather than filed somewhere else. The gateway decides that from what the pair resolves to, never from what the key's name claims.

Nothing registers an agent. The first record carrying its name is what makes it appear.

## Which customer the work was for

An organisation is who holds the plan. A workspace is whose the spend is: one per customer of a product that has customers, or one called `default` that a single-tenant product never names.

If your product has one tenant, set nothing. Every record is filed under your default workspace and every read of your organisation sees it.

If your product has customers of its own, set the workspace on the request in the same place you set the user, and the project if you track one:

```go
ctx = recorder.WithProject(recorder.WithWorkspace(ctx, "acme"), "matter-1183")
```

The workspace is the bare identifier, `acme`, not a full resource name. Create it once when the customer signs up, from the same code that creates the customer's own record. From then on every call in that request is filed under it, a read of `organisations/<id>/workspaces/acme` sees only that customer, and a read of `organisations/<id>` grouped by `workspace` is one row per customer.

Neither is an argument at a model call site. A value a call site can choose is a value that can attribute one customer's spend to another.

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
