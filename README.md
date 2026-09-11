# Agent Pulse

Metering, cost attribution and spend governance for AI agents. This repository holds the part of it that runs inside your own process.

| Path | What it is |
| --- | --- |
| `recorder/` | The Go library an agent or service imports to report what it spent. |
| `recorder/adkhooks/` | Callbacks for agents built on Google ADK v1. |
| `recorder/adkv2hooks/` | The same callbacks for Google ADK v2, as its own module because the two ADK majors are unrelated types. |
| `recorder/pb/` | The generated Go for the metering and governance contracts the recorder speaks. |
| `scripts/` | Maintainer tooling. |

```bash
go get github.com/techbridgeinnovation/agentpulse/recorder
```

Start with [`recorder/README.md`](recorder/README.md).
