"""The plugin in a real Agent Development Kit run, with a scripted model in place of a provider. Skipped where the framework is not installed."""

import asyncio
import contextlib
import importlib.metadata

import pytest

pytest.importorskip("google.adk")

from google.adk.agents import LlmAgent  # noqa: E402
from google.adk.agents.run_config import RunConfig, StreamingMode  # noqa: E402
from google.adk.apps import App  # noqa: E402
from google.adk.models.base_llm import BaseLlm  # noqa: E402
from google.adk.models.llm_response import LlmResponse  # noqa: E402
from google.adk.plugins.base_plugin import BasePlugin  # noqa: E402
from google.adk.runners import InMemoryRunner  # noqa: E402
from google.genai import types  # noqa: E402

import agentpulse  # noqa: E402
from agentpulse import _wire  # noqa: E402

from fakes import MemorySink  # noqa: E402

pytestmark = [pytest.mark.filterwarnings("ignore::UserWarning"), pytest.mark.filterwarnings("ignore::DeprecationWarning")]

AGENT = "organisations/acme/agents/research"
USAGE = types.GenerateContentResponseUsageMetadata(prompt_token_count=10, cached_content_token_count=4, candidates_token_count=2, total_token_count=12)


class Scripted(BaseLlm):
    """Answers each model call with the next scripted reply, or raises it, and remembers what it was asked."""

    replies: list = []
    asked: list = []

    async def generate_content_async(self, llm_request, stream=False):
        self.asked.append((llm_request.model, dict((llm_request.config.labels or {}) if llm_request.config else {})))
        reply = self.replies.pop(0)
        if isinstance(reply, Exception):
            raise reply
        for part in reply if isinstance(reply, list) else [reply]:
            yield part


def text(value, **kw):
    return LlmResponse(content=types.Content(role="model", parts=[types.Part(text=value)]), usage_metadata=USAGE, **kw)


def calls(name, **args):
    return LlmResponse(content=types.Content(role="model", parts=[types.Part(function_call=types.FunctionCall(name=name, args=args, id=f"fc-{name}"))]), usage_metadata=USAGE)


def lookup(city: str) -> dict:
    """Looks a city up."""
    return {"success": city != "nowhere"}


def broken(city: str) -> dict:
    """Always fails."""
    raise RuntimeError("SECRET TOOL FAILURE")


class Answers:
    def __init__(self, decision, **response):
        self.response = _wire.DecideResponse(decision=decision, **response)

    def decide(self, request, timeout):
        return self.response


def run(replies, *, decider=None, streaming=False, prompt="SECRET PROMPT", scope=None, plugin_options=None, tools=(lookup, broken), model=None, ahead=(), agent_options=None):
    sink = MemorySink()
    rec = agentpulse.Recorder(agentpulse.Config(sinks=[sink], exit_timeout=0, flush_every=60))
    rp = agentpulse.Reporter(rec, agentpulse.Attribution(agent=AGENT, service="research-agent", skill="research"))
    if decider is not None:
        rp = rp.governed(decider, cache_ttl=0)
    model = model or Scripted(model="gemini-2.5-pro")
    model.replies, model.asked = list(replies), []
    agent = LlmAgent(name="researcher", model=model, instruction="Answer.", tools=list(tools), **(agent_options or {}))
    app = App(name="probe", root_agent=agent, plugins=[*ahead, rp.adk_plugin(**(plugin_options or {}))])
    events = []

    async def main():
        runner = InMemoryRunner(app=app)
        session = await runner.session_service.create_session(app_name="probe", user_id="framework-user")
        config = RunConfig(streaming_mode=StreamingMode.SSE) if streaming else RunConfig()
        message = types.Content(role="user", parts=[types.Part(text=prompt)])
        async for event in runner.run_async(user_id="framework-user", session_id=session.id, new_message=message, run_config=config):
            events.append(event)

    error = None
    try:
        if scope is not None:
            with agentpulse.scope(**scope):
                asyncio.run(main())
        else:
            asyncio.run(main())
    except Exception as err:
        error = err
    rec.flush(timeout=2)
    return sink, rp, model, events, error


def usage(a):
    return {q.unit: q.quantity for q in a.reported_usage}


