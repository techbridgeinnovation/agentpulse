---
title: Assess your codebase
summary: A prompt for your coding agent that reads your repository and reports what Agent Pulse would show you and what it would take to connect.
section: start
order: 2
---

# Assess your codebase

Give this prompt to your coding agent and it reads your repository, then writes a report on what Agent Pulse would show you and what it would take to connect.

## Run it

1. Open Claude Code, Cursor or another coding agent at the root of your repository.
2. Copy the prompt below and paste it in.
3. When it finishes, open `agentpulse-assessment.html` in a browser.


You are assessing this repository for Agent Pulse, which records what a product's AI agents and services spend on model calls and attributes it to a feature, a person, a customer and a request. Agent Pulse records counts, identifiers and timings, never prompts, responses or tool arguments. A small library inside each process observes calls in-process; it is not a proxy and never carries model traffic, and if Agent Pulse cannot answer it lets every call go ahead.

Your job is to produce an honest readiness report. Follow these rules throughout:

- **Read only.** Do not edit, install, build, run or deploy anything. Do not make network requests. Do not open `.env` files or print the value of any secret; you may note that a variable exists.
- **Write exactly one file,** `agentpulse-assessment.html` at the repository root, made from the template at the end of this prompt.
- **Evidence or nothing.** Every call site, risk and change you report names a file and, where it exists, a line. If you are unsure, say so in the text rather than guessing. Never state a dollar figure, a traffic volume or a saving: you have not measured any. Counts you took from the code are fine.
- **Claim only what Agent Pulse does today,** as listed under *What Agent Pulse can show* below.

## Step 1: Find every place that calls a model

Search the whole repository, skipping `node_modules`, `vendor`, build output and generated code. Look for:

- **SDKs and frameworks in manifests** (`go.mod`, `pyproject.toml`, `requirements*.txt`, `package.json`, `pom.xml`, `*.csproj`, `Cargo.toml`): `google.golang.org/adk`, `google.golang.org/genai`, `google-adk`, `google-genai`, `vertexai`, `openai`, `anthropic`, `langchain`, `langgraph`, `litellm`, `openai-agents`, `ai` and `@ai-sdk/*`, `@google/genai`, `llama-index`, `crewai`, `autogen`, `haystack`, `pydantic-ai`, `semantic-kernel`, `strands`, `mastra`, `boto3` with Bedrock, `azure-ai`.
- **Direct HTTP calls to model APIs:** hosts such as `aiplatform.googleapis.com`, `generativelanguage.googleapis.com`, `api.openai.com`, `api.anthropic.com`, `bedrock-runtime`, `openai.azure.com`, `api.perplexity.ai`, `api.mistral.ai`, and paths ending in `:generateContent`, `/chat/completions`, `/responses`, `/messages`.
- **Embeddings, transcription, image generation and vision calls,** which cost money too.

Group what you find by **deployable unit**: one service, agent, function or job. A model adapter that sits inside a framework (a custom model class handed to an agent framework, for instance) belongs to that framework's call site: count it once, under the framework, not again as a client. For each, record its path, language and version (the `go` line, `requires-python`, `engines.node`), the framework and SDK versions, the models it uses (from code defaults, config and infrastructure files), what in the product triggers it (an API route, a UI action, a schedule, a queue), and whether it streams. Note dead code that is never called, and leave it out of the counts.

## Step 2: Decide how each call site connects

Use this table, and nothing else, to decide.

