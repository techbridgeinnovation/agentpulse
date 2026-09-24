"""Governance telling a running process that a budget changed, so a cached verdict is dropped at once rather than when it expires.

A latency improvement on top of the cache's own lifetime and never a guarantee: without it a changed budget reaches this process within `cache_ttl` seconds, and with it, within the time a message takes to arrive. So it is started lazily by the first spend decision, reconnects on its own whenever the stream ends, and backs off while governance is unreachable instead of retrying as fast as it can.
"""

from __future__ import annotations

import os
import threading

from . import _wire
from .gateway import Gateway
from .governance import CachingDecider

_STREAM = "/techbridge.ap.governance.v1.DecisionsService/StreamDecisionInvalidations"

# How long a stream may sit silent before it is reopened. Governance sends nothing when nothing changes, and a connection held open through a load balancer is cut eventually anyway; reopening on our own schedule keeps that from looking like a failure.
_IDLE = 300.0
_FIRST_RETRY = 0.05
_LONGEST_RETRY = 30.0


class InvalidationSubscriber:
    """Follows governance's invalidations for one organisation and applies each to a cache."""

    def __init__(self, gateway: Gateway, cache: CachingDecider, organisation: str):
        self._gateway = gateway
        self._cache = cache
        self._organisation = organisation
        self._lock = threading.Lock()
        self._thread: threading.Thread | None = None
        self._pid = 0
        self._stopped = threading.Event()
        self.received = 0

    def ensure_running(self) -> None:
        """Starts following if nothing is. Cheap enough to call before every decision."""
        thread = self._thread
        if thread is not None and thread.is_alive() and self._pid == os.getpid():
            return
        with self._lock:
            if self._stopped.is_set() or (self._thread is not None and self._thread.is_alive() and self._pid == os.getpid()):
                return
            thread = threading.Thread(target=self._run, name="agentpulse-invalidations", daemon=True)
            try:
                thread.start()
            except RuntimeError:
                return
            self._thread, self._pid = thread, os.getpid()

    def stop(self) -> None:
        self._stopped.set()

    def _run(self) -> None:
        retry = _FIRST_RETRY
        request = _wire._string(1, self._organisation)
        while not self._stopped.is_set():
            heard = False
            try:
                for message in self._gateway.stream(_STREAM, request, _IDLE, self._stopped):
                    heard = True
                    self._apply(message)
            except Exception:
                pass
            # A stream that delivered something was healthy, so the next one is tried at once; one that failed straight away waits longer each time.
            retry = _FIRST_RETRY if heard else min(retry * 2, _LONGEST_RETRY)
            self._stopped.wait(retry)

    def _apply(self, message: bytes) -> None:
        values: dict[int, str] = {}
        for number, value in _wire.fields(message):
            if isinstance(value, bytes):
                values[number] = value.decode("utf-8", "replace")
        parent = values.get(1, "")
        if not parent:
            return
        self.received += 1
        # The tenant and the project a verdict was reached for are not narrowed on, so a changed budget clears every tenant's verdict within its scope. That errs towards asking governance again, which costs a call; the other way round keeps a verdict governance has said is stale, which costs money.
        self._cache.invalidate_scope(parent, user=values.get(3, ""), agent=values.get(4, ""))
