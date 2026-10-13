"""The way in for code that is not an agent on a framework.

There is nothing to observe such code from, so it reports for itself. What it must not have to do is know the shape of a record: the fields that are easy to fill in wrongly are the ones that quietly cost money, and every service filling them by hand is every service getting them wrong differently. So what does not change is given once, in an `Attribution`, what does is named at the call, and the mapping onto a record happens here.

Neither a model call nor a tool call states a cost. A service says what happened and the server prices it, which is what stops an agent asserting what its own work was worth.
"""

from __future__ import annotations

import dataclasses
import functools
import importlib.metadata
import inspect
import json
import os
import queue
import threading
import time
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any, Callable, Mapping, Sequence

from . import _wire, governance
from .context import current_component, current_project, current_request, current_session, current_skill, current_user, current_workspace, looks_like_email, workspace_name
from .failure import ReportedFailure, blocked_finish, error_code, reported_error_for, truncated_finish
from .gateway import ConfigError, Gateway
from .recorder import Recorder
from .usage import PROVIDER_VERTEX_AI, reported_quantities


@dataclass(frozen=True)
class Attribution:
    """What every record from one service has in common."""

    # The registered agent this service records as. Format: organisations/{organisation}/agents/{agent}
    agent: str
    # The service itself, e.g. "sources-service".
    service: str
    # Who bills for the calls, e.g. "VERTEX_AI" or "ANTHROPIC", which decides whether the spend can ever be checked against an invoice. Vertex unless the service calls a provider directly. Any string is sent as written, so a provider this library has not heard of needs no release of it.
    billed_by: str = PROVIDER_VERTEX_AI
    # The area of the product the work belongs to, where the product wants cost sliced that way. A skill set with `scope` wins over it.
    skill: str = ""


@dataclass
class Tokens:
    """A split a caller has already made, for a caller certain the classes do not overlap.

    Prefer `ModelCall.reported`. Filling this in means deciding at the call site what a provider's numbers mean, and providers disagree about that in ways that are easy to miss and expensive to get wrong: copying Gemini's prompt count into `prompt` and its cache count into `cached` charges the cached tokens twice.
    """

    prompt: int = 0
    candidate: int = 0
    cached: int = 0
    cache_write: int = 0
    reasoning: int = 0
    # As the provider reported it. Zero means the sum of the others.
    total: int = 0

    def total_or_sum(self) -> int:
        return self.total or self.prompt + self.candidate + self.cached + self.cache_write + self.reasoning

    def empty(self) -> bool:
        return not any((self.prompt, self.candidate, self.cached, self.cache_write, self.reasoning, self.total))


@dataclass
class ModelCall:
    """One finished call to a model."""

    # As the provider names it, e.g. "gemini-2.5-pro".
    model: str
    # The part of the service that spent this, e.g. "asset_summary". Defaults to the component named on the current context.
    component: str = ""
    # What the provider said, under its own name for each count, e.g. {"input_tokens": 4211, "cache_read_input_tokens": 21847}. Build it with `reported_from_genai` or `reported_from`, and name the convention in `format`.
    reported: Mapping[str, int] | None = None
    # The convention `reported` is in, e.g. FORMAT_ANTHROPIC. Required beside it: the same name means different things to different providers.
    format: str = ""
    # A split already made, for a caller who has genuinely made it. Prefer `reported`.
    tokens: Tokens | None = None
    # How long the call took, in seconds.
    duration: float = 0.0
    # How long a cache entry this call wrote is kept, in seconds. Stated by the caller because no provider reports it back, and a longer-lived entry is charged a premium.
    cache_write_ttl: float = 0.0
    # What the provider said the call cost, in millionths of a dollar, where it said.
    cost_micros: int = 0
    # How the call was served, where the same tokens are priced differently by tier, e.g. "BATCH". Stated by the caller because it does not come back on the response.
    tier: str = ""
    # Where the call was processed, as the client named it, e.g. "us-central1", "eu" or "global". A regional endpoint is charged a premium.
    region: str = ""
    # Read for whether the call failed and for its code. The message is never recorded: a provider error routinely quotes the prompt back.
    error: BaseException | None = None
    # A call that hit a limit and did partial work, kept apart from a failure because it was charged for what it did.
    truncated: bool = False
    # What the provider said ended the call, e.g. "SAFETY", "MAX_TOKENS", "content_filter". The failures that cost most arrive on a successful response: a call blocked for safety comes back with no error and a bill for the prompt.
    finish_reason: str = ""
    # What the provider said about a failure, for an error this library has no reader for. Built with `reported_error_of`; replaces what was read from `error`.
    reported_error: ReportedFailure | None = None
    # Costs on this call that tokens do not describe.
    charges: Sequence[_wire.Charge] = field(default_factory=tuple)
    # The agent framework or SDK that made the call and its version, e.g. "google/adk-python" and "1.15.0", where the caller knows them.
    framework: str = ""
    framework_version: str = ""


