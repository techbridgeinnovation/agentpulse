"""Recording, and a budget that can stop a call, for an agent built on Google's Agent Development Kit.

One plugin registered on the app and nothing at the call sites. Everything a record needs is already on the framework's callback context or the model's response: which turn, which user, which session, which agent, which model, how many tokens, whether it worked. Which tenant the turn is for, and which unit of work inside it, are the two the framework cannot know, and they are read from `agentpulse.scope` where the product set them at its sign-in.

Every hook catches everything it raises. The framework re-raises an exception from a plugin as a failure of the whole run, so a plugin that let one escape would be a telemetry component able to break production. A hook that fails is counted as `panicked` and the run goes on as if the plugin were not there.

Kept apart from the rest of the library so a service that does not use the framework never imports it.
"""

from __future__ import annotations

import os
import threading
import time
from typing import Any, Callable

from . import _wire
from .context import current_component, current_request
from .failure import blocked_finish
from .report import Reporter, _Framework, _user_id
from .usage import FORMAT_VERTEX, reported_from_genai, reported_quantities

# What a caller sees when governance answers DENY and no message was given. Generic on purpose: the words a product's user reads on a refusal are product copy this library has no basis to guess.
DEFAULT_DENIED_MESSAGE = "This request was declined because it would exceed a configured spending limit."

# The bounds on what is held between the callback that starts a call and the one that records it. A callback that never fires must cost a fixed amount of memory, not a growing one.
_MAX_IN_FLIGHT = 4096
_IN_FLIGHT_TTL = 15 * 60.0

ToolJudge = Callable[[str, Any], "tuple[bool, str]"]