def test_a_turn_is_one_record_per_model_call_and_per_tool_call():
    sink, rp, _, _, error = run([calls("lookup", city="Nairobi"), text("done", finish_reason=types.FinishReason.STOP, model_version="gemini-2.5-pro-002")])
    assert error is None
    first, tool, last = sink.activities
    assert (first.caller_component, first.model, first.status, first.user, first.skill) == ("researcher", "gemini-2.5-pro", _wire.STATUS_OK, "framework-user", "research")
    assert usage(first) == {"promptTokenCount": 10, "cachedContentTokenCount": 4, "candidatesTokenCount": 2, "totalTokenCount": 12}
    assert (tool.caller_component, tool.tool, tool.status) == ("tool:lookup", "lookup", _wire.STATUS_OK)
    assert last.model == "gemini-2.5-pro-002"
    # The tool is filed under the agent that ran it, as its model calls are, and every call was seen through an agent's callbacks.
    assert {a.sub_agent for a in sink.activities} == {"researcher"}
    assert {a.observed_as for a in sink.activities} == {_wire.KIND_AGENT}
    # One turn, one request and one session across every record.
    assert len({a.request for a in sink.activities}) == 1 and len({a.session for a in sink.activities}) == 1
    assert all(a.request and a.session for a in sink.activities)
    assert "SECRET" not in repr(sink.batches)
    assert rp.recorder.stats().panicked == 0


def test_every_record_names_the_framework_and_the_version_installed():
    sink, _, _, _, _ = run([calls("lookup", city="Nairobi"), text("done")])
    assert {(a.framework, a.framework_version) for a in sink.activities} == {("google/adk-python", importlib.metadata.version("google-adk"))}


def test_the_person_and_tenant_the_product_set_win_over_the_framework_user():
    sink, _, _, _, _ = run([text("done")], scope={"request": "req-1", "user": agentpulse.User("u-42", "Ada"), "workspace": "acme"})
    [a] = sink.activities
    assert (a.request, a.user) == ("req-1", "u-42")
    assert sink.batches[0][0] == "acme"
    assert [u.name for _, users in sink.named for u in users] == ["Ada"]


def test_a_tool_that_reports_its_own_failure_is_judged_by_the_product():
    sink, _, _, _, _ = run(
        [calls("lookup", city="nowhere"), text("done")],
        plugin_options={"tool_failed": lambda tool, result: (not result.get("success", True), "")},
    )
    tool = next(a for a in sink.activities if a.tool)
    assert (tool.status, tool.error_code) == (_wire.STATUS_FAILED, "TOOL_ERROR")


def test_a_tool_that_raises_is_a_failure_with_its_code_and_never_its_message():
    sink, _, _, _, _ = run([calls("broken", city="x"), text("done")])
    tool = next(a for a in sink.activities if a.tool)
    assert (tool.tool, tool.status, tool.error_code) == ("broken", _wire.STATUS_FAILED, "Unknown")
    assert "SECRET" not in repr(sink.batches)


def test_a_judge_that_raises_is_counted_and_the_tool_is_recorded_as_the_framework_saw_it():
    def judge(tool, result):
        raise ValueError("bug in the product's judge")

    sink, rp, _, _, error = run([calls("lookup", city="Nairobi"), text("done")], plugin_options={"tool_failed": judge})
    assert error is None
    assert next(a for a in sink.activities if a.tool).status == _wire.STATUS_OK
    assert rp.recorder.stats().panicked == 1


def test_a_streamed_call_is_recorded_once_and_keeps_the_model_its_chunks_reported():
    partial = LlmResponse(content=types.Content(role="model", parts=[types.Part(text="do")]), partial=True, model_version="gemini-2.5-pro-002")
    final = text("done", finish_reason=types.FinishReason.STOP)
    sink, _, _, _, _ = run([[partial, final]], streaming=True)
    [a] = sink.activities
    assert a.model == "gemini-2.5-pro-002" and usage(a)["totalTokenCount"] == 12


def test_a_limit_is_truncated_and_a_safety_block_is_a_failure():
    sink, _, _, _, _ = run([text("cut", finish_reason=types.FinishReason.MAX_TOKENS)])
    assert sink.activities[0].status == _wire.STATUS_TRUNCATED
    sink, _, _, _, _ = run([text("", finish_reason=types.FinishReason.SAFETY)])
    assert (sink.activities[0].status, sink.activities[0].error_code) == (_wire.STATUS_FAILED, "SAFETY")


