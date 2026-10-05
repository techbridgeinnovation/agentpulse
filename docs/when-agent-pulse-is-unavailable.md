---
title: When Agent Pulse is unavailable
summary: The recorder never slows or breaks your agent; what happens when we are down.
section: concepts
order: 4
---

# When Agent Pulse is unavailable

The recorder runs inside your agent but is never in the path of your model calls. Your model traffic goes straight to the provider, so Agent Pulse being slow or down never slows or stops it.

## Recording

Records go into a queue in your process and are sent in batches in the background. Recording never waits on the network and never raises an error into your code.

If records cannot be sent, they are retried. If the queue fills, new records are dropped rather than slowing your agent, and the drop is counted. That count cannot be turned off, and it is also reported to Agent Pulse with the next batch that gets through, so a figure that is incomplete says so.

## Budget checks

When budgets can stop calls, each call asks first. If there is no answer within 1.5 seconds, the call goes ahead.

One exception: a call a budget stopped within the last hour stays stopped without asking again, so an outage does not lift a limit that was already reached.

## Keys

A missing or wrong API key is refused, not waved through. The recorder checks its settings at startup and stops there if one is missing, where a person is watching, rather than losing records quietly later.
