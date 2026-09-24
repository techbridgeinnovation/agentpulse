"""What a failure was, reduced to what is safe to keep.

A code says what went wrong in a way a report can group by. A message says the same thing in prose and often quotes the prompt that caused it, which is the one thing this library must never carry. So an exception is read for its code and for the fields a provider uses to name a failure, such as a status, a reason or a field path, and never for its text.

The codes and field names match what the Go recorder writes for the same failure, because the server reads both with the same reader.
"""

from __future__ import annotations

import asyncio
import concurrent.futures
from dataclasses import dataclass, field
from typing import Any, Iterator, Mapping

from . import _grpcweb
from ._wire import ReportedField
from .usage import PROVIDER_PERPLEXITY

# The conventions a failure can be reported in. Which one decides where a failure's identity lives: Gemini states a status with a reason beneath it, OpenAI a type and a code that mean different things, Anthropic one type.
ERROR_FORMAT_VERTEX = "VERTEX"
ERROR_FORMAT_ANTHROPIC = "ANTHROPIC"
ERROR_FORMAT_OPENAI = "OPENAI"
ERROR_FORMAT_PERPLEXITY = "PERPLEXITY"
# A failure that never reached a provider, or reached one through something that reduced it to a transport status: a deadline, a cancelled turn, an unreachable host.
ERROR_FORMAT_GRPC = "GRPC"

# A failure is named in a word or two, so a longer value is prose that has arrived where prose must never be, and it is cut.
_MAX_VALUE = 200
# Every provider names a failure in under a dozen fields.
_MAX_FIELDS = 16

# The names providers give their error text. It quotes the request back, so it is dropped whoever sent it.
_PROSE = {"message", "localized_message", "localizedmessage", "description", "detail", "details", "error", "error_description"}

_DEADLINE = _grpcweb.CODE_NAMES[_grpcweb.DEADLINE_EXCEEDED]
_CANCELLED = _grpcweb.CODE_NAMES[_grpcweb.CANCELLED]
_UNKNOWN = _grpcweb.CODE_NAMES[_grpcweb.UNKNOWN]

# The reasons a provider gives for a call that ended without producing what it was asked for. Every one arrives on a successful response, usually with no content and a bill for the prompt, so nothing that reads only the error will ever see them. The same set the Go recorder holds.
_BLOCKED = {
    "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT",
    "IMAGE_RECITATION", "LANGUAGE", "MALFORMED_FUNCTION_CALL", "UNEXPECTED_TOOL_CALL", "TOO_MANY_TOOL_CALLS",
    "NO_IMAGE", "CONTENT_FILTER", "CONTENT_FILTERED", "GUARDRAIL_INTERVENED", "REFUSAL", "ERROR_TOXIC",
    "MALFORMED_MODEL_OUTPUT", "MALFORMED_TOOL_USE", "OTHER", "IMAGE_OTHER", "MODEL_ARMOR", "JAILBREAK",
    "CONTENT_BLOCKED", "MALFORMED_TOOL_CALL", "MISSING_THOUGHT_SIGNATURE",
}  # fmt: skip

# The reasons that mean a limit was reached. A call cut short did the work it was paid for, which is what TRUNCATED says.
_TRUNCATED = {"MAX_TOKENS", "LENGTH", "MAX_OUTPUT_TOKENS"}


@dataclass
class ReportedFailure:
    """What a caller was told about a failure, under the names it was told in, and which convention those names belong to."""

    format: str = ""
    fields: list[ReportedField] = field(default_factory=list)


def blocked_finish(reason: str | None) -> bool:
    """Whether a finish reason means the call produced nothing usable, and should be recorded as a failure rather than a cheap success."""
    return _canonical(reason) in _BLOCKED


def truncated_finish(reason: str | None) -> bool:
    """Whether a finish reason means a limit was hit."""
    return _canonical(reason) in _TRUNCATED


def _canonical(reason: str | None) -> str:
    return (reason or "").strip().upper()


def error_code(error: BaseException | None) -> str:
    """The one part of an exception that is safe to keep.

    The code is whatever the layer that failed calls it: a provider's own status, the HTTP status where that is all there is, or a gRPC code. A deadline is read first whichever layer reported it, so the one failure a report is always asked about groups as one row.
    """
    if error is None:
        return ""
    transport = _transport_code(error)
    if transport in (_DEADLINE, _CANCELLED):
        return transport
    for cause in _chain(error):
        if _is_genai(cause):
            return _genai_code(cause)
        if _is_sdk_status(cause):
            code = code_of(_reported_sdk(cause, ""))
            if code:
                return code
    return transport or _UNKNOWN