| Your code | Path | What it takes |
|---|---|---|
| Go, Google ADK v1 | `framework` | `github.com/techbridgeinnovation/agentpulse/recorder` and its `adkhooks` callbacks, registered ahead of your own on each `llmagent.Config` |
| Go, Google ADK v2 | `framework` | `github.com/techbridgeinnovation/agentpulse/recorder/adkv2hooks`, the same callbacks plus `Model` |
| Go, `google.golang.org/genai` client | `client` | `reporter.InstrumentGenAI(client)` once after the client is built |
| Go, OpenAI or Anthropic SDK | `client` | `option.WithMiddleware(reporter.OpenAIMiddleware())` or `reporter.AnthropicMiddleware()` |
| Go, any other way (plain HTTP, another SDK) | `manual` | `reporter.ModelCall` after each call, with the usage counts the provider returned and `Format` (`recorder.FormatVertex`, `FormatAnthropic`, `FormatOpenAIChat`, `FormatOpenAIResponses`, `FormatPerplexity`); `reporter.ToolCall` for tools |
| Python, Google ADK | `framework` | `techbridge-agentpulse`, `reporter.adk_plugin()` in the app's plugins |
| Python, LangChain or LangGraph | `framework` | the callback handler on the chain or graph |
| Python, LiteLLM SDK or proxy | `framework` | the LiteLLM callback, or the proxy handler |
| Python, OpenAI Agents SDK | `framework` | run hooks and the trace processor |
| Python, OpenAI, Anthropic or google-genai client | `client` | the httpx transport for that client |
| TypeScript, Vercel AI SDK | `framework` | `techbridge-agentpulse`: `registerTelemetry(reporter.aiSdkTelemetry())` on AI SDK 7, and the language model middleware and tool wrapper where a budget must stop calls (the only way on AI SDK 6) |
| Anything already emitting OpenTelemetry GenAI spans (CrewAI, AutoGen, Haystack, LlamaIndex, Mastra, Microsoft Agent Framework, Pydantic AI, Semantic Kernel, Strands Agents, OpenLIT, OpenLLMetry) | `otlp` | point the OTLP traces exporter at the Agent Pulse gateway's `/v1/traces` |
| TypeScript without the Vercel AI SDK, Java, .NET, Rust and others with no GenAI spans | `unsupported` | not supported today; say so plainly |

**Version floors.** These are real requirements; check every one against what you found.

- Go: the recorder needs **Go 1.26 or later** and requires **`google.golang.org/adk` v1.6.0** and **`google.golang.org/genai` v1.71.0** or later. Go always selects the highest version required, so adding the recorder to a module on older versions upgrades them; say which versions it is on now.
- Python: **3.10 or later**.
- TypeScript: **Node 18.18 or later**.

