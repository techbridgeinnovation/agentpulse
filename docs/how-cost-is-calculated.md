---
title: How cost is calculated
summary: Priced on the server from token counts, at the rate in force when the call was made.
section: concepts
order: 2
---

# How cost is calculated

Your agent reports what the provider said it used. Agent Pulse prices it on the server, so an agent never states its own cost.

## From token counts to a price

Each kind of token has its own price: input, output, cached input, cache writes and reasoning. They are kept apart because adding them up first would lose the difference. Charges that are not tokens, such as a web search billed per use, are priced from the rate for that unit.

The prices are on the **Models** page and in List prices.

## Past figures never change

Every call is priced at the rate in force when it was made, and records which rate card it used. When a provider changes its prices, calls made before the change keep their old price.

The same holds for a model that was missing from the rate card: its calls are priced at nothing, and adding the model later prices new calls, not old ones. The console counts these unpriced calls so you know a figure is incomplete.

## Two figures per call

| Figure | Where it comes from | When |
| --- | --- | --- |
| Estimated | Token counts at list price | As soon as the call is recorded |
| Billed | The provider's billing export | Overnight, for traffic billed through Google only |

A provider you pay directly, such as OpenAI or Anthropic, stays estimate only. A recent window shows estimates; an older one shows both, and a small difference between them is expected.

## Money is exact

Amounts are stored as whole millionths of a US dollar, never as floating-point numbers, so figures add up to the cent however many calls they cover.
