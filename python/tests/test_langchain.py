"""Calls made through real LangChain chat models and a real LangGraph agent, against a provider on this machine. Skipped where LangChain is not installed."""

import asyncio
import importlib.metadata

import pytest

pytest.importorskip("langchain_core")
langchain_openai = pytest.importorskip("langchain_openai")

import agentpulse  # noqa: E402
from agentpulse import Attribution, Config, Recorder, Reporter, SpendDenied, _wire  # noqa: E402
from agentpulse.pricing import classes_of  # noqa: E402

from fakes import MemorySink, Provider  # noqa: E402

AGENT = "organisations/acme/agents/assistant"


def chat(model="gpt-5-2026-08-01", finish="stop", content="SECRET ANSWER", tool=None, usage=None):
    message = {"role": "assistant", "content": content}
    if tool:
        message = {"role": "assistant", "content": None, "tool_calls": [{"id": f"call-{tool}", "type": "function", "function": {"name": tool, "arguments": '{"q": "SECRET ARGUMENT"}'}}]}
        finish = "tool_calls"
    return {
        "id": "c1",
        "object": "chat.completion",
        "created": 1,
        "model": model,
        "choices": [{"index": 0, "finish_reason": finish, "message": message}],
        "usage": usage or {"prompt_tokens": 100, "completion_tokens": 50, "total_tokens": 150, "prompt_tokens_details": {"cached_tokens": 80}, "completion_tokens_details": {"reasoning_tokens": 10}},
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


def usage(activity):
    return {q.unit: q.quantity for q in activity.reported_usage}


def openai_model(provider, **kwargs):
    return langchain_openai.ChatOpenAI(model="gpt-5", api_key="k", base_url=provider.url + "/v1", max_retries=0, **kwargs)


def test_a_chat_model_call_is_recorded_in_langchain_convention_under_the_scope_it_was_made_in(provider):
    rp, sink = reporter()
    provider.json(chat())
    with agentpulse.scope(request="req-1", workspace="acme"):
        reply = openai_model(provider).invoke("SECRET PROMPT", config={"callbacks": [rp.langchain_handler()]})
    assert reply.content == "SECRET ANSWER"
    [a] = recorded(rp, sink)
    assert (a.model, a.billed_by, a.usage_format, a.request, a.status) == ("gpt-5-2026-08-01", "OPENAI", "LANGCHAIN", "req-1", _wire.STATUS_OK)
    assert usage(a) == {"input_tokens": 100, "output_tokens": 50, "total_tokens": 150, "input_token_details.cache_read": 80, "output_token_details.reasoning": 10}
    assert (a.framework, a.framework_version) == ("langchain-ai/langchain", importlib.metadata.version("langchain-core"))
    assert a.observed_as == _wire.KIND_SERVICE
    assert sink.batches[0][0] == "acme"
    assert "SECRET" not in repr(sink.batches)


def test_anthropic_s_cached_parts_come_out_of_the_input_count_langchain_adds_them_to(provider):
    langchain_anthropic = pytest.importorskip("langchain_anthropic")
    rp, sink = reporter()
    provider.json(
        {
            "id": "m1",
            "type": "message",
            "role": "assistant",
            "model": "claude-sonnet-5-20260801",
            "content": [{"type": "text", "text": "SECRET"}],
            "stop_reason": "max_tokens",
            "usage": {"input_tokens": 10, "output_tokens": 20, "cache_read_input_tokens": 300, "cache_creation_input_tokens": 50},
        }
    )
    model = langchain_anthropic.ChatAnthropic(model="claude-sonnet-5", api_key="k", base_url=provider.url, max_retries=0)
    model.invoke("SECRET PROMPT", config={"callbacks": [rp.langchain_handler()]})
    [a] = recorded(rp, sink)
    assert (a.model, a.billed_by, a.status, a.error_code) == ("claude-sonnet-5-20260801", "ANTHROPIC", _wire.STATUS_TRUNCATED, "max_tokens")
    c = classes_of(a)
    assert (c.prompt, c.cached, c.cache_write, c.candidate) == (10, 300, 50, 20)


def test_gemini_reaches_the_handler_only_in_langchain_convention_and_is_billed_by_google(provider):
    langchain_google_genai = pytest.importorskip("langchain_google_genai")
    rp, sink = reporter()
    provider.json(
        {
            "candidates": [{"content": {"role": "model", "parts": [{"text": "SECRET"}]}, "finishReason": "STOP"}],
            "modelVersion": "gemini-2.5-flash",
            "usageMetadata": {"promptTokenCount": 100, "candidatesTokenCount": 20, "cachedContentTokenCount": 30, "thoughtsTokenCount": 7, "totalTokenCount": 127},
        }
    )
    model = langchain_google_genai.ChatGoogleGenerativeAI(model="gemini-2.5-flash", google_api_key="k", base_url=provider.url)
    model.invoke("SECRET PROMPT", config={"callbacks": [rp.langchain_handler()]})
    [a] = recorded(rp, sink)
    assert (a.model, a.billed_by, a.usage_format) == ("gemini-2.5-flash", "VERTEX_AI", "LANGCHAIN")
    c = classes_of(a)
    assert (c.prompt, c.cached, c.candidate, c.reasoning) == (70, 30, 27, 7)


def test_a_langgraph_agent_files_each_call_under_the_agent_that_made_it_and_one_request(provider):
    langchain_agents = pytest.importorskip("langchain.agents")
    from langchain_core.tools import tool

    rp, sink = reporter()
    handler = rp.langchain_handler()

    @tool
    def lookup(q: str) -> str:
        """Look something up."""
        return "SECRET RESULT"

    model = openai_model(provider)
    researcher = langchain_agents.create_agent(model, tools=[lookup], name="researcher")

    @tool
    def ask_researcher(q: str) -> str:
        """Hand a question to the researcher."""
        return researcher.invoke({"messages": [{"role": "user", "content": q}]})["messages"][-1].content

    boss = langchain_agents.create_agent(model, tools=[ask_researcher], name="boss")
    for reply in (chat(tool="ask_researcher"), chat(tool="lookup"), chat(), chat()):
        provider.json(reply)
    boss.invoke({"messages": [{"role": "user", "content": "SECRET PROMPT"}]}, config={"callbacks": [handler], "configurable": {"thread_id": "t-1"}})

    activities = recorded(rp, sink)
    models = [(a.sub_agent, a.caller_component) for a in activities if not a.tool]
    tools = [(a.sub_agent, a.tool, a.caller_component) for a in activities if a.tool]
    assert models == [("boss", "boss"), ("researcher", "researcher"), ("researcher", "researcher"), ("boss", "boss")]
    assert sorted(tools) == [("boss", "ask_researcher", "tool:ask_researcher"), ("researcher", "lookup", "tool:lookup")]
    assert len({a.request for a in activities}) == 1 and activities[0].request
    assert {a.session for a in activities} == {"t-1"}
    assert {a.observed_as for a in activities} == {_wire.KIND_AGENT}
    assert {(a.framework, a.framework_version) for a in activities} == {("langchain-ai/langgraph", importlib.metadata.version("langgraph"))}
    assert "SECRET" not in repr(sink.batches)
    # The run is over, so nothing is held for it.
    assert rp.spent_on(activities[0].request) == 0
    assert not handler._roots._entries and not handler._models._entries and not handler._tools._entries


def test_a_budget_that_has_run_out_stops_the_call_before_it_is_sent(provider):
    answers = Answers(_wire.DECISION_DENY)
    rp, sink = reporter(answers)
    with agentpulse.scope(workspace="acme", user=agentpulse.User(id="u-42")):
        with pytest.raises(SpendDenied):
            openai_model(provider).invoke("SECRET PROMPT", config={"callbacks": [rp.langchain_handler()]})
    assert provider.received == []
    [asked] = answers.asked
    assert (asked.user, asked.workspace, asked.requested_provider, asked.requested_model) == ("u-42", "organisations/acme/workspaces/acme", "OPENAI", "gpt-5")
    [a] = recorded(rp, sink)
    assert a.status == _wire.STATUS_DENIED
    assert agentpulse.denied(SpendDenied())


def test_a_downgrade_lets_the_call_through_as_asked_and_is_counted_as_not_applied(provider):
    rp, sink = reporter(Answers(_wire.DECISION_DOWNGRADE, replacement_provider="OPENAI", replacement_model="gpt-5-mini"))
    provider.json(chat())
    openai_model(provider).invoke("x", config={"callbacks": [rp.langchain_handler()]})
    assert provider.received[0][1]["model"] == "gpt-5"
    assert rp.recorder.stats().downgrade_not_applied == 1
    [a] = recorded(rp, sink)
    assert a.status == _wire.STATUS_OK


def test_a_governance_that_cannot_answer_lets_every_call_through(provider):
    class Down:
        def decide(self, request, timeout):
            raise ConnectionError("down")

    rp, sink = reporter(Down())
    provider.json(chat())
    openai_model(provider).invoke("x", config={"callbacks": [rp.langchain_handler()]})
    assert rp.recorder.stats().decision_errors == 1
    [a] = recorded(rp, sink)
    assert a.status == _wire.STATUS_OK


def test_a_failed_call_is_recorded_with_its_code_and_the_error_reaches_the_caller_unchanged(provider):
    openai = pytest.importorskip("openai")
    rp, sink = reporter()
    provider.json({"error": {"message": "SECRET QUOTED PROMPT", "type": "invalid_request_error", "code": "context_length_exceeded"}}, status=400)
    with pytest.raises(openai.BadRequestError):
        openai_model(provider).invoke("x", config={"callbacks": [rp.langchain_handler()]})
    [a] = recorded(rp, sink)
    assert a.status == _wire.STATUS_FAILED and a.error_code not in ("", "Unknown")
    assert "SECRET" not in repr(sink.batches)


def test_a_handler_that_fails_inside_is_counted_and_the_run_goes_on(provider):
    rp, sink = reporter()

    def broken(*args, **kwargs):
        raise RuntimeError("broken")

    rp._activity = broken
    provider.json(chat())
    reply = openai_model(provider).invoke("x", config={"callbacks": [rp.langchain_handler()]})
    assert reply.content == "SECRET ANSWER"
    assert rp.recorder.stats().panicked >= 1


def test_a_streamed_call_is_recorded_once_with_its_usage(provider):
    rp, sink = reporter()
    chunk = {"id": "c1", "object": "chat.completion.chunk", "created": 1, "model": "gpt-5-2026-08-01"}
    provider.sse(
        [
            {**chunk, "choices": [{"index": 0, "delta": {"role": "assistant", "content": "SECRET"}, "finish_reason": None}]},
            {**chunk, "choices": [{"index": 0, "delta": {}, "finish_reason": "length"}]},
            {**chunk, "choices": [], "usage": {"prompt_tokens": 12, "completion_tokens": 3, "total_tokens": 15}},
        ]
    )
    parts = list(openai_model(provider, stream_usage=True).stream("x", config={"callbacks": [rp.langchain_handler()]}))
    assert parts
    [a] = recorded(rp, sink)
    assert usage(a)["input_tokens"] == 12 and a.status == _wire.STATUS_TRUNCATED


def test_an_async_call_is_recorded_under_the_scope_it_was_made_in(provider):
    rp, sink = reporter()
    provider.json(chat())

    async def run():
        with agentpulse.scope(request="req-async", project="p1"):
            await openai_model(provider).ainvoke("x", config={"callbacks": [rp.langchain_handler()]})

    asyncio.run(run())
    [a] = recorded(rp, sink)
    assert (a.request, a.project) == ("req-async", "p1")


def test_a_completion_model_is_recorded_with_the_counts_on_its_result(provider):
    rp, sink = reporter()
    provider.json({"id": "t1", "object": "text_completion", "created": 1, "model": "gpt-3.5-turbo-instruct-0914", "choices": [{"text": "SECRET ANSWER", "index": 0, "finish_reason": "length", "logprobs": None}], "usage": {"prompt_tokens": 5, "completion_tokens": 7, "total_tokens": 12}})
    llm = langchain_openai.OpenAI(model="gpt-3.5-turbo-instruct", api_key="k", base_url=provider.url + "/v1", max_retries=0)
    assert llm.invoke("SECRET PROMPT", config={"callbacks": [rp.langchain_handler()]}) == "SECRET ANSWER"
    [a] = recorded(rp, sink)
    assert (a.model, a.usage_format, usage(a), a.status) == ("gpt-3.5-turbo-instruct", "OPENAI_CHAT", {"prompt_tokens": 5, "completion_tokens": 7, "total_tokens": 12}, _wire.STATUS_TRUNCATED)
    assert "SECRET" not in repr(sink.batches)


def test_a_stream_that_fails_part_way_is_recorded_with_the_counts_it_had_reached(provider):
    openai = pytest.importorskip("openai")
    rp, sink = reporter()
    chunk = {"id": "c1", "object": "chat.completion.chunk", "created": 1, "model": "gpt-5-2026-08-01"}
    provider.sse(
        [
            {**chunk, "choices": [{"index": 0, "delta": {"role": "assistant", "content": "SECRET"}, "finish_reason": None}]},
            {**chunk, "choices": [], "usage": {"prompt_tokens": 12, "completion_tokens": 3, "total_tokens": 15}},
            {"error": {"message": "SECRET QUOTED PROMPT", "type": "server_error", "code": "overloaded"}},
        ],
        done=False,
    )
    with pytest.raises(openai.APIError):
        list(openai_model(provider, stream_usage=True).stream("x", config={"callbacks": [rp.langchain_handler()]}))
    [a] = recorded(rp, sink)
    assert a.status == _wire.STATUS_FAILED and usage(a)["input_tokens"] == 12
    assert "SECRET" not in repr(sink.batches)


def test_a_tool_result_is_measured_by_what_the_model_is_handed_and_never_kept():
    import uuid

    from langchain_core.messages import ToolMessage

    rp, sink = reporter()
    handler = rp.langchain_handler()
    for output in (ToolMessage(content="SECRET RESULT", tool_call_id="t1"), "", {"found": "SECRET"}):
        run_id = uuid.uuid4()
        handler.on_tool_start({"name": "lookup"}, "SECRET ARGUMENT", run_id=run_id)
        handler.on_tool_end(output, run_id=run_id)
    message, empty, mapping = recorded(rp, sink)
    assert (message.result_bytes, message.empty_result) == (len("SECRET RESULT"), False)
    assert (empty.result_bytes, empty.empty_result) == (0, True)
    assert (mapping.result_bytes, mapping.empty_result) == (len('{"found":"SECRET"}'), False)
    assert "SECRET" not in repr(sink.batches)
