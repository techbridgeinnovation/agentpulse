"""Records what an agent spent, from inside the agent's own process.

Two lines wire it in::

    import agentpulse
    reporter = agentpulse.connect(service="sources-service")

and each model call is then reported where it finishes::

    reporter.model_call(agentpulse.ModelCall(
        model="gemini-2.5-pro",
        component="asset_summary",
        reported=agentpulse.reported_from_genai(response.usage_metadata),
        format=agentpulse.FORMAT_VERTEX,
        duration=elapsed,
    ))

To let a budget stop a call, govern the reporter and ask before each call::

    reporter = agentpulse.connect(service="sources-service").governed()
    verdict = reporter.decide(model="gemini-2.5-pro")
    if not verdict.proceed:
        return declined_reply()

Recording never blocks, never raises and never waits on the network. See `recorder` for why.
"""

from __future__ import annotations

import os

from ._version import __version__
from ._wire import Activity, Charge, ReportedField, ReportedQuantity
from .clients import SpendDenied, denied
from .context import User, carry, current_request, current_user, current_workspace, scope
from .failure import (
    ERROR_FORMAT_ANTHROPIC,
    ERROR_FORMAT_GRPC,
    ERROR_FORMAT_OPENAI,
    ERROR_FORMAT_PERPLEXITY,
    ERROR_FORMAT_VERTEX,
    ReportedFailure,
    blocked_finish,
    error_code,
    reported_error_for,
    reported_error_of,
)
from .gateway import ConfigError, Gateway, organisation_of_key
from .governance import ALLOW, DENY, DOWNGRADE, NOTIFY, UNDECIDED, CachingDecider, Decider, GatewayDecider, Verdict
from .pricing import GatewayRates, PriceableUnit, RateSource
from .recorder import Config, Recorder, Stats
from .report import Attribution, ModelCall, Reporter, Tokens, ToolCall
from .sinks import Discard, MeteringSink, Sink, UserSink
from .usage import (
    FORMAT_ANTHROPIC,
    FORMAT_LITELLM,
    FORMAT_OPENAI_CHAT,
    FORMAT_OPENAI_RESPONSES,
    FORMAT_PERPLEXITY,
    FORMAT_VERTEX,
    PROVIDER_ANTHROPIC,
    PROVIDER_OPENAI,
    PROVIDER_PERPLEXITY,
    PROVIDER_VERTEX_AI,
    reported_from,
    reported_from_anthropic,
    reported_from_genai,
    reported_from_openai,
)


def connect(
    service: str,
    *,
    billed_by: str = PROVIDER_VERTEX_AI,
    skill: str = "",
    config: Config | None = None,
) -> Reporter:
    """A reporter that records to Agent Pulse through its gateway, set up from the environment.

    Four settings are read, and each is refused here if missing, so a wrong setting stops the process where a person is watching it start: `AP_GATEWAY`, the gateway's address; `AP_API_KEY`, the key's name, `organisations/<id>/apiKeys/<id>`, which carries the organisation the spend is filed under; `AP_API_SECRET`, shown once when the key was issued; and `AP_AGENT`, `organisations/<id>/agents/<name>`, a name of the team's choosing.

    Sinks in `config` receive every record too, beside metering.
    """
    gateway = Gateway.from_env()
    if not gateway.organisation:
        raise ConfigError("agentpulse: AP_API_KEY must be a key's name, organisations/<id>/apiKeys/<id>")
    agent = os.environ.get("AP_AGENT", "").strip()
    if not agent.startswith(gateway.organisation + "/agents/") or agent.count("/") != 3:
        raise ConfigError(f"agentpulse: AP_AGENT must be {gateway.organisation}/agents/<name>, under the key's organisation")
    if not service.strip():
        raise ConfigError("agentpulse: the service name is required")

    config = config or Config()
    config.sinks = [MeteringSink(gateway), *config.sinks]
    if config.rates is None:
        config.rates = GatewayRates(gateway)
    return Reporter(Recorder(config), Attribution(agent=agent, service=service.strip(), billed_by=billed_by, skill=skill), gateway=gateway)


__all__ = [
    "ALLOW",
    "Activity",
    "Attribution",
    "CachingDecider",
    "Charge",
    "Config",
    "ConfigError",
    "DENY",
    "DOWNGRADE",
    "Decider",
    "Discard",
    "ERROR_FORMAT_ANTHROPIC",
    "ERROR_FORMAT_GRPC",
    "ERROR_FORMAT_OPENAI",
    "ERROR_FORMAT_PERPLEXITY",
    "ERROR_FORMAT_VERTEX",
    "FORMAT_ANTHROPIC",
    "FORMAT_LITELLM",
    "FORMAT_OPENAI_CHAT",
    "FORMAT_OPENAI_RESPONSES",
    "FORMAT_PERPLEXITY",
    "FORMAT_VERTEX",
    "Gateway",
    "GatewayDecider",
    "GatewayRates",
    "MeteringSink",
    "ModelCall",
    "PriceableUnit",
    "NOTIFY",
    "PROVIDER_ANTHROPIC",
    "PROVIDER_OPENAI",
    "PROVIDER_PERPLEXITY",
    "PROVIDER_VERTEX_AI",
    "RateSource",
    "Recorder",
    "ReportedFailure",
    "ReportedField",
    "ReportedQuantity",
    "Reporter",
    "Sink",
    "SpendDenied",
    "Stats",
    "Tokens",
    "ToolCall",
    "UNDECIDED",
    "User",
    "UserSink",
    "Verdict",
    "__version__",
    "blocked_finish",
    "carry",
    "connect",
    "current_request",
    "denied",
    "current_user",
    "current_workspace",
    "error_code",
    "organisation_of_key",
    "reported_error_for",
    "reported_error_of",
    "reported_from",
    "reported_from_anthropic",
    "reported_from_genai",
    "reported_from_openai",
    "scope",
]
