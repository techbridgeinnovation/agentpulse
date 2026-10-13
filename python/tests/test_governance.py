import threading
import time
from datetime import datetime, timezone

import pytest

from agentpulse import (
    DENY,
    DOWNGRADE,
    NOTIFY,
    UNDECIDED,
    Attribution,
    BudgetStatus,
    CachingDecider,
    Config,
    Gateway,
    Recorder,
    Reporter,
    User,
    Verdict,
    _wire,
    scope,
)

from fakes import FakeGateway, MemorySink, Reply

AGENT = "organisations/acme/agents/research"


class Answers:
    """A decider that answers from a script and remembers what it was asked."""

    def __init__(self, decision=_wire.DECISION_ALLOW, *, delay=0.0, error=None, **response):
        self.asked: list[_wire.DecideRequest] = []
        self.response = _wire.DecideResponse(decision=decision, **response)
        self.delay = delay
        self.error = error
        self._lock = threading.Lock()

    def decide(self, request, timeout):
        with self._lock:
            self.asked.append(request)
        if self.delay:
            time.sleep(self.delay)
        if self.error is not None:
            raise self.error
        return self.response


def governed(decider, *, agent=AGENT, cache_ttl=0.0, timeout=0.5, billed_by="VERTEX_AI", on_budget_status=None):
    sink = MemorySink()
    rec = Recorder(Config(sinks=[sink], exit_timeout=0, flush_every=60))
    base = Reporter(rec, Attribution(agent=agent, service="sources-service", billed_by=billed_by))
    return base.governed(decider, cache_ttl=cache_ttl, timeout=timeout, on_budget_status=on_budget_status), sink


def recorded(rp, sink):
    rp.recorder.flush(timeout=2)
    return sink.activities


def test_an_ungoverned_reporter_asks_nobody_and_lets_everything_through():
    rp = Reporter(Recorder(Config(exit_timeout=0)), Attribution(agent=AGENT, service="s"))
    verdict = rp.decide(model="m")
    assert verdict.proceed and verdict.decision == UNDECIDED


def test_the_question_names_who_and_which_tenant_from_the_context():
    answers = Answers()
    rp, _ = governed(answers, billed_by="ANTHROPIC")
    with scope(user=User("u1", "Ada"), workspace="w1", project="p1"):
        assert rp.decide(model="claude-sonnet-5").proceed
    [asked] = answers.asked
    assert asked == _wire.DecideRequest(
        parent="organisations/acme",
        agent=AGENT,
        user="u1",
        workspace="organisations/acme/workspaces/w1",
        project="p1",
        requested_provider="ANTHROPIC",
        requested_model="claude-sonnet-5",
    )


def test_a_denial_stops_the_call_and_is_recorded_as_refused_at_no_cost():
    rp, sink = governed(Answers(_wire.DECISION_DENY, reason="monthly budget reached"))
    with scope(request="req-1", component="asset_summary"):
        verdict = rp.decide(model="gemini-2.5-pro")
    assert not verdict.proceed and verdict.decision == DENY and verdict.reason == "monthly budget reached"
    [a] = recorded(rp, sink)
    assert (a.status, a.model, a.request, a.caller_component, a.billed_by, a.duration_ms) == (_wire.STATUS_DENIED, "gemini-2.5-pro", "req-1", "asset_summary", "VERTEX_AI", 0)
    assert not a.reported_usage and not a.charges
    assert rp.recorder.stats().denied == 1


def test_notify_and_downgrade_let_the_call_through_and_are_counted():
    rp, sink = governed(Answers(_wire.DECISION_NOTIFY))
    assert rp.decide(model="m").decision == NOTIFY

    rp2, _ = governed(Answers(_wire.DECISION_DOWNGRADE, replacement_provider="VERTEX_AI", replacement_model="gemini-2.5-flash"))
    verdict = rp2.decide(model="gemini-2.5-pro")
    assert verdict.proceed and verdict.decision == DOWNGRADE and verdict.replacement_model == "gemini-2.5-flash"
    assert rp.recorder.stats().notified == 1 and rp2.recorder.stats().downgraded == 1
    assert not recorded(rp, sink)


def test_a_decision_that_cannot_be_made_lets_the_call_through_and_is_counted():
    rp, _ = governed(Answers(error=RuntimeError("unreachable")))
    assert rp.decide(model="m").proceed
    assert rp.recorder.stats().decision_errors == 1


