"""Recording, and a budget that can stop a call, for an agent built on Google's Agent Development Kit.

One plugin registered on the app and nothing at the call sites. Everything a record needs is already on the framework's callback context or the model's response: which turn, which user, which session, which agent, which model, how many tokens, whether it worked. Which tenant the turn is for, and which unit of work inside it, are the two the framework cannot know, and they are read from `agentpulse.scope` where the product set them at its sign-in.

Every hook catches everything it raises. The framework re-raises an exception from a plugin as a failure of the whole run, so a plugin that let one escape would be a telemetry component able to break production. A hook that fails is counted as `panicked` and the run goes on as if the plugin were not there.

Kept apart from the rest of the library so a service that does not use the framework never imports it.
"""

from __future__ import annotations

import asyncio
import itertools
import os
import threading
import time
import weakref
from typing import Any, Callable

from . import _wire
from .context import current_component, current_request
from .failure import blocked_finish
from .report import Reporter, _Framework, _asked_user, _installed_version, result_size
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

    def take_where(self, keep: Callable[[Any, dict], bool]) -> list[tuple[Any, dict]]:
        """Removes and returns every live entry `keep` picks."""
        with self._lock:
            now = self._now()
            picked = [(k, v) for k, (v, at) in self._entries.items() if now - at < _IN_FLIGHT_TTL and keep(k, v)]
            for k, _ in picked:
                del self._entries[k]
            return picked


def _call_key(ctx: Any) -> tuple:
    # An agent makes its model calls one after another, so the key names the call it has in flight; the agent and the branch keep a sub-agent's or a parallel branch's calls apart from the rest of the turn.
    return (getattr(ctx, "invocation_id", ""), getattr(ctx, "agent_name", ""), getattr(ctx, "branch", None) or "")


def _tool_key(ctx: Any, name: str) -> tuple:
    # The framework's id for the function call tells two calls to the same tool in the same turn apart, which is what a parallel agent makes.
    call_id = getattr(ctx, "function_call_id", None)
    if call_id:
        return ("call", call_id)
    return ("tool", getattr(ctx, "invocation_id", ""), name)


FRAMEWORK = "google/adk-python"