def test_a_model_that_raises_is_recorded_and_the_error_reaches_the_agent_unchanged():
    sink, _, _, _, error = run([TimeoutError("SECRET")])
    assert isinstance(error, TimeoutError)
    [a] = sink.activities
    assert (a.status, a.error_code, a.model) == (_wire.STATUS_FAILED, "DeadlineExceeded", "gemini-2.5-pro")


def test_a_denied_call_never_reaches_the_model_and_the_user_reads_the_product_words():
    sink, rp, model, events, error = run([text("never")], decider=Answers(_wire.DECISION_DENY), plugin_options={"denied_message": "You are out of credits."})
    assert error is None and model.asked == []
    said = [p.text for e in events if e.content for p in e.content.parts if p.text]
    assert said == ["You are out of credits."]
    [a] = sink.activities
    assert (a.status, a.model, a.caller_component) == (_wire.STATUS_DENIED, "gemini-2.5-pro", "researcher")
    assert rp.recorder.stats().denied == 1
    assert (a.framework, a.framework_version) == ("google/adk-python", importlib.metadata.version("google-adk"))


def test_a_downgrade_changes_the_model_the_framework_sends():
    sink, rp, model, _, _ = run([text("done")], decider=Answers(_wire.DECISION_DOWNGRADE, replacement_provider="VERTEX_AI", replacement_model="gemini-2.5-flash"))
    assert model.asked[0][0] == "gemini-2.5-flash"
    assert sink.activities[0].model == "gemini-2.5-flash"
    assert rp.recorder.stats().downgrade_applied == 1


def test_a_downgrade_to_another_provider_is_not_applied():
    _, rp, model, _, _ = run([text("done")], decider=Answers(_wire.DECISION_DOWNGRADE, replacement_provider="ANTHROPIC", replacement_model="claude"))
    assert model.asked[0][0] == "gemini-2.5-pro"
    assert rp.recorder.stats().downgrade_not_applied == 1


def test_a_governance_that_cannot_answer_lets_the_turn_run():
    class Down:
        def decide(self, request, timeout):
            raise ConnectionError("down")

    sink, rp, model, _, error = run([text("done")], decider=Down())
    assert error is None and len(model.asked) == 1
    assert rp.recorder.stats().decision_errors == 1


def test_labels_are_added_only_where_the_model_is_served_through_vertex(monkeypatch):
    monkeypatch.delenv("GOOGLE_GENAI_USE_VERTEXAI", raising=False)
    monkeypatch.setenv("GOOGLE_GENAI_USE_ENTERPRISE", "true")
    _, _, model, _, _ = run([text("done")], scope={"user": agentpulse.User("U-42@Example")})
    labels = model.asked[0][1]
    assert (labels["ap_user"], labels["ap_component"]) == ("u-42_example", "researcher")

    # The Gemini Developer API refuses a request carrying labels, so none are added there.
    monkeypatch.setenv("GOOGLE_GENAI_USE_ENTERPRISE", "false")
    _, _, model, _, _ = run([text("done")])
    assert not any(key.startswith("ap_") for key in model.asked[0][1])


def test_a_plugin_that_breaks_never_breaks_the_turn(monkeypatch):
    def explode(*args, **kwargs):
        raise RuntimeError("bug in the recorder")

    monkeypatch.setattr(agentpulse.Reporter, "_activity", explode)
    _, rp, model, events, error = run([calls("lookup", city="Nairobi"), text("done")])
    assert error is None and len(model.asked) == 2 and events
    assert rp.recorder.stats().panicked >= 3



class OnVertex(Scripted):
    """A scripted model whose client says it calls Vertex in a given location."""

    location: str = ""

    @property
    def api_client(self):
        inner = type("ApiClient", (), {"location": self.location})()
        return type("Client", (), {"vertexai": True, "_api_client": inner})()


def test_a_model_call_on_vertex_records_the_client_location():
    sink, _, _, _, error = run([text("done")], model=OnVertex(model="gemini-2.5-pro", location="europe-west4"))
    assert error is None and sink.activities[0].region == "europe-west4"

    sink, _, _, _, _ = run([text("done")])
    assert sink.activities[0].region == ""


