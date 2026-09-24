"""The queue, the worker, the counters.

The first rule of this library is that it must never degrade its host. Recording never blocks, never raises and never waits on the network: a record goes into a bounded buffer and one background thread delivers it. A full buffer drops the record and counts it, because losing a record is always preferable to delaying the work the agent was asked to do. That ranks above completeness of data: telemetry that can break production gets removed, and rightly.

Dropped records are counted and the count is readable in `stats()`. That counter is the one thing here that cannot be turned off, because silent loss is worse than visible loss.

A thread rather than an asyncio task, because a thread works the same in a synchronous service and an asynchronous one, and never touches the host's event loop. It starts on the first record, so importing the library or building a recorder that never records starts nothing.
"""

from __future__ import annotations

import atexit
import collections
import os
import threading
import time
import weakref
from dataclasses import dataclass, field
from typing import Any, Iterable

from . import _wire, pricing
from .context import User, current_workspace
from .sinks import Discard, Sink

_DEFAULT_QUEUE_SIZE = 2048
_DEFAULT_BATCH_SIZE = 100
_DEFAULT_FLUSH_EVERY = 2.0
_DEFAULT_SEND_TIMEOUT = 10.0
_DEFAULT_EXIT_TIMEOUT = 2.0
# How often, in seconds, the rate card is read again. A rate card is a price list a person maintains, so it changes rarely, and this bounds how long a model added to it stays unpriced here.
_DEFAULT_RATE_REFRESH = 15 * 60.0

# What the recorder remembers about who it has named. Past it, everything is forgotten and people are named again as they reappear; a second write of the same name is harmless.
_MAX_REMEMBERED_NAMES = 10_000


@dataclass
class Config:
    """Where records go and how much memory the recorder may use holding them.

    Every field has a working default, so `Recorder(Config())` is valid and inert. There is deliberately no way to make the queue unbounded or to switch the dropped counter off: an unbounded queue turns a slow destination into the host agent's memory leak.
    """

    # Each sink receives every record, independently, so one failing does not affect the others. Empty means discard.
    sinks: list[Sink] = field(default_factory=list)

    # How many records may wait for delivery. Once full, new records are dropped and counted.
    queue_size: int = _DEFAULT_QUEUE_SIZE

    # The most records handed to a sink at once.
    batch_size: int = _DEFAULT_BATCH_SIZE

    # How long, in seconds, a record may wait before delivery, so a quiet agent still reports.
    flush_every: float = _DEFAULT_FLUSH_EVERY

    # The longest one delivery may take, in seconds, so a hung destination cannot stall the worker for ever.
    send_timeout: float = _DEFAULT_SEND_TIMEOUT

    # How long, in seconds, the process waits at exit for what is still queued. A short, hard limit: shutting down is not a reason to hang. Zero leaves the flush at exit to the host.
    exit_timeout: float = _DEFAULT_EXIT_TIMEOUT

    # Where the prices the per-request running total is measured against come from, read in the background and never on the path of a model call. Optional: without one every record adds nothing and `spent_on` answers zero.
    rates: pricing.RateSource | None = None

    # How often, in seconds, the rate card is read again.
    rate_refresh: float = _DEFAULT_RATE_REFRESH

    def with_defaults(self) -> "Config":
        return Config(
            sinks=list(self.sinks) or [Discard()],
            queue_size=self.queue_size if self.queue_size >= 1 else _DEFAULT_QUEUE_SIZE,
            batch_size=self.batch_size if self.batch_size >= 1 else _DEFAULT_BATCH_SIZE,
            flush_every=self.flush_every if self.flush_every > 0 else _DEFAULT_FLUSH_EVERY,
            send_timeout=self.send_timeout if self.send_timeout > 0 else _DEFAULT_SEND_TIMEOUT,
            exit_timeout=max(0.0, self.exit_timeout),
            rates=self.rates,
            rate_refresh=self.rate_refresh if self.rate_refresh > 0 else _DEFAULT_RATE_REFRESH,
        )


