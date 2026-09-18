# recorder/v1

## Service Overview

**Purpose.** The recorder is a library that runs inside each agent's own process rather than a service. It observes what the agent spent and hands it to whichever destination the consuming team chose. It is the whole of Agent Pulse another team installs, and the reason adoption is one dependency and two lines.

It observes rather than carries. Model traffic goes straight from the agent to its provider and never passes through here.

**The first rule.** It must never degrade its host. Recording never blocks, never returns an error and never panics. A full queue drops records and counts them rather than waiting. A destination that fails, hangs or panics is isolated from the others and from the caller. That ranks above completeness of data: telemetry that can break production gets removed, and rightly.

Dropped records are counted and the count is readable. That counter is the one thing here that cannot be turned off, because silent loss is worse than visible loss.

## Ownership & Contact

| | |
| --- | --- |
| Owner | Moses Otieno — moses@techbridgeinnovation.co |
| Product | Agent Pulse (`techbridge.ap`) |
| Consumers | rezco (Atlas, deep research), dealade (Russell), voyage (Nexus) |

## Dependencies

**Calls out to** `metering/v1` for records and the rate card, and `governance/v1` for spend decisions, one call per batch rather than one per record. An agent inside this product reaches them directly; every other agent reaches them through `gateway/v1`, which is the only one of the three on the public internet, and presents an api key on every call. Nothing else, deliberately: a team can configure a log file or nothing at all and still take the library.

**Called by** each agent's own process, in line.

**Publishes and listens to** nothing.

## Wiring it in

Four settings, read from the environment and refused at startup when missing. The key and the secret are issued together in the console: the key is the key's name, public, and carries the organisation the spend is filed under; the secret is shown once. The agent is a name of the team's choosing under the organisation. A default for any of them would file a stranger's spend under the wrong organisation, and metering would refuse it quietly.

```go
key := os.Getenv("AP_API_KEY")                  // organisations/<id>/apiKeys/<id>
organisation := recorder.OrganisationOfKey(key) // organisations/<id>, read off the key
agent := os.Getenv("AP_AGENT")                  // organisations/<id>/agents/<name>

conn, err := recorder.DialWithKey(os.Getenv("AP_GATEWAY"), key, os.Getenv("AP_API_SECRET"))
if err != nil || organisation == "" || agent == "" {
    log.Fatal("AP_GATEWAY, AP_API_KEY, AP_API_SECRET and AP_AGENT must all be set")
}
```

The key and the secret travel as a pair and resolve only together: the right secret against another key's name is refused exactly as a wrong secret is. Nothing about scope rests on the key's name; the gateway holds every request to the organisation the pair resolves to, so a name edited to claim another tenant is refused rather than believed.

A key issued before keys were presented as a pair is a secret alone. `Dial(gateway, secret)` still sends it, and the organisation is set beside it as `AP_ORGANISATION`, until the key is rotated.

`Dial` opens one TLS connection to the gateway with the key attached to every call, and it is lazy: nothing is touched on the network until the first record is sent. The same connection serves the sink, the rate source and the decider.

Two kinds of code spend money on models, and they cannot report the same way.

**An agent built on Google's ADK.** The framework offers moments to hook into, so the recorder is registered once and no call site changes.

```go
rec := recorder.New(recorder.Config{
    Sinks: []recorder.Sink{recorder.NewGRPCSink(conn, organisation)},
})

opts := adkhooks.Options{Agent: agentName, Service: "atlas-agent"}
// register adkhooks.BeforeModel, adkhooks.AfterModel, adkhooks.AfterTool and adkhooks.AfterAgent on the agent
```

Every field on the record comes from the framework's own callback context: which request, which user, which session, which agent, which model, how many tokens, whether it succeeded. A user set on the context with `recorder.WithUser` before the runner is invoked wins over the framework's own user id, name and all. Which tenant the turn is for, and which unit of work inside it, are read from the same context — the framework knows neither.

