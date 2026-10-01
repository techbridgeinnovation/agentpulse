"""Recording, and a budget that can stop a call, for an agent built on the OpenAI Agents SDK.

Run hooks passed to the run, and nothing at the call sites:

    hooks = reporter.openai_agents_hooks()
    researcher_tool = researcher.as_tool(tool_name="ask_researcher", tool_description="...", hooks=hooks)
    await Runner.run(agent, prompt, hooks=hooks)

The SDK hands the hooks the agent before each model call and the usage after it, and the agent and tool around each tool call. An agent run as another agent's tool is a run of its own, and the SDK gives it the hooks only where `as_tool` is passed them. It shares the outer run's trace, so every call it makes is filed under the same request, the trace; the trace's group, where the product set one, is the session.

The SDK restates every model's usage in the shape of OpenAI's Responses api, so the counts are recorded in that convention. It reports the model an agent asked for rather than the version that answered.

A governed reporter's hooks ask before each model call and refuse one by raising SpendDenied, which the SDK raises out of the run before the call is sent; every other failure inside the hooks is caught and counted. Hooks cannot change the model a call uses, so a DOWNGRADE lets the call through as asked and is counted as not applied.

The SDK tells hooks nothing about a model call that fails: the exception reaches the caller as it would without them, and no hook runs. A call whose start was seen and whose end never came is recorded as failed, with no code because none was shown, when the run's trace ends, which happens however the run ended. Where tracing is off, or the product has replaced the SDK's trace processors since the hooks were built, it is recorded when the run's context is released instead, which waits on the garbage collector and on anything that still holds the run, such as a log record of its exception. A call is also recorded this way when the next call in the same place begins. A run that is cancelled is recorded the same way, since the hooks cannot tell the two apart.

The SDK does not hand hooks the run's configuration either, so a model set on `RunConfig` rather than on the agent is read only where the same configuration is given to the hooks: `reporter.openai_agents_hooks(run_config=config)`.
"""

from __future__ import annotations

import dataclasses
import itertools
import queue
import threading
import time
import weakref
from typing import Any

from . import _wire
from .adk import _InFlight
from .clients import SpendDenied
from .context import current_component
from .litellm import billed_by_of
from .report import Reporter, _Framework, _installed_version, result_size
from .usage import FORMAT_OPENAI_RESPONSES, PROVIDER_OPENAI, reported_from, reported_quantities

FRAMEWORK = "openai/openai-agents-python"

# What a call that never ended is recorded with: it failed, or its run was cancelled, and the SDK showed neither.
_UNKNOWN = "Unknown"


def _str(value: Any) -> str:
    return value if isinstance(value, str) else ""


def _model_and_provider(agent: Any, run_config: Any = None) -> tuple[str, str]:
    """The model a call is made with, and who serves it: OpenAI, unless the model is routed through LiteLLM, which names the provider in front of the model. A model on the run's configuration overrides the agent's, as it does in the SDK."""
    model = getattr(run_config, "model", None)
    if model is None:
        model = getattr(agent, "model", None)
    litellm = type(model).__name__ == "LitellmModel"
    name = _str(model) or _str(getattr(model, "model", None))
    if not name and model is None:
        try:
            from agents.models import get_default_model

            name = get_default_model()
        except Exception:
            name = ""
    if name.startswith("litellm/"):
        litellm, name = True, name[len("litellm/") :]
    if litellm:
        provider, _, served = name.partition("/")
        return (served or name), billed_by_of(provider) if served else ""
    return name, PROVIDER_OPENAI


def _trace() -> tuple[str, str]:
    """The current trace's id and group, or empty where tracing is off."""
    try:
        from agents.tracing import get_current_trace

        trace = get_current_trace()
        trace_id = _str(getattr(trace, "trace_id", None))
        if trace_id and trace_id != "no-op":
            return trace_id, _str(getattr(trace, "group_id", None))
    except Exception:
        pass
    return "", ""


def _framework(agent: Any) -> _Framework:
    request, session = _trace()
    return _Framework(request=request, session=session, agent=_str(getattr(agent, "name", None)), name=FRAMEWORK, version=_installed_version("openai-agents"))


_hooks_class: type | None = None

# Released runs waiting to be swept, and the one thread for the process that sweeps them. A SimpleQueue, since it is the one queue safe to put to from inside the garbage collector.
_sweeps: "queue.SimpleQueue[tuple[Any, int]]" = queue.SimpleQueue()
_sweeper: threading.Thread | None = None


