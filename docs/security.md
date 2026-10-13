---
title: Security and trust
summary: What never leaves your process, what happens when we are down, and how keys and records are kept.
section: concepts
order: 5
---

# Security and trust

Agent Pulse is a library inside your agent, not a proxy in front of it. Your model calls go straight to the provider, and the library sends a small record of each one in the background.

## Your content stays in your process

- Prompts and completions are never recorded.
- Tool arguments and results are never recorded. Only their size is.
- Error messages are never recorded. A failure is kept as its code, because a provider's message often quotes the prompt back.
- A record carries your sign-in's identifier for a person, never a name or an email address.

This is not a setting. A record has no field a prompt could go in. See [what is recorded](what-is-recorded.md) for every field.

## Read the code that runs in your agent

The recorder's code is public, at [github.com/techbridgeinnovation/agentpulse](https://github.com/techbridgeinnovation/agentpulse), in Go, Python and TypeScript. What runs in your process is what you can read there.

## Your agent keeps working when we do not

- Recording never waits on the network and never raises an error into your code. If records cannot be sent, they are queued and retried, and if the queue fills they are dropped and counted.
- A budget check that gets no answer within 1.5 seconds lets the call go ahead.
- A call a budget stopped within the last hour stays stopped while we are unreachable, so an outage does not lift a limit already reached.

See [when Agent Pulse is unavailable](when-agent-pulse-is-unavailable.md).

## Keys

- A key is either write, read, or both, and is held to one organisation.
- Its secret is shown once, when it is issued. We keep only a hash of it.
- A missing or wrong key is refused. The recorder checks its settings at startup and stops there if one is missing.
- The recorder reaches us over TLS. Plain HTTP is accepted only to the machine it runs on, for local testing.

## Records

- Records are append-only. No call edits or deletes one.
- Every read is limited to the organisation the key or the signed-in person belongs to.