`adkhooks` is the first major version of the framework and `adkv2hooks` the second. They are different libraries as far as Go is concerned, with unrelated types, so one package cannot serve both. The callbacks and everything they read are the same.

`AfterAgent` releases the request's running total when the turn ends, and belongs on the agent that owns the turn. On a sub-agent it fires when that sub-agent finishes, while the turn it was delegated from is still running.

**Ordinary service code that calls a model inline.** There is no framework, so there are no moments and nothing can observe on the service's behalf. It reports for itself through a reporter, which holds what does not change so a call site names only what does.

```go
reporter := rec.For(recorder.Attribution{
    Agent:    agentName,
    Service:  "sources-service",
    Provider: pb.Activity_VERTEX_AI,
})

// once per request, where the sign-in has been checked
ctx = recorder.WithUser(recorder.WithRequest(ctx, requestID), recorder.User{
    ID:    claims.Subject,
    Name:  claims.Name,
    Email: claims.Email,
})

// at each model call
reporter.ModelCall(ctx, recorder.ModelCall{
    Model:     "gemini-2.5-pro",
    Component: "asset_summary",
    Duration:  time.Since(started),
    Tokens:    recorder.Tokens{Prompt: usage.PromptTokenCount, Candidate: usage.CandidatesTokenCount},
})
```

`Example` in `example_test.go` is the whole of this path as a runnable program, and it runs in the ordinary suite, so it cannot rot.

Neither path states a cost. A service says what happened and the server prices it, which is what stops an agent asserting what its own work was worth.

## The running total

A spend ceiling has to be readable before a model call, and a network call per model call to read it is not affordable. So the recorder keeps what each in-flight request has spent, priced here, against a rate card it fetches in the background.

```go
rec := recorder.New(recorder.Config{
    Sinks: []recorder.Sink{recorder.NewGRPCSink(conn, organisation)},
    Rates: recorder.NewGRPCRateSource(conn),
})

spent := rec.SpentOn(requestID) // millionths of a dollar
```

`Rates` is optional. Without it every record contributes nothing and `SpentOn` answers zero.

The figure is exact for what this process did and blind to what another replica spent on the same request, so it is a per-request ceiling and never an organisation's budget. That is the other half of the enforcement story, and it is the budget envelope governance hands out.

**What the reporter does that a hand-built record does not.** It keeps each kind of token apart, including the reasoning and cache-write counts that are easy to miss and expensive to miss. It reduces an error to its code and never carries the message, because a provider error routinely quotes the prompt back. It separates a call that was truncated from one that failed, since the first was charged for partial work and the second may have been charged for nothing.

`component` is asked for on this path rather than derived. Nothing can infer which part of a service made a call, and one bucket per service answers no question worth asking.

## Who the work was for

A record carries an identifier and nothing else about the person, because a record is one row per model call and a report over it is read by people who are not that person. The name behind the identifier goes to a directory instead, once per person, and a report is joined to it afterwards.

`recorder.User` is where the two are given together. The identifier is whatever the product's own sign-in issued, bare — `8c21e0b4`, not `users/8c21e0b4` — because it becomes the last segment of the directory row's name and the exact value on every record, and that equality is the join. The name and email are optional; a product that gives only the identifier records exactly as before and names nobody.

Naming takes the same path a record does: a bounded queue, a batch, a sink on the worker, dropped and counted when the queue is full. What is remembered is what was last sent, so a person seen again with the same name costs a map lookup. A changed name is sent again. A send that fails forgets the person, so the next call they make tries again rather than waiting for a restart. `Named`, `NamesDropped` and `NamesFailed` in `Stats` say what happened; `NamesFailed` above zero means a report is showing an identifier where a name was given, and an identifier with a slash in it lands there too.

A sink takes names only if it implements `UserSink`. The metering sink does; a log or a collector has no directory to put a name in and is given none.