def reported_error_for(error: BaseException | None, billed_by: str = "") -> ReportedFailure:
    """What an exception said about itself, for a call billed by `billed_by`, without deciding what it means.

    Who billed matters because Perplexity fills OpenAI's field names with its own meanings. The deciding happens on the server, which is corrected by one deploy and can read records already written again; a failure reduced to one word here could never be reread.
    """
    if error is None:
        return ReportedFailure()
    for cause in _chain(error):
        if _is_genai(cause):
            return _reported_genai(cause)
        if _is_sdk_status(cause):
            reported = _reported_sdk(cause, billed_by)
            if reported.format:
                return reported
    code = _transport_code(error)
    if code:
        return ReportedFailure(ERROR_FORMAT_GRPC, [ReportedField("code", code)])
    return ReportedFailure()


def reported_error_of(format: str, fields: Mapping[str, Any]) -> ReportedFailure:
    """A failure a caller has already read for itself, under the provider's own names: `type`, `code`, `param`, `status`, `reason`, `http_status`, `retry_after`, `request_id`. A message is dropped whatever it is passed under."""
    if not format.strip() or not fields:
        return ReportedFailure()
    collected = _Fields()
    for name in sorted(fields):
        collected.add(name, _text(fields[name]))
    return collected.failure(format.strip())


class _Fields:
    def __init__(self) -> None:
        self.items: list[ReportedField] = []

    def add(self, name: str, value: str) -> None:
        name, value = (name or "").strip(), (value or "").strip()
        if not name or not value or name.lower() in _PROSE or len(self.items) == _MAX_FIELDS:
            return
        self.items.append(ReportedField(name, _bound(value)))

    def failure(self, format: str) -> ReportedFailure:
        if not self.items:
            return ReportedFailure()
        return ReportedFailure(format, sorted(self.items, key=lambda f: f.name))


def _bound(value: str) -> str:
    # Cut at the same byte length the Go recorder cuts at, on a character boundary, so both store the same value.
    data = value.encode("utf-8")
    if len(data) <= _MAX_VALUE:
        return value
    return data[:_MAX_VALUE].decode("utf-8", "ignore")


def _chain(error: BaseException) -> Iterator[BaseException]:
    """The exception and what it was raised from, the way an error is unwrapped in Go."""
    seen = 0
    current: BaseException | None = error
    while current is not None and seen < 8:
        yield current
        seen += 1
        current = current.__cause__ or (None if current.__suppress_context__ else current.__context__)


def _transport_code(error: BaseException) -> str:
    for cause in _chain(error):
        if isinstance(cause, (asyncio.CancelledError, concurrent.futures.CancelledError)):
            return _CANCELLED
        if isinstance(cause, (TimeoutError, asyncio.TimeoutError, concurrent.futures.TimeoutError)):
            return _DEADLINE
        name = type(cause).__name__
        module = type(cause).__module__ or ""
        if name in ("APITimeoutError", "TimeoutException", "ReadTimeout", "ConnectTimeout", "WriteTimeout", "PoolTimeout") and module.split(".")[0] in ("openai", "anthropic", "httpx", "httpx2", "httpcore"):
            return _DEADLINE
        if isinstance(cause, _grpcweb.RPCError):
            return cause.code_name
        code = _grpc_code(cause)
        if code:
            return code
    return ""


def _grpc_code(error: BaseException) -> str:
    """The code of a grpcio error, named the way grpc-go names it, without importing grpcio."""
    read_code = getattr(error, "code", None)
    if not callable(read_code) or (type(error).__module__ or "").split(".")[0] != "grpc":
        return ""
    try:
        value = read_code().value
        number = value[0] if isinstance(value, tuple) else int(value)
    except Exception:
        return ""
    name = _grpcweb.CODE_NAMES.get(number, "")
    return "" if name in ("", "OK", _UNKNOWN) else name


def _is_genai(error: BaseException) -> bool:
    return type(error).__module__.startswith("google.genai") and hasattr(error, "code") and hasattr(error, "details")


def _genai_code(error: BaseException) -> str:
    # The status the provider named, because that is what its own documentation calls the failure. Only when it is one: the sdk fills the same field with an HTTP reason phrase when the body was not JSON, and that would put a different string in the report for the same failure.
    status = _text(getattr(error, "status", ""))
    if _canonical_status(status):
        return status
    code = getattr(error, "code", 0)
    if isinstance(code, int) and code:
        return f"HTTP_{code}"
    return status


def _canonical_status(status: str) -> bool:
    return bool(status) and all(ch == "_" or ("A" <= ch <= "Z") or ("a" <= ch <= "z") for ch in status)


def _reported_genai(error: BaseException) -> ReportedFailure:
    code = getattr(error, "code", 0)
    return reported_genai_body(code if isinstance(code, int) else 0, _text(getattr(error, "status", "")), getattr(error, "details", None))