@dataclass
class ToolCall:
    """One finished call to a tool, worth recording on its own because a search or a lookup is charged per use, which counting tokens can never see."""

    tool: str
    duration: float = 0.0
    error: BaseException | None = None
    charges: Sequence[_wire.Charge] = field(default_factory=tuple)
    framework: str = ""
    framework_version: str = ""


@dataclass(frozen=True)
class _Framework:
    """What an agent framework knows about a call, used where the product set nothing on the context: the framework's own id for the turn, its session, its user, the agent inside the run that made the call, and which framework it is at which version."""

    request: str = ""
    session: str = ""
    user: str = ""
    agent: str = ""
    name: str = ""
    version: str = ""


@dataclass(frozen=True)
class BudgetStatusEvent:
    """What an `on_budget_status` callback is given: the budget statuses governance sent with its answer for one instrumented call, and what that call was.

    The statuses are a snapshot as of `evaluate_time`, from a verdict that may have been reused from the cache: they can trail the calls made since by the cache's lifetime plus the time spend takes to be recorded. They are for warning a person, never an exact live percentage or a hard cap.
    """

    budget_statuses: tuple[governance.BudgetStatus, ...]
    evaluate_time: datetime | None
    # The decision the statuses came with, such as ALLOW or NOTIFY.
    decision: str
    # The model and provider the call asked for, and the part of the service making it.
    model: str
    provider: str
    component: str
    # Who and what the call was for, as the question to governance named them. The user is empty where the identifier looked like an email address, which is never sent.
    user: str = ""
    workspace: str = ""
    project: str = ""
    request: str = ""


BudgetStatusCallback = Callable[[BudgetStatusEvent], Any]

# How many events wait for the callback. A callback slower than decisions arrive loses the newest events rather than growing memory or holding up a model call.
_BUDGET_STATUS_QUEUE = 64


class _BudgetStatusWorker:
    """Delivers budget status events to the product's callback one at a time, in the order they were queued, on a daemon thread of its own.

    Nothing here ever waits on the callback: `offer` puts the event on a bounded queue and returns, and a full queue drops the event and counts it. A callback that is running is never interrupted, so a slow one only delays the deliveries behind it. Daemon, so neither it nor a callback stuck in it holds the interpreter open at exit; what is still queued then is lost, which is delivery at most once.
    """

    def __init__(self, recorder: Recorder, callback: BudgetStatusCallback):
        self._recorder = recorder
        self._callback = callback
        self._lock = threading.Lock()
        self._pid = os.getpid()
        self._queue: queue.Queue = queue.Queue(maxsize=_BUDGET_STATUS_QUEUE)
        self._started = False

    def offer(self, event: BudgetStatusEvent) -> None:
        try:
            self._ensure_started()
            self._queue.put_nowait(event)
        except queue.Full:
            self._recorder._note("budget_status_dropped")
        except Exception:
            self._recorder._note("panicked")

    def _ensure_started(self) -> None:
        with self._lock:
            if self._pid != os.getpid():
                # A forked child has none of the parent's threads, and what the parent had queued stays the parent's.
                self._pid = os.getpid()
                self._queue = queue.Queue(maxsize=_BUDGET_STATUS_QUEUE)
                self._started = False
            if not self._started:
                threading.Thread(target=self._run, args=(self._queue,), name="agentpulse-budget-status", daemon=True).start()
                self._started = True

    def _run(self, events: queue.Queue) -> None:
        while True:
            event = events.get()
            try:
                result = self._callback(event)
                if inspect.isawaitable(result):
                    # Nothing here runs an event loop, so an async callback can never have run. Counted rather than left unnoticed.
                    close = getattr(result, "close", None)
                    if callable(close):
                        close()
                    self._recorder._note("budget_status_failed")
            except BaseException:
                self._recorder._note("budget_status_failed")


