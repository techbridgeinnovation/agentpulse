# Agent Pulse

Metering, cost attribution and spend governance for AI agents. This repository holds the part of it that runs inside your own process.

| Path | What it is |
| --- | --- |
| `recorder/` | The Go library an agent or service imports to report what it spent. |
| `recorder/adkhooks/` | Callbacks for agents built on Google ADK v1. |
| `recorder/adkv2hooks/` | The same callbacks for Google ADK v2, as its own module because the two ADK majors are unrelated types. |
| `recorder/pb/` | The generated Go for the metering and governance contracts the recorder speaks. |
| `python/` | The same library for Python, `techbridge-agentpulse`, imported as `agentpulse`: a reporter, instrumented OpenAI, Anthropic and Gemini clients, a plugin for Google ADK and a callback for LiteLLM. No dependencies of its own. |
| `docs/` | What Agent Pulse records, how to record it, and how to read it back. The same pages the console serves. |
| `scripts/` | Maintainer tooling: syncing the contracts, the documentation and the Python recorder from the private build. |

```bash
go get github.com/techbridgeinnovation/agentpulse/recorder
```

```bash
pip install "techbridge-agentpulse @ git+https://github.com/techbridgeinnovation/agentpulse@python/v0.1.1#subdirectory=python"
```

Each Python release is a `python/v<version>` tag, listed on the [releases page](https://github.com/techbridgeinnovation/agentpulse/releases). Pin to the newest one. A release is made by bumping `python/src/agentpulse/_version.py` in the pull request that carries the change: merging it tags and publishes the release.

Start with [what Agent Pulse records](docs/what-agent-pulse-records.md), then [recording](docs/recording-what-an-agent-spends.md) and [reading it back](docs/reading-your-spend.md). The libraries' own notes are in [`recorder/README.md`](recorder/README.md) and [`python/README.md`](python/README.md).