def _start_sweeper() -> None:
    global _sweeper
    if _sweeper is not None and _sweeper.is_alive():
        return
    try:
        _sweeper = threading.Thread(target=_sweep_released, name="agentpulse-openai-agents", daemon=True)
        _sweeper.start()
    except Exception:
        _sweeper = None


def _sweep_released() -> None:
    while True:
        try:
            hooks, run = _sweeps.get()
            hooks._sweep(run)
            # Not held while waiting for the next, so a reporter nothing else uses can be collected.
            del hooks
        except Exception:
            pass

# The hooks to tell when a trace ends, through one trace processor for the process.
_ends_lock = threading.Lock()
_ends_followers: "weakref.WeakSet[Any]" = weakref.WeakSet()
_ends_registered = False


def _follow_run_ends(follower: Any) -> None:
    global _ends_registered
    with _ends_lock:
        _ends_followers.add(follower)
        if _ends_registered:
            return
        _ends_registered = True
    try:
        from agents.tracing import TracingProcessor, add_trace_processor

        class _RunEnds(TracingProcessor):
            """Reads nothing about a trace but its id, at its end."""

            def on_trace_start(self, trace: Any) -> None:
                pass

            def on_trace_end(self, trace: Any) -> None:
                trace_id = _str(getattr(trace, "trace_id", None))
                if not trace_id:
                    return
                with _ends_lock:
                    followers = list(_ends_followers)
                for f in followers:
                    f._trace_ended(trace_id)

            def on_span_start(self, span: Any) -> None:
                pass

            def on_span_end(self, span: Any) -> None:
                pass

            def shutdown(self) -> None:
                pass

            def force_flush(self) -> None:
                pass

        add_trace_processor(_RunEnds())
    except Exception:
        pass


def hooks(reporter: Reporter, run_config: Any = None) -> Any:
    """The hooks for `Runner.run(..., hooks=...)`. See `Reporter.openai_agents_hooks`."""
    global _hooks_class
    if _hooks_class is None:
        _hooks_class = _build_hooks_class()
    return _hooks_class(reporter, run_config)