@dataclass(frozen=True)
class Stats:
    """What the recorder has done, as a snapshot."""

    # Records accepted.
    recorded: int = 0
    # Records discarded because the queue was full or the recorder was closed. Above zero means cost data is incomplete.
    dropped: int = 0
    # Records that reached a sink.
    delivered: int = 0
    # Records a sink refused or could not be reached with.
    failed: int = 0
    # Times something run on the host's behalf raised where it never should, such as the recorder building a record. Above zero means this library has a bug; the agent was unaffected.
    panicked: int = 0
    # Spend checks that came back NOTIFY, DENY or DOWNGRADE, and ones that could not be completed.
    notified: int = 0
    denied: int = 0
    downgraded: int = 0
    decision_errors: int = 0
    # Downgrades an instrumented client rewrote onto the outbound call, and ones it could not safely apply, which went ahead with the model first asked for.
    downgrade_applied: int = 0
    downgrade_not_applied: int = 0
    # Rate card reads that failed. Above zero and climbing means the running total is priced against a card going stale, or against none.
    rate_fetch_errors: int = 0
    # Running totals dropped because more requests were in flight at once than are kept. Above zero means `spent_on` is an undercount for some request still running.
    totals_evicted: int = 0
    # People queued for the directory, not queued because the queue was full, and not named because a sink refused them or their identifier holds a slash.
    named: int = 0
    names_dropped: int = 0
    names_failed: int = 0