class Answering(BasePlugin):
    """A plugin registered ahead of ours that answers every model and tool error, which the framework then shows no plugin behind it."""

    def __init__(self):
        super().__init__(name="answering")

    async def on_model_error_callback(self, *, callback_context, llm_request, error):
        return text("fallback")

    async def on_tool_error_callback(self, *, tool, tool_args, tool_context, error):
        return {"error": "handled"}


def test_a_model_error_another_plugin_answers_first_is_one_failed_record():
    sink, rp, _, _, error = run([TimeoutError("SECRET")], ahead=[Answering()])
    assert error is None
    [a] = sink.activities
    assert (a.status, a.error_code, a.model) == (_wire.STATUS_FAILED, "DeadlineExceeded", "gemini-2.5-pro")
    assert rp.recorder.stats().panicked == 0


def test_a_model_error_the_agent_answers_after_ours_is_recorded_once():
    sink, _, _, _, error = run([TimeoutError("SECRET")], agent_options={"on_model_error_callback": lambda callback_context, llm_request, error: text("fallback")})
    assert error is None
    [a] = sink.activities
    assert (a.status, a.error_code) == (_wire.STATUS_FAILED, "DeadlineExceeded")


def test_a_tool_error_another_plugin_answers_first_is_one_failed_record():
    sink, _, _, _, error = run([calls("broken", city="x"), text("done")], ahead=[Answering()])
    assert error is None
    [tool] = [a for a in sink.activities if a.tool]
    assert (tool.tool, tool.status, tool.error_code) == ("broken", _wire.STATUS_FAILED, "Unknown")
    assert "SECRET" not in repr(sink.batches)


def test_a_tool_error_the_agent_answers_after_ours_is_recorded_once():
    sink, _, _, _, error = run([calls("broken", city="x"), text("done")], agent_options={"on_tool_error_callback": lambda tool, args, tool_context, error: {"error": "handled"}})
    assert error is None
    [tool] = [a for a in sink.activities if a.tool]
    assert (tool.status, tool.error_code, tool.result_bytes, tool.empty_result) == (_wire.STATUS_FAILED, "Unknown", 0, False)


def test_a_model_that_fails_part_way_through_a_stream_keeps_the_counts_and_the_model_it_served():
    partial = LlmResponse(content=types.Content(role="model", parts=[types.Part(text="do")]), partial=True, model_version="gemini-2.5-pro-002", usage_metadata=USAGE)

    class FailsMidStream(Scripted):
        async def generate_content_async(self, llm_request, stream=False):
            yield partial
            raise ConnectionError("SECRET")

    sink, _, _, _, error = run([], streaming=True, model=FailsMidStream(model="gemini-2.5-pro"))
    assert isinstance(error, ConnectionError)
    [a] = sink.activities
    assert (a.status, a.model, usage(a)["totalTokenCount"]) == (_wire.STATUS_FAILED, "gemini-2.5-pro-002", 12)
    assert "SECRET" not in repr(sink.batches)


def _cancelled(model, tools=(lookup, broken), streaming=False):
    """A run cancelled while the model or a tool is still working, as a server cancels the request of a caller that hung up."""
    sink = MemorySink()
    rec = agentpulse.Recorder(agentpulse.Config(sinks=[sink], exit_timeout=0, flush_every=60))
    rp = agentpulse.Reporter(rec, agentpulse.Attribution(agent=AGENT, service="research-agent"))
    agent = LlmAgent(name="researcher", model=model, instruction="Answer.", tools=list(tools))
    app = App(name="probe", root_agent=agent, plugins=[rp.adk_plugin()])

    async def main():
        runner = InMemoryRunner(app=app)
        session = await runner.session_service.create_session(app_name="probe", user_id="u")
        message = types.Content(role="user", parts=[types.Part(text="hi")])
        config = RunConfig(streaming_mode=StreamingMode.SSE) if streaming else RunConfig()

        async def consume():
            async for _ in runner.run_async(user_id="u", session_id=session.id, new_message=message, run_config=config):
                pass

        task = asyncio.create_task(consume())
        await asyncio.sleep(0.3)
        task.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await task
        await asyncio.sleep(0.05)

    asyncio.run(main())
    rec.flush(timeout=2)
    return sink, rp


class Hangs(Scripted):
    async def generate_content_async(self, llm_request, stream=False):
        yield LlmResponse(content=types.Content(role="model", parts=[types.Part(text="do")]), partial=True, model_version="gemini-2.5-pro-002", usage_metadata=USAGE)
        await asyncio.sleep(30)
        yield text("never")