def test_a_slow_governance_is_waited_on_no_longer_than_the_timeout():
    rp, _ = governed(Answers(_wire.DECISION_DENY, delay=2.0), timeout=0.2)
    started = time.monotonic()
    verdict = rp.decide(model="m")
    assert time.monotonic() - started < 0.5
    assert verdict.proceed and rp.recorder.stats().decision_errors == 1


def test_an_agent_named_wrongly_is_a_decision_error_and_not_a_refusal():
    answers = Answers(_wire.DECISION_DENY)
    rp, _ = governed(answers, agent="research")
    assert rp.decide(model="m").proceed
    assert not answers.asked and rp.recorder.stats().decision_errors == 1


def test_an_answer_this_version_does_not_know_lets_the_call_through():
    rp, _ = governed(Answers(9))
    verdict = rp.decide(model="m")
    assert verdict.proceed and verdict.decision == UNDECIDED


def test_a_verdict_is_reused_only_for_exactly_the_same_question():
    answers = Answers()
    rp, _ = governed(answers, cache_ttl=30)
    with scope(workspace="w1"):
        rp.decide(model="m")
        rp.decide(model="m")
        rp.decide(model="other")
    with scope(workspace="w2"):
        rp.decide(model="m")
    assert [(a.workspace, a.requested_model) for a in answers.asked] == [
        ("organisations/acme/workspaces/w1", "m"),
        ("organisations/acme/workspaces/w1", "other"),
        ("organisations/acme/workspaces/w2", "m"),
    ]


def test_a_cached_verdict_expires():
    clock = [0.0]
    answers = Answers()
    cache = CachingDecider(answers, ttl=30, now=lambda: clock[0])
    request = _wire.DecideRequest(parent="organisations/acme", agent=AGENT)
    cache.decide(request, 1)
    clock[0] = 29.9
    cache.decide(request, 1)
    clock[0] = 30.0
    cache.decide(request, 1)
    assert len(answers.asked) == 2


def test_a_failure_is_never_cached_as_an_answer():
    answers = Answers(error=RuntimeError("down"))
    cache = CachingDecider(answers, ttl=30, failure_backoff=0)
    request = _wire.DecideRequest(parent="organisations/acme", agent=AGENT)
    for _ in range(2):
        with pytest.raises(RuntimeError):
            cache.decide(request, 1)
    assert len(answers.asked) == 2


