---
title: Recorder settings
summary: The environment settings every recorder reads, and the options you can tune.
section: more
order: 1
---

# Recorder settings

## Environment

Every recorder reads these at startup and stops with an error if a required one is missing, so a mistake shows where someone is watching rather than as missing records later.

| Setting | Required | What it is |
| --- | --- | --- |
| `AP_GATEWAY` | Yes | The address on the Connect page. A host, a URL, or a host and port. |
| `AP_API_KEY` | Yes | The key's name, `organisations/<organisation>/apiKeys/<key>`. It says which organisation the spend belongs to. |
| `AP_API_SECRET` | Yes | The key's secret, shown once when it was created. |
| `AP_AGENT` | Yes | `organisations/<organisation>/agents/<name>`, a name you choose, under the key's organisation. |
| `AP_SERVICE` | LiteLLM proxy only | The name the proxy records as. `litellm-proxy` if unset. |

The key needs **Write** ticked to record. One key serves every agent in an organisation.

## Options

Each recorder takes the same options when you build it yourself. The defaults suit almost every agent.

| Option | Python | TypeScript | Go | Default |
| --- | --- | --- | --- | --- |
| Queue size | `queue_size` | `queueSize` | `QueueSize` | 2,048 records |
| Batch size | `batch_size` | `batchSize` | `BatchSize` | 100 records |
| Send a batch at least every | `flush_every` | `flushEveryMs` | `FlushEvery` | 2 seconds |
| Give up on one send after | `send_timeout` | `sendTimeoutMs` | `SendTimeout` | 10 seconds |
| Refresh the price list every | `rate_refresh` | | `RateRefresh` | 15 minutes |

When the queue is full, new records are dropped and counted, never waited for. Raise the queue size if your stats show drops.

## Running without Agent Pulse

For tests and local runs, give the recorder a sink that discards records. No key and no connection are needed.

```python [Python]
rec = agentpulse.Recorder(agentpulse.Config(sinks=[agentpulse.Discard()]))
reporter = agentpulse.Reporter(rec, agentpulse.Attribution(agent="organisations/local/agents/test", service="test"))
```

```ts [TypeScript]
import { Discard, Recorder, Reporter } from "techbridge-agentpulse";

const reporter = new Reporter(new Recorder({ sinks: [new Discard()] }), { agent: "organisations/local/agents/test", service: "test" });
```

```go [Go]
rec := recorder.New(recorder.Config{
    Sinks:      []recorder.Sink{recorder.Discard()},
    FlushEvery: time.Millisecond,
})
```
