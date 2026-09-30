"""Recording, and a budget that can stop a call, for an agent built on the OpenAI Agents SDK.

Run hooks passed to the run, and nothing at the call sites:

    hooks = reporter.openai_agents_hooks()
    researcher_tool = researcher.as_tool(tool_name="ask_researcher", tool_description="...", hooks=hooks)
    await Runner.run(agent, prompt, hooks=hooks)

The SDK hands the hooks the agent before each model call and the usage after it, and the agent and tool around each tool call. An agent run as another agent's tool is a run of its own, and the SDK gives it the hooks only where `as_tool` is passed them. It shares the outer run's trace, so every call it makes is filed under the same request, the trace; the trace's group, where the product set one, is the session.

The SDK restates every model's usage in the shape of OpenAI's Responses api, so the counts are recorded in that convention. It reports the model an agent asked for rather than the version that answered.

A governed reporter's hooks ask before each model call and refuse one by raising SpendDenied, which the SDK raises out of the run before the call is sent; every other failure inside the hooks is caught and counted. Hooks cannot change the model a call uses, so a DOWNGRADE lets the call through as asked and is counted as not applied.

The SDK tells hooks nothing about a model call that fails. The exception reaches the caller as it would without them, and the call is not recorded.
"""

from __future__ import annotations

import dataclasses
import time
from typing import Any

from .adk import _InFlight
from .clients import SpendDenied
from .context import current_component
from .litellm import billed_by_of
from .report import Reporter, _Framework, _installed_version
from .usage import FORMAT_OPENAI_RESPONSES, PROVIDER_OPENAI, reported_from, reported_quantities

FRAMEWORK = "openai/openai-agents-python"


def _str(value: Any) -> str:
    return value if isinstance(value, str) else ""


def _model_and_provider(agent: Any) -> tuple[str, str]:
    """The model an agent asks for, and who serves it: OpenAI, unless the model is routed through LiteLLM, which names the provider in front of the model."""
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


def hooks(reporter: Reporter) -> Any:
    """The hooks for `Runner.run(..., hooks=...)`. See `Reporter.openai_agents_hooks`."""
    global _hooks_class
    if _hooks_class is None:
        _hooks_class = _build_hooks_class()
    return _hooks_class(reporter)


def _build_hooks_class() -> type:
    from agents import RunHooks

    class AgentPulseHooks(RunHooks):
        """Records every model call and tool call an Agents SDK run makes, and asks governance before each model call when the reporter is governed."""

        def __init__(self, reporter: Reporter):
            super().__init__()
            self._reporter = reporter
            self._models = _InFlight()
            self._tools = _InFlight()

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
            model, provider = _model_and_provider(agent)
            verdict = rp._decide(model, provider=provider or rp.attribution.billed_by, component=component, framework=framework)
            if not verdict.proceed:
                # The refusal was recorded by the decision, and raising here stops the run before the call is sent.
                raise SpendDenied()
            if verdict.replacement_model:
                rp.recorder._note("downgrade_not_applied")
            self._models.put((id(context), framework.agent), {"started": time.monotonic(), "model": model, "provider": provider, "framework": framework, "component": component})

        async def on_llm_end(self, context: Any, agent: Any, response: Any) -> None:
            try:
                known = self._models.take((id(context), _str(getattr(agent, "name", None))))
                if known:
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
                self._tools.put(self._tool_key(context, tool), {"started": time.monotonic(), "framework": _framework(agent)})
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