## Which tenant the work was for

One process bills one organisation, and that is configured once where the sink is built. One process can serve many tenants of that organisation, so the tenant is per request rather than per process: it is set on the request context, beside the person, where the product checks its own sign-in.

```go
// once per request, in the same place the user is set
ctx = recorder.WithProject(recorder.WithWorkspace(ctx, "acme"), "matter-1183")
```

The workspace is the bare identifier — `acme`, not `organisations/dealade/workspaces/acme` — because the organisation is already configured and the library builds the name. The project is a unit of work inside the workspace, a label on every record and nothing more: nothing is authorised against it, and everyone who can read the workspace can read every project in it.

Neither is an argument at a model call site. A value a call site can choose is a value that can attribute one tenant's spend to another, and the resulting figure looks exactly like a correct one.

A product with one tenant sets nothing. Its records name no workspace, they are filed under the organisation, and that is the whole of what it has to do.

A flush that covers several tenants goes out as one batch each, because a record's tenant is the parent its batch was written under and one batch has one parent. The batches are independent: a refusal for one tenant leaves the others sent, and `Delivered` and `Failed` go on counting records rather than batches. The directory of names is split the same way, into the workspace whose records carry the identifier, since that equality is the join.

## Layout

```
recorder/v1/
├── recorder.go                 the queue, the worker, the counters
├── config.go                   every field has a working default
├── report.go                   the way in for code that is not an agent
├── context.go                  who a piece of work is for, carried on the context
├── names.go                    the name behind an identifier, sent once per person
├── sink.go                     where records go
├── sink_grpc.go                the metering destination
├── gateway.go                  the connection through the gateway, key on every call
├── decider.go                  asking governance whether a call may proceed
├── decider_cache.go            verdicts reused from memory
├── decider_grpc.go             the governance destination
├── invalidation_subscriber.go  governance pushing a changed verdict in
├── rates.go                    where prices come from
├── rates_cache.go              the card in force, swapped wholesale
├── rates_grpc.go               the metering rate card
├── pricing.go                  tokens and charges into money
├── total.go                    per-request running total, no network call
├── adkhooks/                   the framework callbacks, first ADK major
└── adkv2hooks/                 the same callbacks, second ADK major
```

## Before changing it

**A missing setting fails at startup, never later.** `Dial` refuses an empty address or key, and `NewGRPCSink` refuses an empty organisation. Anything else would be every record rejected and counted for the life of the process, which is the one kind of failure nobody is watching for.

**The queue cannot be made unbounded.** An unbounded queue turns a slow destination into the host agent's memory leak, so there is no option for it.

**A dropped record still counts towards the running total.** It cost money whether or not the record survived, and leaving it out under-reports spend in the one direction that matters for a spend limit.

**The local price and the stored price must agree.** `pricing.go` is the same arithmetic `metering/v1/internal/pricing` runs server-side, down to the rounding, and the tests assert the same figures that suite does. That package is internal to its module and cannot be imported, so the agreement is held by those numbers. Two figures for the same work that disagree are worse than one figure and a gap.

**Rates are fetched in the background and never on the path of a model call.** A rate source that is slow, unreachable or broken delays nothing an agent is doing. What it costs is a card that is stale or absent, and a record priced against no card costs nothing, which is where the running total started. Degrading to that is the correct failure mode; `RateFetchErrors` is what stops it being a silent one.

**The running total cannot grow without bound.** An entry expires on its own and the map has a hard cap, neither of them configurable, so a caller who never calls `FinishRequest` costs a bounded amount of memory rather than a leak. A library running inside someone else's agent does not get to depend on its adopter remembering something. Calling `FinishRequest` still matters: it keeps the map to the requests actually in flight, and an eviction under the cap is an undercount for a request that is still running.

## Running it

```bash
go test -race ./...
```

The race detector matters here, because this is concurrent code running inside someone else's service.