def _build_hooks_class() -> type:
    from agents import RunHooks

    class AgentPulseHooks(RunHooks):
        """Records every model call and tool call an Agents SDK run makes, and asks governance before each model call when the reporter is governed."""

        def __init__(self, reporter: Reporter, run_config: Any = None):
            super().__init__()
            self._reporter = reporter
            self._run_config = run_config
            self._models = _InFlight()
            self._tools = _InFlight()
            # The runs being watched for calls they leave unfinished: a number of their own for each context, by its identity, which the next context can reuse once this one is released.
            self._watched: dict[int, int] = {}
            self._watched_lock = threading.Lock()
            self._runs = itertools.count(1)
            _follow_run_ends(self)

        def _panicked(self) -> None:
            self._reporter.recorder._note("panicked")

        async def on_llm_start(self, context: Any, agent: Any, system_prompt: Any, input_items: Any) -> None:
            try:
                self._before_model(context, agent)
            except SpendDenied:
                raise
            except Exception:
                self._panicked()

        def _before_model(self, context: Any, agent: Any) -> None:
            rp = self._reporter
            framework = _framework(agent)
            component = current_component() or framework.agent or "openai-agents"
            model, provider = _model_and_provider(agent, self._run_config)
            verdict = rp._decide(model, provider=provider or rp.attribution.billed_by, component=component, framework=framework)
            if not verdict.proceed:
                # The refusal was recorded by the decision, and raising here stops the run before the call is sent.
                raise SpendDenied()
            if verdict.replacement_model:
                rp.recorder._note("downgrade_not_applied")
            key = (id(context), framework.agent)
            # A call still waiting here never ended: the agent has moved on to its next one.
            earlier = self._models.take(key)
            if earlier:
                self._record_unended_model(earlier)
            run = self._watch(context)
            self._models.put(key, {"started": time.monotonic(), "model": model, "provider": provider, "framework": framework, "component": component, "run": run})

        def _watch(self, context: Any) -> int:
            """Records what a run left unfinished once its context is released, which is when the run is over however it ended, and returns the run's number."""
            with self._watched_lock:
                run = self._watched.get(id(context))
                if run is not None:
                    return run
                run = next(self._runs)
                try:
                    weakref.finalize(context, self._released, id(context), run)
                except TypeError:
                    return 0
                _start_sweeper()
                self._watched[id(context)] = run
                return run

        def _released(self, identity: int, run: int) -> None:
            # Called by the garbage collector, which can run inside any lock this library holds, so nothing here takes one. It runs before the context's memory is freed, so its identity is forgotten before another context can take it.
            try:
                if self._watched.get(identity) == run:
                    self._watched.pop(identity, None)
                _sweeps.put((self, run))
            except Exception:
                pass

        def _sweep(self, run: int) -> None:
            try:
                for _, known in self._models.take_where(lambda key, entry: entry.get("run") == run):
                    self._record_unended_model(known)
                for _, known in self._tools.take_where(lambda key, entry: entry.get("run") == run):
                    self._record_unended_tool(known)
            except Exception:
                self._panicked()

        def _trace_ended(self, trace_id: str) -> None:
            try:
                for _, known in self._models.take_where(lambda key, entry: entry["framework"].request == trace_id):
                    self._record_unended_model(known)
                for _, known in self._tools.take_where(lambda key, entry: entry["framework"].request == trace_id):
                    self._record_unended_tool(known)
            except Exception:
                self._panicked()

        def _record_unended_model(self, known: dict) -> None:
            try:
                rp = self._reporter
                started = known.get("started")
                activity = rp._activity(known["component"], time.monotonic() - started if started else 0.0, None, (), known["framework"])
                activity.model = known["model"]
                if known.get("provider"):
                    activity.billed_by = known["provider"]
                activity.status = _wire.STATUS_FAILED
                activity.error_code = _UNKNOWN
                rp.recorder.record(activity)
            except Exception:
                self._panicked()

        def _record_unended_tool(self, known: dict) -> None:
            try:
                rp = self._reporter
                name = known.get("name", "")
                started = known.get("started")
                activity = rp._activity(f"tool:{name}", time.monotonic() - started if started else 0.0, None, (), known["framework"])
                activity.tool = name
                activity.status = _wire.STATUS_FAILED
                activity.error_code = _UNKNOWN
                rp.recorder.record(activity)
            except Exception:
                self._panicked()

        async def on_llm_end(self, context: Any, agent: Any, response: Any) -> None:
            try:
                known = self._models.take((id(context), _str(getattr(agent, "name", None))))
                if not known:
                    # A call whose start was not seen is still recorded from what its end carries, with no duration, so its usage is never lost.
                    framework = _framework(agent)
                    model, provider = _model_and_provider(agent, self._run_config)
                    known = {"model": model, "provider": provider, "framework": framework, "component": current_component() or framework.agent or "openai-agents"}
                self._record_model(known, response)
            except Exception:
                self._panicked()

        def _record_model(self, known: dict, response: Any) -> None:
            rp = self._reporter
            started = known.get("started")
            activity = rp._activity(known["component"], time.monotonic() - started if started else 0.0, None, (), known["framework"])
            activity.model = known["model"]
            if known.get("provider"):
                activity.billed_by = known["provider"]
            # The count of requests is the SDK's own tally, not a provider's quantity.
            usage = getattr(response, "usage", None)
            # The SDK's usage is a dataclass, where every provider sdk's is a model or a mapping.
            if dataclasses.is_dataclass(usage) and not isinstance(usage, type):
                usage = dataclasses.asdict(usage)
            usage = {name: count for name, count in reported_from(usage).items() if name != "requests"}
            if usage:
                activity.usage_format = FORMAT_OPENAI_RESPONSES
                activity.reported_usage = reported_quantities(usage)
            rp.recorder.record(activity)

        async def on_tool_start(self, context: Any, agent: Any, tool: Any) -> None:
            try:
                run = self._watch(context)
                self._tools.put(self._tool_key(context, tool), {"started": time.monotonic(), "framework": _framework(agent), "name": _str(getattr(tool, "name", None)), "run": run})
            except Exception:
                self._panicked()

        async def on_tool_end(self, context: Any, agent: Any, tool: Any, result: Any) -> None:
            try:
                known = self._tools.take(self._tool_key(context, tool))
                if not known:
                    return
                # A search or a lookup is charged per use, which is spend that counting tokens can never see.
                rp = self._reporter
                name = _str(getattr(tool, "name", None))
                started = known.get("started")
                activity = rp._activity(f"tool:{name}", time.monotonic() - started if started else 0.0, None, (), known["framework"])
                activity.tool = name
                activity.result_bytes, activity.empty_result = result_size(result)
                rp.recorder.record(activity)
            except Exception:
                self._panicked()

        @staticmethod
        def _tool_key(context: Any, tool: Any) -> tuple:
            # The SDK's id for the tool call tells two calls to the same tool in one turn apart, which is what parallel tool calls make.
            call_id = _str(getattr(context, "tool_call_id", None))
            if call_id:
                return ("call", call_id)
            return ("tool", id(context), _str(getattr(tool, "name", None)))

    return AgentPulseHooks
