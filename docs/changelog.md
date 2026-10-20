---
title: Changelog
summary: How the API changes, and recorder releases, newest first.
section: more
order: 3
---

# Changelog

## How the API changes

- **Added at any time:** new methods, new fields in a reply, new optional parameters, new values a dimension or reason can take. Write your code to ignore what it does not know.
- **Never in `/v1`:** removing or renaming a method, field or parameter, or changing what one means. Such a change comes in `/v2`, and `/v1` keeps working for six months after it with `Deprecation` and `Sunset` headers on every reply.
- Every change to the API is listed here.

| Date | API |
| --- | --- |
| 20 Oct 2026 | A `workspace` or `agent` filter takes the short id as well as the full name; replies carry `RateLimit-Policy` with the limit's window; a badly written time or `orderBy` names its parameter; the reference drops the outcomes and prices `filter`, which was never served |
| 20 Oct 2026 | Errors carry `reason`, `field` and `requestId`; replies carry `X-Request-Id` and `RateLimit-*` headers; empty lists and zero figures are always present; dimensions, filter fields and `readMask` accept camelCase and snake_case; `status`, `error_class` and `kind` filter values are matched in any case and refused outside their set; a filtered list no longer repeats an item between pages |

## Recorder releases

Recorder releases, newest first. Each is a tag on [github.com/techbridgeinnovation/agentpulse](https://github.com/techbridgeinnovation/agentpulse/releases), Python releases from 0.6.1 are also on [PyPI](https://pypi.org/project/techbridge-agentpulse/), and TypeScript releases from 0.2.1 on [npm](https://www.npmjs.com/package/techbridge-agentpulse).

| Date | Release |
| --- | --- |
| 14 Oct 2026 | Python 0.6.1, the first on PyPI; TypeScript 0.2.1, the first on npm |
| 13 Oct 2026 | Python 0.6.0, TypeScript 0.2.0, Go recorder 0.3.0, Go ADK 2 hooks 0.3.0 |
| 6 Oct 2026 | TypeScript 0.1.1 |
| 5 Oct 2026 | TypeScript 0.1.0 |
| 2 Oct 2026 | Python 0.5.0 |
| 2 Oct 2026 | Go ADK 2 hooks 0.2.0 |
| 1 Oct 2026 | Go recorder 0.2.0 |
| 30 Sep 2026 | Python 0.4.0 |
| 30 Sep 2026 | Go recorder 0.1.1, Go ADK 2 hooks 0.1.1 |
| 29 Sep 2026 | Python 0.3.0, Python 0.2.0 |
| 29 Sep 2026 | Go recorder 0.1.0, Go ADK 2 hooks 0.1.0 |
| 25 Sep 2026 | Python 0.1.1, Python 0.1.0 |

Install a version by its number or tag:

```bash [Python]
pip install techbridge-agentpulse==0.6.1
```

```bash [TypeScript]
npm install techbridge-agentpulse@0.2.1
```

```bash [Go]
go get github.com/techbridgeinnovation/agentpulse/recorder@v0.3.0
```