class _InFlight:
    """What one callback learned until the callback that pairs with it runs, bounded by a cap and a lifetime."""

    def __init__(self, now: Callable[[], float] = time.monotonic):
        self._lock = threading.Lock()
        self._entries: dict[Any, tuple[dict, float]] = {}
        self._now = now

    def put(self, key: Any, value: dict) -> None:
        with self._lock:
            now = self._now()
            if key not in self._entries and len(self._entries) >= _MAX_IN_FLIGHT:
                for stale in [k for k, (_, at) in self._entries.items() if now - at >= _IN_FLIGHT_TTL]:
                    del self._entries[stale]
                if len(self._entries) >= _MAX_IN_FLIGHT:
                    for oldest in sorted(self._entries, key=lambda k: self._entries[k][1])[: max(1, _MAX_IN_FLIGHT // 8)]:
                        del self._entries[oldest]
            self._entries[key] = (value, now)

    def update(self, key: Any, **changes: Any) -> None:
        with self._lock:
            value, at = self._entries.get(key, ({}, self._now()))
            self._entries[key] = ({**value, **changes}, at)

    def peek(self, key: Any) -> dict:
        with self._lock:
            value, at = self._entries.get(key, ({}, 0.0))
            return value if value and self._now() - at < _IN_FLIGHT_TTL else {}

    def take(self, key: Any) -> dict:
        with self._lock:
            value, at = self._entries.pop(key, ({}, 0.0))
            if value and self._now() - at >= _IN_FLIGHT_TTL:
                return {}
            return value


def _call_key(ctx: Any) -> tuple:
    # An agent makes its model calls one after another, so the key names the call it has in flight; the agent and the branch keep a sub-agent's or a parallel branch's calls apart from the rest of the turn.
    return (getattr(ctx, "invocation_id", ""), getattr(ctx, "agent_name", ""), getattr(ctx, "branch", None) or "")


def _tool_key(ctx: Any, name: str) -> tuple:
    # The framework's id for the function call tells two calls to the same tool in the same turn apart, which is what a parallel agent makes.
    call_id = getattr(ctx, "function_call_id", None)
    if call_id:
        return ("call", call_id)
    return ("tool", getattr(ctx, "invocation_id", ""), name)


def _framework(ctx: Any) -> _Framework:
    session = getattr(ctx, "session", None)
    return _Framework(
        request=getattr(ctx, "invocation_id", "") or "",
        session=getattr(session, "id", "") or "",
        user=getattr(ctx, "user_id", "") or "",
        # The agent the framework is running, on tool calls as on model calls, so a tool is filed under the sub-agent that ran it.
        agent=getattr(ctx, "agent_name", "") or "",
    )


def _component(ctx: Any) -> str:
    # The framework's agent name, so adopting teams set nothing; a delegating run reports the sub-agent that actually made the call.
    return current_component() or getattr(ctx, "agent_name", "") or "model_call"


def _label(value: str) -> str:
    """A value as the billing export accepts one: lowercase letters, digits, dashes and underscores, at most 63 characters. The whole value is sanitised, so the label is the sanitised form of exactly what was recorded and a billing row joins its activity."""
    value = value.strip().lower()
    out = "".join(ch if ("a" <= ch <= "z") or ("0" <= ch <= "9") or ch in "-_" else "_" for ch in value).strip("_")
    return out[:63]


def _served_by_vertex(ctx: Any) -> bool:
    """Whether the agent's model is served through Vertex, the only place a request may carry labels. The Gemini Developer API refuses a request with labels outright, so a label added there would fail every call."""
    try:
        model = ctx.get_invocation_context().agent.canonical_model
        client = getattr(model, "api_client", None)
        vertex = getattr(client, "vertexai", None)
        if isinstance(vertex, bool):
            return vertex
    except Exception:
        pass
    # The model's own client could not be asked, so the settings the Gemini client reads to choose Vertex are read instead, the newer name first.
    for name in ("GOOGLE_GENAI_USE_ENTERPRISE", "GOOGLE_GENAI_USE_VERTEXAI"):
        value = os.environ.get(name, "").strip().lower()
        if value:
            return value in ("1", "true")
    return False


def _enum(value: Any) -> str:
    return str(getattr(value, "value", value) or "")


_plugin_class: type | None = None


def plugin(reporter: Reporter, *, denied_message: str = "", tool_failed: ToolJudge | None = None, name: str = "agentpulse") -> Any:
    """The plugin for `App(plugins=[...])`. See `Reporter.adk_plugin`."""
    global _plugin_class
    if _plugin_class is None:
        _plugin_class = _build_plugin_class()
    return _plugin_class(reporter, denied_message=denied_message, tool_failed=tool_failed, name=name)


def _build_plugin_class() -> type:
    from google.adk.models.llm_response import LlmResponse
    from google.adk.plugins.base_plugin import BasePlugin
    from google.genai import types

    class AgentPulsePlugin(BasePlugin):
        """Records every model call and tool call an agent makes, and asks governance before each model call when the reporter is governed."""

        def __init__(self, reporter: Reporter, *, denied_message: str, tool_failed: ToolJudge | None, name: str):
            super().__init__(name=name)
            self._reporter = reporter
            self._denied_message = denied_message or DEFAULT_DENIED_MESSAGE
            self._tool_failed = tool_failed
            self._models = _InFlight()
            self._tools = _InFlight()

        def _panicked(self) -> None:
            self._reporter.recorder._note("panicked")

        async def after_run_callback(self, *, invocation_context: Any) -> None:
            # The turn is over, so its running total is memory nothing will read again.
            try:
                self._reporter.finish_request(current_request() or getattr(invocation_context, "invocation_id", "") or "")
            except Exception:
                self._panicked()
            return None

        async def before_model_callback(self, *, callback_context: Any, llm_request: Any) -> Any:
            try:
                return self._before_model(callback_context, llm_request)
            except Exception:
                self._panicked()
                return None

        def _before_model(self, ctx: Any, request: Any) -> Any:
            rp = self._reporter
            key = _call_key(ctx)
            framework = _framework(ctx)
            component = _component(ctx)

            # Labels are carried into the cloud billing export, which is what makes a charge not measured in tokens attributable at all. Only opaque identifiers, never an email. Additive, so a label set closer to the call site is never replaced.
            if _served_by_vertex(ctx):
                labels = {k: v for k, v in (("ap_user", _label(_user_id(framework))), ("ap_component", _label(component))) if v}
                if labels:
                    if request.config is None:
                        request.config = types.GenerateContentConfig()
                    existing = dict(request.config.labels or {})
                    request.config.labels = {**labels, **existing}

            verdict = rp._decide(request.model or "", component=component, framework=framework)
            if not verdict.proceed:
                # The framework skips the model call, and the callback after it, for a response returned here; the refusal was recorded by the decision.
                self._models.take(key)
                return LlmResponse(content=types.Content(role="model", parts=[types.Part(text=self._denied_message)]), finish_reason=types.FinishReason.STOP)
            if verdict.replacement_model:
                billed_by = rp.attribution.billed_by or "VERTEX_AI"
                if verdict.replacement_provider and verdict.replacement_provider.upper() != billed_by.upper():
                    # Rewriting only the model name for another provider would ask this agent's own model client for a model it cannot serve.
                    rp.recorder._note("downgrade_not_applied")
                else:
                    request.model = verdict.replacement_model
                    rp.recorder._note("downgrade_applied")
            # Noted after any downgrade, so a call whose provider reports no model falls back to what was actually sent.
            self._models.put(key, {"requested": request.model or "", "started": time.monotonic()})
            return None

        async def after_model_callback(self, *, callback_context: Any, llm_response: Any) -> Any:
            try:
                self._after_model(callback_context, llm_response)
            except Exception:
                self._panicked()
            return None

        def _after_model(self, ctx: Any, response: Any) -> None:
            key = _call_key(ctx)
            if response.partial:
                # In streaming mode every chunk arrives here, and counting them would multiply the call's usage by however many chunks it was split into. A chunk is read only for the model it reports, which the framework drops from the final response it assembles.
                if response.model_version:
                    self._models.update(key, served=response.model_version)
                return
            known = self._models.take(key)
            rp = self._reporter
            started = known.get("started")
            activity = rp._activity(_component(ctx), time.monotonic() - started if started else 0.0, None, (), _framework(ctx))
            activity.model = response.model_version or known.get("served") or known.get("requested", "")
            finish = _enum(response.finish_reason)
            if response.error_code:
                activity.status = _wire.STATUS_FAILED
                activity.error_code = str(response.error_code)
            elif response.interrupted or finish == "MAX_TOKENS":
                # A truncated call did partial work and was charged for it; a failed one may have been charged for nothing.
                activity.status = _wire.STATUS_TRUNCATED
            elif blocked_finish(finish):
                # The one failure that arrives on a successful response: blocked, refused or malformed, with the prompt billed all the same.
                activity.status = _wire.STATUS_FAILED
                activity.error_code = finish
            reported = reported_from_genai(response.usage_metadata)
            if reported:
                activity.usage_format = FORMAT_VERTEX
                activity.reported_usage = reported_quantities(reported)
            rp.recorder.record(activity)

        async def on_model_error_callback(self, *, callback_context: Any, llm_request: Any, error: Exception) -> Any:
            try:
                known = self._models.take(_call_key(callback_context))
                rp = self._reporter
                started = known.get("started")
                activity = rp._activity(_component(callback_context), time.monotonic() - started if started else 0.0, error, (), _framework(callback_context))
                activity.model = known.get("requested") or getattr(llm_request, "model", "") or ""
                rp.recorder.record(activity)
            except Exception:
                self._panicked()
            # The error goes on to the agent exactly as it would have without this plugin.
            return None

        async def before_tool_callback(self, *, tool: Any, tool_args: dict, tool_context: Any) -> Any:
            try:
                self._tools.put(_tool_key(tool_context, getattr(tool, "name", "")), {"started": time.monotonic()})
            except Exception:
                self._panicked()
            return None

        async def after_tool_callback(self, *, tool: Any, tool_args: dict, tool_context: Any, result: Any) -> Any:
            try:
                self._record_tool(tool, tool_context, result, None)
            except Exception:
                self._panicked()
            return None

        async def on_tool_error_callback(self, *, tool: Any, tool_args: dict, tool_context: Any, error: Exception) -> Any:
            try:
                self._record_tool(tool, tool_context, None, error)
            except Exception:
                self._panicked()
            return None

        def _record_tool(self, tool: Any, ctx: Any, result: Any, error: BaseException | None) -> None:
            # A search or a lookup is charged per use, which is spend that counting tokens can never see.
            name = getattr(tool, "name", "") or ""
            started = self._tools.take(_tool_key(ctx, name)).get("started")
            rp = self._reporter
            activity = rp._activity(f"tool:{name}", time.monotonic() - started if started else 0.0, error, (), _framework(ctx))
            activity.tool = name
            if error is None and self._tool_failed is not None:
                failed, code = self._judge(name, result)
                if failed:
                    activity.status = _wire.STATUS_FAILED
                    # A failure the adopter named with no code is still a failure, recorded under one the server can classify.
                    activity.error_code = code or "TOOL_ERROR"
            rp.recorder.record(activity)

        def _judge(self, name: str, result: Any) -> tuple[bool, str]:
            # The adopter's own code, run inside the framework's callback: one that raises is counted and the call is recorded as the framework saw it.
            try:
                failed, code = self._tool_failed(name, result)  # type: ignore[misc]
                return bool(failed), str(code or "")
            except Exception:
                self._panicked()
                return False, ""

    return AgentPulsePlugin

