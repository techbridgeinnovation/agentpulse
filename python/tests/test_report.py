import asyncio
import threading

from agentpulse import (
    FORMAT_ANTHROPIC,
    Attribution,
    Charge,
    Config,
    ModelCall,
    Recorder,
    Reporter,
    Tokens,
    ToolCall,
    User,
    _wire,
    carry,
    reported_error_of,
    scope,
)

from fakes import MemorySink

AGENT = "organisations/acme/agents/research"


def reporter(**attribution):
    sink = MemorySink()
    rec = Recorder(Config(sinks=[sink], exit_timeout=0, flush_every=60))
    return Reporter(rec, Attribution(agent=AGENT, service="sources-service", **attribution)), sink


def recorded(rp, sink):
    rp.recorder.flush(timeout=2)
    return sink.activities


def test_a_model_call_carries_who_it_was_for_from_the_context():
    rp, sink = reporter(billed_by="ANTHROPIC", skill="summaries")
    with scope(request="req-1", session="s-1", user=User("u1", "Ada"), workspace="acme", project="matter-1"):
        rp.model_call(
            ModelCall(
                model="claude-sonnet-5",
                component="asset_summary",
                reported={"input_tokens": 4211, "cache_read_input_tokens": 21847, "output_tokens": 0},
                format=FORMAT_ANTHROPIC,
                duration=1.2345,
                cache_write_ttl=3600,
                tier="batch",
                charges=[Charge("priceableUnits/web-search", 2)],
            )
        )
    [a] = recorded(rp, sink)
    assert (a.agent, a.request, a.session, a.user, a.project) == (AGENT, "req-1", "s-1", "u1", "matter-1")
    assert (a.caller_service, a.caller_component, a.skill, a.billed_by) == ("sources-service", "asset_summary", "summaries", "ANTHROPIC")
    assert a.usage_format == FORMAT_ANTHROPIC
    # Sorted, and a count of zero is left out.
    assert [(q.unit, q.quantity) for q in a.reported_usage] == [("cache_read_input_tokens", 21847), ("input_tokens", 4211)]
    assert (a.duration_ms, a.cache_write_ttl_seconds, a.service_tier, a.status) == (1234, 3600, "batch", _wire.STATUS_OK)
    assert [(c.priceable_unit, c.quantity) for c in a.charges] == [("priceableUnits/web-search", 2)]
    assert a.occurred_at_ns > 0
    assert sink.batches[0][0] == "acme"
    assert [u.name for _, users in sink.named for u in users] == ["Ada"]


def test_a_reporter_with_no_billing_stated_is_billed_by_vertex():
    rp, sink = reporter()
    rp.model_call(ModelCall(model="gemini-2.5-pro"))
    assert recorded(rp, sink)[0].billed_by == "VERTEX_AI"


def test_a_failure_keeps_its_code_and_never_its_message():
    rp, sink = reporter()
    rp.model_call(ModelCall(model="m", error=TimeoutError("the prompt was: tell me a secret")))
    [a] = recorded(rp, sink)
    assert (a.status, a.error_code) == (_wire.STATUS_FAILED, "DeadlineExceeded")
    assert "secret" not in repr(a)


def test_a_limit_is_truncated_and_a_refusal_is_a_failure():
    rp, sink = reporter()
    rp.model_call(ModelCall(model="m", finish_reason="max_tokens"))
    rp.model_call(ModelCall(model="m", finish_reason="SAFETY"))
    rp.model_call(ModelCall(model="m", finish_reason="stop"))
    rp.model_call(ModelCall(model="m", truncated=True))
    truncated, blocked, fine, cut = recorded(rp, sink)
    assert truncated.status == _wire.STATUS_TRUNCATED
    assert (blocked.status, blocked.error_code) == (_wire.STATUS_FAILED, "SAFETY")
    assert fine.status == _wire.STATUS_OK
    assert cut.status == _wire.STATUS_TRUNCATED


def test_a_failure_the_caller_read_for_itself_is_kept_and_its_message_dropped():
    rp, sink = reporter()
    reported = reported_error_of("OPENAI", {"type": "invalid_request_error", "code": "context_length_exceeded", "message": "your prompt: ..."})
    rp.model_call(ModelCall(model="m", error=RuntimeError("x"), reported_error=reported))
    [a] = recorded(rp, sink)
    assert a.error_format == "OPENAI"
    assert [(f.name, f.value) for f in a.reported_error] == [("code", "context_length_exceeded"), ("type", "invalid_request_error")]


