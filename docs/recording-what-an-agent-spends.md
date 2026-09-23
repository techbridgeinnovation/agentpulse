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

**Calling a model directly?** Instrument the client once, where it is built, and every call through it is recorded, including the ones written later:

```go
reporter := rec.For(recorder.Attribution{Agent: os.Getenv("AP_AGENT"), Service: "documents-service"})

reporter.InstrumentGenAI(geminiClient)                                   // google.golang.org/genai, Gemini api or Vertex
anthropic.NewClient(option.WithMiddleware(reporter.AnthropicMiddleware())) // github.com/anthropics/anthropic-sdk-go
openai.NewClient(option.WithMiddleware(reporter.OpenAIMiddleware()))       // github.com/openai/openai-go, and Perplexity through it
```

Each call is recorded under the user, workspace and project already on its context, and under the function in your code that made it. Name the part of your product instead with `recorder.WithComponent(ctx, "report_generation")` where several functions do one job or every call goes through one shared helper.

A few things the client cannot see for you:

- Claude served through Vertex or Bedrock is billed by Google or Amazon. Set `BilledBy` on the attribution, and register the middleware before the sdk's `vertex` or `bedrock` option.
- A streamed OpenAI chat reports its token counts only when the request sets `stream_options.include_usage`. Without it the call is recorded with no counts.
- Gemini's Live api does not travel over the client's transport, so it is not recorded.
- Do not instrument a client an ADK agent also uses: the agent's callbacks already record its calls.

Its spend counts against your organisation's and your customers' budgets like any other. To have a budget stop it as well, the way an agent's callbacks do, ask before each call:

```go
reporter = reporter.Governed(recorder.Governance{Decider: recorder.NewGRPCDecider(conn)})
```

A call a budget refuses is never sent, and your code sees an error `recorder.Denied` recognises. If governance cannot answer, the call goes ahead.

Once it has recorded, open it from Agents and set **Listed as** to **Service**, so it is shown apart from your agents. Until then it is recorded exactly the same and listed as an agent.

For a provider none of these reach, a reporter records a call you describe yourself.

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
