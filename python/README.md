# recorder/python

## Service Overview

**Purpose.** The Python recorder is a library that runs inside an agent's own process, the Python counterpart of `recorder/v1`. It observes what a service spent on models and tools and hands it to Agent Pulse through the gateway. It observes rather than carries: model traffic goes straight from the agent to its provider and never passes through here.

**The first rule.** It must never degrade its host. Recording never blocks, never raises and never waits on the network. A full queue drops records and counts them rather than waiting. A destination that fails or hangs is isolated from the others and from the caller. That ranks above completeness of data.

Dropped records are counted and the count is readable in `stats()`. That counter cannot be turned off, because silent loss is worse than visible loss. The metering sink also carries the losses on the next batch it delivers, as counts of records dropped, records not delivered after every retry, recorder faults and calls it could not read, so a loss shows where spend is read and not only in this process. A count is taken as reported only once that batch is accepted, so a failed batch passes it to the next.

**It has no dependencies.** Everything it depends on would run inside somebody else's agent, so it speaks to the gateway with the standard library alone: gRPC-web over `http.client`, with the few protobuf messages it sends encoded by hand.

## Ownership & Contact

| | |
| --- | --- |
| Owner | Moses Otieno — moses@techbridgeinnovation.co |
| Product | Agent Pulse (`techbridge.ap`) |
| Package | `techbridge-agentpulse`, imported as `agentpulse` |

## Dependencies

**Calls out to** `gateway/v1`, which forwards to `metering/v1` for records, names and the rate card, one call per batch and never one per record, and to `governance/v1` for spend decisions. Every call carries the api key and its secret.

**Called by** each Python service's own process, in line.

**Publishes and listens to** nothing.

## Wiring it in

Four settings, read from the environment and refused at startup when missing, because a wrong one should fail where a person is watching rather than every record being refused quietly for the life of the process.

| Variable | What it is |
| --- | --- |
| `AP_GATEWAY` | The gateway's address. A bare host, a URL, or a host and port. |
| `AP_API_KEY` | The key's name, `organisations/<id>/apiKeys/<id>`. It carries the organisation the spend is filed under. |
| `AP_API_SECRET` | The secret shown once when the key was issued. |
| `AP_AGENT` | `organisations/<id>/agents/<name>`, under the key's organisation, a name of the team's choosing. |

```python
import agentpulse

reporter = agentpulse.connect(service="sources-service")
```

Once per request, where the sign-in has been checked, say who the work is for:

```python
with agentpulse.scope(
    request=request_id,
    user=agentpulse.User(id=claims.subject, name=claims.name, email=claims.email),
    workspace="acme",
):
    handle(request)
```

At each model call, hand over what the provider said, under its own names:

```python
reporter.model_call(agentpulse.ModelCall(
    model="gemini-2.5-pro",
    component="asset_summary",
    reported=agentpulse.reported_from_genai(response.usage_metadata),
    format=agentpulse.FORMAT_VERTEX,
    duration=time.monotonic() - started,
))
```

`reported_from_genai` reads the `google-genai` sdk's usage object or a REST reply's `usageMetadata`. `reported_from` reads OpenAI's and Anthropic's usage objects, or any usage dictionary, under the JSON's own names. Neither adds, subtracts or renames anything: the split into priced classes happens on the server, in one place that can be corrected and reapplied to records already written.

A failure is passed as `error=` and reduced to its code. Its message is never recorded, because a provider's error text routinely quotes the prompt back.

`tool_call` records a tool, which matters where a search or a lookup is charged per use.

## An agent built on Google's Agent Development Kit

The framework offers moments to hook into, so a plugin is registered once and no call site changes:

```python
reporter = agentpulse.connect(service="research-agent").governed()

app = App(name="research", root_agent=agent, plugins=[
    reporter.adk_plugin(denied_message="You have used this month's research credits."),
])
```

It records one activity per model call and one per tool call. The turn, the session, the user and the agent come from the framework's own context; a request or person set with `scope` wins, since it is what the product's sign-in put there. A streamed call is recorded once, from its final response, keeping the model its chunks reported, which the framework drops from the response it assembles. A tool that reports its own failure in its result rather than raising is judged by `tool_failed(tool, result) -> (failed, code)`, because only the product knows the shape of its tools' answers. What a tool handed back is measured in `result_bytes`, from the JSON it is sent to the model as, and never kept.