def test_a_split_the_caller_made_is_carried_with_its_total():
    rp, sink = reporter()
    rp.model_call(ModelCall(model="m", tokens=Tokens(prompt=10, candidate=5, reasoning=2)))
    [a] = recorded(rp, sink)
    assert (a.prompt_tokens, a.candidate_tokens, a.reasoning_tokens, a.total_tokens) == (10, 5, 2, 17)


def test_a_tool_call_is_its_own_record():
    rp, sink = reporter()
    with scope(request="req-1"):
        rp.tool_call(ToolCall(tool="web_search", duration=0.5, charges=[Charge("priceableUnits/web-search", 1)]))
    [a] = recorded(rp, sink)
    assert (a.tool, a.caller_component, a.request, a.duration_ms) == ("web_search", "tool:web_search", "req-1", 500)


def test_the_component_on_the_context_is_used_when_the_call_names_none():
    rp, sink = reporter()
    with scope(component="report_generation"):
        rp.model_call(ModelCall(model="m"))
    assert recorded(rp, sink)[0].caller_component == "report_generation"


def test_the_context_follows_the_work_across_await():
    rp, sink = reporter()

    async def call():
        await asyncio.sleep(0)
        rp.model_call(ModelCall(model="m"))

    async def main():
        with scope(request="async-req"):
            await asyncio.gather(call(), asyncio.to_thread(rp.model_call, ModelCall(model="m")))

    asyncio.run(main())
    assert [a.request for a in recorded(rp, sink)] == ["async-req", "async-req"]


def test_carry_takes_the_context_into_a_thread_started_by_hand():
    rp, sink = reporter()
    with scope(request="req-t"):
        plain = threading.Thread(target=rp.model_call, args=(ModelCall(model="plain"),))
        carried = threading.Thread(target=carry(rp.model_call), args=(ModelCall(model="carried"),))
    for thread in (plain, carried):
        thread.start()
        thread.join()
    by_model = {a.model: a.request for a in recorded(rp, sink)}
    assert by_model == {"plain": "", "carried": "req-t"}


def test_a_reporter_that_cannot_build_a_record_counts_it_and_does_not_raise():
    rp, sink = reporter()
    rp.model_call(ModelCall(model="m", reported={"input_tokens": "not a number"}))  # type: ignore[dict-item]
    rp.model_call(ModelCall(model="m", charges=[object()]))  # type: ignore[list-item]
    recorded(rp, sink)
    assert rp.recorder.stats().panicked == 1


def test_a_call_made_inside_an_agent_framework_names_the_sub_agent_that_made_it():
    from agentpulse import _wire
    from agentpulse.report import _Framework

    rp, _ = reporter()
    activity = rp._activity("tool:search_meet_transcripts", 0.1, None, (), _Framework(request="inv-1", agent="meetings_researcher"))
    assert activity.sub_agent == "meetings_researcher"
    # Field 46, a string: the key is 46 << 3 | 2 = 370, written as the varint f2 02.
    assert _wire.Activity(sub_agent="m").encode().endswith(b"\xf2\x02\x01m")
    assert rp._activity("tool:web_search", 0.1, None, ()).sub_agent == ""


def test_a_call_says_whether_it_was_observed_from_an_agent_or_a_service():
    from agentpulse import _wire
    from agentpulse.report import _Framework

    rp, _ = reporter()
    assert rp._activity("assistant", 0.1, None, (), _Framework(agent="assistant")).observed_as == _wire.KIND_AGENT
    # LiteLLM carries who a call was for but names no agent: a direct call.
    assert rp._activity("summary", 0.1, None, (), _Framework(request="r1")).observed_as == _wire.KIND_SERVICE
    assert rp._activity("summary", 0.1, None, ()).observed_as == _wire.KIND_SERVICE
    # Field 45, a varint: the key is 45 << 3 = 360, written as e8 02.
    assert b"\xe8\x02\x02" in _wire.Activity(observed_as=_wire.KIND_SERVICE).encode()