class Recorder:
    """Accepts records from an agent and delivers them in the background."""

    def __init__(self, config: Config | None = None):
        self._config = (config or Config()).with_defaults()
        self._counters: collections.Counter[str] = collections.Counter()
        self._card: pricing.RateCard | None = None
        self._reset_state()

        # A process that forks carries this recorder into the child without its worker, since only the thread that forked survives. The child starts from empty, with its own lock and its own worker, and anything the parent had queued stays the parent's to send, or it would be sent twice.
        if hasattr(os, "register_at_fork"):
            ref = weakref.ref(self)
            os.register_at_fork(after_in_child=lambda: _call(ref, "_after_fork"))
        # The card is read from the start, where one is configured, so the first call a process makes is priced rather than free.
        if self._config.rates is not None:
            with self._lock:
                self._start_rates()

        exit_timeout = self._config.exit_timeout
        if exit_timeout > 0:
            ref = weakref.ref(self)
            atexit.register(lambda: _call(ref, "close", exit_timeout))

    def _reset_state(self) -> None:
        self._lock = threading.Lock()
        self._wake = threading.Condition(self._lock)
        self._records: collections.deque[tuple[_wire.Activity, str]] = collections.deque()
        self._people: collections.deque[tuple[User, str]] = collections.deque()
        self._seen: dict[tuple[str, str], User] = {}
        self._flushes: list[threading.Event] = []
        self._thread: threading.Thread | None = None
        self._rates_thread: threading.Thread | None = None
        self._stopped = threading.Event()
        self._totals = pricing.RunningTotals()
        self._pid = os.getpid()
        self._closing = False

    def _after_fork(self) -> None:
        self._reset_state()

    def record(self, activity: _wire.Activity, workspace: str | None = None) -> None:
        """Hands over one record and returns immediately.

        Never blocks, never raises. The record is filed under the workspace in force on the current context, which is the tenant the request was authenticated for, unless the caller states one; a product with one tenant has neither and the record is filed under the organisation.
        """
        try:
            if workspace is None:
                workspace = current_workspace()
            # Kept whether or not the record survives the queue: a dropped record still cost money, and a spend decision that ignored it would be wrong in the one direction that matters.
            card = self._card
            if card is not None and activity.request:
                self._totals.add(activity.request, pricing.price_of(card, activity))
            with self._lock:
                if not self._ensure_worker():
                    self._counters["dropped"] += 1
                    return
                if len(self._records) >= self._config.queue_size:
                    self._counters["dropped"] += 1
                    return
                self._records.append((activity, workspace))
                self._counters["recorded"] += 1
                if len(self._records) >= self._config.batch_size:
                    self._wake.notify()
        except Exception:
            self._note("panicked")

    def note_user(self, user: User | None, workspace: str | None = None) -> None:
        """Hands over a person to name and returns immediately.

        A person with no name and no email is nothing to say and is ignored. One whose identifier holds a slash cannot be named, because the identifier becomes a path segment, and is counted as failed so the mistake is visible. Someone already named with the same name costs a dictionary lookup.
        """
        try:
            if user is None or not user.id or not user.named():
                return
            if "/" in user.id:
                self._note("names_failed")
                return
            if workspace is None:
                workspace = current_workspace()
            key = (workspace, user.id)
            with self._lock:
                if self._seen.get(key) == user:
                    return
                if not self._ensure_worker() or len(self._people) >= self._config.queue_size:
                    self._counters["names_dropped"] += 1
                    return
                self._people.append((user, workspace))
                if len(self._seen) >= _MAX_REMEMBERED_NAMES:
                    self._seen.clear()
                self._seen[key] = user
                self._counters["named"] += 1
                if len(self._people) >= self._config.batch_size:
                    self._wake.notify()
        except Exception:
            self._note("panicked")

    def stats(self) -> Stats:
        with self._lock:
            counts = {name: self._counters[name] for name in Stats.__dataclass_fields__}
        counts["totals_evicted"] = self._totals.evicted
        return Stats(**counts)

    def spent_on(self, request: str) -> int:
        """What a request has cost so far in this process, in millionths of a dollar, priced against the rate card in hand. Zero without a rate source, or before the first card arrives."""
        try:
            return self._totals.spent_on(request)
        except Exception:
            return 0

    def finish_request(self, request: str) -> None:
        """Releases a request's running total. An entry expires on its own, but releasing it keeps the totals to the requests actually in flight, and an eviction under the cap is an undercount for one still running."""
        try:
            self._totals.finish(request)
        except Exception:
            pass

    def flush(self, timeout: float = 5.0) -> bool:
        """Delivers everything queued so far and returns whether that finished within `timeout` seconds.

        For a process that may be frozen or stopped as soon as it answers, such as a serverless function or a request-billed container: call it before returning. It never waits longer than `timeout`, whatever the destination is doing.
        """
        try:
            done = threading.Event()
            with self._lock:
                if not self._records and not self._people:
                    return True
                if not self._ensure_worker():
                    return False
                self._flushes.append(done)
                self._wake.notify()
            return done.wait(max(0.0, timeout))
        except Exception:
            self._note("panicked")
            return False

    def close(self, timeout: float = 5.0) -> bool:
        """Stops the recorder after a final attempt to deliver what is queued, and returns whether that finished within `timeout` seconds. Records handed over afterwards are dropped and counted."""
        try:
            with self._lock:
                self._closing = True
                self._stopped.set()
                self._wake.notify()
                thread = self._thread
            if thread is None or thread is threading.current_thread():
                return True
            thread.join(max(0.0, timeout))
            return not thread.is_alive()
        except Exception:
            self._note("panicked")
            return False

    def _note(self, counter: str, n: int = 1) -> None:
        try:
            with self._lock:
                self._counters[counter] += n
        except Exception:
            pass

    def _ensure_worker(self) -> bool:
        """Starts the worker if it is not running. Called with the lock held. False once the recorder is closed."""
        if self._closing:
            return False
        if self._pid != os.getpid():
            # A fork this interpreter could not tell us about. What was queued belongs to the parent.
            self._records.clear()
            self._people.clear()
            self._seen.clear()
            self._flushes.clear()
            self._thread = None
            self._pid = os.getpid()
        if self._thread is None or not self._thread.is_alive():
            thread = threading.Thread(target=self._run, name="agentpulse-recorder", daemon=True)
            try:
                thread.start()
            except RuntimeError:
                # The interpreter is shutting down and will start no more threads.
                return False
            self._thread = thread
        self._start_rates()
        return True

    def _start_rates(self) -> None:
        """Starts the rate card reader if one is configured and it is not running. Called with the lock held."""
        if self._config.rates is None or self._closing or (self._rates_thread is not None and self._rates_thread.is_alive()):
            return
        rates = threading.Thread(target=self._keep_rates_current, name="agentpulse-rates", daemon=True)
        try:
            rates.start()
            self._rates_thread = rates
        except RuntimeError:
            pass

    def _keep_rates_current(self) -> None:
        """Reads the rate card straight away and then on its own schedule, on a thread of its own, far from anything a model call waits on. A read that fails leaves the card in hand; one that returns nothing does too."""
        stopped = self._stopped
        while not stopped.is_set():
            try:
                units = self._config.rates.list_rates(self._config.send_timeout)  # type: ignore[union-attr]
                if units:
                    self._card = pricing.RateCard(units)
            except Exception:
                self._note("rate_fetch_errors")
            stopped.wait(self._config.rate_refresh)

    def _run(self) -> None:
        while True:
            with self._lock:
                deadline = time.monotonic() + self._config.flush_every
                while not (
                    self._closing
                    or self._flushes
                    or len(self._records) >= self._config.batch_size
                    or len(self._people) >= self._config.batch_size
                ):
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        break
                    self._wake.wait(remaining)
                flushes, self._flushes = self._flushes, []
                closing = self._closing

            self._drain()
            for done in flushes:
                done.set()
            if closing:
                return

    def _drain(self) -> None:
        """Delivers everything queued, a batch at a time, taking each batch off the queue only as it is sent so the queue's bound holds while a slow destination is being waited on."""
        while True:
            with self._lock:
                records = _take(self._records, self._config.batch_size)
                people = _take(self._people, self._config.batch_size)
            if not records and not people:
                return
            try:
                if records:
                    self._deliver(records)
                if people:
                    self._deliver_names(people)
            except Exception:
                self._note("panicked")

    def _deliver(self, batch: list[tuple[_wire.Activity, str]]) -> None:
        jobs = [
            (sink, workspace, activities)
            for workspace, activities in _by_workspace(batch)
            for sink in self._config.sinks
        ]
        _run_all([lambda s=s, w=w, a=a: self._send(s, w, a) for s, w, a in jobs])

    def _send(self, sink: Sink, workspace: str, activities: list[_wire.Activity]) -> None:
        try:
            sink.send(activities, workspace, self._config.send_timeout)
        except Exception:
            self._note("failed", len(activities))
            return
        self._note("delivered", len(activities))

    def _deliver_names(self, batch: list[tuple[User, str]]) -> None:
        jobs = [
            (sink, workspace, users)
            for workspace, users in _by_workspace(batch)
            for sink in self._config.sinks
            if callable(getattr(sink, "send_users", None))
        ]
        _run_all([lambda s=s, w=w, u=u: self._send_names(s, w, u) for s, w, u in jobs])

    def _send_names(self, sink: Any, workspace: str, users: list[User]) -> None:
        try:
            sink.send_users(users, workspace, self._config.send_timeout)
        except Exception:
            self._note("names_failed", len(users))
            # Forgotten, so the next call each of them makes names them again rather than waiting for a restart.
            with self._lock:
                for user in users:
                    self._seen.pop((workspace, user.id), None)


