# recorder

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

**Calls out to** `metering/v1`, the default destination, one call per batch rather than one per record. Nothing else, deliberately: a team can configure a log file or nothing at all and still take the library.

**Called by** each agent's own process, in line.

**Publishes and listens to** nothing.

## Installing it

```bash
go get github.com/techbridgeinnovation/agentpulse/recorder
```

Agents on Google ADK v2 also take the callbacks module, which is separate because the two ADK majors are unrelated types:

```bash
go get github.com/techbridgeinnovation/agentpulse/recorder/adkv2hooks
```

Every dependency is public. The metering and governance contracts the recorder speaks are checked in under `pb/` as generated Go, so no private registry is involved.

## Wiring it in

Two kinds of code spend money on models, and they cannot report the same way.

**An agent built on Google's ADK.** The framework offers moments to hook into, so the recorder is registered once and no call site changes.

```go
rec := recorder.New(recorder.Config{
    Sinks: []recorder.Sink{recorder.NewGRPCSink(conn, "organisations/techbridge")},
})

opts := adkhooks.Options{Agent: agentName, Service: "atlas-agent"}
// register adkhooks.BeforeModel, adkhooks.AfterModel, adkhooks.AfterTool and adkhooks.AfterAgent on the agent
```

Every field on the record comes from the framework's own callback context: which request, which user, which session, which agent, which model, how many tokens, whether it succeeded.

`adkhooks` is the first major version of the framework and `adkv2hooks` the second. They are different libraries as far as Go is concerned, with unrelated types, so one package cannot serve both. The callbacks and everything they read are the same.

`AfterAgent` releases the request's running total when the turn ends, and belongs on the agent that owns the turn. On a sub-agent it fires when that sub-agent finishes, while the turn it was delegated from is still running.

**Ordinary service code that calls a model inline.** There is no framework, so there are no moments and nothing can observe on the service's behalf. It reports for itself through a reporter, which holds what does not change so a call site names only what does.

```go
reporter := rec.For(recorder.Attribution{
    Agent:    agentName,
    Service:  "sources-service",
    Provider: pb.Activity_VERTEX_AI,
})

// once per request
ctx = recorder.WithUser(recorder.WithRequest(ctx, requestID), userID)

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
    Sinks: []recorder.Sink{recorder.NewGRPCSink(conn, "organisations/techbridge")},
    Rates: recorder.NewGRPCRateSource(conn),
})

spent := rec.SpentOn(requestID) // millionths of a dollar
```

`Rates` is optional. Without it every record contributes nothing and `SpentOn` answers zero.

The figure is exact for what this process did and blind to what another replica spent on the same request, so it is a per-request ceiling and never an organisation's budget. That is the other half of the enforcement story, and it is the budget envelope governance hands out.

**What the reporter does that a hand-built record does not.** It keeps each kind of token apart, including the reasoning and cache-write counts that are easy to miss and expensive to miss. It reduces an error to its code and never carries the message, because a provider error routinely quotes the prompt back. It separates a call that was truncated from one that failed, since the first was charged for partial work and the second may have been charged for nothing.

`component` is asked for on this path rather than derived. Nothing can infer which part of a service made a call, and one bucket per service answers no question worth asking.

## Layout

```
recorder/
├── recorder.go                 the queue, the worker, the counters
├── config.go                   every field has a working default
├── report.go                   the way in for code that is not an agent
├── context.go                  who a piece of work is for, carried on the context
├── sink.go                     where records go
├── sink_grpc.go                the metering destination
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
├── adkv2hooks/                 the same callbacks, second ADK major
└── pb/                         the metering and governance contracts, generated
```

## The contracts under pb/

`pb/metering` and `pb/governance` are the generated Go for the two Agent Pulse contracts, copied in verbatim. `pb/VERSIONS` names the contract release each copy came from. `scripts/sync-contracts.sh` refreshes them from a machine that can resolve the private modules; nobody edits them by hand.

The proto package names inside them are `techbridge.ap.metering.v1` and `techbridge.ap.governance.v1`. Go's protobuf runtime allows one registration per proto name in a binary, so a program must not import these alongside the same contracts from another module path. A TechBridge service that already depends on `alis.build/techbridge/ap/metering` takes its metering types from here instead.

## Before changing it

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
