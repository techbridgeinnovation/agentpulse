"""Asking governance whether a call may proceed, before it is made.

Kept apart from the record path: deciding is a synchronous question on the calling request's own critical path, nothing like a batch handed to the background worker, so it has no place on the recorder's queue.

Only a genuine DENY stops a call. Every other answer, and no answer at all — governance unreachable, slow, refusing the question, or saying something this version does not know — lets it go ahead, because a spend decision that cannot be made must not be the reason the product stops working. Each of those is counted, so a degraded governance is visible rather than silent.
"""

from __future__ import annotations

import threading
import time
from dataclasses import dataclass
from typing import Callable, Protocol

from . import _wire
from .gateway import Gateway

_DECIDE = "/techbridge.ap.governance.v1.DecisionsService/Decide"

# How long a call waits for an answer before it goes ahead without one.
DEFAULT_DECIDE_TIMEOUT = 0.5

# How long, in seconds, a question that just failed is not asked again. Without it a governance that is down costs every call the whole timeout for as long as the outage lasts, which is the host being slowed by the thing that is meant to watch it; with it, an outage costs one timeout per question per interval.
DEFAULT_FAILURE_BACKOFF = 5.0

# The most verdicts one cache holds. A decision context includes the person and the tenant, so a service with many of either would otherwise grow the cache without end.
_MAX_CACHED_VERDICTS = 4096

ALLOW = "ALLOW"
NOTIFY = "NOTIFY"
DOWNGRADE = "DOWNGRADE"
DENY = "DENY"
# No answer was reached, and the call goes ahead.
UNDECIDED = "UNDECIDED"

_NAMES = {
    _wire.DECISION_ALLOW: ALLOW,
    _wire.DECISION_NOTIFY: NOTIFY,
    _wire.DECISION_DOWNGRADE: DOWNGRADE,
    _wire.DECISION_DENY: DENY,
}


@dataclass(frozen=True)
class Verdict:
    """What governance decided about one call."""

    decision: str = UNDECIDED
    # Why, as governance put it. Its own words, never a provider's.
    reason: str = ""
    # For a DOWNGRADE, the provider and model to call instead. Applying them is the caller's to do, and a replacement served by a different provider than the one the caller talks to cannot be applied by changing the model name alone.
    replacement_provider: str = ""
    replacement_model: str = ""

    @property
    def proceed(self) -> bool:
        """False only for a genuine DENY."""
        return self.decision != DENY


class GovernanceUnavailable(Exception):
    """A question governance failed to answer a moment ago, not asked again yet."""


class Decider(Protocol):
    """Asks governance for a verdict. Raises on any failure; the caller treats a failure as no answer."""

    def decide(self, request: _wire.DecideRequest, timeout: float) -> _wire.DecideResponse: ...


class GatewayDecider:
    """Asks the governance service through the gateway, on the same connection the records travel on."""

    def __init__(self, gateway: Gateway):
        self._gateway = gateway

    def decide(self, request: _wire.DecideRequest, timeout: float) -> _wire.DecideResponse:
        return _wire.DecideResponse.decode(self._gateway.call(_DECIDE, request.encode(), timeout))


_Key = tuple[str, str, str, str, str, str, str]


def _key_of(request: _wire.DecideRequest) -> _Key:
    # Every part of the question is part of the key. A budget can be narrowed to a tenant, a project, a person or an agent, so two calls that differ in any of them are two questions with two answers, and the requested model is part of it because a cached DOWNGRADE names a replacement for the model it was asked about.
    return (request.parent, request.workspace, request.project, request.user, request.agent, request.requested_provider, request.requested_model)


class _Inflight:
    def __init__(self) -> None:
        self.done = threading.Event()
        self.response: _wire.DecideResponse | None = None
        self.error: BaseException | None = None