A call is recorded once however other plugins answer it. The framework stops at the first plugin that answers a model or tool error, so the plugin also watches the plugin manager's error dispatch, and the answer some plugin or callback gives for a failure is not recorded as a second, successful call. A failure part way through a stream keeps the counts and the model its chunks had reported. A call the framework never finishes, because the run was cancelled or its reader stopped reading, is recorded as `TRUNCATED` with the code `Canceled` and the counts it had reached, when the task that made it ends, when its run ends, when the next call in the same place begins, or when the runner closes the plugin. A run nested inside a tool releases the running total only of a request it opened itself. An agent run as a tool with `include_plugins=False` runs without this plugin, and its calls are not recorded.

On a governed reporter each model call asks governance first. A DENY returns `denied_message` as the model's reply, so the framework never calls the model and the user reads the product's own words; a DOWNGRADE changes the model the framework sends, unless the replacement is another provider's.

Labels naming the user and the component are added to the request only where the model is served through Vertex, which carries them into the billing export. The Gemini Developer API refuses a request carrying labels, so none are added there.

Every hook catches everything it raises and counts it as `panicked`. The framework turns an exception from a plugin into a failure of the whole run, so a hook that let one escape could break the agent it observes.

## LiteLLM, as a proxy or an SDK

A team running the LiteLLM proxy adds one line to its config, and every model and every service behind the proxy is recorded with no application change:

```yaml
litellm_settings:
  callbacks: agentpulse.litellm_proxy.handler
```

The handler is built from the four `AP_*` settings when the proxy loads it, and `AP_SERVICE` names what the proxy records as. It is governed: before a call is forwarded it asks governance, a DENY answers the client with a 403 marked not to be retried, and a DOWNGRADE forwards the replacement model when it is the same provider's. A proxy with no budgets configured is answered ALLOW and forwards everything, and a governance that cannot answer forwards everything too.

A service using the LiteLLM SDK registers `litellm.callbacks = [reporter.litellm_callback()]`. The SDK has no hook that can stop a call, so a service that needs a budget to stop one asks `reporter.decide()` first.

LiteLLM restates every provider's usage in OpenAI's shape, so the provider's own counts never reach this. They are recorded in LiteLLM's convention, `LITELLM`, which differs from OpenAI's in one place that matters: the input count holds both the cache read and the cache write, and both come out of it before either is priced. Who served the call is LiteLLM's to say, since it routes each one.

A proxy holds none of its callers' context, so who a call was for travels on the request: LiteLLM's own `user` field, and `agentpulse_request`, `agentpulse_session`, `agentpulse_workspace`, `agentpulse_project`, `agentpulse_component`, `agentpulse_skill`, `agentpulse_user`, `agentpulse_user_name` and `agentpulse_user_email` in its `metadata`. An SDK call made inside `scope` records what the scope named: LiteLLM reports a call's outcome from a thread of its own, so the scope is captured when the call starts. LiteLLM reads its callback list once, when the first call is made.

## LangChain and LangGraph

One handler, passed on the outermost call, covers every model call and tool call the run makes, an agent called from inside another agent's tool included:

```python
reporter = agentpulse.connect(service="research-agent").governed()

agent.invoke(inputs, config={"callbacks": [reporter.langchain_handler()], "configurable": {"thread_id": conversation_id}})
```

Each call is filed under the agent LangChain names for it (`create_agent(name=...)`), the LangGraph thread as the session, and the outermost run as the request, unless `agentpulse.scope` names them. A graph built without an agent is filed under the node the call ran in.

LangChain restates every chat model's usage as `usage_metadata`, and Gemini's reaches a handler in no other form, so the counts are recorded in LangChain's convention, `LANGCHAIN`: the input count holds both the cache read and the cache write, and the output count holds the reasoning. Who served the call is LangChain's `ls_provider`. A completion model has no message to carry them and states the provider's own counts on its result, which are recorded as `OPENAI_CHAT` where they are in OpenAI's shape and unread otherwise. A stream that fails part way is recorded with the counts it had reached, and a call cancelled by its caller as `TRUNCATED` with the code `Canceled`. What a tool handed back is measured in `result_bytes`, never kept.

A governed handler asks before each model call and refuses one by raising `SpendDenied`, so the call is never sent. A handler cannot change the model a call uses, so a DOWNGRADE lets the call through as asked and is counted in `stats()` as not applied.

## OpenAI Agents SDK

Run hooks, passed to the run and to every agent run as a tool:

```python
hooks = reporter.openai_agents_hooks()

researcher_tool = researcher.as_tool(tool_name="ask_researcher", tool_description="...", hooks=hooks)
result = await Runner.run(agent, prompt, hooks=hooks)
```

An agent run as another agent's tool is a run of its own and gets the hooks only where `as_tool` is given them. Each call is filed under the agent that made it, the run's trace as the request, and the trace's `group_id` as the session.

The SDK restates every model's usage in the shape of OpenAI's Responses api, recorded as `OPENAI_RESPONSES`, and names the model the agent asked for rather than the version that answered. A model routed through LiteLLM, `litellm/<provider>/<model>`, is billed under that provider.

The SDK does not show hooks the run's configuration, so a model set on `RunConfig` is read only where the same configuration is given to the hooks, `reporter.openai_agents_hooks(run_config=config)`. What a tool handed back is measured in `result_bytes`, never kept.

A governed reporter's hooks refuse a call by raising `SpendDenied` before it is sent. A DOWNGRADE lets the call through as asked and is counted as not applied.

The SDK tells hooks nothing about a model call that fails, and its exception reaches the caller as usual. A call whose start was seen and whose end never came is recorded as `FAILED` with the code `Unknown`, since no code was shown, when the run's trace ends, through a trace processor the hooks add once per process. Where tracing is off, or the product has replaced the SDK's trace processors since the hooks were built, it is recorded when the run's context is released instead, which waits on the garbage collector and on anything still holding the run, such as a log record of its exception. A cancelled run is recorded the same way, because the hooks cannot tell it from a failure.

## Recording from the client

A line after every model call is a line some call site forgets. The other way is to record from the client, set once where it is built, so every call through it is recorded, the ones written later included:

```python
client = OpenAI(http_client=reporter.openai_http_client())
client = AsyncOpenAI(http_client=reporter.openai_http_client(asynchronous=True))
client = Anthropic(http_client=reporter.anthropic_http_client())
client = genai.Client(http_options=reporter.genai_http_options())
```

Each sits in the client's HTTP transport and recognises a model call by its URL: Chat Completions and Responses, Messages native and through Vertex, and generate calls on the Gemini api and Vertex. Anything else the client does passes through unrecorded and is counted in `stats().unrecognised`, so a model endpoint this version does not know shows up as a number. What is read is the reply as the caller reads it, and only the model, the counts, the finish and the failure; every byte reaches the caller as it arrived. The part of the product that made the call is the component named on the context, or else the first function outside this library, the sdks and their transports.

The HTTP client is built from the sdk's own default client, so the sdk's timeouts and connection limits still apply. For Gemini, giving the async transport is also what keeps the sdk on httpx rather than switching to aiohttp, which this could not see. `reporter.transport(inner)` wraps any other httpx or httpx2 transport the same way.

A reply compressed with gzip or deflate is read; one compressed with brotli or zstd is read where the `brotli`, `brotlicffi` or `zstandard` package is installed, or on Python 3.14 for zstd. Any other is delivered untouched and recorded without its counts, and a refusal is a failure whether its body can be read or not. A reply the caller stops reading before it says it is over, by closing it or leaving it unread, is recorded as `TRUNCATED` with the code `Canceled` and the counts it had reached. A streamed chat reports its counts only when the request asks for them with `stream_options={"include_usage": True}`; this does not add that to a request, because it changes the stream the caller reads. A call routed through a proxy the HTTP client mounts from the environment does not pass through this transport.

On a governed reporter every call through the client asks governance first. A refused call is never sent: an OpenAI or Anthropic client raises its own permission error, marked not to be retried, and a Gemini client raises `SpendDenied`, and `agentpulse.denied(err)` recognises all three. A DOWNGRADE is applied to the outbound call by rewriting the model where the request names it, in the path or in the JSON body and nothing else in the body, unless the replacement is served by another provider, in which case the call goes ahead as asked. `stats()` counts both in `downgrade_applied` and `downgrade_not_applied`.

## Letting a budget stop a call

A governed reporter asks governance before a call is made:

```python
reporter = agentpulse.connect(service="sources-service").governed()

verdict = reporter.decide(model="gemini-2.5-pro")
if not verdict.proceed:
    return declined_reply()
model = verdict.replacement_model or "gemini-2.5-pro"
```

