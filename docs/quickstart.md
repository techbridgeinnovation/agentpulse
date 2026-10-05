---
title: Quickstart
summary: Record your agent's first model call and see it in the console.
section: start
order: 1
steps: true
---

# Quickstart

Record your agent's first model call and see it in the console. Pick your language in any code block and every block on every page follows it.

You need an organisation and an API key with **Write** ticked, from the API keys page.

## Install the recorder

```bash [Python]
pip install "techbridge-agentpulse @ git+https://github.com/techbridgeinnovation/agentpulse@python/v0.5.0#subdirectory=python"
```

```bash [TypeScript]
npm install https://github.com/techbridgeinnovation/agentpulse/releases/download/typescript/v0.1.0/techbridge-agentpulse-0.1.0.tgz
```

```bash [Go]
go get github.com/techbridgeinnovation/agentpulse/recorder
```

The recorder has no dependencies of its own. The TypeScript recorder needs Node 18.18 or newer.

## Set four settings

Copy them from the Connect page into your agent's environment. The recorder stops at startup if one is missing, so a mistake shows straight away.

```env
AP_GATEWAY=gateway.agentspulse.ai
AP_API_KEY=organisations/northwind/apiKeys/7f2c
AP_API_SECRET=<shown once when the key is created>
AP_AGENT=organisations/northwind/agents/support-triage
```

| Setting | What it is |
| --- | --- |
| `AP_GATEWAY` | The address on the Connect page. |
| `AP_API_KEY` | The key's name. It says which organisation the spend belongs to, and is not secret. |
| `AP_API_SECRET` | The secret, shown once. Keep it in your secrets manager. |
| `AP_AGENT` | A name you choose for this agent, under your organisation. |

One key serves every agent in the organisation. Nothing registers an agent: it appears after its first call.

## Connect, once where your agent starts

```python [Python]
import agentpulse

reporter = agentpulse.connect(service="support-triage")
```

```ts [TypeScript]
import { connect } from "techbridge-agentpulse";

const reporter = connect("support-triage");
```

```go [Go]
key := os.Getenv("AP_API_KEY")
organisation := recorder.OrganisationOfKey(key)
agent := os.Getenv("AP_AGENT")

conn, err := recorder.DialWithKey(os.Getenv("AP_GATEWAY"), key, os.Getenv("AP_API_SECRET"))
if err != nil || organisation == "" || agent == "" {
    log.Fatal("AP_GATEWAY, AP_API_KEY, AP_API_SECRET and AP_AGENT must all be set")
}

rec := recorder.New(recorder.Config{
    Sinks: []recorder.Sink{recorder.NewGRPCSink(conn, organisation)},
})
defer rec.Close(ctx)

reporter := rec.For(recorder.Attribution{Agent: agent, Service: "support-triage"})
```

The service name is how this process shows up in the console. Records are sent in the background, in batches.

## Record your model calls

Wrap your model client once, where you create it. Every call through it is recorded from then on.

```python [Python]
from openai import OpenAI
from anthropic import Anthropic
from google import genai

gpt = OpenAI(http_client=reporter.openai_http_client())
claude = Anthropic(http_client=reporter.anthropic_http_client())
gemini = genai.Client(http_options=reporter.genai_http_options())
```

```ts [TypeScript]
import { wrapLanguageModel } from "ai";
import { openai } from "@ai-sdk/openai";

const model = wrapLanguageModel({ model: openai("gpt-5"), middleware: reporter.aiSdkMiddleware() });
```

```go [Go]
gemini, err := genai.NewClient(ctx, &genai.ClientConfig{Backend: genai.BackendVertexAI})
reporter.InstrumentGenAI(gemini)

claude := anthropic.NewClient(option.WithMiddleware(reporter.AnthropicMiddleware()))
gpt := openai.NewClient(option.WithMiddleware(reporter.OpenAIMiddleware()))
```

Built on a framework instead? See [Google ADK](google-adk.md), [LangChain](langchain.md), [OpenAI Agents SDK](openai-agents.md) or the [LiteLLM proxy](litellm.md).

## Say who the work is for

Once per request, where you have checked the sign-in. Every call inside is counted against that request, customer and user.

```python [Python]
with agentpulse.scope(request=request_id, workspace=customer_id, user=agentpulse.User(id=user_id)):
    handle(request)
```

```ts [TypeScript]
import { scope } from "techbridge-agentpulse";

await scope({ request: requestId, workspace: customerId, user: { id: userId } }, () => handle(req));
```

```go [Go]
ctx = recorder.WithUser(recorder.WithRequest(ctx, requestID), recorder.User{ID: userID})
ctx = recorder.WithWorkspace(ctx, customerID)
```

Leave out the workspace if your product has no customers of its own. See [Track cost per customer and user](cost-per-customer.md).

## Run your agent

Make one model call as you normally would. The agent appears on the **Agents** page with the call's model, tokens and cost.

To check from code, read the recorder's counts, for example when the process stops:

```python [Python]
stats = reporter.recorder.stats()
print(f"recorded {stats.recorded}, delivered {stats.delivered}, dropped {stats.dropped}")
```

```ts [TypeScript]
const s = reporter.recorder.stats();
console.log(`recorded ${s.recorded}, delivered ${s.delivered}, dropped ${s.dropped}`);
```

```go [Go]
s := rec.Stats()
log.Printf("recorded %d, delivered %d, dropped %d", s.Recorded, s.Delivered, s.Dropped)
```

`dropped` above zero means the recorder's queue filled and some calls were not recorded. It never slows your agent down to avoid that.