def reported_genai_body(code: int, status: str, body: Any) -> ReportedFailure:
    """A Gemini or Vertex failure under Google's own names, including the typed details beneath the status: the reason, the field that was wrong, the quota that was hit. The descriptions inside those details are left behind, because a field violation's description quotes the value that was rejected.

    `body` is the reply's JSON, `{"error": {...}}` or the error object itself.
    """
    collected = _Fields()
    inner = body.get("error", body) if isinstance(body, Mapping) else None
    if isinstance(inner, Mapping):
        code = code or (inner.get("code") if isinstance(inner.get("code"), int) else 0)
        status = status or _text(inner.get("status"))
    if code:
        collected.add("http_status", str(code))
    collected.add("status", status)
    details = inner.get("details") if isinstance(inner, Mapping) else None
    for detail in details if isinstance(details, list) else ():
        if not isinstance(detail, Mapping):
            continue
        collected.add("reason", _text(detail.get("reason")))
        collected.add("domain", _text(detail.get("domain")))
        collected.add("retryDelay", _text(detail.get("retryDelay")))
        collected.add("requestId", _text(detail.get("requestId")))
        for violation in _mappings(detail.get("fieldViolations")):
            collected.add("fieldViolations.field", _text(violation.get("field")))
            collected.add("fieldViolations.reason", _text(violation.get("reason")))
        for violation in _mappings(detail.get("violations")):
            collected.add("violations.quotaId", _text(violation.get("quotaId")))
            collected.add("violations.quotaMetric", _text(violation.get("quotaMetric")))
            collected.add("violations.quotaValue", _text(violation.get("quotaValue")))
    return collected.failure(ERROR_FORMAT_VERTEX)


def _is_sdk_status(error: BaseException) -> bool:
    return (type(error).__module__ or "").split(".")[0] in ("openai", "anthropic") and isinstance(getattr(error, "status_code", None), int)


def _reported_sdk(error: BaseException, billed_by: str) -> ReportedFailure:
    headers = getattr(getattr(error, "response", None), "headers", None)
    return reported_body(getattr(error, "body", None), error.status_code, headers, billed_by)  # type: ignore[attr-defined]


def reported_body(body: Any, status: int, headers: Any, billed_by: str) -> ReportedFailure:
    """A failure from an OpenAI- or Anthropic-shaped error body, the status it came with and the headers beside it.

    Anthropic's body is an envelope, `{"type": "error", "error": {"type": ...}, "request_id": ...}`, with one type and nothing beneath it. OpenAI's names a type, a code and the parameter that was wrong, and is the shape many other providers copy. The retry wait and the request id arrive as headers.
    """
    body = body if isinstance(body, Mapping) else {}
    inner = body.get("error") if isinstance(body.get("error"), Mapping) else body

    collected = _Fields()
    if status:
        collected.add("http_status", str(status))
    collected.add("retry_after", _header(headers, "retry-after"))

    if _text(body.get("type")) == "error":
        collected.add("type", _text(inner.get("type")))
        collected.add("request_id", _text(body.get("request_id")) or _header(headers, "request-id"))
        return collected.failure(ERROR_FORMAT_ANTHROPIC)

    collected.add("type", _text(inner.get("type")))
    collected.add("code", _text(inner.get("code")))
    collected.add("param", _text(inner.get("param")))
    collected.add("request_id", _header(headers, "x-request-id"))
    if billed_by.upper() == PROVIDER_PERPLEXITY:
        return collected.failure(ERROR_FORMAT_PERPLEXITY)
    return collected.failure(ERROR_FORMAT_OPENAI)


def code_of(reported: ReportedFailure) -> str:
    """The name a stated failure gives itself: the specific code where the provider gave one, and its type where it did not."""
    values = {f.name: f.value for f in reported.fields}
    if reported.format in (ERROR_FORMAT_ANTHROPIC, ERROR_FORMAT_PERPLEXITY):
        return values.get("type", "")
    if reported.format == ERROR_FORMAT_OPENAI:
        return values.get("code") or values.get("type", "")
    if reported.format == ERROR_FORMAT_VERTEX:
        if values.get("status"):
            return values["status"]
        return f"HTTP_{values['http_status']}" if values.get("http_status") else ""
    return ""


def _header(headers: Any, name: str) -> str:
    getter = getattr(headers, "get", None)
    if not callable(getter):
        return ""
    try:
        return _text(getter(name))
    except Exception:
        return ""


def _text(value: Any) -> str:
    # A value a provider stated as a string or a number; one stated as a structure is not a name.
    if value is None or isinstance(value, (Mapping, list, tuple)):
        return ""
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, float) and value.is_integer():
        return str(int(value))
    return str(value)


def _mappings(value: Any) -> list[Mapping[str, Any]]:
    return [item for item in value if isinstance(item, Mapping)] if isinstance(value, list) else []
