"""Recording every call made through LiteLLM, its SDK or its proxy, and, on the proxy, refusing a call a budget has run out on before it is forwarded.

A team already running the LiteLLM proxy has one config file as its integration point: one line there covers every model and every service behind the proxy with no application change.

    litellm_settings:
      callbacks: agentpulse.litellm_proxy.handler

A service using the LiteLLM SDK registers the callback itself:

    litellm.callbacks = [reporter.litellm_callback()]

LiteLLM restates every provider's usage in OpenAI's shape, so the provider's own counts never reach this and they are recorded in LiteLLM's convention, `LITELLM`, which the server reads with a reader of its own. Nothing about a call's content is read: LiteLLM's logging payload carries the messages and the reply, and this takes only the model, the counts, the finish and the failure from it.

Who a call was for comes from the request itself, because a proxy serves many callers and holds none of their context: the `user` LiteLLM already takes, and `agentpulse_*` keys in the request's metadata for the request, session, workspace, project and component. An SDK call made inside `agentpulse.scope` records what the scope named, captured when the call starts, since LiteLLM reports its outcome from a thread of its own.
"""

from __future__ import annotations

import contextvars
from datetime import datetime
from typing import Any

from . import _wire, governance
from .adk import _InFlight
from .context import User, scope
from .failure import blocked_finish, truncated_finish
from .report import Reporter, _Framework
from .usage import FORMAT_LITELLM, reported_from, reported_quantities

# LiteLLM's name for who serves a call, as the rate card names who bills for it. A provider not listed is billed under its own name upper-cased, so a provider nobody has priced yet is recorded and visible rather than filed under someone else.
_BILLED_BY = {
    "vertex_ai": "VERTEX_AI",
    "vertex_ai_beta": "VERTEX_AI",
    "gemini": "VERTEX_AI",
    "anthropic": "ANTHROPIC",
    "openai": "OPENAI",
    "text-completion-openai": "OPENAI",
    "perplexity": "PERPLEXITY",
}

DENIED_TYPE = "agentpulse_denied"
DEFAULT_DENIED_MESSAGE = "This request was declined because it would exceed a configured spending limit."


def billed_by_of(provider: str | None) -> str:
    provider = (provider or "").strip().lower()
    return _BILLED_BY.get(provider, provider.upper())


def _metadata(kwargs: dict) -> dict:
    params = kwargs.get("litellm_params") or {}
    metadata = dict(params.get("metadata") or {})
    for key, value in (kwargs.get("metadata") or {}).items():
        metadata.setdefault(key, value)
    return metadata


def _str(value: Any) -> str:
    return value if isinstance(value, str) else ""


_callback_class: type | None = None


def callback(reporter: Reporter, *, denied_message: str = "") -> Any:
    """The callback for `litellm.callbacks` or the proxy's `litellm_settings.callbacks`. See `Reporter.litellm_callback`."""
    global _callback_class
    if _callback_class is None:
        _callback_class = _build_callback_class()
    return _callback_class(reporter, denied_message=denied_message)


