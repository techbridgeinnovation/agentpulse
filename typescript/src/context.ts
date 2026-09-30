// Who a piece of work is for, carried on the async context rather than through every function between the handler and the model call.
//
// A service that threads these as parameters passes them through code that has no other reason to know about them, and the ones deepest down are the ones that get dropped. Set them once where a request starts, with `scope`. They follow the work across `await`, timers and promises started inside it.

import { AsyncLocalStorage } from "node:async_hooks";

/**
 * The person a piece of work is for.
 *
 * The identifier is what every record carries, and it is the whole of what a report can group by. The name and email never travel on a record: they go once per person to a directory the report is joined to.
 */
export interface User {
  /** The identifier the product's own sign-in issued, bare, e.g. `8c21e0b4`. It must not contain a slash, because it becomes the last segment of the directory row's name. */
  id: string;
  name?: string;
  email?: string;
}

export interface Scope {
  /** One end-user request. Everything recorded under it groups together, which is what makes cost per unit of business work possible. */
  request?: string;
  /** The person, set where the sign-in has been checked. */
  user?: User;
  session?: string;
  /** The tenant of the organisation, bare, e.g. `acme`. Set here and deliberately not at a model call site: a value a call site can choose is a value that can attribute one tenant's spend to another. */
  workspace?: string;
  /** A unit of work inside the tenant, and a label only: nothing is authorised against it. */
  project?: string;
  /** The part of the product a model call belongs to. */
  component?: string;
}

const storage = new AsyncLocalStorage<Scope>();

/** Runs `fn` as belonging to what `values` names. What is not named keeps the value it already had. */
export function scope<T>(values: Scope, fn: () => T): T {
  const current = storage.getStore() ?? {};
  const merged: Scope = { ...current };
  for (const [name, value] of Object.entries(values)) {
    if (value !== undefined) (merged as Record<string, unknown>)[name] = value;
  }
  return storage.run(merged, fn);
}

export function currentScope(): Scope {
  return storage.getStore() ?? {};
}

/** The platform's name for a tenant, e.g. `organisations/dealade/workspaces/acme`, or an empty string for no workspace. */
export function workspaceName(organisation: string, workspace: string | undefined): string {
  return workspace ? `${organisation}/workspaces/${workspace}` : "";
}
