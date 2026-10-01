"""Runs of the real OpenAI Agents SDK against a provider on this machine. Skipped where the SDK is not installed."""

import asyncio
import gc
import importlib.metadata
import time

import pytest

agents = pytest.importorskip("agents")
openai = pytest.importorskip("openai")

import agentpulse  # noqa: E402
from agentpulse import Attribution, Config, Recorder, Reporter, SpendDenied, _wire  # noqa: E402
from agentpulse.pricing import classes_of  # noqa: E402

from fakes import MemorySink, Provider  # noqa: E402

AGENT = "organisations/acme/agents/assistant"

# The SDK's own exporter would try to send every trace to OpenAI; the traces still exist without it.
agents.set_trace_processors([])


def response(n, tool=None):
    if tool:
        output = [{"type": "function_call", "id": f"fc{n}", "call_id": f"call{n}", "name": tool, "arguments": '{"input": "SECRET ARGUMENT"}', "status": "completed"}]
    else:
        output = [{"type": "message", "id": f"m{n}", "role": "assistant", "status": "completed", "content": [{"type": "output_text", "text": "SECRET ANSWER", "annotations": []}]}]
    return {
        "id": f"resp{n}",
        "object": "response",
        "created_at": 1,
        "status": "completed",
        "model": "gpt-5-2026-08-01",
        "output": output,
        "parallel_tool_calls": True,
        "tool_choice": "auto",
        "tools": [],
        "text": {"format": {"type": "text"}},
        "usage": {"input_tokens": 100, "output_tokens": 20, "total_tokens": 120, "input_tokens_details": {"cached_tokens": 40}, "output_tokens_details": {"reasoning_tokens": 5}},
    }


class Answers:
    def __init__(self, decision, **response):
        self.response = _wire.DecideResponse(decision=decision, **response)
        self.asked = []

    def decide(self, request, timeout):
        self.asked.append(request)
        return self.response


@pytest.fixture
def provider():
    p = Provider()
    yield p
    p.close()


def reporter(decider=None):
    sink = MemorySink()
    rp = Reporter(Recorder(Config(sinks=[sink], exit_timeout=0, flush_every=60)), Attribution(agent=AGENT, service="svc"))
    if decider is not None:
        rp = rp.governed(decider, cache_ttl=0)
    return rp, sink


def recorded(rp, sink):
    rp.recorder.flush(timeout=2)
    return sink.activities


def model(provider, name="gpt-5"):
    client = openai.AsyncOpenAI(api_key="k", base_url=provider.url + "/v1", max_retries=0)
    return agents.OpenAIResponsesModel(model=name, openai_client=client)


def run(agent, rp, prompt="SECRET PROMPT", **kwargs):
    return asyncio.run(agents.Runner.run(agent, prompt, hooks=rp.openai_agents_hooks(), **kwargs))


def test_a_model_call_is_recorded_in_the_responses_convention_under_the_agent_that_made_it(provider):
    rp, sink = reporter()
    provider.json(response(1))
    with agentpulse.scope(workspace="acme"):
        result = run(agents.Agent(name="helper", instructions="x", model=model(provider)), rp)
    assert result.final_output == "SECRET ANSWER"
    [a] = recorded(rp, sink)
    assert (a.model, a.billed_by, a.usage_format, a.sub_agent, a.caller_component, a.status) == ("gpt-5", "OPENAI", "OPENAI_RESPONSES", "helper", "helper", _wire.STATUS_OK)
    assert {q.unit: q.quantity for q in a.reported_usage} == {"input_tokens": 100, "output_tokens": 20, "total_tokens": 120, "input_tokens_details.cached_tokens": 40, "output_tokens_details.reasoning_tokens": 5}
    c = classes_of(a)
    assert (c.prompt, c.cached, c.candidate, c.reasoning) == (60, 40, 20, 5)
    assert (a.framework, a.framework_version) == ("openai/openai-agents-python", importlib.metadata.version("openai-agents"))
    assert a.observed_as == _wire.KIND_AGENT and a.request.startswith("trace_")
    assert sink.batches[0][0] == "acme"
    assert "SECRET" not in repr(sink.batches)