@functools.lru_cache(maxsize=None)
def _installed_version(distribution: str) -> str:
    """The installed version of a package, read once, or empty when it cannot be read."""
    try:
        version = importlib.metadata.version(distribution)
    except Exception:
        return ""
    return version if isinstance(version, str) else ""


def _jsonable(value: Any) -> Any:
    dump = getattr(value, "model_dump", None)
    if callable(dump):
        return dump(mode="json")
    if dataclasses.is_dataclass(value) and not isinstance(value, type):
        return dataclasses.asdict(value)
    raise TypeError("not serialisable")


def result_size(result: Any) -> tuple[int, bool]:
    """How many bytes a tool handed back, and whether it handed back nothing, measured the way the Go recorder measures it: from the JSON the result is sent to the model as. Text is measured as itself, since that is what the model is given.

    A size, never the content. A result that cannot be serialised is unmeasured, zero bytes and not empty, because calling it empty would claim the tool returned nothing when it may have returned plenty.
    """
    try:
        if result is None:
            return 0, True
        if isinstance(result, str):
            size = len(result.encode("utf-8"))
            return size, size == 0
        if isinstance(result, (bytes, bytearray)):
            return len(result), len(result) == 0
        if isinstance(result, (Mapping, list, tuple, set, frozenset)) and len(result) == 0:
            return 0, True
        if isinstance(result, (set, frozenset)):
            result = list(result)
        encoded = json.dumps(result, ensure_ascii=False, separators=(",", ":"), default=_jsonable)
        return len(encoded.encode("utf-8")), False
    except Exception:
        return 0, False


def _user_id(framework: _Framework | None) -> str:
    # A person set on the context wins, because it is what the product's own sign-in put there, name and all.
    user = current_user()
    if user is not None and user.id:
        return user.id
    return framework.user if framework else ""


def _asked_user(framework: _Framework | None) -> str:
    """`_user_id` for the places that label or ask rather than record. A record has an address removed where it is queued; a label and a spend decision remove their own."""
    user = _user_id(framework)
    return "" if looks_like_email(user) else user


