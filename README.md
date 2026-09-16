# Agent Pulse

Metering, cost attribution and spend governance for AI agents. This repository holds the part of it that runs inside your own process.

| Path | What it is |
| --- | --- |
| `recorder/` | The Go library an agent or service imports to report what it spent. |
| `recorder/adkhooks/` | Callbacks for agents built on Google ADK v1. |
| `recorder/adkv2hooks/` | The same callbacks for Google ADK v2, as its own module because the two ADK majors are unrelated types. |
| `recorder/pb/` | The generated Go for the metering and governance contracts the recorder speaks. |
| `docs/` | What Agent Pulse records, how to record it, and how to read it back. The same pages the console serves. |
| `scripts/` | Maintainer tooling: syncing the contracts and the documentation from the private build. |

```bash
go get github.com/techbridgeinnovation/agentpulse/recorder
```

Start with [what Agent Pulse records](docs/what-agent-pulse-records.md), then [recording](docs/recording-what-an-agent-spends.md) and [reading it back](docs/reading-your-spend.md). The library's own notes are in [`recorder/README.md`](recorder/README.md).