def _call(ref: "weakref.ref[Recorder]", method: str, *args: Any) -> None:
    recorder = ref()
    if recorder is not None:
        try:
            getattr(recorder, method)(*args)
        except Exception:
            pass


def _take(queue: collections.deque, n: int) -> list:
    taken = []
    while queue and len(taken) < n:
        taken.append(queue.popleft())
    return taken


def _by_workspace(batch: Iterable[tuple[Any, str]]) -> list[tuple[str, list[Any]]]:
    """One group per workspace, in the order the workspaces were first seen.

    A destination files one batch under one parent, and a record's tenant is the parent its batch was written under, so a flush covering several tenants is several batches. Splitting here keeps a refusal for one tenant from touching another's records.
    """
    groups: dict[str, list[Any]] = {}
    for item, workspace in batch:
        groups.setdefault(workspace, []).append(item)
    return list(groups.items())


def _run_all(jobs: list) -> None:
    """Runs each job, concurrently when there is more than one so a slow destination does not delay the rest.

    At interpreter shutdown no new thread can be started, so the jobs run one after another instead.
    """
    if len(jobs) == 1:
        jobs[0]()
        return
    threads = []
    for job in jobs:
        thread = threading.Thread(target=job, name="agentpulse-send", daemon=True)
        try:
            thread.start()
        except RuntimeError:
            job()
            continue
        threads.append(thread)
    for thread in threads:
        thread.join()