def test_concurrent_calls_asking_the_same_question_share_one_request():
    answers = Answers(delay=0.2)
    cache = CachingDecider(answers, ttl=30)
    request = _wire.DecideRequest(parent="organisations/acme", agent=AGENT)
    results = []
    threads = [threading.Thread(target=lambda: results.append(cache.decide(request, 2))) for _ in range(8)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()
    assert len(answers.asked) == 1 and len(results) == 8


def test_a_call_waiting_on_another_still_keeps_its_own_timeout():
    cache = CachingDecider(Answers(delay=1.0), ttl=30)
    request = _wire.DecideRequest(parent="organisations/acme", agent=AGENT)
    threading.Thread(target=lambda: cache.decide(request, 2), daemon=True).start()
    time.sleep(0.05)
    started = time.monotonic()
    with pytest.raises(TimeoutError):
        cache.decide(request, 0.1)
    assert time.monotonic() - started < 0.5


def test_the_cache_cannot_grow_without_bound():
    cache = CachingDecider(Answers(), ttl=30, max_entries=16)
    for i in range(1000):
        cache.decide(_wire.DecideRequest(parent="organisations/acme", agent=AGENT, user=f"u{i}"), 1)
    assert len(cache._entries) <= 16


def test_a_scope_is_invalidated_across_every_tenant_and_model_within_it():
    answers = Answers()
    cache = CachingDecider(answers, ttl=30)
    asks = [
        _wire.DecideRequest(parent="organisations/acme", agent=AGENT, user="u1", workspace="organisations/acme/workspaces/w1", requested_model="a"),
        _wire.DecideRequest(parent="organisations/acme", agent=AGENT, user="u1", workspace="organisations/acme/workspaces/w2", requested_model="b"),
        _wire.DecideRequest(parent="organisations/acme", agent=AGENT, user="u2"),
    ]
    for request in asks:
        cache.decide(request, 1)
    cache.invalidate_scope("organisations/acme", user="u1")
    for request in asks:
        cache.decide(request, 1)
    assert len(answers.asked) == 5


def test_governed_by_default_asks_governance_through_the_gateway():
    fake = FakeGateway()
    try:
        fake.replies.append(Reply(message=_wire._integer(1, _wire.DECISION_DENY) + _wire._string(2, "over budget")))
        gateway = Gateway(fake.address, "organisations/acme/apiKeys/k1", "s")
        rp = Reporter(Recorder(Config(exit_timeout=0)), Attribution(agent=AGENT, service="s"), gateway=gateway).governed(follow_changes=False)
        verdict = rp.decide(model="gemini-2.5-pro")
        assert (verdict.decision, verdict.reason) == (DENY, "over budget")
        assert fake.calls[0].path == "/techbridge.ap.governance.v1.DecisionsService/Decide"
        assert fake.calls[0].message == _wire.DecideRequest(parent="organisations/acme", agent=AGENT, requested_provider="VERTEX_AI", requested_model="gemini-2.5-pro").encode()
        # The second call is answered from the cache.
        rp.decide(model="gemini-2.5-pro")
        assert len(fake.calls) == 1
    finally:
        fake.close()


def test_a_governance_that_refuses_the_question_lets_the_call_through():
    fake = FakeGateway()
    try:
        fake.replies.append(Reply(header_status=7))
        gateway = Gateway(fake.address, "organisations/acme/apiKeys/k1", "s")
        rp = Reporter(Recorder(Config(exit_timeout=0)), Attribution(agent=AGENT, service="s"), gateway=gateway).governed(follow_changes=False)
        assert rp.decide(model="m").proceed
        assert rp.recorder.stats().decision_errors == 1
    finally:
        fake.close()


def test_an_outage_slows_no_call_past_the_first():
    slow = Answers(delay=1.0)
    rp, _ = governed(slow, timeout=0.2)
    rp = Reporter(rp.recorder, rp.attribution).governed(slow, cache_ttl=30, timeout=0.2, failure_backoff=5)
    started = time.monotonic()
    for _ in range(10):
        assert rp.decide(model="m").proceed
    assert time.monotonic() - started < 0.6
    assert len(slow.asked) == 1 and rp.recorder.stats().decision_errors == 10


def test_a_failed_question_is_asked_again_after_the_backoff():
    clock = [0.0]
    answers = Answers(error=RuntimeError("down"))
    cache = CachingDecider(answers, ttl=30, failure_backoff=5, now=lambda: clock[0])
    request = _wire.DecideRequest(parent="organisations/acme", agent=AGENT)
    for _ in range(3):
        with pytest.raises(Exception):
            cache.decide(request, 1)
    assert len(answers.asked) == 1
    clock[0] = 5.0
    answers.error = None
    assert cache.decide(request, 1).decision == _wire.DECISION_ALLOW


def test_a_changed_budget_drops_the_verdicts_it_covers_as_soon_as_governance_says_so():
    from agentpulse.invalidations import InvalidationSubscriber

    fake = FakeGateway()
    try:
        invalidation = _wire._string(1, "organisations/acme") + _wire._string(3, "u1")
        fake.replies.append(Reply(messages=[invalidation], delay=0.3))
        answers = Answers()
        cache = CachingDecider(answers, ttl=3600)
        for user in ("u1", "u2"):
            cache.decide(_wire.DecideRequest(parent="organisations/acme", agent=AGENT, user=user), 1)
        subscriber = InvalidationSubscriber(Gateway(fake.address, "organisations/acme/apiKeys/k1", "s"), cache, "organisations/acme")
        subscriber.ensure_running()
        deadline = time.monotonic() + 3
        while subscriber.received < 1 and time.monotonic() < deadline:
            time.sleep(0.01)
        subscriber.stop()
        assert fake.calls[0].path == "/techbridge.ap.governance.v1.DecisionsService/StreamDecisionInvalidations"
        assert fake.calls[0].message == _wire._string(1, "organisations/acme")
        for user in ("u1", "u2"):
            cache.decide(_wire.DecideRequest(parent="organisations/acme", agent=AGENT, user=user), 1)
        # Only the person whose budget changed is asked about again.
        assert [a.user for a in answers.asked] == ["u1", "u2", "u1"]
    finally:
        fake.close()


def test_a_governed_reporter_starts_following_changes_on_its_first_decision():
    fake = FakeGateway()
    try:
        fake.replies.append(Reply(messages=[], delay=0.5))
        gateway = Gateway(fake.address, "organisations/acme/apiKeys/k1", "s")
        rp = Reporter(Recorder(Config(exit_timeout=0)), Attribution(agent=AGENT, service="s"), gateway=gateway).governed(Answers())
        assert rp._subscriber is None
        rp = Reporter(Recorder(Config(exit_timeout=0)), Attribution(agent=AGENT, service="s"), gateway=gateway).governed()
        assert rp._subscriber is not None and rp._subscriber._thread is None
    finally:
        fake.close()


def test_decisions_run_on_threads_that_are_kept_so_their_connections_are_reused():
    from agentpulse import governance as g

    seen = set()

    class Decider:
        def decide(self, request, timeout):
            seen.add(threading.get_ident())
            return _wire.DecideResponse(decision=1)

    for _ in range(20):
        g.ask(Decider(), _wire.DecideRequest(parent="organisations/acme", agent="organisations/acme/agents/a"), 1.0)
    # A new thread per decision would be a new connection, and a new TLS handshake, per decision.
    assert 1 <= len(seen) <= g._DECISION_WORKERS


def test_a_decision_that_hangs_holds_up_no_other_past_its_own_timeout():
    from agentpulse import governance as g

    release = threading.Event()

    class Hangs:
        def decide(self, request, timeout):
            release.wait(5)
            return _wire.DecideResponse(decision=1)

    class Answers:
        def decide(self, request, timeout):
            return _wire.DecideResponse(decision=1)

    try:
        for _ in range(g._DECISION_WORKERS - 1):
            with pytest.raises(TimeoutError):
                g.ask(Hangs(), _wire.DecideRequest(parent="organisations/acme", agent="organisations/acme/agents/a"), 0.05)
        started = time.monotonic()
        assert g.ask(Answers(), _wire.DecideRequest(parent="organisations/acme", agent="organisations/acme/agents/a"), 1.0).decision == g.ALLOW
        assert time.monotonic() - started < 1.0
    finally:
        release.set()


def test_a_refusal_is_repeated_when_governance_does_not_answer():
    answers = Answers(_wire.DECISION_DENY)
    rp, _ = governed(answers)
    assert not rp.decide(model="m").proceed
    answers.error = TimeoutError("slow")
    assert not rp.decide(model="m").proceed
    assert rp.decide(model="another").proceed


def test_a_user_that_looks_like_an_email_is_left_out_of_the_question():
    answers = Answers()
    rp, _ = governed(answers)
    with scope(user=User("ada@example.com")):
        rp.decide(model="m")
    assert answers.asked[0].user == ""


def test_governed_through_the_gateway_opens_every_decision_connection_without_waiting():
    import socket

    from agentpulse import governance

    listener = socket.socket()
    listener.bind(("127.0.0.1", 0))
    listener.listen(16)
    listener.settimeout(3)
    try:
        gateway = Gateway(f"http://127.0.0.1:{listener.getsockname()[1]}", "organisations/acme/apiKeys/k1", "s")
        started = time.monotonic()
        Reporter(Recorder(Config(exit_timeout=0)), Attribution(agent=AGENT, service="s"), gateway=gateway).governed(follow_changes=False)
        assert time.monotonic() - started < 0.1
        accepted = [listener.accept()[0] for _ in range(governance._DECISION_WORKERS)]
        for conn in accepted:
            conn.close()
    finally:
        listener.close()


def test_governed_through_a_gateway_that_cannot_be_reached_still_starts_and_lets_calls_through():
    gateway = Gateway("http://127.0.0.1:1", "organisations/acme/apiKeys/k1", "s")
    started = time.monotonic()
    rp = Reporter(Recorder(Config(exit_timeout=0)), Attribution(agent=AGENT, service="s"), gateway=gateway).governed(follow_changes=False, timeout=0.5)
    assert time.monotonic() - started < 0.1
    assert rp.decide(model="m").proceed


STATUSES = [
    _wire.BudgetStatus(
        budget="organisations/acme/budgets/eng",
        display_name="Engineering",
        scope=_wire.SCOPE_WORKSPACE,
        workspace="organisations/acme/workspaces/w1",
        spent_micros=9_600_000,
        limit_micros=10_000_000,
        period_end_time_ns=1793491200_000000000,
        level=_wire.LEVEL_PASSED_95_PERCENT,
    ),
    _wire.BudgetStatus(budget="organisations/acme/budgets/org", scope=_wire.SCOPE_RESERVE, level=_wire.LEVEL_EXHAUSTED),
    _wire.BudgetStatus(budget="organisations/acme/budgets/later", scope=9, level=7, warn_only=True),
]


def with_statuses(decision=_wire.DECISION_ALLOW):
    return Answers(decision, budget_statuses=list(STATUSES), evaluate_time_ns=1791892800_123456789)


def test_a_verdict_carries_every_budget_status_and_when_governance_worked_them_out():
    rp, _ = governed(with_statuses(_wire.DECISION_NOTIFY))
    verdict = rp.decide(model="m")
    assert verdict.decision == NOTIFY
    assert verdict.evaluate_time == datetime(2026, 10, 13, 12, 0, 0, 123456, tzinfo=timezone.utc)
    workspace, reserve, later = verdict.budget_statuses
    assert workspace == BudgetStatus(
        budget="organisations/acme/budgets/eng",
        display_name="Engineering",
        scope="WORKSPACE",
        workspace="organisations/acme/workspaces/w1",
        spent_micros=9_600_000,
        limit_micros=10_000_000,
        period_end_time=datetime(2026, 11, 1, tzinfo=timezone.utc),
        level="PASSED_95_PERCENT",
    )
    # A reserve with nothing in it is exhausted at a limit of zero, and nothing here divides by it.
    assert (reserve.scope, reserve.level, reserve.spent_micros, reserve.limit_micros, reserve.period_end_time) == ("RESERVE", "EXHAUSTED", 0, 0, None)
    # A scope or level this version does not know is read as unspecified rather than refused.
    assert (later.scope, later.level, later.warn_only) == ("SCOPE_UNSPECIFIED", "LEVEL_UNSPECIFIED", True)


def test_a_governance_that_sends_no_statuses_leaves_the_verdict_without_any():
    verdict = governed(Answers(_wire.DECISION_ALLOW))[0].decide(model="m")
    assert verdict.budget_statuses == () and verdict.evaluate_time is None
    assert Verdict().budget_statuses == () and Verdict(decision=DENY, reason="r").evaluate_time is None


def test_a_cached_verdict_keeps_the_statuses_of_the_answer_it_reuses():
    answers = with_statuses()
    rp, _ = governed(answers, cache_ttl=30)
    first, second = rp.decide(model="m"), rp.decide(model="m")
    assert len(answers.asked) == 1
    assert second.budget_statuses == first.budget_statuses and second.evaluate_time == first.evaluate_time


def until(condition, timeout=2.0):
    deadline = time.monotonic() + timeout
    while not condition() and time.monotonic() < deadline:
        time.sleep(0.005)
    return condition()


def test_an_instrumented_call_hands_its_statuses_to_the_callback():
    told = []
    rp, _ = governed(with_statuses(), on_budget_status=told.append)
    with scope(user=User("u1", "Ada"), workspace="w1", project="p1", request="req-1"):
        # What every wrapped client and framework hook calls before the model call.
        assert rp._decide("gemini-2.5-pro", component="asset_summary").proceed
    assert until(lambda: told)
    [event] = told
    assert len(event.budget_statuses) == 3 and event.budget_statuses[0].level == "PASSED_95_PERCENT"
    assert event.evaluate_time == datetime(2026, 10, 13, 12, 0, 0, 123456, tzinfo=timezone.utc)
    assert (event.decision, event.model, event.provider, event.component) == ("ALLOW", "gemini-2.5-pro", "VERTEX_AI", "asset_summary")
    assert (event.user, event.workspace, event.project, event.request) == ("u1", "organisations/acme/workspaces/w1", "p1", "req-1")


def test_a_call_never_waits_for_a_callback_that_is_blocked():
    release, entered = threading.Event(), threading.Event()

    def blocked(event):
        entered.set()
        release.wait(10)

    rp, _ = governed(with_statuses(), on_budget_status=blocked)
    try:
        started = time.monotonic()
        assert rp._decide("m").proceed
        assert entered.wait(2)
        # The callback is still running, and further calls still return at once.
        assert rp._decide("m").proceed
        assert time.monotonic() - started < 1.0
        assert not release.is_set()
    finally:
        release.set()


def test_events_are_delivered_one_at_a_time_in_the_order_the_decisions_were_obtained():
    seen, running, overlapped = [], [0], []

    def callback(event):
        running[0] += 1
        overlapped.append(running[0] > 1)
        seen.append(event.model)
        time.sleep(0.002)
        running[0] -= 1

    rp, _ = governed(with_statuses(), on_budget_status=callback)
    models = [f"m{i}" for i in range(20)]
    for model in models:
        rp._decide(model)
    assert until(lambda: len(seen) == 20)
    assert seen == models and not any(overlapped)


def test_a_cached_verdict_s_statuses_are_delivered_for_every_call():
    told = []
    answers = with_statuses()
    rp, _ = governed(answers, cache_ttl=30, on_budget_status=told.append)
    rp._decide("m")
    rp._decide("m")
    assert until(lambda: len(told) == 2)
    assert len(answers.asked) == 1 and told[0].budget_statuses == told[1].budget_statuses


def test_a_callback_that_raises_or_is_async_does_not_stop_the_call_and_is_counted_failed():
    def broken(event):
        raise RuntimeError("the product's own bug")

    async def asynchronous(event):
        raise AssertionError("never run")

    for callback in (broken, asynchronous):
        rp, _ = governed(with_statuses(_wire.DECISION_NOTIFY), on_budget_status=callback)
        verdict = rp._decide("m")
        assert verdict.proceed and verdict.decision == NOTIFY and len(verdict.budget_statuses) == 3
        assert until(lambda: rp.recorder.stats().budget_status_failed == 1)
        stats = rp.recorder.stats()
        assert (stats.budget_status_dropped, stats.notified, stats.panicked, stats.decision_errors) == (0, 1, 0, 0)
    # One failure does not stop the deliveries after it.
    told = []

    def sometimes(event):
        told.append(event.model)
        if event.model == "first":
            raise RuntimeError("first only")

    rp, _ = governed(with_statuses(), on_budget_status=sometimes)
    rp._decide("first")
    rp._decide("second")
    assert until(lambda: told == ["first", "second"])


def test_a_full_queue_drops_events_and_counts_them_without_ever_blocking_a_call():
    release, entered = threading.Event(), threading.Event()
    told = []

    def blocked(event):
        entered.set()
        release.wait(10)
        told.append(event.model)

    rp, _ = governed(with_statuses(), on_budget_status=blocked)
    try:
        rp._decide("held")
        assert entered.wait(2)
        started = time.monotonic()
        for i in range(70):
            assert rp._decide(f"m{i}").proceed
        assert time.monotonic() - started < 2.0
        # The worker holds one event, the queue holds 64, and the other 6 were dropped.
        assert rp.recorder.stats().budget_status_dropped == 6
    finally:
        release.set()
    assert until(lambda: len(told) == 65)
    assert told[0] == "held" and told[1:] == [f"m{i}" for i in range(64)]


def test_the_callback_is_not_called_without_statuses_without_a_decision_or_for_decide():
    told = []
    rp, _ = governed(Answers(_wire.DECISION_ALLOW), on_budget_status=told.append)
    rp._decide("m")
    rp, _ = governed(Answers(error=RuntimeError("unreachable")), on_budget_status=told.append)
    assert rp._decide("m").proceed
    rp, _ = governed(with_statuses(), on_budget_status=told.append)
    assert len(rp.decide(model="m").budget_statuses) == 3
    ungoverned = Reporter(Recorder(Config(exit_timeout=0)), Attribution(agent=AGENT, service="s"))
    assert ungoverned._decide("m").budget_statuses == ()
    time.sleep(0.1)
    assert told == []


def test_statuses_reach_no_callback_when_none_is_set_and_no_thread_is_started():
    before = {t.name for t in threading.enumerate()}
    rp, _ = governed(with_statuses())
    assert len(rp._decide("m").budget_statuses) == 3
    assert not {t.name for t in threading.enumerate() if "budget-status" in t.name} - before
    stats = rp.recorder.stats()
    assert (stats.budget_status_dropped, stats.budget_status_failed) == (0, 0)
