"""Who a piece of work is for, carried on the context rather than through every function between the handler and the model call.

A service that threads these as parameters passes them through code that has no other reason to know about them, and the ones deepest down are the ones that get dropped. Set them once where a request starts, with `scope`.

They are context variables, so they follow the work across `await` and into `asyncio.to_thread`, and they do not follow it into a thread started by hand or a plain executor. `carry` takes the current values along into one of those.
"""

from __future__ import annotations

import contextlib
import contextvars
import functools
from dataclasses import dataclass
from typing import Any, Callable, Iterator, TypeVar

T = TypeVar("T")


@dataclass(frozen=True)
class User:
    """The person a piece of work is for.

    The identifier is what every record carries, and it is the whole of what a report can group by. The name and email travel differently: never on a record, but once per person to a directory the report is joined to.
    """

    # The identifier the product's own sign-in issued, bare, e.g. `8c21e0b4` rather than `users/8c21e0b4`. It must not contain a slash, because it becomes the last segment of the directory row's name.
    id: str
    name: str = ""
    email: str = ""

    def named(self) -> bool:
        return bool(self.name or self.email)


_request: contextvars.ContextVar[str] = contextvars.ContextVar("agentpulse_request", default="")
_user: contextvars.ContextVar[User | None] = contextvars.ContextVar("agentpulse_user", default=None)
_session: contextvars.ContextVar[str] = contextvars.ContextVar("agentpulse_session", default="")
_workspace: contextvars.ContextVar[str] = contextvars.ContextVar("agentpulse_workspace", default="")
_project: contextvars.ContextVar[str] = contextvars.ContextVar("agentpulse_project", default="")
_component: contextvars.ContextVar[str] = contextvars.ContextVar("agentpulse_component", default="")

_UNSET: Any = object()


@contextlib.contextmanager
def scope(
    *,
    request: str = _UNSET,
    user: User | None = _UNSET,
    session: str = _UNSET,
    workspace: str = _UNSET,
    project: str = _UNSET,
    component: str = _UNSET,
) -> Iterator[None]:
    """Marks the work inside the block as belonging to a request, a person, a tenant and so on. What is not named keeps the value it already had.

    `request` is one end-user request; everything recorded under it groups together, which is what makes cost per unit of business work possible.

    `user` is the person, set where the sign-in has been checked, which is the one place a product has the identifier and the name in hand together.

    `workspace` is the tenant of the organisation, bare, e.g. `acme`; the organisation is already known from the key. `project` is a unit of work inside it and a label only: nothing is authorised against it.

    `component` names the part of the product a model call belongs to, where the function that makes the call is not a good enough name.

    The tenant is set here and deliberately not at a model call site: a value a call site can choose is a value that can attribute one tenant's spend to another, and a figure wrong that way looks exactly like one that is right.
    """
    tokens = []
    for var, value in (
        (_request, request),
        (_user, user),
        (_session, session),
        (_workspace, workspace),
        (_project, project),
        (_component, component),
    ):
        if value is not _UNSET:
            tokens.append((var, var.set(value)))
    try:
        yield
    finally:
        for var, token in reversed(tokens):
            var.reset(token)


def current_request() -> str:
    return _request.get()


def current_user() -> User | None:
    return _user.get()


def current_session() -> str:
    return _session.get()


def current_workspace() -> str:
    return _workspace.get()


def current_project() -> str:
    return _project.get()


def current_component() -> str:
    return _component.get()


def carry(fn: Callable[..., T]) -> Callable[..., T]:
    """`fn`, bound to the request, person and tenant in force now, for handing to a thread or an executor that would otherwise run it with none of them."""
    context = contextvars.copy_context()

    @functools.wraps(fn)
    def run(*args: Any, **kwargs: Any) -> T:
        return context.run(fn, *args, **kwargs)

    return run


def workspace_name(organisation: str, workspace: str) -> str:
    """The platform's name for a tenant, e.g. `organisations/dealade/workspaces/acme`, or an empty string for no workspace."""
    if not workspace:
        return ""
    return f"{organisation}/workspaces/{workspace}"