def _framework(ctx: Any) -> _Framework:
    session = getattr(ctx, "session", None)
    return _Framework(
        request=getattr(ctx, "invocation_id", "") or "",
        session=getattr(session, "id", "") or "",
        user=getattr(ctx, "user_id", "") or "",
        # The agent the framework is running, on tool calls as on model calls, so a tool is filed under the sub-agent that ran it.
        agent=getattr(ctx, "agent_name", "") or "",
        name=FRAMEWORK,
        version=_installed_version("google-adk"),
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


def _region(ctx: Any) -> str:
    """The Vertex location the agent's Gemini client calls, or empty where it is not on Vertex or does not say."""
    try:
        client = ctx.get_invocation_context().agent.canonical_model.api_client
        if getattr(client, "vertexai", None) is True:
            location = getattr(getattr(client, "_api_client", None), "location", None)
            return location if isinstance(location, str) else ""
    except Exception:
        pass
    return ""


def _enum(value: Any) -> str:
    return str(getattr(value, "value", value) or "")


def _invocation(ctx: Any) -> Any:
    invocation = getattr(ctx, "_invocation_context", None)
    if invocation is None:
        try:
            invocation = ctx.get_invocation_context()
        except Exception:
            return None
    return invocation


def _model_calls(invocation: Any) -> tuple[Any, int | None, int]:
    """The invocation's tally of model calls, its count now, and its limit. The framework counts a call after every callback before it has run, so a count that has not moved since ours ran is a call some callback answered instead of the model."""
    try:
        tally = invocation._invocation_cost_manager
        count = tally._number_of_llm_calls
        limit = getattr(invocation.run_config, "max_llm_calls", 0) or 0
        if isinstance(count, int):
            return tally, count, limit if isinstance(limit, int) else 0
    except Exception:
        pass
    return None, None, 0


def _reached_model(entry: dict) -> bool:
    """Whether a call left unfinished was sent to the model. Where the framework's tally cannot be read, it is taken to have been."""
    tally, before = entry.get("tally"), entry.get("calls")
    if tally is None or before is None:
        return True
    now = getattr(tally, "_number_of_llm_calls", None)
    if not isinstance(now, int):
        return True
    if now <= before:
        return False
    # The framework counts a call before it checks the limit, so the one the limit refused looks sent.
    limit = entry.get("limit") or 0
    return not (limit > 0 and now > limit and now == before + 1)


_plugin_class: type | None = None

# Why a call that never finished is recorded as cut short: the run it belonged to was cancelled or abandoned before the framework said how it ended.
_CANCELED = "Canceled"


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
        """Records every model call and tool call an agent makes, and asks governance before each model call when the reporter is governed.

        One record per call, whatever the other plugins do. The framework stops at the first plugin that answers a model or tool error, so an error another plugin answers ahead of this one would never reach this plugin's own error callback, and the answer then arrives at the after callback looking like a success. So the framework's error dispatch is watched as well, on the plugin manager this plugin runs under, and a call whose error was recorded is not recorded again when an answer to it arrives.

        A call the framework never finishes, because the run was cancelled or its reader stopped reading, reaches no callback at all. It is recorded as cut short when the task that made it ends, when its run ends, when the next call in the same place begins, or when the runner closes the plugin, whichever comes first.
        """

        def __init__(self, reporter: Reporter, *, denied_message: str, tool_failed: ToolJudge | None, name: str):
            super().__init__(name=name)
            self._reporter = reporter
            self._denied_message = denied_message or DEFAULT_DENIED_MESSAGE
            self._tool_failed = tool_failed
            self._models = _InFlight()
            self._tools = _InFlight()
            self._watched: "weakref.WeakSet[Any]" = weakref.WeakSet()
            self._owners: "weakref.WeakKeyDictionary[Any, int]" = weakref.WeakKeyDictionary()
            self._owner_ids = itertools.count(1)
            # The runs open for each request, so a run nested inside a tool does not release the total of the turn it runs within. Held by invocation so a run the framework reports ending twice, once as finished and once as failed, is let go once.
            self._open_runs: dict[str, set[str]] = {}
            self._runs_lock = threading.Lock()

        def _panicked(self) -> None:
            self._reporter.recorder._note("panicked")

        def _request_of(self, invocation: Any) -> str:
            return current_request() or getattr(invocation, "invocation_id", "") or ""

        async def before_run_callback(self, *, invocation_context: Any) -> Any:
            try:
                self._watch(getattr(invocation_context, "plugin_manager", None))
                request = self._request_of(invocation_context)
                if request:
                    with self._runs_lock:
                        if request not in self._open_runs and len(self._open_runs) >= _MAX_IN_FLIGHT:
                            self._open_runs.clear()
                        self._open_runs.setdefault(request, set()).add(getattr(invocation_context, "invocation_id", "") or "")
            except Exception:
                self._panicked()
            return None

        async def after_run_callback(self, *, invocation_context: Any) -> None:
            self._run_ended(invocation_context)
            return None

        async def on_run_error_callback(self, *, invocation_context: Any, error: Exception) -> None:
            self._run_ended(invocation_context)
            return None

        def _run_ended(self, invocation: Any) -> None:
            try:
                invocation_id = getattr(invocation, "invocation_id", "") or ""
                if invocation_id:
                    self._abandon(lambda key, entry: entry.get("invocation") == invocation_id)
                request = self._request_of(invocation)
                with self._runs_lock:
                    open_runs = self._open_runs.get(request, set())
                    open_runs.discard(invocation_id)
                    left = len(open_runs)
                    if not left:
                        self._open_runs.pop(request, None)
                if not left:
                    # The turn is over, so its running total is memory nothing will read again.
                    self._reporter.finish_request(request)
            except Exception:
                self._panicked()

        async def close(self) -> None:
            try:
                self._abandon(lambda key, entry: True)
            except Exception:
                self._panicked()

        def _watch(self, manager: Any) -> None:
            """Wraps the plugin manager's model and tool error dispatch, once, so every error is seen before any plugin can answer it. The errors and the answers pass through untouched."""
            # Guarded on its own, so a manager this cannot wrap never costs the call its record or its spend check.
            try:
                self._wrap(manager)
            except Exception:
                self._panicked()

        def _wrap(self, manager: Any) -> None:
            if manager is None or manager in self._watched:
                return
            model_errors = getattr(manager, "run_on_model_error_callback", None)
            tool_errors = getattr(manager, "run_on_tool_error_callback", None)
            if not callable(model_errors) or not callable(tool_errors):
                return
            plugin = self

            # Whatever the framework passes is passed on as it came, so a change to how it calls these cannot break the run.
            async def run_on_model_error_callback(*args: Any, **kwargs: Any) -> Any:
                plugin._model_failed(kwargs.get("callback_context"), kwargs.get("llm_request"), kwargs.get("error"))
                return await model_errors(*args, **kwargs)

            async def run_on_tool_error_callback(*args: Any, **kwargs: Any) -> Any:
                plugin._tool_failed_with(kwargs.get("tool"), kwargs.get("tool_context"), kwargs.get("error"))
                return await tool_errors(*args, **kwargs)

            manager.run_on_model_error_callback = run_on_model_error_callback
            manager.run_on_tool_error_callback = run_on_tool_error_callback
            self._watched.add(manager)

        def _owner(self) -> int:
            """A number for the task the current callback runs in, watched so that whatever it leaves unfinished is recorded when it ends."""
            try:
                task = asyncio.current_task()
            except RuntimeError:
                return 0
            if task is None:
                return 0
            owner = self._owners.get(task)
            if owner is None:
                owner = next(self._owner_ids)
                self._owners[task] = owner
                task.add_done_callback(self._task_done)
            return owner

        def _task_done(self, task: Any) -> None:
            try:
                owner = self._owners.pop(task, None)
                if owner:
                    self._abandon(lambda key, entry: entry.get("owner") == owner)
            except Exception:
                self._panicked()

        def _abandon(self, pick: Callable[[Any, dict], bool]) -> None:
            """Records each call `pick` chooses that is still waiting for the framework to finish it, as cut short, and forgets the ones already recorded."""
            for _, entry in self._models.take_where(pick):
                if not entry.get("done") and _reached_model(entry):
                    self._record_cut_model(entry)
            for _, entry in self._tools.take_where(pick):
                if not entry.get("done"):
                    self._record_cut_tool(entry)

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
            invocation = _invocation(ctx)
            self._watch(getattr(invocation, "plugin_manager", None))

            # A call still waiting here was never finished: its place has moved on to the next call.
            earlier = self._models.take(key)
            if earlier and not earlier.get("done") and _reached_model(earlier):
                self._record_cut_model(earlier)

            # Labels are carried into the cloud billing export, which is what makes a charge not measured in tokens attributable at all. Only opaque identifiers, never an email. Additive, so a label set closer to the call site is never replaced.
            if _served_by_vertex(ctx):
                labels = {k: v for k, v in (("ap_user", _label(_asked_user(framework))), ("ap_component", _label(component))) if v}
                if labels:
                    if request.config is None:
                        request.config = types.GenerateContentConfig()
                    existing = dict(request.config.labels or {})
                    request.config.labels = {**labels, **existing}

            verdict = rp._decide(request.model or "", component=component, framework=framework)
            if not verdict.proceed:
                # The framework skips the model call, and the callback after it, for a response returned here; the refusal was recorded by the decision.
                return LlmResponse(content=types.Content(role="model", parts=[types.Part(text=self._denied_message)]), finish_reason=types.FinishReason.STOP)
            if verdict.replacement_model:
                billed_by = rp.attribution.billed_by or "VERTEX_AI"
                if verdict.replacement_provider and verdict.replacement_provider.upper() != billed_by.upper():
                    # Rewriting only the model name for another provider would ask this agent's own model client for a model it cannot serve.
                    rp.recorder._note("downgrade_not_applied")
                else:
                    request.model = verdict.replacement_model
                    rp.recorder._note("downgrade_applied")
            tally, calls, limit = _model_calls(invocation)
            # Noted after any downgrade, so a call whose provider reports no model falls back to what was actually sent.
            self._models.put(
                key,
                {
                    "requested": request.model or "",
                    "started": time.monotonic(),
                    "region": _region(ctx),
                    "component": component,
                    "framework": framework,
                    "invocation": getattr(ctx, "invocation_id", "") or "",
                    "owner": self._owner(),
                    "tally": tally,
                    "calls": calls,
                    "limit": limit,
                },
            )
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
                # In streaming mode every chunk arrives here, and counting them would multiply the call's usage by however many chunks it was split into. A chunk is read for the model it reports, which the framework drops from the final response it assembles, and for the counts so far, which are all a call that fails part way through ever states.
                changes = {}
                if response.model_version:
                    changes["served"] = response.model_version
                if response.usage_metadata is not None:
                    changes["usage"] = response.usage_metadata
                if changes and self._models.peek(key):
                    self._models.update(key, **changes)
                return
            known = self._models.take(key)
            if known.get("done"):
                # The answer a callback gave for a failure already recorded.
                return
            rp = self._reporter
            started = known.get("started")
            activity = rp._activity(_component(ctx), time.monotonic() - started if started else 0.0, None, (), _framework(ctx))
            activity.model = response.model_version or known.get("served") or known.get("requested", "")
            activity.region = known.get("region", "")
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
            _apply_usage(activity, response.usage_metadata)
            rp.recorder.record(activity)

        async def on_model_error_callback(self, *, callback_context: Any, llm_request: Any, error: Exception) -> Any:
            self._model_failed(callback_context, llm_request, error)
            # The error goes on to the agent exactly as it would have without this plugin.
            return None

        def _model_failed(self, ctx: Any, request: Any, error: BaseException | None) -> None:
            try:
                if ctx is None or error is None:
                    return
                key = _call_key(ctx)
                known = self._models.peek(key)
                if known.get("done"):
                    return
                # Kept, marked as recorded, so an answer some callback gives for this failure is not recorded as a second call.
                self._models.put(key, {**known, "done": True})
                rp = self._reporter
                started = known.get("started")
                activity = rp._activity(_component(ctx), time.monotonic() - started if started else 0.0, error, (), _framework(ctx))
                activity.model = known.get("served") or known.get("requested") or getattr(request, "model", "") or ""
                activity.region = known.get("region", "")
                _apply_usage(activity, known.get("usage"))
                rp.recorder.record(activity)
            except Exception:
                self._panicked()

        def _record_cut_model(self, known: dict) -> None:
            try:
                rp = self._reporter
                started = known.get("started")
                activity = rp._activity(known.get("component", "") or "model_call", time.monotonic() - started if started else 0.0, None, (), known.get("framework"))
                activity.model = known.get("served") or known.get("requested", "")
                activity.region = known.get("region", "")
                activity.status = _wire.STATUS_TRUNCATED
                activity.error_code = _CANCELED
                _apply_usage(activity, known.get("usage"))
                rp.recorder.record(activity)
            except Exception:
                self._panicked()

        async def before_tool_callback(self, *, tool: Any, tool_args: dict, tool_context: Any) -> Any:
            try:
                self._watch(getattr(_invocation(tool_context), "plugin_manager", None))
                name = getattr(tool, "name", "") or ""
                self._tools.put(
                    _tool_key(tool_context, name),
                    {
                        "started": time.monotonic(),
                        "name": name,
                        "framework": _framework(tool_context),
                        "invocation": getattr(tool_context, "invocation_id", "") or "",
                        "owner": self._owner(),
                    },
                )
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
            self._tool_failed_with(tool, tool_context, error)
            return None

        def _tool_failed_with(self, tool: Any, ctx: Any, error: BaseException | None) -> None:
            try:
                if ctx is None or error is None:
                    return
                self._record_tool(tool, ctx, None, error)
            except Exception:
                self._panicked()

        def _record_tool(self, tool: Any, ctx: Any, result: Any, error: BaseException | None) -> None:
            # A search or a lookup is charged per use, which is spend that counting tokens can never see.
            name = getattr(tool, "name", "") or ""
            key = _tool_key(ctx, name)
            if error is not None:
                known = self._tools.peek(key)
                if known.get("done"):
                    return
                # Kept, marked as recorded, so an answer some callback gives for this failure is not recorded as a second call.
                self._tools.put(key, {**known, "done": True})
            else:
                known = self._tools.take(key)
                if known.get("done"):
                    # The answer a callback gave for a failure already recorded.
                    return
            started = known.get("started")
            rp = self._reporter
            activity = rp._activity(f"tool:{name}", time.monotonic() - started if started else 0.0, error, (), _framework(ctx))
            activity.tool = name
            if error is None:
                activity.result_bytes, activity.empty_result = result_size(result)
                if self._tool_failed is not None:
                    failed, code = self._judge(name, result)
                    if failed:
                        activity.status = _wire.STATUS_FAILED
                        # A failure the adopter named with no code is still a failure, recorded under one the server can classify.
                        activity.error_code = code or "TOOL_ERROR"
                        activity.empty_result = False
            rp.recorder.record(activity)

        def _record_cut_tool(self, known: dict) -> None:
            try:
                name = known.get("name", "")
                started = known.get("started")
                rp = self._reporter
                activity = rp._activity(f"tool:{name}", time.monotonic() - started if started else 0.0, None, (), known.get("framework"))
                activity.tool = name
                activity.status = _wire.STATUS_TRUNCATED
                activity.error_code = _CANCELED
                rp.recorder.record(activity)
            except Exception:
                self._panicked()

        def _judge(self, name: str, result: Any) -> tuple[bool, str]:
            # The adopter's own code, run inside the framework's callback: one that raises is counted and the call is recorded as the framework saw it.
            try:
                failed, code = self._tool_failed(name, result)  # type: ignore[misc]
                return bool(failed), str(code or "")
            except Exception:
                self._panicked()
                return False, ""

    return AgentPulsePlugin


def _apply_usage(activity: _wire.Activity, usage: Any) -> None:
    reported = reported_from_genai(usage)
    if reported:
        activity.usage_format = FORMAT_VERTEX
        activity.reported_usage = reported_quantities(reported)