def _build_callback_class() -> type:
    from litellm.integrations.custom_logger import CustomLogger

    class AgentPulseCallback(CustomLogger):
        """Records every completion LiteLLM makes, and on the proxy asks governance before each one when the reporter is governed."""

        def __init__(self, reporter: Reporter, *, denied_message: str):
            super().__init__()
            self._reporter = reporter
            self._denied_message = denied_message or DEFAULT_DENIED_MESSAGE
            # The context each SDK call started in, until LiteLLM reports how it ended from a thread of its own.
            self._contexts = _InFlight()
            # Calls already recorded. LiteLLM reports some calls through both its sync and async handlers, and a call is recorded once.
            self._recorded = _InFlight()

        def _panicked(self) -> None:
            self._reporter.recorder._note("panicked")

        def log_pre_api_call(self, model: Any, messages: Any, kwargs: dict) -> None:
            try:
                call_id = kwargs.get("litellm_call_id")
                if call_id:
                    self._contexts.put(call_id, {"context": contextvars.copy_context()})
            except Exception:
                self._panicked()

        async def async_log_pre_api_call(self, model: Any, messages: Any, kwargs: dict) -> None:
            self.log_pre_api_call(model, messages, kwargs)

        def log_success_event(self, kwargs: dict, response_obj: Any, start_time: Any, end_time: Any) -> None:
            self._record(kwargs, response_obj, start_time, end_time, None)

        async def async_log_success_event(self, kwargs: dict, response_obj: Any, start_time: Any, end_time: Any) -> None:
            self._record(kwargs, response_obj, start_time, end_time, None)

        def log_failure_event(self, kwargs: dict, response_obj: Any, start_time: Any, end_time: Any) -> None:
            self._record(kwargs, response_obj, start_time, end_time, kwargs.get("exception") or RuntimeError())

        async def async_log_failure_event(self, kwargs: dict, response_obj: Any, start_time: Any, end_time: Any) -> None:
            self._record(kwargs, response_obj, start_time, end_time, kwargs.get("exception") or RuntimeError())

        def _record(self, kwargs: dict, response: Any, start_time: Any, end_time: Any, error: BaseException | None) -> None:
            try:
                call_id = kwargs.get("litellm_call_id")
                if call_id:
                    if self._recorded.peek(call_id):
                        return
                    self._recorded.put(call_id, {"recorded": True})
                held = self._contexts.take(call_id) if call_id else {}
                context = held.get("context") or contextvars.copy_context()
                context.run(self._record_in_context, kwargs, response, start_time, end_time, error)
            except Exception:
                self._panicked()

        def _record_in_context(self, kwargs: dict, response: Any, start_time: Any, end_time: Any, error: BaseException | None) -> None:
            payload = kwargs.get("standard_logging_object") or {}
            metadata = _metadata(kwargs)
            rp = self._reporter
            user_id = _str(metadata.get("agentpulse_user")) or _str(payload.get("end_user")) or _str(kwargs.get("user"))
            with scope(**_scope_from(metadata, user_id)):
                framework = _Framework(
                    request=_str(payload.get("trace_id")) or _str(kwargs.get("litellm_call_id")),
                    session=_str(payload.get("session_id")),
                    user=user_id,
                )
                component = _str(metadata.get("agentpulse_component")) or _str(metadata.get("user_api_key_alias")) or _str(payload.get("call_type")) or "litellm"
                duration = (end_time - start_time).total_seconds() if isinstance(start_time, datetime) and isinstance(end_time, datetime) else 0.0
                activity = rp._activity(component, duration, error, (), framework)
                activity.model = _str(getattr(response, "model", None)) or _str(payload.get("model")) or _str(kwargs.get("model"))
                # Who served the call is LiteLLM's to say, since it routes each call; the reporter's own setting stands only where LiteLLM named nobody.
                provider = billed_by_of(payload.get("custom_llm_provider") or (kwargs.get("litellm_params") or {}).get("custom_llm_provider"))
                activity.billed_by = provider or rp.attribution.billed_by
                usage = reported_from(getattr(response, "usage", None))
                if usage:
                    activity.usage_format = FORMAT_LITELLM
                    activity.reported_usage = reported_quantities(usage)
                if error is None:
                    finish = _finish_reason(response)
                    if truncated_finish(finish):
                        activity.status = _wire.STATUS_TRUNCATED
                        activity.error_code = finish
                    elif blocked_finish(finish):
                        activity.status = _wire.STATUS_FAILED
                        activity.error_code = finish
                rp.recorder.record(activity)

        async def async_pre_call_hook(self, user_api_key_dict: Any, cache: Any, data: dict, call_type: Any) -> Any:
            """On the proxy, asks governance before a call is forwarded. A DENY refuses the request with a 403 the client is told not to retry; a DOWNGRADE forwards the replacement model; anything else, and no answer, forwards the call as asked."""
            try:
                verdict, metadata = self._decide(data)
            except Exception:
                self._panicked()
                return None
            if not verdict.proceed:
                from fastapi import HTTPException

                raise HTTPException(
                    status_code=403,
                    detail={"error": {"type": DENIED_TYPE, "code": DENIED_TYPE, "message": self._denied_message}},
                    headers={"x-should-retry": "false"},
                )
            if verdict.replacement_model and _same_provider(verdict, data.get("model")):
                data["model"] = verdict.replacement_model
                self._reporter.recorder._note("downgrade_applied")
                return data
            if verdict.replacement_model:
                self._reporter.recorder._note("downgrade_not_applied")
            return None

        def _decide(self, data: dict) -> tuple[governance.Verdict, dict]:
            metadata = dict(data.get("metadata") or {})
            model = _str(data.get("model"))
            user_id = _str(metadata.get("agentpulse_user")) or _str(data.get("user"))
            with scope(**_scope_from(metadata, user_id)):
                verdict = self._reporter._decide(
                    model,
                    provider=_provider_of(model) or self._reporter.attribution.billed_by,
                    component=_str(metadata.get("agentpulse_component")) or "litellm",
                    framework=_Framework(request=_str(metadata.get("agentpulse_request")), user=user_id),
                )
            return verdict, metadata

    return AgentPulseCallback


def _scope_from(metadata: dict, user_id: str) -> dict:
    """What the request's metadata says the call was for, as `scope` takes it. What it does not say keeps what the surrounding context already holds."""
    named: dict = {}
    for key, name in (("agentpulse_request", "request"), ("agentpulse_session", "session"), ("agentpulse_project", "project"), ("agentpulse_component", "component")):
        if _str(metadata.get(key)):
            named[name] = metadata[key]
    if _str(metadata.get("agentpulse_workspace")):
        named["workspace"] = metadata["agentpulse_workspace"]
    if user_id and _str(metadata.get("agentpulse_user")):
        named["user"] = User(id=user_id, name=_str(metadata.get("agentpulse_user_name")), email=_str(metadata.get("agentpulse_user_email")))
    return named


def _finish_reason(response: Any) -> str:
    choices = getattr(response, "choices", None)
    if isinstance(choices, list) and choices:
        return _str(getattr(choices[0], "finish_reason", None))
    return ""


def _provider_of(model: str) -> str:
    try:
        import litellm

        return billed_by_of(litellm.get_llm_provider(model)[1])
    except Exception:
        return ""


def _same_provider(verdict: governance.Verdict, model: Any) -> bool:
    if not verdict.replacement_provider:
        return True
    return verdict.replacement_provider.upper() == _provider_of(_str(model)).upper()