Only a genuine DENY refuses. The refusal is recorded by `decide` itself, as refused and costing nothing, so refusals are countable; the caller skips the call and answers its own user in its own words. A DOWNGRADE names a replacement for the caller to use. Every other answer, and no answer at all, lets the call go ahead: governance unreachable, slow, refusing the question, or saying something this version does not know. A spend decision that cannot be made must not be the reason the product stops working. `stats()` counts each case in `notified`, `denied`, `downgraded` and `decision_errors`.

A verdict is reused for `cache_ttl` seconds, 30 by default, for exactly the same question: the same person, tenant, project, agent, provider and model. That is how far past a ceiling spend can run before it is stopped, times the number of processes running, and a smaller value buys a tighter ceiling with more calls to governance. Concurrent calls asking the same question share one request. A failure is never cached as an answer. A question governance failed to answer, or answered too slowly, fails at once for the next `failure_backoff` seconds, 5 by default, and is then asked again, so an outage slows no call past the first rather than every call by the whole timeout. The cache holds at most 4,096 verdicts.

A governed reporter asking governance through the gateway also follows its stream of changed budgets, started by the first decision, and drops the verdicts a change covers at once rather than when they expire. It clears every tenant's verdicts within the changed scope, which errs towards asking again. The stream is an improvement on the cache's own lifetime and never a guarantee: it reopens on its own when it ends, backs off while governance is unreachable, and without it a changed budget still arrives within `cache_ttl`. `follow_changes=False` turns it off.

A call waits at most `timeout` seconds, a second and a half by default, for an answer, and a call governance refused within the hour is refused again when no answer comes in time. That wait is enforced by no longer waiting rather than by socket timeouts, which bound each read and write on their own and not the whole exchange. Governance through the gateway opens its connections in the background when the reporter is governed, so the first decision on a new instance is not spent on a handshake; nothing waits on them, and one that fails is opened again by the next call.

## The running total

A spend ceiling has to be readable before a model call, and a network call per model call to read it is not affordable. So the recorder keeps what each in-flight request has spent, priced here against the rate card, which is read through the gateway in the background every 15 minutes and never on the path of a model call:

```python
spent = reporter.spent_on(request_id)  # millionths of a dollar
reporter.finish_request(request_id)     # when the request is done
```

`connect` reads the card by default; `Config(rates=...)` supplies another source, and a recorder with none answers zero. The agent kit plugin releases a turn's total when the run ends.

The arithmetic is the server's, ported: the same reader per reporting convention, the same choice of one rate per kind, the most specific that applies, and the same rounding. It is priced from the provider's own counts whichever way a call was recorded. The figure is exact for this process and blind to other processes spending on the same request, so it is a per-request ceiling and never an organisation's budget. It is never written onto a record: what is stored and billed is what metering prices.

A record dropped from a full queue still counts, because it cost money whether or not the record survived. A total expires on its own after 15 minutes and at most 4,096 are kept; an eviction under that cap is an undercount for a request still running, counted in `totals_evicted`. A card that cannot be read leaves the one in hand, counted in `rate_fetch_errors`.

## Who and which tenant

The request, the person, the session, the workspace, the project, the component and the skill are context variables set with `scope`, never arguments at a model call site. A component set there wins over the name a framework gives, such as the agent's, and a skill set there wins over the reporter's own; a tool record keeps `tool:<name>` as its component. A value a call site can choose is a value that can attribute one tenant's spend to another, and a figure wrong that way looks exactly like one that is right.

A record carries the person's bare identifier and nothing else about them. An identifier shaped like an email address, from the framework or from `scope`, is recorded as no user, never sent to the directory, and counted in `stats().email_users_refused`. The name and email go once per person to a directory, and again when they change, in the workspace whose records carry the identifier.

Context variables follow the work across `await` and into `asyncio.to_thread`. They do not follow it into a thread started by hand or a plain executor: wrap the function with `agentpulse.carry` before handing it over.

## Layout

