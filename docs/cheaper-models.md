---
title: Compare cheaper models
summary: See what an agent's own calls would have cost on other models that can do the same work.
section: guides
order: 5
---

# Compare cheaper models

For each agent, Agent Pulse prices the agent's own calls against other models, so you can see what switching would save before you try it.

## Where to find it

Open an agent from the **Agents** page. Under **Price against another model**, **Cheaper models that would have worked** lists up to five models that would have cost less for the same calls, cheapest first, with the estimate and how much less it is.

## How the estimate is made

- **The agent's own token counts.** Its input, output, cached and reasoning tokens over the chosen dates, priced at each model's standard rates.
- **Only models that can do the job.** A model is left out if it does not support what the agent used: its longest prompt, tool calls, or reasoning. A model whose limits are unknown is left out rather than assumed to fit.
- **Only cheaper ones.** A model that would have cost the same or more is not listed.

Choose **Price exactly** on a model to price the same dates again at that model's rates, beside what they were actually billed. Calls the model has no rate for are counted and left out of both totals.

## What it does not tell you

The estimate is about price, not quality. A cheaper model may answer worse for your use. Try it on real traffic first. A [budget with a downgrade](budgets.md) moves an agent to a cheaper model only once its spend passes a threshold you set.

Every model's prices and limits are on the **Models** page.