def test_an_agent_run_as_another_agent_s_tool_is_filed_under_itself_and_the_same_request(provider):
    rp, sink = reporter()

    @agents.function_tool
    def lookup(input: str) -> str:
        """Look something up."""
        return "SECRET RESULT"

    hooks = rp.openai_agents_hooks()
    researcher = agents.Agent(name="researcher", instructions="x", tools=[lookup], model=model(provider))
    boss = agents.Agent(name="boss", instructions="x", tools=[researcher.as_tool(tool_name="ask_researcher", tool_description="Ask the researcher.", hooks=hooks)], model=model(provider))
    for reply in (response(1, tool="ask_researcher"), response(2, tool="lookup"), response(3), response(4)):
        provider.json(reply)
    asyncio.run(agents.Runner.run(boss, "SECRET PROMPT", hooks=hooks, run_config=agents.RunConfig(group_id="conversation-7")))

    activities = recorded(rp, sink)
    assert [a.sub_agent for a in activities if not a.tool] == ["boss", "researcher", "researcher", "boss"]
    assert sorted((a.sub_agent, a.tool) for a in activities if a.tool) == [("boss", "ask_researcher"), ("researcher", "lookup")]
    # What a tool handed back is measured, never kept.
    assert next((a.result_bytes, a.empty_result) for a in activities if a.tool == "lookup") == (len("SECRET RESULT"), False)
    assert len({a.request for a in activities}) == 1
    assert {a.session for a in activities} == {"conversation-7"}
    assert "SECRET" not in repr(sink.batches)


def test_a_budget_that_has_run_out_stops_the_call_before_it_is_sent(provider):
    answers = Answers(_wire.DECISION_DENY)
    rp, sink = reporter(answers)
    with agentpulse.scope(user=agentpulse.User(id="u-42")):
        with pytest.raises(SpendDenied):
            run(agents.Agent(name="helper", instructions="x", model=model(provider)), rp)
    assert provider.received == []
    [asked] = answers.asked
    assert (asked.user, asked.requested_provider, asked.requested_model) == ("u-42", "OPENAI", "gpt-5")
    [a] = recorded(rp, sink)
    assert a.status == _wire.STATUS_DENIED


def test_a_downgrade_lets_the_call_through_as_asked_and_is_counted_as_not_applied(provider):
    rp, sink = reporter(Answers(_wire.DECISION_DOWNGRADE, replacement_provider="OPENAI", replacement_model="gpt-5-mini"))
    provider.json(response(1))
    run(agents.Agent(name="helper", instructions="x", model=model(provider)), rp)
    assert provider.received[0][1]["model"] == "gpt-5"
    assert rp.recorder.stats().downgrade_not_applied == 1


def test_hooks_that_fail_inside_are_counted_and_the_run_goes_on(provider):
    rp, sink = reporter()

    def broken(*args, **kwargs):
        raise RuntimeError("broken")

    rp._activity = broken
    provider.json(response(1))
    result = run(agents.Agent(name="helper", instructions="x", model=model(provider)), rp)
    assert result.final_output == "SECRET ANSWER"
    assert rp.recorder.stats().panicked >= 1


FAILURE = {"error": {"message": "SECRET QUOTED PROMPT", "type": "invalid_request_error", "code": "context_length_exceeded"}}


def test_a_failed_model_call_reaches_the_caller_unchanged_and_is_recorded_as_failed_when_its_trace_ends(provider):
    rp, sink = reporter()
    provider.json(FAILURE, status=400)
    with pytest.raises(openai.BadRequestError):
        run(agents.Agent(name="helper", instructions="x", model=model(provider)), rp)
    [a] = recorded(rp, sink)
    assert (a.model, a.sub_agent, a.status, a.error_code) == ("gpt-5", "helper", _wire.STATUS_FAILED, "Unknown")
    assert "SECRET" not in repr(sink.batches)
    assert rp.recorder.stats().panicked == 0


def test_with_tracing_off_a_failed_model_call_is_recorded_when_its_run_is_released(provider):
    rp, sink = reporter()
    provider.json(FAILURE, status=400)
    try:
        run(agents.Agent(name="helper", instructions="x", model=model(provider)), rp, run_config=agents.RunConfig(tracing_disabled=True))
    except openai.BadRequestError:
        pass
    gc.collect()
    deadline = time.monotonic() + 2
    while time.monotonic() < deadline and not recorded(rp, sink):
        time.sleep(0.02)
    [a] = recorded(rp, sink)
    assert (a.status, a.error_code) == (_wire.STATUS_FAILED, "Unknown")