```
recorder/python/
├── pyproject.toml
├── src/agentpulse/
│   ├── __init__.py      connect, and the public names
│   ├── recorder.py      the queue, the worker, the counters, fork and exit handling
│   ├── report.py        Attribution, ModelCall, ToolCall, Reporter
│   ├── clients.py       recording from an OpenAI, Anthropic or Gemini client's transport
│   ├── adk.py           the plugin for Google's Agent Development Kit
│   ├── litellm.py       the callback for LiteLLM's SDK and proxy
│   ├── litellm_proxy.py the proxy's handler, built from the environment
│   ├── langchain.py     the callback handler for LangChain and LangGraph
│   ├── openai_agents.py the run hooks for the OpenAI Agents SDK
│   ├── context.py       scope, carry, and who a piece of work is for
│   ├── sinks.py         Sink, LossSink, Discard, MeteringSink
│   ├── governance.py    asking whether a call may proceed, and verdicts reused from memory
│   ├── invalidations.py governance saying a budget changed, applied to the cache
│   ├── pricing.py       the rate card, the server's pricing arithmetic, per-request running totals
│   ├── gateway.py       the key, the secret and the address
│   ├── usage.py         a provider's counts under its own names
│   ├── failure.py       an exception reduced to its code and its named fields
│   ├── _grpcweb.py      one unary gRPC-web call over http.client
│   └── _wire.py         the protobuf encoding of what is sent and read
├── scripts/
│   └── wire_fixtures.py writes tests/wire_fixtures.json from the contract
└── tests/
```

## Before changing it

**It must name everything exactly as `recorder/v1` does.** The server reads records from both with the same readers. A usage count, a finish reason, an error code (`DeadlineExceeded`, not `DEADLINE_EXCEEDED`) and a reported error field spelt differently here is a record the server reads wrongly, and it looks exactly like a correct one.

**The local price and the stored price must agree.** `pricing.py` is the arithmetic `metering/v1/internal/usage` and `metering/v1/internal/pricing` run server-side, and `tests/test_pricing.py` holds that suite's own cases and figures. A change to either side's readers or rate choice is a change to both.

**The encoder is checked against the official runtime.** `tests/test_wire.py` compares every message byte for byte with fixtures the official protobuf runtime wrote from the contract. After a contract release that touches a message sent from here, add the fields to `_wire.py` and regenerate the fixtures:

```bash
DEFINE=~/alis.build/techbridge/define python scripts/wire_fixtures.py
```

That script needs `grpcio-tools` and `googleapis-common-protos`. The library does not.

**HTTP 200 does not mean a call succeeded.** gRPC-web states the outcome in a trailer frame, and a refusal before any reply states it in the headers, both under 200. A call succeeds only when a status of zero was read from one of them.

**A batch is tried again only after a transient failure,** the gateway or metering unavailable, overloaded, aborted or past its deadline, at most twice more with a growing pause and all within `Config.send_timeout`. Every attempt carries the same request id, which is what lets metering store a batch once however many attempts reached it. A refused key or a malformed batch fails the same way every time and is never retried.

**Nothing taken from the queue goes uncounted.** A batch lost to a fault in delivery is counted as `failed`, what is still queued when `close` runs out of time as `dropped`, and a record whose price cannot be worked out is sent unpriced.

**The queue cannot be made unbounded,** and the worker takes each batch off the queue only as it sends it, so the bound holds while a slow destination is waited on.

**A fork starts the child from empty.** Only the forking thread survives a fork, so the child gets a new lock, a new queue and a new worker, and what the parent had queued stays the parent's to send. It also gets new connections, because a socket inherited from the parent is the parent's.

**Exit waits a short, fixed time.** What is queued at exit is sent within `Config.exit_timeout`, two seconds by default, and never longer. A process that is frozen or stopped as soon as it answers, such as a serverless function, calls `reporter.recorder.flush(timeout)` before answering instead. No signal handler is installed: the host's own shutdown decides when exit happens.

**Plain HTTP is refused except to this machine,** so a test can reach a local gateway and a key can never travel in clear anywhere else.

## Running it

```bash
cd recorder/python
python -m pytest
```

The tests against the real OpenAI, Anthropic and Gemini sdks, the Agent Development Kit, LiteLLM, LangChain and the OpenAI Agents SDK run where those are installed and are skipped where they are not. Install them to run everything:

```bash
uv venv && uv pip install -e '.[test]' openai anthropic google-genai google-adk && .venv/bin/python -m pytest
uv venv .litellm && VIRTUAL_ENV=.litellm uv pip install -e '.[test]' litellm fastapi && .litellm/bin/python -m pytest tests/test_litellm.py
uv venv .langchain && VIRTUAL_ENV=.langchain uv pip install -e '.[test]' langchain langgraph langchain-openai langchain-anthropic langchain-google-genai && .langchain/bin/python -m pytest tests/test_langchain.py
uv venv .agents && VIRTUAL_ENV=.agents uv pip install -e '.[test]' openai-agents && .agents/bin/python -m pytest tests/test_openai_agents.py
```

LiteLLM, LangChain and the Agents SDK each run in an environment of their own because they pin versions of the provider sdks the other tests use.
