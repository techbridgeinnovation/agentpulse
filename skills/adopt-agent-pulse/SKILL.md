---
name: adopt-agent-pulse
description: Wire the Agent Pulse recorder into a Go or Python agent or service so what it spends on models is recorded and attributed. Use when asked to add Agent Pulse, add spend recording, meter an agent, or connect an agent to Agent Pulse.
---

# Adopt Agent Pulse

Agent Pulse records what an agent spends on models and tools, attributed to the organisation, the agent, the request and the person it ran for. The recorder is a library, in Go and in Python, that runs inside the agent's own process. It observes and reports; model traffic never passes through it.

Steps 1 to 5 are the Go library. For a Python agent or service, the settings in "Before you start" are the same and the steps are in "Python" below.

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
- The code calls a model directly with no framework → the **client path**. The model client is instrumented once where it is built, and nothing at a call site changes either.

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

Read all four at startup and refuse to start without them. A default would file spend under somebody else's organisation, and the gateway would refuse it silently.

```go
key := os.Getenv("AP_API_KEY")                  // organisations/<id>/apiKeys/<id>, public
organisation := recorder.OrganisationOfKey(key) // organisations/<id>, read off the key
agent := os.Getenv("AP_AGENT")                  // organisations/<id>/agents/<name>

conn, err := recorder.DialWithKey(os.Getenv("AP_GATEWAY"), key, os.Getenv("AP_API_SECRET"))
if err != nil || organisation == "" || agent == "" {
	log.Fatal("AP_GATEWAY, AP_API_KEY, AP_API_SECRET and AP_AGENT must all be set")
}
```

The key is its name and is public; it carries the organisation, so the organisation is not a separate setting. The secret is the value shown once when the key was issued and is the part that authenticates. The two travel as a pair and resolve only together. The gateway holds every request to the organisation the pair resolves to, so a key name is never a check on its own.

`recorder.DialWithKey` opens one TLS connection to the gateway with the pair attached to every call. It is lazy: nothing is touched on the network until the first record is sent. The same connection serves every client below.

Add all four to the service's deployment configuration next to its other environment variables. The secret is a secret; put it where the service keeps secrets, never in a file that is committed. The key is not, and may sit beside the gateway address.

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

On ADK v1 the same three functions live in `recorder/adkhooks` with `adkhooks.Options`, and there is a fourth: register `adkhooks.BeforeTool` beside `adkhooks.AfterTool` as a `BeforeToolCallback`. It records nothing on its own — it notes when a tool call began, so that the record of it says how long it ran. Without it a tool is still recorded, with no duration, and a timeout reads as an ordinary failure.

## Step 4b: client path (no framework)

Construct a reporter once, beside the recorder:

```go
reporter := rec.For(recorder.Attribution{
	Agent:   agent,
	Service: "<service-name>",
})
```

Then instrument each model client once, where it is built. Every call through it is recorded from then on:

```go
reporter.InstrumentGenAI(geminiClient) // google.golang.org/genai, after genai.NewClient

anthropic.NewClient(option.WithMiddleware(reporter.AnthropicMiddleware()))

openai.NewClient(option.WithMiddleware(reporter.OpenAIMiddleware())) // also Perplexity and other OpenAI-compatible apis
```

Once per request, where the request arrives and the sign-in has been checked, so everything recorded under it groups together and the person can be named:

```go
ctx = recorder.WithUser(recorder.WithRequest(ctx, requestID), recorder.User{
	ID:    claims.Subject, // bare identifier, no users/ prefix
	Name:  claims.Name,    // optional
	Email: claims.Email,   // optional
})
```

Each call is recorded under the function that made it. Where several functions do one job, or every call goes through one shared helper, name the part of the product instead: `ctx = recorder.WithComponent(ctx, "report_generation")`.

Check before finishing:

- Claude through Vertex or Bedrock: set `BilledBy` on the attribution to who bills, and pass `AnthropicMiddleware` before the sdk's `vertex` or `bedrock` option.
- A streamed OpenAI chat reports counts only with `stream_options.include_usage` set on the request.
- A client an ADK agent also uses is not instrumented: its callbacks already record it.
- Once the service has recorded, tell the person to open it from Agents in the console and set **Listed as** to **Service**.

To have budgets stop the service as well as count it, the way an agent's callbacks do:

```go
reporter = reporter.Governed(recorder.Governance{Decider: recorder.NewGRPCDecider(conn)})
```

A call a budget refuses is never sent and returns an error `recorder.Denied(err)` recognises. If governance cannot answer, the call goes ahead.

For a provider none of the clients reach, report the call yourself with the provider's own counts, untouched. Never split them into prompt and cached yourself: Gemini's and OpenAI's prompt counts already include the cached tokens, and splitting them charges those twice.

```go
reporter.ModelCall(ctx, recorder.ModelCall{
	Model:    "<model called>",
	Duration: time.Since(started),
	Err:      err,
	Format:   recorder.FormatAnthropic, // the convention the counts are in
	Reported: map[string]int64{"input_tokens": in, "output_tokens": out},
})
```

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

