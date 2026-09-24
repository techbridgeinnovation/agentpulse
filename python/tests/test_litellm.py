"""Calls made through the real LiteLLM, SDK and proxy hook, against a provider on this machine. Skipped where LiteLLM is not installed."""

import asyncio

import pytest

litellm = pytest.importorskip("litellm")

import agentpulse  # noqa: E402
from agentpulse import Attribution, Config, Recorder, Reporter, _wire  # noqa: E402
from agentpulse.pricing import classes_of  # noqa: E402

from fakes import MemorySink, Provider  # noqa: E402

AGENT = "organisations/acme/agents/gateway"

CHAT = {
    "id": "c1",
    "object": "chat.completion",
    "created": 1,
    "model": "gpt-5-2026-08-01",
    "choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "SECRET ANSWER"}}],
    "usage": {"prompt_tokens": 100, "completion_tokens": 50, "total_tokens": 150, "prompt_tokens_details": {"cached_tokens": 80}},
}

MESSAGE = {
    "id": "m1",
    "type": "message",
    "role": "assistant",
    "model": "claude-sonnet-5",
    "content": [{"type": "text", "text": "SECRET"}],
    "stop_reason": "max_tokens",
    "usage": {"input_tokens": 100, "output_tokens": 50, "cache_read_input_tokens": 800, "cache_creation_input_tokens": 40},
}


@pytest.fixture
def provider():
    p = Provider()
    yield p
    p.close()


# LiteLLM reads its callback list once, the first time a call is made, so one callback serves the whole file and each test starts from an empty sink.
_SINK = MemorySink()
_REPORTER = Reporter(Recorder(Config(sinks=[_SINK], exit_timeout=0, flush_every=60)), Attribution(agent=AGENT, service="svc"))
litellm.callbacks = [_REPORTER.litellm_callback()]


@pytest.fixture
def recorded():
    sink, rp, rec = _SINK, _REPORTER, _REPORTER.recorder
    rec.flush(timeout=1)
    sink.batches.clear()
    sink.named.clear()

    def flush():
        # LiteLLM reports a call's outcome from a thread of its own, after the call returns.
        deadline = __import__("time").monotonic() + 3
        while not sink.activities and __import__("time").monotonic() < deadline:
            rec.flush(timeout=0.2)
        rec.flush(timeout=1)
        return sink.activities

    yield rp, sink, flush


def test_an_sdk_call_is_recorded_in_litellm_convention_under_the_scope_it_was_made_in(provider, recorded):
    rp, sink, flush = recorded
    provider.json(CHAT)
    with agentpulse.scope(request="req-1", workspace="acme"):
        reply = litellm.completion(model="openai/gpt-5", api_base=provider.url + "/v1", api_key="k", messages=[{"role": "user", "content": "SECRET PROMPT"}])
    assert reply.choices[0].message.content == "SECRET ANSWER"
    [a] = flush()
    assert (a.model, a.billed_by, a.usage_format, a.request, a.status) == ("gpt-5-2026-08-01", "OPENAI", "LITELLM", "req-1", _wire.STATUS_OK)
    usage = {q.unit: q.quantity for q in a.reported_usage}
    assert (usage["prompt_tokens"], usage["completion_tokens"], usage["prompt_tokens_details.cached_tokens"]) == (100, 50, 80)
    assert sink.batches[0][0] == "acme"
    assert "SECRET" not in repr(sink.batches)


def test_an_anthropic_call_through_litellm_is_priced_with_both_cached_parts_taken_out(provider, recorded):
    rp, sink, flush = recorded
    provider.json(MESSAGE)
    litellm.completion(model="anthropic/claude-sonnet-5", api_base=provider.url, api_key="k", messages=[{"role": "user", "content": "x"}])
    [a] = flush()
    assert (a.billed_by, a.status, a.error_code) == ("ANTHROPIC", _wire.STATUS_TRUNCATED, "length")
    c = classes_of(a)
    assert (c.prompt, c.cached, c.cache_write, c.candidate) == (100, 800, 40, 50)


def test_an_async_call_is_recorded_once(provider, recorded):
    rp, sink, flush = recorded
    provider.json(CHAT)

    async def call():
        await litellm.acompletion(model="openai/gpt-5", api_base=provider.url + "/v1", api_key="k", messages=[{"role": "user", "content": "x"}])
        await asyncio.sleep(0.3)

    asyncio.run(call())
    assert len(flush()) == 1


