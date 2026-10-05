---
title: Track cost per customer and user
summary: Tag each request with the customer and the signed-in user.
section: guides
order: 1
---

# Track cost per customer and user

Tag each request with your customer and the signed-in user, and every model and tool call inside it is counted against them.

## What you get

- Spend by customer on the Home page, for any day, week or month.
- Spend by user, with names if you send them.
- The same figures over the API, grouped by `workspace` or `user`.
- Budgets narrowed to one customer, or set for one user.

## Tag the request

Set it where your code has checked the sign-in, once per request. A workspace is your customer's identifier, such as `brightline`. A user is the identifier your sign-in issued, never an email address.

```python [Python]
with agentpulse.scope(
    request=request_id,
    workspace=account.slug,
    user=agentpulse.User(id=claims.subject, name=claims.name),
):
    handle(request)
```

```ts [TypeScript]
await scope(
  {
    request: requestId,
    workspace: account.slug,
    user: { id: claims.sub, name: claims.name },
  },
  () => handle(req),
);
```

```go [Go]
ctx = recorder.WithWorkspace(ctx, account.Slug)
ctx = recorder.WithUser(recorder.WithRequest(ctx, requestID), recorder.User{
    ID:   claims.Subject,
    Name: claims.Name,
})
```

Set it here rather than at each model call. A value a call site chooses is a value that can put one customer's spend under another.

> **Only one tenant?** Set no workspace. Everything is filed under `default`.

## Show names instead of identifiers

Pass the user's name, and email if you want it, with their identifier, as above. Agent Pulse stores them once and shows the name wherever that user appears. The records themselves only ever carry the identifier.

Send no names and the console shows identifiers. Nothing is missing; Agent Pulse never sees your sign-in, so it cannot look them up.

## Track projects inside a customer

If your product has a unit of work inside a customer, such as a matter or a campaign, set a project too:

```python [Python]
with agentpulse.scope(workspace="brightline", project="matter-1183"):
    handle(request)
```

```go [Go]
ctx = recorder.WithProject(recorder.WithWorkspace(ctx, "brightline"), "matter-1183")
```

A project is a label for grouping. Access is never decided by it.

## See it

On **Home**, choose **by customer** or **by user**. Over the API, ask Aggregate spend with `groupBy=workspace` or `groupBy=user`. To read one customer only, use the workspace path, `/v1/organisations/{organisation}/workspaces/{workspace}/activities:aggregate`.
