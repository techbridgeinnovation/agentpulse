---
title: What is recorded, and what never is
summary: The fields on each record, and the content that never leaves your process.
section: concepts
order: 1
---

# What is recorded, and what never is

The recorder sends one record per model call or tool call: who it was for, what was called, how many tokens, how long it took and whether it worked. Never what was said.

## What never leaves your process

- Prompts and completions.
- Tool arguments and tool results. Only their size is recorded.
- Error messages. A failure is kept as its code, because a provider's message often quotes the prompt back.
- Names and email addresses on a record. A record carries only the identifier your sign-in issued; a name, if you send one, is stored once, apart from the records.

This is not a setting. A record has no field a prompt could go in.

## What a record carries

| Field | What it is |
| --- | --- |
| Workspace | Which of your customers the call was for, or `default`. |
| `agent` | Which agent made the call. |
| `request` | Your identifier for the end-user request it was part of. |
| `session` | The conversation, where there is one. |
| `user` | Your sign-in's identifier for the person. |
| `callerService`, `callerComponent`, `skill` | Which part of your product made the call. |
| `project` | A unit of work inside the customer, where you set one. |
| `model`, `billedBy` | What was called, and who bills for it. |
| Token counts | Input, output, cached, cache-write and reasoning, as the provider reported them. |
| `durationMs`, `status`, `errorCode` | How long it took, how it ended, and the error code if it failed. |
| `estimatedCostMicros`, `billedCostMicros` | What it cost. See [how cost is calculated](how-cost-is-calculated.md). |

Records are never changed or deleted once written. The one later addition is the billed figure, written beside the estimate.

## Who a person is

A record carries an identifier, whatever your sign-in issued. Agent Pulse never sees your sign-in, so it cannot turn an identifier into a name. Send the name once with the user and it is shown wherever they appear; send none and the console shows identifiers.
