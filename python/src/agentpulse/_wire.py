"""The protobuf encoding of the few messages the recorder sends, and the reading of the few it receives.

Written by hand rather than generated, because the generated code needs the protobuf runtime, and protobuf is the package most likely to already be installed in the host at a version our generated code refuses to load under. A library running inside somebody else's agent does not get to choose that version for them. What is here is small, fixed by the contract's field numbers, and checked byte for byte against the official encoder by `tests/test_wire.py`.

Only proto3 scalar rules are needed: a field holding its zero value is left out, a repeated field is one entry per element, and fields are written in field-number order, which is the order every official encoder uses.
"""

from __future__ import annotations

import dataclasses
from dataclasses import dataclass, field
from typing import Iterator

_VARINT = 0
_FIXED64 = 1
_LENGTH = 2
_FIXED32 = 5

# Activity.Status, as the contract numbers it.
STATUS_OK = 1
STATUS_FAILED = 2
STATUS_DENIED = 3
STATUS_TRUNCATED = 4

# DecideResponse.Decision, as the contract numbers it.
DECISION_UNSPECIFIED = 0
DECISION_ALLOW = 1
DECISION_NOTIFY = 2
DECISION_DOWNGRADE = 3
DECISION_DENY = 4


def _varint(value: int) -> bytes:
    # A negative int32 or int64 is written as its 64-bit two's complement, ten bytes long, which is what the contract's signed types require.
    if value < 0:
        value += 1 << 64
    out = bytearray()
    while True:
        byte = value & 0x7F
        value >>= 7
        if value:
            out.append(byte | 0x80)
        else:
            out.append(byte)
            return bytes(out)


def _key(number: int, wire_type: int) -> bytes:
    return _varint(number << 3 | wire_type)


def _string(number: int, value: str) -> bytes:
    if not value:
        return b""
    data = value.encode("utf-8")
    return _key(number, _LENGTH) + _varint(len(data)) + data


def _integer(number: int, value: int) -> bytes:
    if not value:
        return b""
    return _key(number, _VARINT) + _varint(int(value))


def _boolean(number: int, value: bool) -> bytes:
    if not value:
        return b""
    return _key(number, _VARINT) + b"\x01"


def _message(number: int, data: bytes) -> bytes:
    return _key(number, _LENGTH) + _varint(len(data)) + data


@dataclass
class Charge:
    """A cost on a call that tokens do not describe, such as one web search."""

    # The rate card entry this is charged against. Format: priceableUnits/{id}
    priceable_unit: str
    quantity: int

    def encode(self) -> bytes:
        return _string(1, self.priceable_unit) + _integer(2, self.quantity)


@dataclass
class ReportedQuantity:
    """One count a provider reported, under the provider's own name for it."""

    unit: str
    quantity: int

    def encode(self) -> bytes:
        return _string(1, self.unit) + _integer(2, self.quantity)


@dataclass
class ReportedField:
    """One thing a provider said about a failure, under the provider's own name for it."""

    name: str
    value: str

    def encode(self) -> bytes:
        return _string(1, self.name) + _string(2, self.value)


@dataclass
class Activity:
    """One model call or tool call, as metering stores it.

    The field names are the contract's. The fields metering writes itself, such as the estimated and billed costs, are absent, because a recorder never states what its own work cost.
    """

    agent: str = ""
    request: str = ""
    session: str = ""
    user: str = ""
    caller_service: str = ""
    caller_component: str = ""
    skill: str = ""
    model: str = ""
    prompt_tokens: int = 0
    candidate_tokens: int = 0
    cached_tokens: int = 0
    cache_write_tokens: int = 0
    reasoning_tokens: int = 0
    total_tokens: int = 0
    charges: list[Charge] = field(default_factory=list)
    duration_ms: int = 0
    status: int = 0
    error_code: str = ""
    # When the work happened, in nanoseconds since the Unix epoch. Zero leaves it for the server to fill in.
    occurred_at_ns: int = 0
    project: str = ""
    tool: str = ""
    error_detail: str = ""
    attempt: int = 0
    retry_of: str = ""
    result_bytes: int = 0
    result_tokens: int = 0
    empty_result: bool = False
    args_bytes: int = 0
    args_fingerprint: str = ""
    billed_by: str = ""
    usage_format: str = ""
    reported_usage: list[ReportedQuantity] = field(default_factory=list)
    service_tier: str = ""
    cache_write_ttl_seconds: int = 0
    provider_cost_micros: int = 0
    error_format: str = ""
    reported_error: list[ReportedField] = field(default_factory=list)

    def copy(self) -> "Activity":
        return dataclasses.replace(
            self,
            charges=list(self.charges),
            reported_usage=list(self.reported_usage),
            reported_error=list(self.reported_error),
        )

    def encode(self) -> bytes:
        parts = [
            _string(2, self.agent),
            _string(3, self.request),
            _string(4, self.session),
            _string(5, self.user),
            _string(6, self.caller_service),
            _string(7, self.caller_component),
            _string(8, self.skill),
            _string(9, self.model),
            _integer(11, self.prompt_tokens),
            _integer(12, self.candidate_tokens),
            _integer(13, self.cached_tokens),
            _integer(14, self.cache_write_tokens),
            _integer(15, self.reasoning_tokens),
            _integer(16, self.total_tokens),
        ]
        parts += [_message(20, charge.encode()) for charge in self.charges]
        parts += [
            _integer(21, self.duration_ms),
            _integer(22, self.status),
            _string(23, self.error_code),
            _timestamp(24, self.occurred_at_ns),
            _string(25, self.project),
            _string(26, self.tool),
            _string(28, self.error_detail),
            _integer(29, self.attempt),
            _string(30, self.retry_of),
            _integer(31, self.result_bytes),
            _integer(32, self.result_tokens),
            _boolean(33, self.empty_result),
            _integer(34, self.args_bytes),
            _string(35, self.args_fingerprint),
            _string(36, self.billed_by),
            _string(37, self.usage_format),
        ]
        parts += [_message(38, quantity.encode()) for quantity in self.reported_usage]
        parts += [
            _string(39, self.service_tier),
            _integer(40, self.cache_write_ttl_seconds),
            _integer(41, self.provider_cost_micros),
            _string(42, self.error_format),
        ]
        parts += [_message(43, reported.encode()) for reported in self.reported_error]
        return b"".join(parts)