def test_a_run_whose_context_takes_the_memory_of_a_released_one_is_neither_swept_by_it_nor_left_unwatched(monkeypatch):
    from agentpulse import openai_agents

    class Deferred(list):
        put = list.append

    swept = Deferred()
    monkeypatch.setattr(openai_agents, "_sweeps", swept)
    rp, sink = reporter()
    hooks = rp.openai_agents_hooks()
    agent = agents.Agent(name="helper", instructions="x", model="gpt-5")

    class Response:
        usage = None

    async def main():
        first = agents.RunContextWrapper(context=None)
        identity = id(first)
        await hooks.on_llm_start(first, agent, None, [])
        await hooks.on_llm_end(first, agent, Response())
        del first
        gc.collect()
        held, second = [], agents.RunContextWrapper(context=None)
        while id(second) != identity and len(held) < 100000:
            held.append(second)
            second = agents.RunContextWrapper(context=None)
        if id(second) != identity:
            pytest.skip("the interpreter did not reuse the context's memory")
        await hooks.on_llm_start(second, agent, None, [])
        # The released run is swept while the next one's call is in flight.
        for released, run in list(swept):
            released._sweep(run)
        await hooks.on_llm_end(second, agent, Response())
        del second, held
        gc.collect()

    asyncio.run(main())
    assert [(a.status, a.error_code) for a in recorded(rp, sink)] == [(_wire.STATUS_OK, "")] * 2
    assert len(swept) == 2


def test_the_model_a_run_configuration_names_wins_over_the_agent_s(provider):
    rp, sink = reporter()
    provider.json(response(1))
    config = agents.RunConfig(model=model(provider, name="gpt-5-mini"))
    asyncio.run(agents.Runner.run(agents.Agent(name="helper", instructions="x", model="gpt-5"), "hi", hooks=rp.openai_agents_hooks(run_config=config), run_config=config))
    [a] = recorded(rp, sink)
    assert a.model == "gpt-5-mini"


def test_the_model_and_provider_come_from_how_the_agent_names_its_model():
    from agentpulse.openai_agents import _model_and_provider

    class Agent:
        def __init__(self, model):
            self.model = model

    class LitellmModel:
        model = "anthropic/claude-sonnet-5"

    assert _model_and_provider(Agent("gpt-5")) == ("gpt-5", "OPENAI")
    assert _model_and_provider(Agent("litellm/anthropic/claude-sonnet-5")) == ("claude-sonnet-5", "ANTHROPIC")
    assert _model_and_provider(Agent(LitellmModel())) == ("claude-sonnet-5", "ANTHROPIC")
    assert _model_and_provider(Agent(None))[1] == "OPENAI"


def test_a_call_whose_start_was_not_seen_is_still_recorded():
    rp, sink = reporter()
    hooks = rp.openai_agents_hooks()

    class Agent:
        name = "helper"
        model = "gpt-5"

    class Response:
        usage = {"input_tokens": 10, "output_tokens": 2}

    asyncio.run(hooks.on_llm_end(object(), Agent(), Response()))
    [a] = recorded(rp, sink)
    assert (a.model, a.sub_agent, a.duration_ms, a.usage_format) == ("gpt-5", "helper", 0, "OPENAI_RESPONSES")


def test_a_component_and_skill_set_on_the_request_win_over_the_agent_and_a_tool_keeps_its_own(provider):
    rp, sink = reporter()

    @agents.function_tool
    def lookup(input: str) -> str:
        """Look something up."""
        return "SECRET RESULT"

    for reply in (response(1, tool="lookup"), response(2)):
        provider.json(reply)
    with agentpulse.scope(component="triage", skill="search"):
        run(agents.Agent(name="helper", instructions="x", tools=[lookup], model=model(provider)), rp)
    activities = recorded(rp, sink)
    assert [a.caller_component for a in activities if not a.tool] == ["triage", "triage"]
    assert [a.caller_component for a in activities if a.tool] == ["tool:lookup"]
    assert {a.skill for a in activities} == {"search"}
