"""The connection an agent uses to reach Agent Pulse: the gateway's address, with an api key and its secret on every call."""

from __future__ import annotations

import os
from typing import Any, Iterator

from . import _grpcweb
from ._version import __version__

# Where the gateway reads the key's name and its secret. Two headers rather than HTTP basic authentication, because the gateway keeps the Authorization header for platform identity and a customer secret must never be mistaken for a verified token.
API_KEY_HEADER = "x-api-key"
API_SECRET_HEADER = "x-api-secret"


class ConfigError(ValueError):
    """A setting the recorder cannot run without is missing or malformed.

    Raised where the recorder is set up and nowhere later, so a wrong setting stops the process where a person is watching it start instead of every record being refused quietly for the life of the process.
    """


def organisation_of_key(key: str) -> str:
    """The organisation a key was issued for, e.g. `organisations/acme` from `organisations/acme/apiKeys/k1`, or an empty string when the value is not a key's name.

    Nothing about scope rests on this: the gateway holds every request to the organisation the key and secret resolve to, so a name edited to claim another tenant is refused, not believed.
    """
    organisation, found, _ = key.strip().partition("/apiKeys/")
    if not found or not organisation.startswith("organisations/") or organisation.count("/") != 1:
        return ""
    return organisation


class Gateway:
    """TLS to the gateway, presenting a key and its secret as a pair on every call.

    Lazy: nothing touches the network until the first record is sent, so a gateway that is unreachable at startup delays nothing the agent does. One gateway serves the metering sink and the spend check alike.
    """

    def __init__(self, address: str, key: str, secret: str):
        if not address or not address.strip():
            raise ConfigError("agentpulse: the gateway address is required")
        if not key or not key.strip():
            raise ConfigError("agentpulse: the api key is required")
        if not secret or not secret.strip():
            raise ConfigError("agentpulse: the api secret is required")
        try:
            host, port, secure = _grpcweb.parse_address(address)
        except ValueError as err:
            raise ConfigError(f"agentpulse: {err}") from None

        self.key = key.strip()
        self.organisation = organisation_of_key(self.key)
        self._channel = _grpcweb.Channel(
            host,
            port,
            secure,
            {
                API_KEY_HEADER: self.key,
                API_SECRET_HEADER: secret.strip(),
                "x-user-agent": f"agentpulse-python/{__version__}",
            },
        )

    @classmethod
    def from_env(cls) -> "Gateway":
        """The gateway named by `AP_GATEWAY`, `AP_API_KEY` and `AP_API_SECRET`, all of which are required."""
        return cls(os.environ.get("AP_GATEWAY", ""), os.environ.get("AP_API_KEY", ""), os.environ.get("AP_API_SECRET", ""))

    def call(self, method: str, message: bytes, timeout: float) -> bytes:
        """One unary call. Raises `RPCError` on any failure."""
        return self._channel.unary(method, message, timeout)

    def stream(self, method: str, message: bytes, idle: float, stopped: Any) -> Iterator[bytes]:
        """One server-streaming call, yielding each message as it arrives. See `Channel.server_stream`."""
        return self._channel.server_stream(method, message, idle, stopped)