def test_a_cancelled_model_call_is_recorded_as_cut_short_with_what_it_had_used():
    sink, rp = _cancelled(Hangs(model="gemini-2.5-pro"), streaming=True)
    [a] = sink.activities
    assert (a.status, a.error_code, a.model, usage(a)["totalTokenCount"]) == (_wire.STATUS_TRUNCATED, "Canceled", "gemini-2.5-pro-002", 12)
    assert rp.recorder.stats().panicked == 0


async def slow(city: str) -> dict:
    """Takes its time."""
    await asyncio.sleep(30)
    return {}


def test_a_cancelled_tool_call_is_recorded_as_cut_short():
    model = Scripted(model="gemini-2.5-pro")
    model.replies = [calls("slow", city="x")]
    sink, _ = _cancelled(model, tools=(slow,))
    model_call, tool = sink.activities
    assert model_call.status == _wire.STATUS_OK
    assert (tool.tool, tool.status, tool.error_code) == ("slow", _wire.STATUS_TRUNCATED, "Canceled")


def empty(city: str) -> dict:
    """Finds nothing."""
    return {}


def test_a_tool_result_is_measured_and_never_kept():
    sink, _, _, _, _ = run([calls("lookup", city="Nairobi"), calls("empty", city="x"), text("done")], tools=(lookup, empty))
    found, nothing = [a for a in sink.activities if a.tool]
    assert (found.result_bytes, found.empty_result) == (len(b'{"success":true}'), False)
    assert (nothing.result_bytes, nothing.empty_result) == (0, True)
    assert "success" not in repr(sink.batches)


def test_a_run_nested_inside_a_tool_does_not_release_the_total_of_the_turn_around_it():
    rec = agentpulse.Recorder(agentpulse.Config(exit_timeout=0))
    rp = agentpulse.Reporter(rec, agentpulse.Attribution(agent=AGENT, service="s"))
    released = []
    rp.finish_request = released.append
    plugin = rp.adk_plugin()
    outer, inner = type("Run", (), {"invocation_id": "e-outer"})(), type("Run", (), {"invocation_id": "e-inner"})()

    async def turn():
        with agentpulse.scope(request="req-1"):
            await plugin.before_run_callback(invocation_context=outer)
            await plugin.before_run_callback(invocation_context=inner)
            await plugin.after_run_callback(invocation_context=inner)
            assert released == []
            await plugin.after_run_callback(invocation_context=outer)
        assert released == ["req-1"]

    asyncio.run(turn())


def test_a_run_reported_ending_twice_lets_go_of_its_turn_once():
    rec = agentpulse.Recorder(agentpulse.Config(exit_timeout=0))
    rp = agentpulse.Reporter(rec, agentpulse.Attribution(agent=AGENT, service="s"))
    released = []
    rp.finish_request = released.append
    plugin = rp.adk_plugin()
    outer, inner = type("Run", (), {"invocation_id": "e-outer"})(), type("Run", (), {"invocation_id": "e-inner"})()

    async def turn():
        with agentpulse.scope(request="req-1"):
            await plugin.before_run_callback(invocation_context=outer)
            await plugin.before_run_callback(invocation_context=inner)
            # The framework reports a run whose after-run callbacks failed as failed as well.
            await plugin.after_run_callback(invocation_context=inner)
            await plugin.on_run_error_callback(invocation_context=inner, error=RuntimeError())
            assert released == []
            await plugin.after_run_callback(invocation_context=outer)
        assert released == ["req-1"]

    asyncio.run(turn())


class _RenamedTally:
    """The framework's tally of model calls under names the plugin does not know."""

    def increment_and_enforce_llm_calls_limit(self, run_config):
        pass


def _rename_the_tally(monkeypatch):
    from google.adk.agents.invocation_context import InvocationContext

    monkeypatch.setattr(InvocationContext.__private_attributes__["_invocation_cost_manager"], "default_factory", _RenamedTally)


