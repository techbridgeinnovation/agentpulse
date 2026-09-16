---
title: What Agent Pulse records
summary: The shape of the data, and what the figures mean.
order: 0
---

# What Agent Pulse records

## One row per unit of work

An **activity** is one priced unit of an agent's work: usually a single model call, sometimes a tool call, sometimes any other charge an agent incurs. It is written when the work finishes, priced server-side, and never changed again.

Activities are append-only. There is no update and no delete, because the record is what spend was reported. Reconciliation is the one exception and it writes a separate figure rather than rewriting the estimate.

## What an activity carries

| | |
| --- | --- |
| `agent` | which agent did the work, under your organisation |
| `request` | the end-user request it belongs to — every call made while serving one request shares it, across a handoff from one agent to another |
| `session` | the conversation, where there is one |
| `user` | the identifier of the person the work was for |
| `caller_service`, `caller_component`, `skill` | which part of your product spent it |
| `model`, `provider` | what was called and who bills for it |
| token counts | prompt, candidate, cached, cache-write, reasoning, and the total the provider reported |
| `duration_ms`, `status`, `error_code` | how long, whether it worked, and why not |
| `estimated_cost_micros`, `billed_cost_micros` | what it cost |
| `rate_card_version` | the prices it was charged against |

## Why there are two cost figures

**The estimate** is computed from the token counts when the record is written, against the rate card in force at that moment. It is available immediately and never changes.

**The billed figure** comes from the provider's own invoice, written overnight against the record it belongs to. Only traffic billed through Google can ever be reconciled this way; a provider you call directly stays estimate-only. That is a boundary rather than a gap waiting to close.

So a fresh window shows estimates and a settled one shows both, and the two disagreeing slightly is the system working.

## Why a price never changes retroactively

Each activity records the rate card version it was priced against. A provider changing its prices tomorrow does not rewrite what last month cost.

The consequence worth knowing: a call priced at zero was priced when the card had no entry for its model, and it stays at zero. Adding the model to the card prices calls made after that, not before.

## Outcomes attach to a request, not a call

An **outcome** is a unit of business work your product counts — a shortlist produced, a report finished, a booking made.

It attaches to the request, because one request makes many model calls and a shortlist is produced by the run. Counting it per activity would multiply it by however many calls that run happened to make.

That is what makes cost per shortlist possible rather than only cost per million tokens.

## Who a person is

A record carries an identifier and nothing else about the person: whatever your own sign-in issued. Never a name, never an email.

Agent Pulse never sees your sign-in and cannot resolve an identifier itself. Names live once per person in a directory your agents fill in, and a report joins the two when it renders. An organisation that sends no names sees identifiers — correct, rather than broken.

## What is never stored

No prompt text, no completions, no tool arguments, and no error messages. An error is kept as its code.

This is not a setting. There is no field on an activity that a prompt could be put in.