def test_who_the_call_was_for_can_travel_in_its_metadata(provider, recorded):
    rp, sink, flush = recorded
    provider.json(CHAT)
    litellm.completion(
        model="openai/gpt-5",
        api_base=provider.url + "/v1",
        api_key="k",
        messages=[{"role": "user", "content": "x"}],
        metadata={"agentpulse_workspace": "acme", "agentpulse_user": "u-42", "agentpulse_user_name": "Ada", "agentpulse_request": "req-9", "agentpulse_component": "summaries"},
    )
    [a] = flush()
    assert (a.user, a.request, a.caller_component) == ("u-42", "req-9", "summaries")
    assert sink.batches[0][0] == "acme"


def test_a_refused_call_keeps_its_code_and_never_its_message(provider, recorded):
    rp, sink, flush = recorded
    provider.json({"error": {"message": "SECRET", "type": "requests", "code": "rate_limit_exceeded"}}, status=429)
    with pytest.raises(Exception):
        litellm.completion(model="openai/gpt-5", api_base=provider.url + "/v1", api_key="k", messages=[{"role": "user", "content": "x"}], num_retries=0, max_retries=0)
    [a] = flush()
    assert a.status == _wire.STATUS_FAILED and a.error_code not in ("", "Unknown")
    assert "SECRET" not in repr(sink.batches)


class Answers:
    def __init__(self, decision, **response):
        self.response = _wire.DecideResponse(decision=decision, **response)
        self.asked = []

    def decide(self, request, timeout):
        self.asked.append(request)
        return self.response


def proxy_hook(decider):
    sink = MemorySink()
    rec = Recorder(Config(sinks=[sink], exit_timeout=0, flush_every=60))
    rp = Reporter(rec, Attribution(agent=AGENT, service="litellm-proxy")).governed(decider, cache_ttl=0)
    return rp, sink, rp.litellm_callback(denied_message="Your team is out of budget.")


def test_the_proxy_refuses_a_call_a_budget_has_run_out_on_before_forwarding_it():
    fastapi = pytest.importorskip("fastapi")
    answers = Answers(_wire.DECISION_DENY)
    rp, sink, callback = proxy_hook(answers)
    data = {"model": "anthropic/claude-sonnet-5", "messages": [], "user": "u-42", "metadata": {"agentpulse_workspace": "acme"}}
    with pytest.raises(fastapi.HTTPException) as raised:
        asyncio.run(callback.async_pre_call_hook(None, None, data, "completion"))
    assert raised.value.status_code == 403 and raised.value.detail["error"]["type"] == "agentpulse_denied"
    [asked] = answers.asked
    assert (asked.user, asked.workspace, asked.requested_provider, asked.requested_model) == ("u-42", "organisations/acme/workspaces/acme", "ANTHROPIC", "anthropic/claude-sonnet-5")
    rp.recorder.flush(timeout=2)
    [a] = sink.activities
    assert a.status == _wire.STATUS_DENIED and sink.batches[0][0] == "acme"


def test_the_proxy_forwards_a_downgrade_s_replacement_and_otherwise_the_call_as_asked():
    rp, _, callback = proxy_hook(Answers(_wire.DECISION_DOWNGRADE, replacement_provider="ANTHROPIC", replacement_model="anthropic/claude-haiku-5"))
    data = {"model": "anthropic/claude-sonnet-5", "messages": []}
    assert asyncio.run(callback.async_pre_call_hook(None, None, data, "completion"))["model"] == "anthropic/claude-haiku-5"
    rp2, _, allowing = proxy_hook(Answers(_wire.DECISION_ALLOW))
    assert asyncio.run(allowing.async_pre_call_hook(None, None, {"model": "openai/gpt-5"}, "completion")) is None


def test_a_proxy_whose_governance_is_down_forwards_everything():
    class Down:
        def decide(self, request, timeout):
            raise ConnectionError("down")

    rp, _, callback = proxy_hook(Down())
    assert asyncio.run(callback.async_pre_call_hook(None, None, {"model": "openai/gpt-5"}, "completion")) is None
    assert rp.recorder.stats().decision_errors == 1