Every service also needs four settings: `AP_GATEWAY` (the gateway address), `AP_API_KEY` (the key's name, not secret), `AP_API_SECRET` (the secret, kept in a secrets manager) and `AP_AGENT` (a name for this agent or service). Note where the repository keeps settings and secrets (infrastructure files, a secrets manager) so the change list says exactly where these go.

## Step 3: Map identity

Agent Pulse attributes each call to:

- **user**: the person the work was done for, as a **stable opaque identifier**. Never an email address or a name: the recorder blanks any user id that looks like an email, so a call would show no person at all. If the code identifies people by email, find the opaque id beside it (a token `sub`, a user resource name, a database id) and make using it a **required** change.
- **request**: one user action, which may make several model calls (a chat turn, an upload, a job run).
- **workspace**: the customer or tenant whose spend it is, for a product whose own customers each have one. Leave it empty for a single-tenant product.
- **component**: the part of the product that spent it, such as `chat`, `receipt_scan` or `nightly_summary`. Suggest one per trigger you found; this is what gives cost per feature.
- **environment**: development, staging or production, which the API key sets.

For each, name the field in this codebase that would supply it, or say none exists.

## Step 4: Spot what drives spend

From the code alone, note patterns Agent Pulse would make visible, each with its file: one user action that fans out into many model calls, an expensive default model, a model the caller can override, retries, large context, missing per-user limits, model calls a person can trigger without signing in, and failures that are still charged. Do not estimate how much any of it costs.

## Step 5: What Agent Pulse can show

Only these, today:

- Cost per agent, model, person, customer (workspace), request, feature (component) and environment, by day or hour, and how it changed from the period before.
- Spend on calls that failed or were retried, and what the failures were.
- What the same work would have cost on another model, and cheaper models that could do it.
- Budgets on the whole organisation, a person or group of people, an agent or a customer, daily to yearly, that warn or stop calls before they are made. Stopping needs the Python governed reporter, a Go `Decider`, or the LiteLLM proxy hook.
- Alerts at 75% and 95% of a budget and when it runs out, on a day far above an ordinary one, and when an agent fails far more than usual, by email, Slack or webhook. A notice when a model you use is about to be retired.
- A weekly report and an Excel export.
- Estimated costs at published prices. Not the provider's invoice.

For each agent or service, mark each capability `yes` (works as soon as the service is connected), `after` (works once a listed *required* or *recommended* change is made; name it in that change) or `no` (does not apply to this product, such as cost per customer for a single-tenant product). A service that serves one feature has cost per feature on day one: the service is the feature. Use the keys `feature`, `person`, `customer`, `request`, `failed`, `models`, `budgets`, `alerts`.

## Step 6: Classify changes and score readiness

**Connecting is the work, never a change.** For each service it is: adding the library, the four settings, and the hook or calls from Step 2, including reading the usage counts the provider's response already returns and choosing which existing id identifies a request. A service whose every call is one request-response already has its request: the call itself.

**Changes come from this fixed list, at most once each per service:**

| Change | Severity |
|---|---|
| Upgrade the agent framework to the version Agent Pulse needs (the floors in Step 2, for example Google ADK v1.1 to v1.6), together with the SDK versions that upgrade brings | blocker |
| Model calls Agent Pulse cannot record yet (a call site whose path is `unsupported`, counted once per call site) | blocker |
| Raise the language version to the one Agent Pulse needs (Go, Python or Node) | required |
| Raise an SDK the code calls itself, outside any framework, to the version Agent Pulse needs | required |
| Identify people by an opaque id instead of an email or name | required |
| Keep `AP_API_SECRET` in a secrets manager, where neither one nor the platform's own secret variables are wired up | recommended |
| Name a component per feature, where one service serves several features | recommended |
| Set a workspace per customer, for a product whose customers each have one | recommended |
| Add a budget check so budgets can stop calls | recommended |
| Limit which models a caller may choose, where the caller can name one | recommended |

Raising the language version counts even when a blocker would force it too, because it is separate work. A dependency the recorder brings into a module that does not use it otherwise (ADK in a service that only calls a model over HTTP, say) is not a change.

**Score:** start at 100; subtract 20 for each blocker and 4 for each required change; do not go below 0. The verdict is *Ready to connect* at 80 or more, *Ready after a few changes* from 50 to 79, and *Needs groundwork first* below 50. List each deduction as a reason, and title each change for a reader who has not seen this prompt: name the real thing (*Upgrade Google ADK from v1.1.0 to v1.6.0*), never a step number or a rule from here.

**Effort:** hours per agent or service for one engineer who knows the code: about 1 to connect a framework hook or a client wrap, about 1 per manual call site, plus the hours of that service's blockers and required changes. Round to the half hour, and give each change its own hours too.

## Step 7: Write the report

Copy the template below to `agentpulse-assessment.html` and replace `__DATA__` with one JSON object of this shape, and change nothing else in the template:

```json
{
  "product": "name of the product",
  "generatedAt": "YYYY-MM-DD",
  "summary": "two sentences: what the product does with AI, and the headline finding",
  "readiness": {"score": 0, "verdict": "", "reasons": [""]},
  "services": [{
    "name": "", "path": "", "language": "Go 1.25", "framework": "Google ADK v1.1.0",
    "triggers": ["what a person does that reaches it"], "models": ["gemini-2.5-pro"],
    "callSites": [{"file": "", "line": 0, "how": "how it calls the model", "path": "framework|client|manual|otlp|unsupported"}],
    "capabilities": {"feature": "yes|after|no", "person": "", "customer": "", "request": "", "failed": "", "models": "", "budgets": "", "alerts": ""},
    "effortHours": 0
  }],
  "value": [{"question": "a question the product team can answer once connected", "evidence": "why, tied to a file or trigger"}],
  "risks": [{"title": "", "evidence": "the pattern and where it is"}],
  "changes": [{"severity": "blocker|required|recommended", "service": "", "title": "", "detail": "", "file": "path:line", "effortHours": 0}],
  "identity": [{"concept": "user|request|workspace|component|environment", "theirField": "", "note": "", "pii": false}],
  "nextSteps": [""]
}
```

Write four to eight `value` items in the product's own terms (*What does one receipt scan cost?*, not *cost per component*). Order `changes` blockers first. The last next step is to connect: sign up at agentspulse.ai, create a key, and ask your coding agent to *add Agent Pulse* using the install guide at agentspulse.ai/docs.

When the file is written, tell the person where it is, the score and verdict, and the blockers in two or three lines.

## The template

```html
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Agent Pulse readiness</title>
<style>
:root{--bg:#f7f7f5;--card:#fff;--ink:#16181d;--ink2:#5b616e;--line:#e3e4e8;--accent:#2f6fed;--good:#1f9d63;--warn:#c98a12;--bad:#d1453b;--p1:#2f6fed;--p2:#1f9d63;--p3:#c98a12;--p4:#7a5af5;--p5:#9aa0ab}
@media (prefers-color-scheme:dark){:root{--bg:#0f1115;--card:#171a21;--ink:#eceef2;--ink2:#9aa1ad;--line:#2a2f3a;--accent:#6b9bff;--good:#3ccf8e;--warn:#e7b64a;--bad:#ff6b5f;--p1:#6b9bff;--p2:#3ccf8e;--p3:#e7b64a;--p4:#a58bff;--p5:#6b7280}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font:15px/1.55 system-ui,-apple-system,Segoe UI,Roboto,sans-serif}
.wrap{max-width:1080px;margin:0 auto;padding:32px 16px 64px}
h1{font-size:26px;margin:0 0 4px}h2{font-size:18px;margin:0 0 4px}.sub{color:var(--ink2);margin:0 0 16px}
.card{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:20px;margin:16px 0}
.grid{display:grid;gap:12px;grid-template-columns:repeat(auto-fit,minmax(150px,1fr))}
.kpi{border:1px solid var(--line);border-radius:10px;padding:12px}.kpi b{display:block;font-size:24px}.kpi span{color:var(--ink2);font-size:13px}
.hero{display:grid;grid-template-columns:200px 1fr;gap:20px;align-items:center}@media (max-width:640px){.hero{grid-template-columns:1fr}}
ul.reasons{margin:8px 0 0;padding-left:18px}ul.reasons li{margin:4px 0}
.legend{display:flex;flex-wrap:wrap;gap:14px;font-size:13px;color:var(--ink2);margin-top:10px}.sw{display:inline-block;width:10px;height:10px;border-radius:2px;margin-right:6px;vertical-align:-1px}
table{width:100%;border-collapse:collapse;font-size:14px}th,td{text-align:left;padding:8px;border-bottom:1px solid var(--line);vertical-align:top}th{color:var(--ink2);font-weight:600;font-size:12px;text-transform:uppercase;letter-spacing:.04em}
.scroll{overflow-x:auto}.dot{font-size:16px}.c{text-align:center}
.badge{display:inline-block;font-size:11px;font-weight:700;letter-spacing:.04em;text-transform:uppercase;border-radius:999px;padding:2px 8px}
.blocker{background:color-mix(in srgb,var(--bad) 15%,transparent);color:var(--bad)}.required{background:color-mix(in srgb,var(--warn) 18%,transparent);color:var(--warn)}.recommended{background:color-mix(in srgb,var(--accent) 15%,transparent);color:var(--accent)}
.change{border-top:1px solid var(--line);padding:12px 0}.change:first-child{border-top:0}.change h3{font-size:15px;margin:4px 0}.mono{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12.5px;color:var(--ink2)}
.vals{display:grid;gap:12px;grid-template-columns:repeat(auto-fit,minmax(240px,1fr))}.val{border:1px solid var(--line);border-radius:10px;padding:14px}.val h3{font-size:15px;margin:0 0 6px}.val p{margin:0;color:var(--ink2);font-size:14px}
.pii{color:var(--bad);font-weight:600}.val p,td,.change div,ul.reasons li,.sub{overflow-wrap:anywhere}.foot{color:var(--ink2);font-size:13px;margin-top:24px}
svg text{fill:var(--ink);font:12px system-ui,sans-serif}svg .muted{fill:var(--ink2)}
</style>
</head>
<body>
<div class="wrap" id="app"></div>
<script>
/* The assessment fills DATA and nothing else; everything below draws from it. */
const DATA = __DATA__;

const PATHS = {
  framework: {label: 'Framework hook', color: 'var(--p1)'},
  client:    {label: 'Wrap the model client', color: 'var(--p2)'},
  manual:    {label: 'Report each call', color: 'var(--p3)'},
  otlp:      {label: 'OpenTelemetry export', color: 'var(--p4)'},
  unsupported: {label: 'Not supported yet', color: 'var(--p5)'},
};
const CAPS = [
  ['feature', 'Cost per feature'], ['person', 'Cost per person'], ['customer', 'Cost per customer'],
  ['request', 'Cost per request'], ['failed', 'Spend on failed calls'], ['models', 'Cheaper-model comparison'],
  ['budgets', 'Budgets that stop calls'], ['alerts', 'Spend and failure alerts'],
];
const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const svgNS = 'http://www.w3.org/2000/svg';
const sites = DATA.services.flatMap(s => s.callSites.map(c => ({...c, service: s.name})));
const models = [...new Set(DATA.services.flatMap(s => s.models))];
const effort = DATA.services.reduce((t, s) => t + (s.effortHours || 0), 0);
const blockers = DATA.changes.filter(c => c.severity === 'blocker').length;

function gauge(score) {
  const r = 70, c = 2 * Math.PI * r, color = score >= 75 ? 'var(--good)' : score >= 50 ? 'var(--warn)' : 'var(--bad)';
  return `<svg viewBox="0 0 180 180" width="180" height="180" role="img" aria-label="Readiness ${score} of 100">
    <circle cx="90" cy="90" r="${r}" fill="none" stroke="var(--line)" stroke-width="14"/>
    <circle cx="90" cy="90" r="${r}" fill="none" stroke="${color}" stroke-width="14" stroke-linecap="round"
      stroke-dasharray="${c * score / 100} ${c}" transform="rotate(-90 90 90)"/>
    <text x="90" y="92" text-anchor="middle" style="font-size:36px;font-weight:700">${score}</text>
    <text x="90" y="116" text-anchor="middle" class="muted">out of 100</text></svg>`;
}

function flow() {
  const triggers = [...new Set(DATA.services.flatMap(s => s.triggers))];
  const cols = [triggers, DATA.services.map(s => s.name), models];
  const W = 1000, rowH = 34, H = Math.max(...cols.map(c => c.length)) * rowH + 40, xs = [0, 380, 760], bw = 240;
  const y = (col, i) => 30 + i * rowH + (H - 40 - cols[col].length * rowH) / 2;
  let out = `<svg viewBox="0 0 ${W} ${H}" width="100%" role="img" aria-label="What users do, which part of the product calls a model, and which model">`;
  const link = (x1, y1, x2, y2, color) => out += `<path d="M${x1} ${y1} C${(x1 + x2) / 2} ${y1},${(x1 + x2) / 2} ${y2},${x2} ${y2}" fill="none" stroke="${color}" stroke-opacity=".45" stroke-width="2"/>`;
  DATA.services.forEach((s, si) => {
    const tally = {};
    s.callSites.forEach(c => tally[c.path] = (tally[c.path] || 0) + 1);
    const main = Object.keys(tally).sort((a, b) => tally[b] - tally[a])[0] || 'unsupported';
    const color = PATHS[main].color;
    s.triggers.forEach(t => link(xs[0] + bw, y(0, triggers.indexOf(t)) + 11, xs[1], y(1, si) + 11, color));
    s.models.forEach(m => link(xs[1] + bw, y(1, si) + 11, xs[2], y(2, models.indexOf(m)) + 11, color));
  });
  ['What people do', 'Agents and services', 'Models'].forEach((h, i) => out += `<text x="${xs[i]}" y="14" class="muted" style="font-weight:600">${h}</text>`);
  cols.forEach((col, ci) => col.forEach((label, i) => {
    out += `<rect x="${xs[ci]}" y="${y(ci, i)}" width="${bw}" height="22" rx="6" fill="var(--card)" stroke="var(--line)"/>`;
    out += `<text x="${xs[ci] + 8}" y="${y(ci, i) + 15}">${esc(label.length > 34 ? label.slice(0, 33) + '…' : label)}</text>`;
  }));
  return out + '</svg>';
}

function coverage() {
  const counts = Object.keys(PATHS).map(p => [p, sites.filter(s => s.path === p).length]).filter(([, n]) => n);
  const total = sites.length || 1;
  let x = 0, bar = '';
  counts.forEach(([p, n]) => { const w = n / total * 100; bar += `<rect x="${x}%" y="0" width="${w}%" height="28" fill="${PATHS[p].color}"/>`; x += w; });
  return `<svg width="100%" height="28" role="img" aria-label="Model call sites by how they connect">${bar}</svg>
    <div class="legend">${counts.map(([p, n]) => `<span><i class="sw" style="background:${PATHS[p].color}"></i>${PATHS[p].label}: ${n}</span>`).join('')}</div>`;
}

function effortBars() {
  const max = Math.max(1, ...DATA.services.map(s => s.effortHours || 0));
  return DATA.services.map(s => `<div style="display:grid;grid-template-columns:200px 1fr 60px;gap:10px;align-items:center;margin:6px 0">
    <span>${esc(s.name)}</span><svg height="14" width="100%"><rect width="${(s.effortHours || 0) / max * 100}%" height="14" rx="3" fill="var(--accent)"/></svg>
    <span class="mono">${s.effortHours ?? '?'} h</span></div>`).join('');
}

function matrix() {
  const mark = v => v === 'yes' ? '<span class="dot" style="color:var(--good)">●</span>' : v === 'after' ? '<span class="dot" style="color:var(--warn)">◐</span>' : '<span class="dot" style="color:var(--ink2)">○</span>';
  return `<div class="scroll"><table><thead><tr><th>Agent or service</th>${CAPS.map(([, l]) => `<th class="c">${l}</th>`).join('')}</tr></thead><tbody>
    ${DATA.services.map(s => `<tr><td><b>${esc(s.name)}</b><div class="mono">${esc(s.path)}</div></td>${CAPS.map(([k]) => `<td class="c">${mark((s.capabilities || {})[k])}</td>`).join('')}</tr>`).join('')}
    </tbody></table></div><div class="legend"><span>● on day one</span><span>◐ once a listed change is made</span><span>○ not applicable</span></div>`;
}

const app = document.getElementById('app');
app.innerHTML = `
  <p class="mono">Agent Pulse readiness assessment · ${esc(DATA.generatedAt)}</p>
  <h1>${esc(DATA.product)}</h1><p class="sub">${esc(DATA.summary)}</p>
  <div class="card hero"><div>${gauge(DATA.readiness.score)}</div><div>
    <h2>${esc(DATA.readiness.verdict)}</h2><ul class="reasons">${DATA.readiness.reasons.map(r => `<li>${esc(r)}</li>`).join('')}</ul></div></div>
  <div class="grid">
    <div class="kpi"><b>${DATA.services.length}</b><span>agents and services that call a model</span></div>
    <div class="kpi"><b>${sites.length}</b><span>model call sites found</span></div>
    <div class="kpi"><b>${models.length}</b><span>models in use</span></div>
    <div class="kpi"><b>${blockers}</b><span>blockers to clear first</span></div>
    <div class="kpi"><b>${effort} h</b><span>estimated work to connect</span></div>
  </div>
  <div class="card"><h2>Where your AI spend happens</h2><p class="sub">Every path from something a person does to a model call. Each line is coloured by how that part would connect.</p>${flow()}</div>
  <div class="card"><h2>What Agent Pulse would show you</h2><p class="sub">Questions about your product that the console answers once connected, each tied to code in this repository.</p>
    <div class="vals">${DATA.value.map(v => `<div class="val"><h3>${esc(v.question)}</h3><p>${esc(v.evidence)}</p></div>`).join('')}</div></div>
  <div class="card"><h2>Questions you can answer, by agent and service</h2>${matrix()}</div>
  ${DATA.risks.length ? `<div class="card"><h2>What it would surface early</h2><p class="sub">Patterns in the code that drive spend, which Agent Pulse makes visible.</p>
    <div class="vals">${DATA.risks.map(r => `<div class="val"><h3>${esc(r.title)}</h3><p>${esc(r.evidence)}</p></div>`).join('')}</div></div>` : ''}
  <div class="card"><h2>How each call site connects</h2>${coverage()}
    <div class="scroll" style="margin-top:14px"><table><thead><tr><th>Where</th><th>Service</th><th>How it calls a model</th><th>How Agent Pulse connects</th></tr></thead><tbody>
    ${sites.map(c => `<tr><td class="mono">${esc(c.file)}${c.line ? ':' + c.line : ''}</td><td>${esc(c.service)}</td><td>${esc(c.how)}</td><td><i class="sw" style="background:${PATHS[c.path].color}"></i>${PATHS[c.path].label}</td></tr>`).join('')}
    </tbody></table></div></div>
  <div class="card"><h2>What has to change</h2><p class="sub">In order. Blockers first.</p>
    ${DATA.changes.map(c => `<div class="change"><span class="badge ${c.severity}">${c.severity}</span> <span class="mono">${esc(c.service)}${c.effortHours ? ' · ' + c.effortHours + ' h' : ''}</span>
      <h3>${esc(c.title)}</h3><div>${esc(c.detail)}</div>${c.file ? `<div class="mono">${esc(c.file)}</div>` : ''}</div>`).join('')}</div>
  <div class="card"><h2>Work to connect, by agent and service</h2>${effortBars()}</div>
  <div class="card"><h2>Who and what each call is attributed to</h2><p class="sub">Agent Pulse records identifiers, never names or emails on a call.</p>
    <div class="scroll"><table><thead><tr><th>In Agent Pulse</th><th>From your code</th><th>Note</th></tr></thead><tbody>
    ${DATA.identity.map(i => `<tr><td><b>${esc(i.concept)}</b></td><td class="mono">${esc(i.theirField)}</td><td>${i.pii ? '<span class="pii">Personal data: </span>' : ''}${esc(i.note)}</td></tr>`).join('')}
    </tbody></table></div></div>
  <div class="card"><h2>Next steps</h2><ol>${DATA.nextSteps.map(s => `<li>${esc(s)}</li>`).join('')}</ol></div>
  <p class="foot">Made by reading this repository. Nothing was changed, run or sent anywhere. Figures are counts from the code, not measured spend; Agent Pulse measures that once connected.</p>`;
</script>
</body>
</html>
```


## What it never does

- **Changes nothing.** It reads your code, and installs, builds and runs nothing.
- **Sends nothing anywhere.** It makes no network requests, opens no `.env` file and prints no secret.
- **Writes one file**, `agentpulse-assessment.html`, at the root of your repository.

## What you get

One page you can open in a browser and share with your team:

- A readiness score and what it means.
- Every place your code calls a model, and how each would connect.
- What has to change first, with hours for each change.
- The questions Agent Pulse would answer about your product.

Every finding names a file and a line. It gives no dollar figures, because nothing has been measured yet.

To see a report for a made-up product, open the prompt and choose **Preview** on its template.
