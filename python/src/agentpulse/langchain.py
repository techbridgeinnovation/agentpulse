"""Recording, and a budget that can stop a call, for anything built on LangChain or LangGraph.

One callback handler, passed where a run starts, and nothing at the call sites:

    agent.invoke(inputs, config={"callbacks": [reporter.langchain_handler()]})

LangChain hands a run's handlers to everything the run starts, so one handler on the outermost call reaches every model call and tool call beneath it, an agent called from inside another agent's tool included. Each call is filed under the agent LangChain names for it, the conversation LangGraph names as its thread, and the outermost run as the request, unless `agentpulse.scope` says otherwise.

LangChain restates every chat model's usage as `usage_metadata`, the one shape all of its chat models report and the only one Gemini's reaches a handler in, so the counts are recorded in LangChain's convention, `LANGCHAIN`. Nothing about a call's content is read: a handler is handed the messages, the reply and each tool's input and output, and this takes only the model, the counts, the finish and the failure.

A governed reporter's handler asks before each model call, and refuses one by raising SpendDenied, the same refusal an instrumented Gemini client raises. LangChain lets an exception out of a handler only when the handler asks it to, which this one does, so every other failure inside it is caught and counted instead. A handler has no way to change the model a call uses, so a DOWNGRADE lets the call through as asked and is counted as not applied.
"""

from __future__ import annotations

import contextvars
import time
from typing import Any

from . import _wire
from .adk import _InFlight
from .clients import SpendDenied
from .context import current_component, current_request
from .failure import blocked_finish, reported_error_for, truncated_finish
from .report import Reporter, _Framework, _installed_version
from .usage import FORMAT_LANGCHAIN, reported_from, reported_quantities

LANGCHAIN = "langchain-ai/langchain"
LANGGRAPH = "langchain-ai/langgraph"

# LangChain's name for who serves a call, as the rate card names who bills for it. A provider not listed is billed under its own name upper-cased, so a provider nobody has priced yet is recorded and visible rather than filed under someone else.
_BILLED_BY = {
    "openai": "OPENAI",
    "anthropic": "ANTHROPIC",
    "google_genai": "VERTEX_AI",
    "google_vertexai": "VERTEX_AI",
    "google_anthropic_vertex": "VERTEX_AI",
    "perplexity": "PERPLEXITY",
}


def billed_by_of(provider: Any) -> str:
    provider = provider.strip().lower() if isinstance(provider, str) else ""
    return _BILLED_BY.get(provider, provider.upper())


def _str(value: Any) -> str:
    return value if isinstance(value, str) else ""


def _framework(metadata: dict, request: str) -> _Framework:
    # A call made inside a LangGraph graph carries the node it ran in; one made by LangChain alone carries none.
    graph = "langgraph_node" in metadata
    return _Framework(
        request=request,
        session=_str(metadata.get("thread_id")),
        agent=_str(metadata.get("lc_agent_name")),
        name=LANGGRAPH if graph else LANGCHAIN,
        version=_installed_version("langgraph" if graph else "langchain-core"),
    )


def _component(metadata: dict) -> str:
    # The agent LangChain names, so adopting teams set nothing; a graph built without an agent names the node instead.
    return current_component() or _str(metadata.get("lc_agent_name")) or _str(metadata.get("langgraph_node")) or "langchain"


def _requested_model(metadata: dict, invocation: dict) -> str:
    return _str(metadata.get("ls_model_name")) or _str(invocation.get("model")) or _str(invocation.get("model_name"))


_handler_class: type | None = None


def handler(reporter: Reporter) -> Any:
    """The handler for a run's `callbacks`. See `Reporter.langchain_handler`."""
    global _handler_class
    if _handler_class is None:
        _handler_class = _build_handler_class()
    return _handler_class(reporter)