def test_a_framework_without_the_tally_the_plugin_reads_still_runs_and_records(monkeypatch):
    _rename_the_tally(monkeypatch)
    sink, rp, _, _, error = run([calls("lookup", city="Nairobi"), TimeoutError("SECRET")], agent_options={"on_model_error_callback": lambda callback_context, llm_request, error: text("fallback")})
    assert error is None
    assert [(a.tool, a.status, a.error_code) for a in sink.activities] == [("", _wire.STATUS_OK, ""), ("lookup", _wire.STATUS_OK, ""), ("", _wire.STATUS_FAILED, "DeadlineExceeded")]
    assert rp.recorder.stats().panicked == 0


def test_without_the_tally_a_cancelled_call_is_still_recorded_once(monkeypatch):
    _rename_the_tally(monkeypatch)
    sink, rp = _cancelled(Hangs(model="gemini-2.5-pro"), streaming=True)
    assert [(a.status, a.error_code) for a in sink.activities] == [(_wire.STATUS_TRUNCATED, "Canceled")]
    assert rp.recorder.stats().panicked == 0


def _plugin():
    sink = MemorySink()
    rec = agentpulse.Recorder(agentpulse.Config(sinks=[sink], exit_timeout=0, flush_every=60))
    rp = agentpulse.Reporter(rec, agentpulse.Attribution(agent=AGENT, service="s"))
    return sink, rp, rp.adk_plugin()


def test_a_manager_without_the_error_dispatch_the_plugin_wraps_is_left_as_it_is():
    _, rp, plugin = _plugin()
    manager = type("Manager", (), {})()
    plugin._watch(manager)
    assert vars(manager) == {} and rp.recorder.stats().panicked == 0


def test_a_manager_that_cannot_be_wrapped_costs_the_call_neither_its_record_nor_its_spend_check():
    class Refuses:
        def __contains__(self, item):
            raise TypeError()

    sink, rp, plugin = _plugin()
    rp = rp.governed(Answers(_wire.DECISION_DENY), cache_ttl=0)
    plugin = rp.adk_plugin()
    plugin._watched = Refuses()
    model = Scripted(model="gemini-2.5-pro")
    model.replies, model.asked = [text("never")], []
    app = App(name="probe", root_agent=LlmAgent(name="researcher", model=model, instruction="Answer."), plugins=[plugin])

    async def main():
        runner = InMemoryRunner(app=app)
        session = await runner.session_service.create_session(app_name="probe", user_id="u")
        async for _ in runner.run_async(user_id="u", session_id=session.id, new_message=types.Content(role="user", parts=[types.Part(text="hi")])):
            pass

    asyncio.run(main())
    rp.recorder.flush(timeout=2)
    assert model.asked == []
    assert [a.status for a in sink.activities] == [_wire.STATUS_DENIED]
    assert rp.recorder.stats().panicked >= 1


def test_one_plugin_on_two_runners_that_share_a_manager_wraps_it_once():
    sink = MemorySink()
    rec = agentpulse.Recorder(agentpulse.Config(sinks=[sink], exit_timeout=0, flush_every=60))
    rp = agentpulse.Reporter(rec, agentpulse.Attribution(agent=AGENT, service="s"))
    plugin = rp.adk_plugin()
    model = Scripted(model="gemini-2.5-pro")
    model.replies = [TimeoutError("SECRET"), TimeoutError("SECRET")]
    agent = LlmAgent(name="researcher", model=model, instruction="Answer.", on_model_error_callback=lambda callback_context, llm_request, error: text("fallback"))
    app = App(name="probe", root_agent=agent, plugins=[Answering(), plugin])

    async def main():
        first = InMemoryRunner(app=app)
        second = InMemoryRunner(app=app)
        second.plugin_manager = first.plugin_manager
        for runner in (first, second):
            session = await runner.session_service.create_session(app_name="probe", user_id="u")
            async for _ in runner.run_async(user_id="u", session_id=session.id, new_message=types.Content(role="user", parts=[types.Part(text="hi")])):
                pass
        return first.plugin_manager

    manager = asyncio.run(main())
    rec.flush(timeout=2)
    assert [(a.status, a.error_code) for a in sink.activities] == [(_wire.STATUS_FAILED, "DeadlineExceeded")] * 2
    # Wrapped once: the instance's method wraps the class's directly.
    assert manager.run_on_model_error_callback.__closure__ is not None
    inner = [c.cell_contents for c in manager.run_on_model_error_callback.__closure__ if callable(c.cell_contents) and getattr(c.cell_contents, "__self__", None) is manager]
    assert len(inner) == 1
