"""The way in for code that is not an agent on a framework.

There is nothing to observe such code from, so it reports for itself. What it must not have to do is know the shape of a record: the fields that are easy to fill in wrongly are the ones that quietly cost money, and every service filling them by hand is every service getting them wrong differently. So what does not change is given once, in an `Attribution`, what does is named at the call, and the mapping onto a record happens here.

Neither a model call nor a tool call states a cost. A service says what happened and the server prices it, which is what stops an agent asserting what its own work was worth.
"""

from __future__ import annotations

import time
from dataclasses import dataclass, field
from typing import Any, Mapping, Sequence

from . import _wire, governance
from .context import current_component, current_project, current_request, current_session, current_user, current_workspace, workspace_name
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
    # The area of the product the work belongs to, where the product wants cost sliced that way.
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


@dataclass
class ToolCall:
    """One finished call to a tool, worth recording on its own because a search or a lookup is charged per use, which counting tokens can never see."""

    tool: str
    duration: float = 0.0
    error: BaseException | None = None
    charges: Sequence[_wire.Charge] = field(default_factory=tuple)


@dataclass(frozen=True)
class _Framework:
    """What an agent framework knows about a call, used where the product set nothing on the context: the framework's own id for the turn, its session, its user, and the agent inside the run that made the call."""

    request: str = ""
    session: str = ""
    user: str = ""
    agent: str = ""


def _user_id(framework: _Framework | None) -> str:
    # A person set on the context wins, because it is what the product's own sign-in put there, name and all.
    user = current_user()
    if user is not None and user.id:
        return user.id
    return framework.user if framework else ""


class Reporter:
    """Records on behalf of one service. Cheap to hold and safe to share across threads."""

    def __init__(self, recorder: Recorder, attribution: Attribution, *, gateway: Gateway | None = None):
        self.recorder = recorder
        self.attribution = attribution
        self._gateway = gateway
        self._decider: governance.Decider | None = None
        self._decide_timeout = governance.DEFAULT_DECIDE_TIMEOUT
        self._subscriber: Any = None

    def governed(
        self,
        decider: governance.Decider | None = None,
        *,
        cache_ttl: float = 30.0,
        timeout: float = governance.DEFAULT_DECIDE_TIMEOUT,
        failure_backoff: float = governance.DEFAULT_FAILURE_BACKOFF,
        follow_changes: bool = True,
    ) -> "Reporter":
        """A reporter whose `decide` asks governance, so a budget that stops an agent stops this service spending against the same budget too.

        The decider defaults to governance through the gateway this reporter records to. A verdict is reused for `cache_ttl` seconds, which is how far past a ceiling spend can run before it is noticed, times the number of processes running; zero asks every time. `timeout` bounds how long a call waits for an answer before it goes ahead without one, and after governance fails to answer a question it is not asked again for `failure_backoff` seconds, so an outage slows no call past the first.

        With `follow_changes`, governance through the gateway also tells this process when a budget changes, and the verdicts it covers are dropped at once rather than when they expire.
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
        return governed

    def decide(self, model: str = "", *, provider: str = "", component: str = "") -> governance.Verdict:
        """Asks whether a call to `model` may proceed, before it is made, and never raises.

        Call it where the service is about to call a model, and skip the call when `proceed` is false: the refusal is recorded here, with nothing spent, so refusals are countable. A DOWNGRADE names a replacement model for the caller to use instead. Who bills for the call is the reporter's unless `provider` says otherwise.

        Only a genuine DENY refuses. Governance unreachable, slow or saying something this version does not know lets the call go ahead, and is counted. A reporter that is not governed answers UNDECIDED to everything and asks nobody.
        """
        return self._decide(model, provider=provider, component=component)

    def _decide(self, model: str, *, provider: str = "", component: str = "", framework: "_Framework | None" = None) -> governance.Verdict:
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
                user=_user_id(framework),
                workspace=workspace_name(organisation, current_workspace()),
                project=current_project(),
                requested_provider=billed_by,
                requested_model=model,
            )
            verdict = governance.ask(self._decider, request, self._decide_timeout)
        except Exception:
            self.recorder._note("decision_errors")
            return governance.Verdict()

        if verdict.decision == governance.NOTIFY:
            self.recorder._note("notified")
        elif verdict.decision == governance.DOWNGRADE:
            self.recorder._note("downgraded")
        elif verdict.decision == governance.DENY:
            self.recorder._note("denied")
            self._record_denied(component or current_component(), model, billed_by, framework)
        return verdict

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
            activity.provider_cost_micros = int(call.cost_micros)
            activity.cache_write_ttl_seconds = int(call.cache_write_ttl)

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
            skill=self.attribution.skill,
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
