# Agent Pulse

Metering, cost attribution and spend governance for AI agents. This repository holds the part of it that runs inside your own process.

| Path | What it is |
| --- | --- |
| `recorder/` | The Go library an agent or service imports to report what it spent. |
| `recorder/adkhooks/` | Callbacks for agents built on Google ADK v1. |
| `recorder/adkv2hooks/` | The same callbacks for Google ADK v2, as its own module because the two ADK majors are unrelated types. |
| `recorder/pb/` | The generated Go for the metering and governance contracts the recorder speaks. |
| `python/` | The same library for Python, `techbridge-agentpulse`, imported as `agentpulse`: a reporter, instrumented OpenAI, Anthropic and Gemini clients, and integrations for Google ADK, LangChain and LangGraph, the OpenAI Agents SDK and LiteLLM. No dependencies of its own. |
| `typescript/` | The same library for Node, `techbridge-agentpulse`: a reporter, a telemetry integration and middleware for the Vercel AI SDK, and calls reported by hand. No dependencies of its own. |
| `skills/adopt-agent-pulse/` | Instructions a coding agent follows to add Agent Pulse to a service. Claude Code installs them as the `agentpulse` plugin, from `.claude-plugin/`. |
| `cursor/` | The same instructions as a Cursor rule, written from the skill by `scripts/sync-cursor-rule.sh`. |
| `docs/` | Quickstart, guides, framework pages and concepts. The same pages the console serves, which also hosts the API reference. |
| `scripts/` | Maintainer tooling: syncing the contracts, the documentation and the Python recorder from the private build. |

```bash
go get github.com/techbridgeinnovation/agentpulse/recorder
```

```bash
pip install techbridge-agentpulse
```

```bash
npm install techbridge-agentpulse
```

## Supported integrations

| Where your agent runs | Integration | Budgets can stop a call | Guide |
| --- | --- | --- | --- |
| Vercel AI SDK 7, TypeScript | `registerTelemetry(reporter.aiSdkTelemetry())` | No, records only | [Vercel AI SDK](docs/vercel-ai-sdk.md) |
| Vercel AI SDK 7 and 6, TypeScript | `reporter.aiSdkMiddleware()` on `wrapLanguageModel`, `aiSdkEmbeddingMiddleware()`, `aiSdkTools()` | Yes | [Vercel AI SDK](docs/vercel-ai-sdk.md) |
| OpenAI Agents SDK, Python | `reporter.openai_agents_hooks()` | Yes | [OpenAI Agents SDK](docs/openai-agents.md) |
| LangChain and LangGraph, Python | `reporter.langchain_handler()` | Yes | [LangChain](docs/langchain.md) |
| Google ADK, Python | `reporter.adk_plugin()` | Yes | [Google ADK](docs/google-adk.md) |
| Google ADK v1 and v2, Go | `recorder/adkhooks`, `recorder/adkv2hooks` | Yes | [Google ADK](docs/google-adk.md) |
| LiteLLM SDK and proxy, Python | `reporter.litellm_callback()`, `agentpulse.litellm_proxy.handler` | Yes | [LiteLLM](docs/litellm.md) |
| OpenAI, Anthropic and Gemini clients, Python | `openai_http_client()`, `anthropic_http_client()`, `genai_http_options()` | Yes | [Model clients](docs/model-clients.md) |
| Gemini, OpenAI and Anthropic clients, Go | `InstrumentGenAI`, `OpenAIMiddleware`, `AnthropicMiddleware` | Yes | [Model clients](docs/model-clients.md) |
| Anything that emits OpenTelemetry GenAI spans | the OTLP exporter pointed at the gateway, no library | No, records only | [OpenTelemetry](docs/opentelemetry.md) |
| Any other code | `decide` before a call, `modelCall` and `toolCall` after it | Yes, where it calls `decide` | [Model clients](docs/model-clients.md) |

Each Python release is published to PyPI and tagged `python/v<version>`, and each TypeScript release to npm and tagged `typescript/v<version>`, all listed on the [releases page](https://github.com/techbridgeinnovation/agentpulse/releases). A release is made by bumping `python/src/agentpulse/_version.py` or the `version` in `typescript/package.json` in the pull request that carries the change: merging it tags and publishes the release.

## Add it with your coding agent

In Claude Code:

```
/plugin install agentpulse --marketplace techbridgeinnovation/agentpulse
```

In Cursor, copy [`cursor/adopt-agent-pulse.mdc`](cursor/adopt-agent-pulse.mdc) into the project's `.cursor/rules/`.

Then ask it to add Agent Pulse to the service. It asks for the four settings, adds the dependency and the few lines that connect it, and changes no call site.

Start with the [quickstart](docs/quickstart.md), then [what is recorded](docs/what-is-recorded.md) and [reading your spend over the API](docs/reading-spend-api.md). The libraries' own notes are in [`recorder/README.md`](recorder/README.md) and [`python/README.md`](python/README.md).