def _timestamp(number: int, nanoseconds: int) -> bytes:
    if not nanoseconds:
        return b""
    seconds, nanos = divmod(nanoseconds, 1_000_000_000)
    return _message(number, _integer(1, seconds) + _integer(2, nanos))


def batch_create_activities(parent: str, activities: list[Activity], request_id: str = "") -> bytes:
    """A BatchCreateActivitiesRequest, encoded."""
    parts = [_string(1, parent)]
    parts += [_message(2, activity.encode()) for activity in activities]
    parts.append(_string(3, request_id))
    return b"".join(parts)


@dataclass
class User:
    """A directory row: the person behind an identifier."""

    # Format: organisations/{organisation}/workspaces/{workspace}/users/{user}, or organisations/{organisation}/users/{user}
    name: str
    display_name: str = ""
    email: str = ""

    def encode(self) -> bytes:
        return _string(1, self.name) + _string(2, self.display_name) + _string(3, self.email)


def batch_upsert_users(parent: str, users: list[User]) -> bytes:
    """A BatchUpsertUsersRequest, encoded."""
    return _string(1, parent) + b"".join(_message(2, user.encode()) for user in users)


@dataclass
class DecideRequest:
    parent: str
    agent: str
    user: str = ""
    workspace: str = ""
    project: str = ""
    requested_provider: str = ""
    requested_model: str = ""

    def encode(self) -> bytes:
        return b"".join(
            [
                _string(1, self.parent),
                _string(2, self.agent),
                _string(4, self.user),
                _string(5, self.workspace),
                _string(6, self.project),
                _string(7, self.requested_provider),
                _string(8, self.requested_model),
            ]
        )


@dataclass
class DecideResponse:
    decision: int = DECISION_UNSPECIFIED
    reason: str = ""
    policy_version: str = ""
    replacement_provider: str = ""
    replacement_model: str = ""

    @classmethod
    def decode(cls, data: bytes) -> "DecideResponse":
        response = cls()
        for number, value in fields(data):
            if number == 1 and isinstance(value, int):
                response.decision = value
            elif number == 2 and isinstance(value, bytes):
                response.reason = value.decode("utf-8", "replace")
            elif number == 3 and isinstance(value, bytes):
                response.policy_version = value.decode("utf-8", "replace")
            elif number == 4 and isinstance(value, bytes):
                response.replacement_provider = value.decode("utf-8", "replace")
            elif number == 5 and isinstance(value, bytes):
                response.replacement_model = value.decode("utf-8", "replace")
        return response


class DecodeError(ValueError):
    """A message that is not valid protobuf."""


def _read_varint(data: bytes, at: int) -> tuple[int, int]:
    value = 0
    shift = 0
    while True:
        if at >= len(data) or shift > 63:
            raise DecodeError("truncated varint")
        byte = data[at]
        at += 1
        value |= (byte & 0x7F) << shift
        if not byte & 0x80:
            return value, at
        shift += 7


def fields(data: bytes) -> Iterator[tuple[int, int | bytes]]:
    """Every field in an encoded message, as its number and its raw value: an int for a varint or a fixed-width field, bytes for a length-delimited one.

    An unknown field is yielded like any other and the caller skips it, which is what keeps a response from a newer server readable by an older recorder.
    """
    at = 0
    while at < len(data):
        key, at = _read_varint(data, at)
        number, wire_type = key >> 3, key & 0x7
        if wire_type == _VARINT:
            value, at = _read_varint(data, at)
            yield number, value
        elif wire_type == _LENGTH:
            length, at = _read_varint(data, at)
            if at + length > len(data):
                raise DecodeError("truncated field")
            yield number, data[at : at + length]
            at += length
        elif wire_type == _FIXED64:
            if at + 8 > len(data):
                raise DecodeError("truncated field")
            yield number, int.from_bytes(data[at : at + 8], "little")
            at += 8
        elif wire_type == _FIXED32:
            if at + 4 > len(data):
                raise DecodeError("truncated field")
            yield number, int.from_bytes(data[at : at + 4], "little")
            at += 4
        else:
            raise DecodeError(f"unsupported wire type {wire_type}")
