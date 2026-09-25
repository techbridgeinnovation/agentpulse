import threading
import time

import pytest

from agentpulse import (
    DENY,
    DOWNGRADE,
    NOTIFY,
    UNDECIDED,
    Attribution,
    CachingDecider,
    Config,
    Gateway,
    Recorder,
    Reporter,
    User,
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


def governed(decider, *, agent=AGENT, cache_ttl=0.0, timeout=0.5, billed_by="VERTEX_AI"):
    sink = MemorySink()
    rec = Recorder(Config(sinks=[sink], exit_timeout=0, flush_every=60))
    base = Reporter(rec, Attribution(agent=agent, service="sources-service", billed_by=billed_by))
    return base.governed(decider, cache_ttl=cache_ttl, timeout=timeout), sink


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
