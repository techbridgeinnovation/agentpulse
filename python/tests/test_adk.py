"""The plugin in a real Agent Development Kit run, with a scripted model in place of a provider. Skipped where the framework is not installed."""

import asyncio

import pytest

pytest.importorskip("google.adk")

from google.adk.agents import LlmAgent  # noqa: E402
from google.adk.agents.run_config import RunConfig, StreamingMode  # noqa: E402
from google.adk.apps import App  # noqa: E402
from google.adk.models.base_llm import BaseLlm  # noqa: E402
from google.adk.models.llm_response import LlmResponse  # noqa: E402
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


def run(replies, *, decider=None, streaming=False, prompt="SECRET PROMPT", scope=None, plugin_options=None, tools=(lookup, broken)):
    sink = MemorySink()
    rec = agentpulse.Recorder(agentpulse.Config(sinks=[sink], exit_timeout=0, flush_every=60))
    rp = agentpulse.Reporter(rec, agentpulse.Attribution(agent=AGENT, service="research-agent", skill="research"))
    if decider is not None:
        rp = rp.governed(decider, cache_ttl=0)
    model = Scripted(model="gemini-2.5-pro", replies=list(replies), asked=[])
    agent = LlmAgent(name="researcher", model=model, instruction="Answer.", tools=list(tools))
    app = App(name="probe", root_agent=agent, plugins=[rp.adk_plugin(**(plugin_options or {}))])
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
    # One turn, one request and one session across every record.
    assert len({a.request for a in sink.activities}) == 1 and len({a.session for a in sink.activities}) == 1
    assert all(a.request and a.session for a in sink.activities)
    assert "SECRET" not in repr(sink.batches)
    assert rp.recorder.stats().panicked == 0


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