## Python

The same four settings, from the same environment variables, and the same rules: nothing at a call site waits on recording, nothing about content is recorded, and nothing states a cost.

### The dependency

```bash
pip install "techbridge-agentpulse @ git+https://github.com/techbridgeinnovation/agentpulse@python/v0.1.0#subdirectory=python"
```

It has no dependencies of its own and needs Python 3.10 or later.

### The reporter, once, where the service starts

```python
import agentpulse

reporter = agentpulse.connect(service="<service-name>")
```

`connect` reads `AP_GATEWAY`, `AP_API_KEY`, `AP_API_SECRET` and `AP_AGENT` and raises `agentpulse.ConfigError` at startup if any is missing or the agent is outside the key's organisation. Let that stop the service; do not catch it.

To have budgets stop calls as well as count them, govern it: `reporter = agentpulse.connect(service="<service-name>").governed()`.

### Who the work was for, once per request

Where the request arrives and the sign-in has been checked:

```python
with agentpulse.scope(
    request=request_id,
    user=agentpulse.User(id=claims.subject, name=claims.name, email=claims.email),
    workspace=tenant_id,  # only a product with customers of its own
):
    handle(request)
```

The same rules as Go: `id` is bare, the workspace is the bare tenant identifier, and neither is ever passed at a model call site. The scope follows `await` and `asyncio.to_thread`; wrap a function with `agentpulse.carry` before handing it to a thread or an executor of your own.

### Then one of these

**An agent built on Google ADK.** Register the plugin on the app:

```python
app = App(name="<app>", root_agent=agent, plugins=[reporter.adk_plugin(denied_message="<the words your user reads when a budget refuses a call>")])
```

**Code that calls a model client directly.** Instrument each client once, where it is built:

```python
client = OpenAI(http_client=reporter.openai_http_client())           # AsyncOpenAI: asynchronous=True
client = Anthropic(http_client=reporter.anthropic_http_client())     # AsyncAnthropic: asynchronous=True
client = genai.Client(http_options=reporter.genai_http_options())    # sync and async
```

A framework that builds its model client for you, such as LangChain's `ChatOpenAI`, takes the same `http_client`.

**LiteLLM.** On the proxy, one line in its config:

```yaml
litellm_settings:
  callbacks: agentpulse.litellm_proxy.handler
```

In a service using the LiteLLM SDK: `litellm.callbacks = [reporter.litellm_callback()]`.

**Anything else.** Report the call yourself, with the provider's own counts untouched:

```python
reporter.model_call(agentpulse.ModelCall(
    model="<model called>",
    component="<part of the product>",
    reported=agentpulse.reported_from(response.usage),  # reported_from_genai(...) for Gemini
    format=agentpulse.FORMAT_ANTHROPIC,                 # the convention the counts are in
    duration=elapsed_seconds,
    error=error,                                        # reduced to its code, never its message
))
```

With a governed reporter, ask before a call you make yourself: `if not reporter.decide(model="<model>").proceed:` skip it and answer your user in your own words.

### Serverless

On Cloud Run with request-based billing, or a function that is frozen once it answers, call `reporter.recorder.flush(timeout=2)` before answering. Everywhere else the recorder flushes on its own and at exit.

### Verify

```python
print(reporter.recorder.stats())
```

`delivered` rising after a deploy is the proof. `failed` rising with `delivered` at zero means the gateway refused the records, and the cause is one of the four settings. Tests can build `agentpulse.Recorder(agentpulse.Config())`, which records to nothing.

## Reading it back

Recording and reading are separate keys. The `AP_API_KEY` this skill wires in records and cannot read; a dashboard of the team's own needs a second key, issued on the console's API keys page as **Reads**, and asks the gateway directly over HTTPS. Nothing in the agent changes for it.

Point the team at [`docs/reading-your-spend.md`](../../docs/reading-your-spend.md) and stop there — building their dashboard is not part of adopting the recorder.

## What you never write

- **A cost.** The service says what happened; the server prices it against the rate card in force. Never compute or send a price.
- **Content.** No prompt text, no completions, no tool arguments, no error messages. An error is reduced to its code. A provider error routinely quotes the prompt back.
- **Error handling around recording.** Nothing in the recorder returns an error to handle or blocks a turn. Do not wrap it, retry it, or make the agent wait on it. A full queue drops and counts, and the dropped count cannot be turned off.

## Done when

For Python: the four settings are set and `connect` is left to stop the service without them; one reporter built once; the plugin registered, every model client instrumented, or the LiteLLM callback in place; no call site waits on recording.

For Go:

- The four environment variables are set and the service refuses to start without them.
- One recorder, constructed once, closed on shutdown.
- Either the three callbacks are registered, or every model client is instrumented where it is built.
- The stats line is logged on shutdown.
- No existing call site changed, and nothing the agent does waits on recording.