class Reporter:
    """Records on behalf of one service. Cheap to hold and safe to share across threads."""

    def __init__(self, recorder: Recorder, attribution: Attribution, *, gateway: Gateway | None = None):
        self.recorder = recorder
        self.attribution = attribution
        self._gateway = gateway
        self._decider: governance.Decider | None = None
        self._decide_timeout = governance.DEFAULT_DECIDE_TIMEOUT
        self._subscriber: Any = None
        self._budget_status: _BudgetStatusWorker | None = None

    def governed(
        self,
        decider: governance.Decider | None = None,
        *,
        cache_ttl: float = 30.0,
        timeout: float = governance.DEFAULT_DECIDE_TIMEOUT,
        failure_backoff: float = governance.DEFAULT_FAILURE_BACKOFF,
        follow_changes: bool = True,
        on_budget_status: BudgetStatusCallback | None = None,
    ) -> "Reporter":
        """A reporter whose `decide` asks governance, so a budget that stops an agent stops this service spending against the same budget too.

        The decider defaults to governance through the gateway this reporter records to. A verdict is reused for `cache_ttl` seconds, which is how far past a ceiling spend can run before it is noticed, times the number of processes running; zero asks every time. `timeout` bounds how long a call waits for an answer before it goes ahead without one, and after governance fails to answer a question it is not asked again for `failure_backoff` seconds, so an outage slows no call past the first.

        Governance through the gateway starts opening its connections here, in the background, so the first decision on a new instance does not spend its timeout on a handshake.

        With `follow_changes`, governance through the gateway also tells this process when a budget changes, and the verdicts it covers are dropped at once rather than when they expire.

        `on_budget_status` is called with a `BudgetStatusEvent` for every instrumented call (a wrapped client, a framework plugin, hook or callback) whose decision came with at least one budget status, at any level including WITHIN, so the product can warn its own users; a call to `decide` returns the statuses on its verdict instead. Delivery is asynchronous and the model call never waits for it: the event goes on a queue of 64 and one daemon thread per reporter calls the function, one event at a time and in the order the decisions were obtained, at most once, so what is still queued at exit is lost. The function therefore runs on that thread, not the caller's thread or event loop, and must be thread-safe. A full queue drops the event and counts it in `budget_status_dropped`. Whatever the function raises is counted in `budget_status_failed`, as is an async function, which is never awaited. A running function is never interrupted, so a slow one only delays the deliveries behind it. It is not called when no decision was reached.
        """
        through_gateway = decider is None
        if decider is None:
            if self._gateway is None:
                raise ConfigError("agentpulse: a reporter built without a gateway needs a decider to be governed")
            decider = governance.GatewayDecider(self._gateway)
        governed = Reporter(self.recorder, self.attribution, gateway=self._gateway)
        governed._decider = governance.CachingDecider(decider, cache_ttl, failure_backoff=failure_backoff) if cache_ttl > 0 or failure_backoff > 0 else decider
        organisation, found, _ = self.attribution.agent.partition("/agents/")
        if follow_changes and through_gateway and cache_ttl > 0 and found and self._gateway is not None:
            from .invalidations import InvalidationSubscriber

            governed._subscriber = InvalidationSubscriber(self._gateway, governed._decider, organisation)
        governed._decide_timeout = timeout if timeout > 0 else governance.DEFAULT_DECIDE_TIMEOUT
        if on_budget_status is not None:
            governed._budget_status = _BudgetStatusWorker(self.recorder, on_budget_status)
        if through_gateway:
            governance.warm(decider, governed._decide_timeout)
        return governed

    def decide(self, model: str = "", *, provider: str = "", component: str = "") -> governance.Verdict:
        """Asks whether a call to `model` may proceed, before it is made, and never raises.

        Call it where the service is about to call a model, and skip the call when `proceed` is false: the refusal is recorded here, with nothing spent, so refusals are countable. A DOWNGRADE names a replacement model for the caller to use instead. Who bills for the call is the reporter's unless `provider` says otherwise.

        Only a genuine DENY refuses. Governance unreachable, slow or saying something this version does not know lets the call go ahead, and is counted. A reporter that is not governed answers UNDECIDED to everything and asks nobody.

        The verdict carries how each budget that applies stood, in `budget_statuses`, as of `evaluate_time`. The reporter's `on_budget_status` is not called for it: the caller already holds the statuses.
        """
        return self._decide(model, provider=provider, component=component, instrumented=False)

    def _decide(self, model: str, *, provider: str = "", component: str = "", framework: "_Framework | None" = None, instrumented: bool = True) -> governance.Verdict:
        verdict = governance.Verdict()
        if self._decider is None:
            return verdict
        if self._subscriber is not None:
            try:
                self._subscriber.ensure_running()
            except Exception:
                self.recorder._note("panicked")
        billed_by = provider or self.attribution.billed_by or PROVIDER_VERTEX_AI
        try:
            organisation, found, _ = self.attribution.agent.partition("/agents/")
            if not found or not organisation:
                self.recorder._note("decision_errors")
                return verdict
            request = _wire.DecideRequest(
                parent=organisation,
                agent=self.attribution.agent,
                user=_asked_user(framework),
                workspace=workspace_name(organisation, current_workspace()),
                project=current_project(),
                requested_provider=billed_by,
                requested_model=model,
            )
            verdict = governance.ask(self._decider, request, self._decide_timeout)
        except Exception:
            self.recorder._note("decision_errors")
            return governance.Verdict()

        if instrumented and verdict.budget_statuses and self._budget_status is not None:
            self._tell_budget_status(verdict, request, component or current_component(), framework)
        if verdict.decision == governance.NOTIFY:
            self.recorder._note("notified")
        elif verdict.decision == governance.DOWNGRADE:
            self.recorder._note("downgraded")
        elif verdict.decision == governance.DENY:
            self.recorder._note("denied")
            self._record_denied(component or current_component(), model, billed_by, framework)
        return verdict

    def _tell_budget_status(self, verdict: governance.Verdict, asked: _wire.DecideRequest, component: str, framework: "_Framework | None") -> None:
        """Queues the statuses for the product's callback and returns at once. The event is built here, on the calling thread, because the request on the context is only this call's here."""
        try:
            self._budget_status.offer(  # type: ignore[union-attr]
                BudgetStatusEvent(
                    budget_statuses=verdict.budget_statuses,
                    evaluate_time=verdict.evaluate_time,
                    decision=verdict.decision,
                    model=asked.requested_model,
                    provider=asked.requested_provider,
                    component=component,
                    user=asked.user,
                    workspace=asked.workspace,
                    project=asked.project,
                    request=current_request() or (framework.request if framework else ""),
                )
            )
        except Exception:
            self.recorder._note("panicked")

    def spent_on(self, request: str) -> int:
        """What a request has cost so far in this process, in millionths of a dollar. See `Recorder.spent_on`."""
        return self.recorder.spent_on(request)

    def finish_request(self, request: str) -> None:
        """Releases a request's running total when the request is done."""
        self.recorder.finish_request(request)

    def openai_http_client(self, *, asynchronous: bool = False, **kwargs: Any) -> Any:
        """An HTTP client for `OpenAI(http_client=...)` or `AsyncOpenAI(...)` that records every Chat Completions and Responses call made through it, and asks governance first when the reporter is governed.

        The same client pointed at another provider that speaks OpenAI's api, such as Perplexity, is recorded the same way. Built from the sdk's own default client, so the sdk's timeouts and limits still apply; `kwargs` go to it.
        """
        from . import clients

        return clients.http_client(self, "openai", asynchronous, kwargs)

    def anthropic_http_client(self, *, asynchronous: bool = False, **kwargs: Any) -> Any:
        """An HTTP client for `Anthropic(http_client=...)` or `AsyncAnthropic(...)` that records every Messages call made through it. Claude through Vertex is billed by Google; set `billed_by` on the reporter for that client."""
        from . import clients

        return clients.http_client(self, "anthropic", asynchronous, kwargs)

    def genai_http_options(self, **kwargs: Any) -> Any:
        """`HttpOptions` for `genai.Client(http_options=...)` that record every generate call, sync and async, on the Gemini api and Vertex alike. `kwargs` are any other HttpOptions fields."""
        from . import clients

        return clients.genai_http_options(self, kwargs)

    def adk_plugin(self, *, denied_message: str = "", tool_failed: Any = None, name: str = "agentpulse") -> Any:
        """A plugin for Google's Agent Development Kit, for `App(plugins=[...])`, that records every model call and tool call the agent makes, and asks governance before each model call when the reporter is governed.

        `denied_message` is what the agent's user reads when a budget refuses a call, in the model's own voice; the words are product copy, so give your own. `tool_failed(tool, result)` returns `(failed, code)` for a tool that reports its own failure in its result rather than raising, which only the product can judge.
        """
        from . import adk

        return adk.plugin(self, denied_message=denied_message, tool_failed=tool_failed, name=name)

    def litellm_callback(self, *, denied_message: str = "") -> Any:
        """A LiteLLM callback, for `litellm.callbacks` in a service or the proxy's `litellm_settings.callbacks`, that records every completion LiteLLM makes. On the proxy, a governed reporter's callback refuses a call a budget has run out on before it is forwarded, with `denied_message` as the reason."""
        from . import litellm

        return litellm.callback(self, denied_message=denied_message)

    def langchain_handler(self) -> Any:
        """A callback handler for a LangChain or LangGraph run's `callbacks`, that records every model call and tool call the run makes, and refuses a model call a budget has run out on by raising SpendDenied when the reporter is governed. Pass it on the outermost call and everything the run starts is covered."""
        from . import langchain

        return langchain.handler(self)

    def openai_agents_hooks(self, *, run_config: Any = None) -> Any:
        """Run hooks for the OpenAI Agents SDK, for `Runner.run(..., hooks=...)`, that record every model call and tool call the run makes, and refuse a model call a budget has run out on by raising SpendDenied when the reporter is governed.

        `run_config` is the `RunConfig` the run is given, where it names the model, since the SDK does not show it to hooks."""
        from . import openai_agents

        return openai_agents.hooks(self, run_config)

    def transport(self, inner: Any, *, asynchronous: bool = False) -> Any:
        """Wraps an httpx or httpx2 transport so every model call through it is recorded, for a client this library has no helper for."""
        from . import clients

        return (clients.AsyncObservingTransport if asynchronous else clients.ObservingTransport)(self, inner)

    def _record_denied(self, component: str, model: str, billed_by: str, framework: "_Framework | None" = None) -> None:
        try:
            activity = self._activity(component, 0.0, None, (), framework)
            activity.model = model
            activity.billed_by = billed_by
            activity.status = _wire.STATUS_DENIED
        except Exception:
            self.recorder._note("panicked")
            return
        self.recorder.record(activity)

    def model_call(self, call: ModelCall) -> None:
        """Records a finished model call and returns immediately. Never blocks and never raises: recording is not allowed to be the reason a request failed."""
        try:
            activity = self._activity(call.component or current_component(), call.duration, call.error, call.charges)
            activity.model = call.model
            activity.usage_format = call.format
            activity.reported_usage = reported_quantities(call.reported)
            activity.service_tier = call.tier
            activity.region = call.region if isinstance(call.region, str) else ""
            activity.provider_cost_micros = int(call.cost_micros)
            activity.cache_write_ttl_seconds = int(call.cache_write_ttl)
            activity.framework = call.framework if isinstance(call.framework, str) else ""
            activity.framework_version = call.framework_version if isinstance(call.framework_version, str) else ""

            # A split the caller made is carried as given, beside the provider's own counts where both are present, which is the cheapest way to prove the server reads a convention the way the caller already did.
            if call.tokens is not None and not call.tokens.empty():
                activity.prompt_tokens = call.tokens.prompt
                activity.candidate_tokens = call.tokens.candidate
                activity.cached_tokens = call.tokens.cached
                activity.cache_write_tokens = call.tokens.cache_write
                activity.reasoning_tokens = call.tokens.reasoning
                activity.total_tokens = call.tokens.total_or_sum()

            # A reading the call site made beats one guessed at here: the call site held the provider's own error.
            if call.reported_error is not None and call.reported_error.format:
                activity.error_format = call.reported_error.format
                activity.reported_error = list(call.reported_error.fields)

            if call.error is None:
                if call.truncated or truncated_finish(call.finish_reason):
                    activity.status = _wire.STATUS_TRUNCATED
                elif blocked_finish(call.finish_reason):
                    activity.status = _wire.STATUS_FAILED
                    activity.error_code = call.finish_reason
        except Exception:
            self.recorder._note("panicked")
            return
        self.recorder.record(activity)

    def tool_call(self, call: ToolCall) -> None:
        """Records a finished tool call and returns immediately."""
        try:
            activity = self._activity(f"tool:{call.tool}", call.duration, call.error, call.charges)
            activity.tool = call.tool
            activity.framework = call.framework if isinstance(call.framework, str) else ""
            activity.framework_version = call.framework_version if isinstance(call.framework_version, str) else ""
        except Exception:
            self.recorder._note("panicked")
            return
        self.recorder.record(activity)

    def _activity(self, component: str, duration: float, error: BaseException | None, charges: Sequence[_wire.Charge], framework: "_Framework | None" = None) -> _wire.Activity:
        user = current_user()
        self.recorder.note_user(user)

        activity = _wire.Activity(
            agent=self.attribution.agent,
            request=current_request() or (framework.request if framework else ""),
            session=current_session() or (framework.session if framework else ""),
            user=_user_id(framework),
            project=current_project(),
            caller_service=self.attribution.service,
            caller_component=component,
            sub_agent=framework.agent if framework else "",
            # Seen through an agent framework's callbacks, or made directly from the service's own code: what an agent nobody registered is listed as. Only an agent framework names the agent it is running; LiteLLM hands over who a call was for but calls the model directly.
            observed_as=_wire.KIND_AGENT if framework and framework.agent else _wire.KIND_SERVICE,
            framework=framework.name if framework else "",
            framework_version=framework.version if framework else "",
            skill=current_skill() or self.attribution.skill,
            billed_by=self.attribution.billed_by or PROVIDER_VERTEX_AI,
            duration_ms=max(0, int(duration * 1000)),
            status=_wire.STATUS_OK,
            occurred_at_ns=time.time_ns(),
            charges=[_wire.Charge(charge.priceable_unit, int(charge.quantity)) for charge in charges],
        )
        if error is not None:
            activity.status = _wire.STATUS_FAILED
            activity.error_code = error_code(error)
            # What the provider said, beside the one word made of it here, so the server can read the failure again later and read it differently if this library got it wrong.
            reported = reported_error_for(error, activity.billed_by)
            if reported.format:
                activity.error_format = reported.format
                activity.reported_error = reported.fields
        return activity
