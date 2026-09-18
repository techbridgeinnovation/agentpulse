---
name: adopt-agent-pulse
description: Wire the Agent Pulse recorder into a Go agent or service so what it spends on models is recorded and attributed. Use when asked to add Agent Pulse, add spend recording, meter an agent, or connect an agent to Agent Pulse. Go only.
---

# Adopt Agent Pulse

Agent Pulse records what an agent spends on models and tools, attributed to the organisation, the agent, the request and the person it ran for. The recorder is a Go library that runs inside the agent's own process. It observes and reports; model traffic never passes through it.

The whole adoption is one dependency and a few lines. Do not build anything around it.

## Before you start

Ask for, or find in the environment, four settings. Do not invent any of them and do not fall back to a default.

- **Gateway**: the address an agent sends records to, shown on the console's Connect page. For this organisation it is `<gateway address>`.
- **API key**: issued on the console's API keys page for the organisation, shown once, and kept as a secret. One key serves every agent in the organisation.
- **Organisation**: `organisations/<id>`, created in the Agent Pulse console by its administrator.
- **Agent**: `organisations/<id>/agents/<name>`, a name of the team's choosing. Nothing registers it; the first record that carries it is what makes it appear.

The key belongs to one organisation. A record naming a different organisation, or an agent outside it, is refused at the gateway, never filed elsewhere.

Confirm which of the two paths applies:

- The agent is built on Google ADK v2 → the **framework path**. Nothing at a call site changes.
- The code calls a model directly with no framework → the **reporter path**. Each model call reports itself.

## Step 1: the dependency

```bash
go get github.com/techbridgeinnovation/agentpulse/recorder
```

For the framework path, also:

```bash
go get github.com/techbridgeinnovation/agentpulse/recorder/adkv2hooks
```

Every dependency is public. No registry credential is involved.

## Step 2: the settings, from the environment

Read all three at startup and refuse to start without them. A default would file spend under somebody else's organisation, and the gateway would refuse it silently.

```go
key := os.Getenv("AP_API_KEY")
organisation := recorder.OrganisationOfKey(key) // organisations/<id>, read off the key
agent := os.Getenv("AP_AGENT")                  // organisations/<id>/agents/<name>

conn, err := recorder.Dial(os.Getenv("AP_GATEWAY"), key)
if err != nil || organisation == "" || agent == "" {
	log.Fatal("AP_GATEWAY, AP_API_KEY and AP_AGENT must all be set")
}
```

The key carries the organisation it was issued for, so the organisation is not a separate setting. A key issued before keys carried it reads back empty from `OrganisationOfKey`; in that one case fall back to an `AP_ORGANISATION` setting. The gateway holds every request to the organisation it resolves the whole key to, so the prefix is a convenience and never a check.

`recorder.Dial` opens one TLS connection to the gateway with the key attached to every call. It is lazy: nothing is touched on the network until the first record is sent. The same connection serves every client below.

Add all three to the service's deployment configuration next to its other environment variables. The key is a secret; put it where the service keeps secrets, never in a file that is committed.

## Step 3: construct the recorder once

Where the service starts. The sink names the organisation the spend is filed under and sends over the connection from step 2.

```go
rec := recorder.New(recorder.Config{
	Sinks:      []recorder.Sink{recorder.NewGRPCSink(conn, organisation)},
	FlushEvery: 2 * time.Second,
})
defer rec.Close(ctx)
```

Every other field on `recorder.Config` has a working default. Do not tune them.

## Step 4a: framework path (Google ADK v2)

Register three callbacks on the agent. Which request, which user, which session, which model and how many tokens are all already on the framework's own callback context.

```go
recording := adkv2hooks.Options{
	Agent:   agent,
	Service: "<service-name>",
	Model:   "<model the agent is configured with>",
}

agentConfig := llmagent.Config{
	// ...existing fields unchanged...
	BeforeModelCallbacks: []llmagent.BeforeModelCallback{adkv2hooks.BeforeModel(rec, recording)},
	AfterModelCallbacks:  []llmagent.AfterModelCallback{adkv2hooks.AfterModel(rec, recording)},
	AfterToolCallbacks:   []llmagent.AfterToolCallback{adkv2hooks.AfterTool(rec, recording)},
}
```

`Model` is named because a streamed answer arrives in pieces the framework assembles into one summary that carries token counts but not the model. Pricing looks a rate up by model.

The framework's own user id is an identifier and nothing more. To have a cost report say who spent what by name, set the person on the context before the runner is invoked, where the sign-in has been checked; it wins over the framework's id, and the name goes once to the directory rather than on every record:

```go
ctx = recorder.WithUser(ctx, recorder.User{ID: claims.Subject, Name: claims.Name, Email: claims.Email})
```

