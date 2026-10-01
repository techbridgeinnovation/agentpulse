"""Where records go: the metering service, or nothing at all.

A sink runs on the recorder's worker and never on the caller's turn. It is handed one workspace's records at a time and signals a failure by raising; the recorder counts it and carries on, so a sink never has to protect its host. A sink must not assume it is the only one.
"""

from __future__ import annotations

import random
import time
import uuid
from typing import Callable, Protocol, runtime_checkable

from . import _grpcweb, _wire
from .context import User, workspace_name
from .gateway import ConfigError, Gateway

_BATCH_CREATE_ACTIVITIES = "/techbridge.ap.metering.v1.ActivitiesService/BatchCreateActivities"
_BATCH_UPSERT_USERS = "/techbridge.ap.metering.v1.UsersService/BatchUpsertUsers"

# The outcomes worth another attempt: the gateway or metering busy, restarting or briefly out of reach. A refused key or a malformed batch fails the same way every time, so it is never retried.
_TRANSIENT = {_grpcweb.UNAVAILABLE, _grpcweb.RESOURCE_EXHAUSTED, _grpcweb.ABORTED, _grpcweb.DEADLINE_EXCEEDED}
_DEFAULT_RETRIES = 2
_FIRST_BACKOFF = 0.5
_MAX_BACKOFF = 4.0


@runtime_checkable
class Sink(Protocol):
    """A destination for records."""

    # Identifies the sink, so a team can tell which destination is failing.
    name: str

    def send(self, activities: list[_wire.Activity], workspace: str, timeout: float) -> None:
        """Delivers one workspace's records. An empty workspace is the ordinary case for a product with one tenant. Raises on failure."""
        ...


@runtime_checkable
class UserSink(Protocol):
    """A sink that also takes names. One that does not receives records and no names, which is right for a destination with no directory to put a name in."""

    def send_users(self, users: list[User], workspace: str, timeout: float) -> None:
        """Delivers one workspace's people to name. Raises on failure."""
        ...


class Discard:
    """Accepts everything and keeps nothing: the default, so a recorder with nowhere to send is inert rather than broken."""

    name = "discard"

    def send(self, activities: list[_wire.Activity], workspace: str, timeout: float) -> None:
        return None


class MeteringSink:
    """Records, and the names behind them, to the metering service through the gateway.

    The organisation is fixed here because one process bills one organisation, and an empty one is refused now rather than every batch being refused later. The workspace is not fixed, because one process can serve many tenants.
    """

    name = "metering"

    def __init__(self, gateway: Gateway, organisation: str = "", *, retries: int = _DEFAULT_RETRIES):
        organisation = (organisation or gateway.organisation).strip()
        if not organisation:
            raise ConfigError("agentpulse: the metering sink needs the organisation the records are filed under")
        self._gateway = gateway
        self._organisation = organisation
        self._retries = max(0, retries)
        self._sleep: Callable[[float], None] = time.sleep

    def _parent(self, workspace: str) -> str:
        # A batch naming no workspace is filed under the organisation on its own, which lands it in the organisation's default workspace. A tenant a caller did not name is never invented here.
        return workspace_name(self._organisation, workspace) or self._organisation

    def send(self, activities: list[_wire.Activity], workspace: str, timeout: float) -> None:
        """One call per batch, tried again after a transient failure. Every attempt carries the same request id, so metering stores a batch once however many attempts reached it."""
        if not activities:
            return
        message = _wire.batch_create_activities(self._parent(workspace), activities, request_id=str(uuid.uuid4()))
        self._call(_BATCH_CREATE_ACTIVITIES, message, timeout)

    def send_users(self, users: list[User], workspace: str, timeout: float) -> None:
        """Names people in the directory of the workspace their records are filed in, which is the one they join to."""
        if not users:
            return
        parent = self._parent(workspace)
        rows = [_wire.User(name=f"{parent}/users/{user.id}", display_name=user.name, email=user.email) for user in users]
        self._call(_BATCH_UPSERT_USERS, _wire.batch_upsert_users(parent, rows), timeout)

    def _call(self, method: str, message: bytes, timeout: float) -> None:
        """The call, with a few attempts more for a transient failure, all inside `timeout` so a struggling gateway holds up the worker no longer than one slow call would. Runs on the worker, never on the host's turn."""
        deadline = time.monotonic() + timeout
        backoff = _FIRST_BACKOFF
        attempt = 0
        while True:
            try:
                self._gateway.call(method, message, max(0.001, deadline - time.monotonic()))
                return
            except _grpcweb.RPCError as err:
                pause = backoff * random.uniform(0.5, 1.0)
                if err.code not in _TRANSIENT or attempt >= self._retries or time.monotonic() + pause >= deadline:
                    raise
            attempt += 1
            self._sleep(pause)
            backoff = min(backoff * 2, _MAX_BACKOFF)