class CachingDecider:
    """A decider that reuses a verdict for `ttl` seconds, so a warm call answers from memory instead of asking governance again.

    Only an answer is cached. A failure is remembered for `failure_backoff` seconds and the same question fails at once in that time, without waiting, so an outage slows no call past the first; after it the question is asked again. Concurrent calls asking the same question share one request to governance, and each still waits no longer than its own timeout for it.

    `ttl` is required: how long a verdict may be reused is a policy choice, and it decides how far past a ceiling spend can go, since a cached ALLOW is honoured until it expires.
    """

    def __init__(
        self,
        inner: Decider,
        ttl: float,
        *,
        failure_backoff: float = DEFAULT_FAILURE_BACKOFF,
        max_entries: int = _MAX_CACHED_VERDICTS,
        now: Callable[[], float] = time.monotonic,
    ):
        self._inner = inner
        self._ttl = ttl
        self._backoff = max(0.0, failure_backoff)
        self._failed: dict[_Key, float] = {}
        self._max = max(1, max_entries)
        self._now = now
        self._lock = threading.Lock()
        self._entries: dict[_Key, tuple[_wire.DecideResponse, float]] = {}
        self._inflight: dict[_Key, _Inflight] = {}

    def cached(self, request: _wire.DecideRequest) -> _wire.DecideResponse | None:
        """The live verdict for exactly this question, if one is cached."""
        key = _key_of(request)
        with self._lock:
            return self._live(key)

    def _live(self, key: _Key) -> _wire.DecideResponse | None:
        entry = self._entries.get(key)
        if entry is None:
            return None
        if self._now() < entry[1]:
            return entry[0]
        del self._entries[key]
        return None

    def decide(self, request: _wire.DecideRequest, timeout: float) -> _wire.DecideResponse:
        key = _key_of(request)
        with self._lock:
            live = self._live(key)
            if live is not None:
                return live
            until = self._failed.get(key)
            if until is not None:
                if self._now() < until:
                    raise GovernanceUnavailable()
                del self._failed[key]
            call = self._inflight.get(key)
            leader = call is None
            if leader:
                call = self._inflight[key] = _Inflight()

        if not leader:
            if not call.done.wait(timeout):
                raise TimeoutError("waiting on a decision another call is asking for")
            if call.error is not None:
                raise call.error
            assert call.response is not None
            return call.response

        try:
            call.response = self._inner.decide(request, timeout)
        except BaseException as err:
            call.error = err
            self._back_off(key)
            raise
        else:
            self._store(key, call.response)
            return call.response
        finally:
            with self._lock:
                self._inflight.pop(key, None)
            call.done.set()

    def note_failure(self, request: _wire.DecideRequest) -> None:
        """Records that a caller stopped waiting for this question. A governance too slow to answer within the timeout is as unavailable to the caller as one that refused, and backing off from it is what stops the next call waiting too."""
        self._back_off(_key_of(request))

    def _back_off(self, key: _Key) -> None:
        if self._backoff <= 0:
            return
        with self._lock:
            if len(self._failed) >= self._max:
                self._failed.clear()
            self._failed[key] = self._now() + self._backoff

    def _store(self, key: _Key, response: _wire.DecideResponse) -> None:
        if self._ttl <= 0:
            return
        with self._lock:
            now = self._now()
            if len(self._entries) >= self._max and key not in self._entries:
                for stale in [k for k, (_, expires) in self._entries.items() if expires <= now]:
                    del self._entries[stale]
                if len(self._entries) >= self._max:
                    # The oldest eighth goes, which is cheaper than finding the single oldest on every store once the cache is full.
                    for oldest in list(self._entries)[: max(1, self._max // 8)]:
                        del self._entries[oldest]
            self._entries[key] = (response, now + self._ttl)

    def invalidate_scope(self, parent: str, *, workspace: str = "", project: str = "", user: str = "", agent: str = "") -> None:
        """Drops every cached verdict within a scope, where an empty part means any: a parent alone drops the whole organisation's verdicts. For when a budget is known to have changed."""
        with self._lock:
            for key in list(self._entries):
                entry_parent, entry_workspace, entry_project, entry_user, entry_agent = key[:5]
                if entry_parent != parent:
                    continue
                if (workspace and entry_workspace != workspace) or (project and entry_project != project):
                    continue
                if (user and entry_user != user) or (agent and entry_agent != agent):
                    continue
                del self._entries[key]


def _bounded(fn: Callable[[], _wire.DecideResponse], timeout: float) -> _wire.DecideResponse:
    """Runs a decision on its own thread and stops waiting for it at the deadline.

    A socket timeout bounds each read and each write on its own, not the whole exchange, and not the name lookup before it. The only way to promise a call waits no longer than its timeout, whatever the network does, is to stop waiting. The thread is a daemon and finishes or times out on its own, so neither a slow governance nor interpreter exit waits on it, and only a call that missed the cache comes here.
    """
    done = threading.Event()
    outcome: list = []

    def run() -> None:
        try:
            outcome.append((fn(), None))
        except BaseException as err:
            outcome.append((None, err))
        finally:
            done.set()

    threading.Thread(target=run, name="agentpulse-decide", daemon=True).start()
    if not done.wait(timeout):
        raise TimeoutError("governance did not answer in time")
    response, error = outcome[0]
    if error is not None:
        raise error
    return response


def ask(decider: Decider, request: _wire.DecideRequest, timeout: float) -> Verdict:
    """Asks `decider`, waiting no longer than `timeout` seconds. Raises on any failure, a timeout included."""
    cached = getattr(decider, "cached", None)
    response = cached(request) if callable(cached) else None
    if response is None:
        try:
            response = _bounded(lambda: decider.decide(request, timeout), timeout)
        except TimeoutError:
            note_failure = getattr(decider, "note_failure", None)
            if callable(note_failure):
                note_failure(request)
            raise
    return Verdict(
        decision=_NAMES.get(response.decision, UNDECIDED),
        reason=response.reason,
        replacement_provider=response.replacement_provider,
        replacement_model=response.replacement_model,
    )