`ID` is the bare identifier the sign-in issued — `8c21e0b4`, not `users/8c21e0b4` — because it becomes the last segment of the directory row's name. `Name` and `Email` are optional; with only `ID` the person is recorded and not named.

**If the product has customers of its own**, set the customer on the same context, at the same place, and the unit of work inside it if the product tracks one:

```go
ctx = recorder.WithProject(recorder.WithWorkspace(ctx, tenantID), projectID)
```

`tenantID` is the bare identifier of the customer, `acme`, not a resource name; the library builds the name under the organisation the key carries. Create the workspace once when the customer signs up, from the code that creates the customer's own record, with `WorkspacesService.CreateWorkspace` under the organisation. A product with one tenant sets neither: its records are filed under its default workspace and it never names it.

Never pass the workspace or the project as an argument at a model call site. A value a call site can choose is a value that can attribute one customer's spend to another.

On ADK v1 the same three functions live in `recorder/adkhooks` with `adkhooks.Options`.

## Step 4b: reporter path (no framework)

Construct a reporter once, beside the recorder:

```go
reporter := rec.For(recorder.Attribution{
	Agent:    agent,
	Service:  "<service-name>",
	Provider: pb.Activity_VERTEX_AI, // or the provider actually called
})
```

Once per request, where the request arrives and the sign-in has been checked, so everything recorded under it groups together and the person can be named:

```go
ctx = recorder.WithUser(recorder.WithRequest(ctx, requestID), recorder.User{
	ID:    claims.Subject, // bare identifier, no users/ prefix
	Name:  claims.Name,    // optional
	Email: claims.Email,   // optional
})
```

At each model call:

```go
started := time.Now()
resp, err := model.GenerateContent(ctx, contents)

usage := resp.UsageMetadata
reporter.ModelCall(ctx, recorder.ModelCall{
	Model:     "<model called>",
	Component: "<which part of the service made this call>",
	Duration:  time.Since(started),
	Err:       err,
	Tokens: recorder.Tokens{
		Prompt:    usage.PromptTokenCount,
		Candidate: usage.CandidatesTokenCount,
		Cached:    usage.CachedContentTokenCount,
		Reasoning: usage.ThoughtsTokenCount,
	},
})
```

Keep every token kind apart. Cache writes and reasoning are the ones that get forgotten and the ones that cost.

A tool charged per use is recorded too, naming the rate card entry, never a price:

```go
reporter.ToolCall(ctx, recorder.ToolCall{
	Tool:     "<tool name>",
	Duration: time.Since(started),
	Charges:  []recorder.Charge{{PriceableUnit: "priceableUnits/<entry>", Quantity: 1}},
})
```

When the request finishes:

```go
reporter.FinishRequest(requestID)
```

`pb` is `github.com/techbridgeinnovation/agentpulse/recorder/pb/metering`.

## Step 5: verify

Locally, with no connection and no credential, swap the sink:

```go
rec := recorder.New(recorder.Config{
	Sinks:      []recorder.Sink{recorder.Discard()},
	FlushEvery: time.Millisecond,
})
```

On shutdown, read the counters and log them:

```go
s := rec.Stats()
log.Printf("recorded %d, delivered %d, dropped %d, rejected %d, sink panics %d",
	s.Recorded, s.Delivered, s.Dropped, s.Failed, s.Panicked)
```

`Delivered` rising after a deploy is the proof. `Rejected` rising with `Delivered` at zero means the gateway refused the records, and the cause is one of the four settings: a key that is not this organisation's, an organisation or agent name that does not match the key, or a wrong gateway address. Check them against the console's Connect page; do not work around it.

## Reading it back

Recording and reading are separate keys. The `AP_API_KEY` this skill wires in records and cannot read; a dashboard of the team's own needs a second key, issued on the console's API keys page as **Reads**, and asks the gateway directly over HTTPS. Nothing in the agent changes for it.

Point the team at [`docs/reading-your-spend.md`](../../docs/reading-your-spend.md) and stop there — building their dashboard is not part of adopting the recorder.

## What you never write

- **A cost.** The service says what happened; the server prices it against the rate card in force. Never compute or send a price.
- **Content.** No prompt text, no completions, no tool arguments, no error messages. An error is reduced to its code. A provider error routinely quotes the prompt back.
- **Error handling around recording.** Nothing in the recorder returns an error to handle or blocks a turn. Do not wrap it, retry it, or make the agent wait on it. A full queue drops and counts, and the dropped count cannot be turned off.

## Done when

- The four environment variables are set and the service refuses to start without them.
- One recorder, constructed once, closed on shutdown.
- Either the three callbacks are registered, or every model call reports through the reporter.
- The stats line is logged on shutdown.
- No existing call site changed except to add a report, and nothing the agent does waits on recording.
