---
name: adopt-agent-pulse
description: Wire the Agent Pulse recorder into a Go agent or service so what it spends on models is recorded and attributed. Use when asked to add Agent Pulse, add spend recording, meter an agent, or connect an agent to Agent Pulse. Go only.
---

# Adopt Agent Pulse

Agent Pulse records what an agent spends on models and tools, attributed to the organisation, the agent, the request and the person it ran for. The recorder is a Go library that runs inside the agent's own process. It observes and reports; model traffic never passes through it.

The whole adoption is one dependency and a few lines. Do not build anything around it.

## Before you start

Ask for, or find in the environment, two names. Do not invent either and do not fall back to a default.

- **Organisation**: `organisations/<id>`, created in the Agent Pulse console by its administrator.
- **Agent**: `organisations/<id>/agents/<name>`, a name of the team's choosing. Nothing registers it; the first record that carries it is what makes it appear.

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

## Step 2: the names, from the environment

Read both at startup and refuse to start without them. A default would file spend under somebody else's organisation, and metering would refuse it silently.

```go
organisation := os.Getenv("AP_ORGANISATION") // organisations/<id>
agent := os.Getenv("AP_AGENT")               // organisations/<id>/agents/<name>
if organisation == "" || agent == "" {
	log.Fatal("AP_ORGANISATION and AP_AGENT must both be set")
}
```

Add both to the service's deployment configuration next to its other environment variables.

## Step 3: construct the recorder once

Where the service starts. The sink names the organisation the spend is filed under; `conn` is a gRPC connection to metering that presents the agent's own identity.

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

Once per request, where the request arrives, so everything recorded under it groups together:

```go
ctx = recorder.WithUser(recorder.WithRequest(ctx, requestID), userID)
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

`Delivered` rising after a deploy is the proof. `Rejected` rising with `Delivered` at zero means metering refused the identity: the organisation's administrator has to grant the agent's identity the recorder role. Ask for that; do not work around it.

## What you never write

- **A cost.** The service says what happened; the server prices it against the rate card in force. Never compute or send a price.
- **Content.** No prompt text, no completions, no tool arguments, no error messages. An error is reduced to its code. A provider error routinely quotes the prompt back.
- **Error handling around recording.** Nothing in the recorder returns an error to handle or blocks a turn. Do not wrap it, retry it, or make the agent wait on it. A full queue drops and counts, and the dropped count cannot be turned off.

## Done when

- The two environment variables are set and the service refuses to start without them.
- One recorder, constructed once, closed on shutdown.
- Either the three callbacks are registered, or every model call reports through the reporter.
- The stats line is logged on shutdown.
- No existing call site changed except to add a report, and nothing the agent does waits on recording.