def _build_handler_class() -> type:
    from langchain_core.callbacks import BaseCallbackHandler

    class AgentPulseHandler(BaseCallbackHandler):
        """Records every model call and tool call a LangChain or LangGraph run makes, and asks governance before each model call when the reporter is governed."""

        # Only SpendDenied is ever let out; see _before_model.
        raise_error = True

        def __init__(self, reporter: Reporter):
            super().__init__()
            self._reporter = reporter
            # The outermost run each run belongs to, which is the request its calls are filed under.
            self._roots = _InFlight()
            self._models = _InFlight()
            self._tools = _InFlight()

        def _panicked(self) -> None:
            self._reporter.recorder._note("panicked")

        def _root(self, run_id: Any, parent_run_id: Any) -> str:
            if parent_run_id is not None:
                root = self._roots.peek(parent_run_id).get("root")
                if root:
                    return root
                return str(parent_run_id)
            return str(run_id)

        def on_chain_start(self, serialized: Any, inputs: Any, *, run_id: Any, parent_run_id: Any = None, **kwargs: Any) -> None:
            try:
                self._roots.put(run_id, {"root": self._root(run_id, parent_run_id)})
            except Exception:
                self._panicked()

        def on_chain_end(self, outputs: Any, *, run_id: Any, parent_run_id: Any = None, **kwargs: Any) -> None:
            self._chain_done(run_id, parent_run_id)

        def on_chain_error(self, error: BaseException, *, run_id: Any, parent_run_id: Any = None, **kwargs: Any) -> None:
            self._chain_done(run_id, parent_run_id)

        def _chain_done(self, run_id: Any, parent_run_id: Any) -> None:
            try:
                root = self._roots.take(run_id).get("root")
                if parent_run_id is None and root:
                    # The run is over, so its running total is memory nothing will read again.
                    self._reporter.finish_request(current_request() or root)
            except Exception:
                self._panicked()

        def on_chat_model_start(self, serialized: Any, messages: Any, *, run_id: Any, parent_run_id: Any = None, metadata: dict | None = None, **kwargs: Any) -> None:
            try:
                self._before_model(run_id, parent_run_id, metadata or {}, kwargs.get("invocation_params") or {})
            except SpendDenied:
                raise
            except Exception:
                self._panicked()

        def on_llm_start(self, serialized: Any, prompts: Any, *, run_id: Any, parent_run_id: Any = None, metadata: dict | None = None, **kwargs: Any) -> None:
            self.on_chat_model_start(serialized, prompts, run_id=run_id, parent_run_id=parent_run_id, metadata=metadata, **kwargs)

        def _before_model(self, run_id: Any, parent_run_id: Any, metadata: dict, invocation: dict) -> None:
            rp = self._reporter
            framework = _framework(metadata, self._root(run_id, parent_run_id))
            component = _component(metadata)
            model = _requested_model(metadata, invocation)
            provider = billed_by_of(metadata.get("ls_provider"))
            verdict = rp._decide(model, provider=provider or rp.attribution.billed_by, component=component, framework=framework)
            if not verdict.proceed:
                # The refusal was recorded by the decision, and raising here is what stops the call before it is sent.
                raise SpendDenied()
            if verdict.replacement_model:
                rp.recorder._note("downgrade_not_applied")
            self._models.put(
                run_id,
                {
                    "started": time.monotonic(),
                    "requested": model,
                    "provider": provider,
                    "framework": framework,
                    "component": component,
                    "context": contextvars.copy_context(),
                },
            )

        def on_llm_end(self, response: Any, *, run_id: Any, parent_run_id: Any = None, **kwargs: Any) -> None:
            try:
                known = self._models.take(run_id)
                if known:
                    known["context"].run(self._record_model, known, response, None)
            except Exception:
                self._panicked()

        def on_llm_error(self, error: BaseException, *, run_id: Any, parent_run_id: Any = None, **kwargs: Any) -> None:
            try:
                known = self._models.take(run_id)
                if known:
                    known["context"].run(self._record_model, known, None, error)
            except Exception:
                self._panicked()

        def _record_model(self, known: dict, response: Any, error: BaseException | None) -> None:
            rp = self._reporter
            started = known.get("started")
            activity = rp._activity(known["component"], time.monotonic() - started if started else 0.0, error, (), known["framework"])
            activity.model = known.get("requested", "")
            if known.get("provider"):
                activity.billed_by = known["provider"]
                if error is not None:
                    # Read again for who actually served the call, which LangChain named and the reporter's own setting did not.
                    reported = reported_error_for(error, activity.billed_by)
                    if reported.format:
                        activity.error_format = reported.format
                        activity.reported_error = reported.fields
            message, info = _reply(response)
            meta = getattr(message, "response_metadata", None) or {}
            if isinstance(meta, dict):
                activity.model = _str(meta.get("model_name")) or _str(meta.get("model")) or activity.model
            usage = reported_from(getattr(message, "usage_metadata", None))
            if usage:
                activity.usage_format = FORMAT_LANGCHAIN
                activity.reported_usage = reported_quantities(usage)
            if error is None:
                finish = ""
                for source in (meta, info):
                    if isinstance(source, dict):
                        finish = finish or _str(source.get("finish_reason")) or _str(source.get("stop_reason"))
                if truncated_finish(finish):
                    activity.status = _wire.STATUS_TRUNCATED
                    activity.error_code = finish
                elif blocked_finish(finish):
                    activity.status = _wire.STATUS_FAILED
                    activity.error_code = finish
            rp.recorder.record(activity)

        def on_tool_start(self, serialized: Any, input_str: Any, *, run_id: Any, parent_run_id: Any = None, metadata: dict | None = None, **kwargs: Any) -> None:
            try:
                metadata = metadata or {}
                name = _str((serialized or {}).get("name")) or _str(kwargs.get("name"))
                root = self._root(run_id, parent_run_id)
                # A tool can run an agent of its own, whose calls belong to the same request.
                self._roots.put(run_id, {"root": root})
                self._tools.put(
                    run_id,
                    {
                        "started": time.monotonic(),
                        "name": name,
                        "framework": _framework(metadata, root),
                        "context": contextvars.copy_context(),
                    },
                )
            except Exception:
                self._panicked()

        def on_tool_end(self, output: Any, *, run_id: Any, parent_run_id: Any = None, **kwargs: Any) -> None:
            self._tool_done(run_id, None)

        def on_tool_error(self, error: BaseException, *, run_id: Any, parent_run_id: Any = None, **kwargs: Any) -> None:
            self._tool_done(run_id, error)

        def _tool_done(self, run_id: Any, error: BaseException | None) -> None:
            try:
                self._roots.take(run_id)
                known = self._tools.take(run_id)
                if known:
                    known["context"].run(self._record_tool, known, error)
            except Exception:
                self._panicked()

        def _record_tool(self, known: dict, error: BaseException | None) -> None:
            # A search or a lookup is charged per use, which is spend that counting tokens can never see.
            rp = self._reporter
            name = known.get("name", "")
            started = known.get("started")
            activity = rp._activity(f"tool:{name}", time.monotonic() - started if started else 0.0, error, (), known["framework"])
            activity.tool = name
            rp.recorder.record(activity)

    return AgentPulseHandler


def _reply(response: Any) -> tuple[Any, dict]:
    """The message a model call answered with and the generation's own info, from the first generation that has a message."""
    for generations in getattr(response, "generations", None) or ():
        for generation in generations or ():
            message = getattr(generation, "message", None)
            if message is not None:
                return message, getattr(generation, "generation_info", None) or {}
    return None, {}
