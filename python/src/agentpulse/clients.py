"""Recording a model call from where the product's model client is created.

A service that calls a model directly has no framework to observe it from, so the other way is a line after every call, and a call site that forgets it spends money nothing sees. This records from the client instead: set once where the client is built, every call through it is recorded, including the ones written later.

It sits in the client's HTTP transport, which OpenAI's, Anthropic's and Google's Python sdks all accept, and recognises a model call by its URL. What is read is the reply as the caller reads it, and only the fields that say what the call cost and how it ended: the model, the counts, the finish, the failure. Every byte reaches the caller exactly as it arrived, nothing is held beyond what those fields need, and prompts and answers are never recorded. A reply this cannot read is still delivered and still recorded, without its counts.

The providers are read exactly as the Go recorder reads them, down to the field names, so the server reads both the same way.
"""

from __future__ import annotations

import contextvars
import importlib
import json
import sys
import time
import weakref
import zlib
from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Any, Callable

from . import _wire, governance
from .context import current_component
from .failure import (
    ERROR_FORMAT_ANTHROPIC,
    ERROR_FORMAT_OPENAI,
    ReportedFailure,
    _Fields,
    blocked_finish,
    code_of,
    error_code,
    reported_body,
    reported_error_for,
    reported_genai_body,
    truncated_finish,
)
from .usage import (
    FORMAT_ANTHROPIC,
    FORMAT_OPENAI_CHAT,
    FORMAT_OPENAI_RESPONSES,
    FORMAT_PERPLEXITY,
    FORMAT_VERTEX,
    PROVIDER_ANTHROPIC,
    PROVIDER_OPENAI,
    PROVIDER_PERPLEXITY,
    PROVIDER_VERTEX_AI,
    reported_from,
    reported_from_genai,
    reported_quantities,
)

if TYPE_CHECKING:
    from .report import Reporter

# The bounds on what is held while a reply is read. A reply longer than the whole is read from its two ends, which is where every provider puts the model and the counts.
_MAX_WHOLE_REPLY = 1 << 20
_REPLY_HEAD = 16 << 10
_REPLY_TAIL = 256 << 10
_MAX_EVENT = 1 << 20
_MAX_ERROR_REPLY = 64 << 10

# The error type a refused call carries in the reply an OpenAI or Anthropic sdk raises for it. Not a type either provider uses, so a refusal by a spend limit is never read as the provider's own.
DENIED_TYPE = "agentpulse_denied"
_DENIED_MESSAGE = "agentpulse: the call was declined because it would exceed a spend limit"


class SpendDenied(Exception):
    """A call a spend limit refused before it was sent, raised by an instrumented Gemini client. Test for any client's refusal with `denied`."""

    def __init__(self) -> None:
        super().__init__(_DENIED_MESSAGE)


def denied(error: BaseException | None) -> bool:
    """Whether an exception is a call a spend limit refused before it was sent, from any instrumented client.

    A Gemini client raises SpendDenied. An OpenAI or Anthropic client raises its own permission error, from a refusal it is told not to retry, and this recognises it by the type the refusal carries.
    """
    while error is not None:
        if isinstance(error, SpendDenied):
            return True
        body = getattr(error, "body", None)
        if isinstance(body, dict):
            inner = body.get("error", body)
            if isinstance(inner, dict) and inner.get("type") == DENIED_TYPE:
                return True
        error = error.__cause__ or error.__context__
    return False


@dataclass
class _Observed:
    """What one call said about itself."""

    model: str = ""
    reported: dict[str, int] = field(default_factory=dict)
    tier: str = ""
    finish: str = ""
    failed: bool = False
    code: str = ""
    failure: ReportedFailure = field(default_factory=ReportedFailure)

    def set_reported(self, usage: Any) -> None:
        # A streamed reply states its counts more than once, each a running total, so the last one stated is the one that stands.
        if isinstance(usage, dict):
            self.reported.update(reported_from(usage))


def _loads(data: bytes | str) -> Any:
    try:
        return json.loads(data)
    except (ValueError, UnicodeDecodeError):
        return None


def _dict(value: Any) -> dict:
    return value if isinstance(value, dict) else {}


def _str(value: Any) -> str:
    return value if isinstance(value, str) else ""


class _Protocol:
    """What differs between one provider's api and another's: which requests are model calls, and where a reply says what the call cost."""

    def call(self, method: str, path: str) -> tuple[bool, str]:
        raise NotImplementedError

    def format(self, path: str, billed_by: str) -> str:
        raise NotImplementedError

    def billed_by(self, host: str, path: str) -> str:
        raise NotImplementedError

    def reply(self, field: Callable[[str], Any], o: _Observed) -> None:
        raise NotImplementedError

    def event(self, data: Any, o: _Observed) -> None:
        raise NotImplementedError

    def failure(self, body: Any, status: int, headers: Any, billed_by: str) -> ReportedFailure:
        raise NotImplementedError

    # A refusal the caller's sdk reads as an error of its own, or None to raise SpendDenied instead.
    denied_body: str | None = None

    # Whether the model is named in the request body rather than the path.
    model_in_body = False

    def model_in_path(self, path: str, model: str) -> str | None:
        """The path rewritten to call `model`, or None where the model is not in the path."""
        return None


class _OpenAI(_Protocol):
    """Chat Completions and Responses, and the same api served by another provider, such as Perplexity. Who billed is read from the host where the reporter does not say, because Perplexity uses OpenAI's field names with its own meanings.

    A streamed chat reports its counts only when the request asks for them with `stream_options.include_usage`. This does not add that to a request, because it changes the stream the caller reads; a streamed chat without it is recorded with no counts.
    """

    model_in_body = True
    denied_body = json.dumps({"error": {"type": DENIED_TYPE, "code": DENIED_TYPE, "message": _DENIED_MESSAGE}})
    _hosts = {"api.openai.com": PROVIDER_OPENAI, "api.perplexity.ai": PROVIDER_PERPLEXITY}

    def call(self, method, path):
        path = path.rstrip("/")
        return method == "POST" and (path.endswith("/chat/completions") or path.endswith("/responses")), ""

    def format(self, path, billed_by):
        if path.rstrip("/").endswith("/responses"):
            return FORMAT_OPENAI_RESPONSES
        return FORMAT_PERPLEXITY if billed_by.upper() == PROVIDER_PERPLEXITY else FORMAT_OPENAI_CHAT

    def billed_by(self, host, path):
        return self._hosts.get(host, PROVIDER_OPENAI)

    def reply(self, field, o):
        o.model = _str(field("model")) or o.model
        o.tier = _str(field("service_tier"))
        o.set_reported(field("usage"))
        choices = field("choices")
        if isinstance(choices, list) and choices:
            o.finish = _str(_dict(choices[0]).get("finish_reason"))
        status = field("status")
        if isinstance(status, str):
            self._response_finish(status, field("incomplete_details"), field("error"), o)

    def _response_finish(self, status, incomplete, error, o):
        if status == "incomplete":
            o.finish = _str(_dict(incomplete).get("reason")) or "incomplete"
        elif status == "failed":
            code = _str(_dict(error).get("code"))
            o.failed, o.code = True, code or "failed"
            collected = _Fields()
            collected.add("code", code)
            o.failure = collected.failure(ERROR_FORMAT_OPENAI)

    def event(self, data, o):
        e = _dict(data)
        kind = _str(e.get("type"))
        if kind in ("response.created", "response.in_progress", "response.completed", "response.incomplete", "response.failed"):
            response = _dict(e.get("response"))
            o.model = _str(response.get("model")) or o.model
            o.tier = _str(response.get("service_tier")) or o.tier
            o.set_reported(response.get("usage"))
            self._response_finish(_str(response.get("status")), response.get("incomplete_details"), response.get("error"), o)
            return
        if kind == "error":
            code = _str(e.get("code"))
            o.failed, o.code = True, code or "error"
            collected = _Fields()
            collected.add("code", code)
            o.failure = collected.failure(ERROR_FORMAT_OPENAI)
            return
        if kind:
            return
        # A chat chunk.
        o.model = _str(e.get("model")) or o.model
        o.tier = _str(e.get("service_tier")) or o.tier
        choices = e.get("choices")
        if isinstance(choices, list) and choices and _str(_dict(choices[0]).get("finish_reason")):
            o.finish = _dict(choices[0])["finish_reason"]
        error = _dict(e.get("error"))
        if _str(error.get("type")) or _str(error.get("code")):
            o.failed, o.code = True, _str(error.get("code")) or _str(error.get("type"))
            collected = _Fields()
            collected.add("type", _str(error.get("type")))
            collected.add("code", _str(error.get("code")))
            o.failure = collected.failure(ERROR_FORMAT_OPENAI)
        o.set_reported(e.get("usage"))

    def failure(self, body, status, headers, billed_by):
        return reported_body(body, status, headers, billed_by)


_ANTHROPIC_ON_VERTEX = "/publishers/anthropic/models/"


class _Anthropic(_Protocol):
    """Messages, native and served through Vertex. Claude through Vertex is billed by Google, not Anthropic."""

    denied_body = json.dumps({"type": "error", "error": {"type": DENIED_TYPE, "message": _DENIED_MESSAGE}})

    def call(self, method, path):
        if method != "POST":
            return False, ""
        if path.endswith("/v1/messages"):
            return True, ""
        if _ANTHROPIC_ON_VERTEX in path and (path.endswith(":rawPredict") or path.endswith(":streamRawPredict")) and "count-tokens" not in path:
            model = path[path.index(_ANTHROPIC_ON_VERTEX) + len(_ANTHROPIC_ON_VERTEX) :]
            return True, model[: model.index(":")]
        return False, ""

    # A native call names its model in the body; one through Vertex names it in the path.
    model_in_body = True

    def format(self, path, billed_by):
        return FORMAT_ANTHROPIC

    def billed_by(self, host, path):
        return PROVIDER_VERTEX_AI if _ANTHROPIC_ON_VERTEX in path else PROVIDER_ANTHROPIC

    def reply(self, field, o):
        o.model = _str(field("model")) or o.model
        o.finish = _str(field("stop_reason"))
        usage = field("usage")
        o.tier = _str(_dict(usage).get("service_tier"))
        o.set_reported(usage)

    def event(self, data, o):
        # The input counts arrive on message_start and the output counts on message_delta, each a running total.
        e = _dict(data)
        kind = _str(e.get("type"))
        if kind == "message_start":
            message = _dict(e.get("message"))
            o.model = _str(message.get("model")) or o.model
            o.set_reported(message.get("usage"))
            o.tier = _str(_dict(message.get("usage")).get("service_tier")) or o.tier
        elif kind == "message_delta":
            o.finish = _str(_dict(e.get("delta")).get("stop_reason")) or o.finish
            o.set_reported(e.get("usage"))
        elif kind == "error":
            # A failure after the reply began, such as an overload part way through, arrives as an event rather than a status.
            error_type = _str(_dict(e.get("error")).get("type"))
            o.failed, o.code = True, error_type
            collected = _Fields()
            collected.add("type", error_type)
            o.failure = collected.failure(ERROR_FORMAT_ANTHROPIC)

    def failure(self, body, status, headers, billed_by):
        return reported_body(body, status, headers, PROVIDER_ANTHROPIC)

    def model_in_path(self, path, model):
        at = path.find(_ANTHROPIC_ON_VERTEX)
        if at < 0:
            return None
        rest = path[at + len(_ANTHROPIC_ON_VERTEX) :]
        colon = rest.find(":")
        if colon < 0:
            return None
        return path[: at + len(_ANTHROPIC_ON_VERTEX)] + model + rest[colon:]


class _GenAI(_Protocol):
    """generateContent and its streamed form, on the Gemini api and Vertex alike."""

    def call(self, method, path):
        if method != "POST" or not (path.endswith(":generateContent") or path.endswith(":streamGenerateContent")):
            return False, ""
        model = path[: path.rindex(":")]
        at = model.rfind("models/")
        return True, model[at + len("models/") :] if at >= 0 else model

    def format(self, path, billed_by):
        return FORMAT_VERTEX

    def billed_by(self, host, path):
        return PROVIDER_VERTEX_AI

    def reply(self, field, o):
        self._generate(field("usageMetadata"), field("modelVersion"), field("candidates"), field("promptFeedback"), o)

    def event(self, data, o):
        e = _dict(data)
        error = e.get("error")
        if isinstance(error, dict):
            o.failed = True
            o.failure = reported_genai_body(0, "", error)
            o.code = code_of(o.failure) or "error"
            return
        self._generate(e.get("usageMetadata"), e.get("modelVersion"), e.get("candidates"), e.get("promptFeedback"), o)

    def _generate(self, usage, version, candidates, feedback, o):
        o.model = _str(version) or o.model
        if isinstance(usage, dict):
            o.reported.update(reported_from_genai(usage))
            o.tier = _str(usage.get("trafficType")) or o.tier
        if isinstance(candidates, list) and candidates and _str(_dict(candidates[0]).get("finishReason")):
            o.finish = candidates[0]["finishReason"]
        # A prompt refused before the model saw it says so on the feedback, and the reply carries no candidate to name a finish of its own.
        block = _str(_dict(feedback).get("blockReason"))
        if block:
            o.finish = block

    def failure(self, body, status, headers, billed_by):
        return reported_genai_body(status, "", body if isinstance(body, dict) else None)

    def model_in_path(self, path, model):
        colon = path.rfind(":")
        if colon < 0:
            return None
        head = path[:colon]
        at = head.rfind("models/")
        if at < 0:
            return None
        return head[: at + len("models/")] + model + path[colon:]


_PROTOCOLS: tuple[_Protocol, ...] = (_OpenAI(), _Anthropic(), _GenAI())


def _recognise(method: str, path: str) -> tuple[_Protocol | None, str]:
    for protocol in _PROTOCOLS:
        try:
            recordable, model = protocol.call(method, path)
        except Exception:
            continue
        if recordable:
            return protocol, model
    return None, ""


class _Call:
    """One model call in flight, recorded once: when the caller reads the reply to its end, closes it, or lets it go unread."""

    def __init__(self, rp: "Reporter", protocol: _Protocol, context: contextvars.Context, component: str, billed_by: str, format: str, attempt: int, model: str):
        self.rp = rp
        self.protocol = protocol
        self.context = context
        self.component = component
        self.billed_by = billed_by
        self.format = format
        self.attempt = attempt
        self.start = time.monotonic()
        self.o = _Observed(model=model)
        self.status = 0
        self.headers: Any = None
        self.stream = False
        self.unreadable = False
        self.decoder: Any = None
        self.done = False
        self.whole = bytearray()
        self.head = b""
        self.tail = bytearray()
        self.overflow = False
        self.line = bytearray()
        self.skipping = False

    def responded(self, status: int, headers: Any) -> None:
        self.status = status
        self.headers = headers
        content_type = _header(headers, "content-type")
        self.stream = content_type.startswith("text/event-stream")
        encoding = _header(headers, "content-encoding").strip().lower()
        # The sdk decodes the body after it leaves the transport, so what passes through here is still compressed. gzip and deflate are decoded again for reading; anything else is delivered untouched and recorded without its counts.
        if encoding in ("gzip", "x-gzip"):
            self.decoder = zlib.decompressobj(16 + zlib.MAX_WBITS)
        elif encoding == "deflate":
            self.decoder = zlib.decompressobj()
        elif encoding not in ("", "identity"):
            self.unreadable = True

    def see(self, chunk: bytes) -> None:
        try:
            if self.done or self.unreadable or not chunk:
                return
            if self.decoder is not None:
                chunk = self.decoder.decompress(chunk, _MAX_WHOLE_REPLY)
            if self.status >= 400:
                room = _MAX_ERROR_REPLY - len(self.whole)
                if room > 0:
                    self.whole += chunk[:room]
            elif self.stream:
                self._see_lines(chunk)
            else:
                self._see_whole(chunk)
        except Exception:
            self.unreadable = True

    def _see_whole(self, chunk: bytes) -> None:
        if not self.overflow and len(self.whole) + len(chunk) <= _MAX_WHOLE_REPLY:
            self.whole += chunk
            return
        if not self.overflow:
            self.overflow = True
            self.head = bytes(self.whole[:_REPLY_HEAD])
            self.tail = self.whole
            self.whole = bytearray()
        self.tail += chunk
        if len(self.tail) > 2 * _REPLY_TAIL:
            self.tail = self.tail[-_REPLY_TAIL:]

    def _see_lines(self, chunk: bytes) -> None:
        while chunk:
            at = chunk.find(b"\n")
            if at < 0:
                if not self.skipping:
                    if len(self.line) + len(chunk) > _MAX_EVENT:
                        self.line, self.skipping = bytearray(), True
                    else:
                        self.line += chunk
                return
            if not self.skipping and len(self.line) + at <= _MAX_EVENT:
                self.line += chunk[:at]
                self._read_line(bytes(self.line))
            self.line, self.skipping = bytearray(), False
            chunk = chunk[at + 1 :]

    def _read_line(self, line: bytes) -> None:
        line = line.rstrip(b"\r")
        if not line.startswith(b"data:"):
            return
        data = line[5:].strip()
        if not data or data == b"[DONE]":
            return
        event = _loads(data)
        if event is not None:
            self.protocol.event(event, self.o)

    def failed_with(self, error: BaseException) -> None:
        self.o.failed = True
        self.o.code = error_code(error)
        self.o.failure = reported_error_for(error, self.billed_by)
        self.finish()

    def finish(self) -> None:
        try:
            if self.done:
                return
            self.done = True
            self._read()
            observed = self.o
            self.whole = self.tail = self.line = bytearray()
            self.head = b""
        except Exception:
            self.rp.recorder._note("panicked")
            return
        try:
            self.context.run(record_observed, self.rp, self, observed)
        except Exception:
            self.rp.recorder._note("panicked")

    def _read(self) -> None:
        if self.unreadable:
            return
        if self.status >= 400:
            self.o.failed = True
            self.o.failure = self.protocol.failure(_loads(bytes(self.whole)), self.status, self.headers, self.billed_by)
            self.o.code = code_of(self.o.failure) or f"HTTP_{self.status}"
        elif self.stream:
            if self.line and not self.skipping:
                self._read_line(bytes(self.line))
        elif self.overflow:
            head, tail = self.head.decode("utf-8", "replace"), self.tail.decode("utf-8", "replace")
            self.protocol.reply(lambda name: _last_value(tail, name) if _last_value(tail, name) is not None else _first_value(head, name), self.o)
        elif self.whole:
            fields = _dict(_loads(bytes(self.whole)))
            if fields:
                self.protocol.reply(fields.get, self.o)


def _header(headers: Any, name: str) -> str:
    getter = getattr(headers, "get", None)
    if not callable(getter):
        return ""
    try:
        return getter(name) or ""
    except Exception:
        return ""


_decoder = json.JSONDecoder()


def _value_at(text: str, at: int) -> Any:
    while at < len(text) and text[at] in " \t\r\n":
        at += 1
    try:
        value, _ = _decoder.raw_decode(text, at)
    except ValueError:
        return None
    return value


def _last_value(text: str, name: str) -> Any:
    """The value of the last `"name":` in a piece of JSON, for a reply read from its end. A key inside a string cannot match, because a quote inside a string is escaped."""
    key = f'"{name}":'
    end = len(text)
    while end > 0:
        at = text.rfind(key, 0, end)
        if at < 0:
            return None
        if at == 0 or text[at - 1] != "\\":
            return _value_at(text, at + len(key))
        end = at
    return None


def _first_value(text: str, name: str) -> Any:
    key = f'"{name}":'
    start = 0
    while start < len(text):
        at = text.find(key, start)
        if at < 0:
            return None
        if at == 0 or text[at - 1] != "\\":
            return _value_at(text, at + len(key))
        start = at + len(key)
    return None


# The code a model call passes through on its way out that is never the product's own. The first frame outside these names the part of the product that made the call.
_INFRASTRUCTURE = (
    "agentpulse", "openai", "anthropic", "google.genai", "google.auth", "httpx", "httpx2", "httpcore", "h11", "h2",
    "anyio", "sniffio", "asyncio", "concurrent", "threading", "contextlib", "functools", "tenacity", "ssl", "socket",
    "selectors", "http", "urllib3", "requests", "runpy", "importlib", "typing_extensions", "pydantic",
)  # fmt: skip


def _infrastructure(module: str) -> bool:
    return any(module == name or module.startswith(name + ".") for name in _INFRASTRUCTURE)


def caller_of() -> str:
    """The function in the product's own code that made a model call, for a call made with no component named, e.g. `summary.generate` or `PromptsService.generate_prompt`."""
    frame = sys._getframe(1)
    depth = 0
    while frame is not None and depth < 64:
        module = frame.f_globals.get("__name__", "") or ""
        if not _infrastructure(module):
            code = frame.f_code
            qualname = getattr(code, "co_qualname", code.co_name).replace("<locals>.", "")
            parts = [p for p in qualname.split(".") if not p.startswith("<")] or [code.co_name]
            if len(parts) >= 2:
                return ".".join(parts[-2:])
            prefix = module.rsplit(".", 1)[-1]
            return parts[0] if prefix in ("", "__main__") else f"{prefix}.{parts[0]}"
        frame = frame.f_back
        depth += 1
    return ""


def _attempt(headers: Any) -> int:
    # The OpenAI and Anthropic sdks number their retries on every attempt they send.
    try:
        n = int(_header(headers, "x-stainless-retry-count"))
    except ValueError:
        return 0
    return n + 1 if n >= 0 else 0


class _Transport:
    """What the sync and async transports share: recognising a call, asking governance, and building what the caller receives."""

    def __init__(self, rp: "Reporter", inner: Any):
        self._rp = rp
        self._inner = inner

    def _begin(self, request: Any) -> tuple[Any, _Call | None, Any]:
        """The request to send, the call being observed, and a reply to hand back instead of sending, for a call a spend limit refused."""
        try:
            protocol, model = _recognise(request.method.upper(), request.url.path)
            if protocol is None:
                return request, None, None
            rp = self._rp
            component = current_component() or caller_of()
            billed_by = rp.attribution.billed_by or protocol.billed_by(request.url.host, request.url.path)
            governed = rp._decider is not None
            if governed and not model and protocol.model_in_body:
                model = _str(_dict(_loads(request.content)).get("model")) if _readable(request) else ""
            if governed:
                verdict = rp.decide(model, provider=billed_by, component=component)
                if not verdict.proceed:
                    return request, None, self._refusal(protocol, request)
                if verdict.replacement_model:
                    rewritten = self._downgraded(protocol, request, verdict, billed_by)
                    if rewritten is not None:
                        request, model = rewritten, verdict.replacement_model
                        rp.recorder._note("downgrade_applied")
                    else:
                        rp.recorder._note("downgrade_not_applied")
            call = _Call(
                rp,
                protocol,
                contextvars.copy_context(),
                component,
                billed_by,
                protocol.format(request.url.path, billed_by),
                _attempt(request.headers),
                model,
            )
            return request, call, None
        except SpendDenied:
            raise
        except Exception:
            self._rp.recorder._note("panicked")
            return request, None, None

    def _refusal(self, protocol: _Protocol, request: Any) -> Any:
        if protocol.denied_body is None:
            raise SpendDenied()
        module = _module_of(request)
        return module.Response(
            403,
            headers={"content-type": "application/json", "x-should-retry": "false"},
            content=protocol.denied_body.encode(),
            request=request,
        )

    def _downgraded(self, protocol: _Protocol, request: Any, verdict: governance.Verdict, billed_by: str) -> Any:
        """The request rewritten to call the replacement governance named, or None where that cannot be done safely.

        A replacement served by a different provider than this client talks to is never applied: changing only the model name would send the call to the wrong provider entirely. A model named in the path is rewritten there; one named in a JSON body is rewritten there, and nothing else in the body is touched.
        """
        if verdict.replacement_provider and verdict.replacement_provider.upper() != billed_by.upper():
            return None
        module = _module_of(request)
        path = protocol.model_in_path(request.url.path, verdict.replacement_model)
        if path is not None:
            return module.Request(request.method, request.url.copy_with(path=path), headers=request.headers, stream=request.stream, extensions=request.extensions)
        if not protocol.model_in_body or not _readable(request):
            return None
        body = _loads(request.content)
        if not isinstance(body, dict) or not isinstance(body.get("model"), str):
            return None
        body["model"] = verdict.replacement_model
        headers = [(k, v) for k, v in request.headers.multi_items() if k.lower() != "content-length"]
        return module.Request(request.method, request.url, headers=headers, content=json.dumps(body, separators=(",", ":")).encode(), extensions=request.extensions)

    def _observe(self, call: _Call, response: Any, asynchronous: bool) -> Any:
        try:
            call.responded(response.status_code, response.headers)
            response.stream = (_async_stream if asynchronous else _sync_stream)(_module_of(response), response.stream, call)
        except Exception:
            self._rp.recorder._note("panicked")
        return response


def _readable(request: Any) -> bool:
    try:
        request.content
    except Exception:
        return False
    return True


def _module_of(value: Any) -> Any:
    return importlib.import_module(type(value).__module__.split(".")[0])


class ObservingTransport(_Transport):
    """A synchronous httpx or httpx2 transport that records every model call passing through it and changes nothing about them."""

    def handle_request(self, request: Any) -> Any:
        request, call, refusal = self._begin(request)
        if refusal is not None:
            return refusal
        if call is None:
            return self._inner.handle_request(request)
        try:
            response = self._inner.handle_request(request)
        except BaseException as err:
            call.failed_with(err)
            raise
        return self._observe(call, response, asynchronous=False)

    def close(self) -> None:
        self._inner.close()

    def __enter__(self) -> "ObservingTransport":
        self._inner.__enter__()
        return self

    def __exit__(self, *args: Any) -> None:
        self._inner.__exit__(*args)


class AsyncObservingTransport(_Transport):
    """The asynchronous counterpart of ObservingTransport."""

    async def handle_async_request(self, request: Any) -> Any:
        request, call, refusal = self._begin(request)
        if refusal is not None:
            return refusal
        if call is None:
            return await self._inner.handle_async_request(request)
        try:
            response = await self._inner.handle_async_request(request)
        except BaseException as err:
            call.failed_with(err)
            raise
        return self._observe(call, response, asynchronous=True)

    async def aclose(self) -> None:
        await self._inner.aclose()

    async def __aenter__(self) -> "AsyncObservingTransport":
        await self._inner.__aenter__()
        return self

    async def __aexit__(self, *args: Any) -> None:
        await self._inner.__aexit__(*args)


_stream_classes: dict[tuple[str, bool], type] = {}


def _sync_stream(module: Any, inner: Any, call: _Call) -> Any:
    cls = _stream_classes.get((module.__name__, False))
    if cls is None:

        class Observed(module.SyncByteStream):  # type: ignore[name-defined]
            def __init__(self, inner: Any, call: _Call):
                self._inner = inner
                self._call = call
                # A reply the caller abandons without reading or closing is recorded when it is collected.
                self._finalizer = weakref.finalize(self, call.finish)

            def __iter__(self):
                try:
                    for chunk in self._inner:
                        self._call.see(chunk)
                        yield chunk
                except BaseException as err:
                    self._call.failed_with(err)
                    raise
                self._call.finish()

            def close(self) -> None:
                try:
                    self._inner.close()
                finally:
                    self._call.finish()

        cls = _stream_classes[(module.__name__, False)] = Observed
    return cls(inner, call)


def _async_stream(module: Any, inner: Any, call: _Call) -> Any:
    cls = _stream_classes.get((module.__name__, True))
    if cls is None:

        class Observed(module.AsyncByteStream):  # type: ignore[name-defined]
            def __init__(self, inner: Any, call: _Call):
                self._inner = inner
                self._call = call
                self._finalizer = weakref.finalize(self, call.finish)

            async def __aiter__(self):
                try:
                    async for chunk in self._inner:
                        self._call.see(chunk)
                        yield chunk
                except BaseException as err:
                    self._call.failed_with(err)
                    raise
                self._call.finish()

            async def aclose(self) -> None:
                try:
                    await self._inner.aclose()
                finally:
                    self._call.finish()

        cls = _stream_classes[(module.__name__, True)] = Observed
    return cls(inner, call)


def record_observed(rp: "Reporter", call: _Call, o: _Observed) -> None:
    """Turns what a call said about itself into a record. Runs in the context the call was made in."""
    activity = rp._activity(call.component, time.monotonic() - call.start, None, ())
    activity.model = o.model
    activity.billed_by = call.billed_by
    activity.attempt = call.attempt
    activity.service_tier = o.tier
    if o.reported:
        activity.usage_format = call.format
        activity.reported_usage = reported_quantities(o.reported)
    if o.failed:
        activity.status = _wire.STATUS_FAILED
        activity.error_code = o.code
        activity.error_format = o.failure.format
        activity.reported_error = list(o.failure.fields)
    elif truncated_finish(o.finish):
        activity.status = _wire.STATUS_TRUNCATED
        activity.error_code = o.finish
    elif blocked_finish(o.finish):
        activity.status = _wire.STATUS_FAILED
        activity.error_code = o.finish
    rp.recorder.record(activity)


def _sdk_default_client(sdk: str, asynchronous: bool) -> tuple[Any, Any]:
    """The sdk's own client class, with the defaults it uses internally, and the default transport of the HTTP package it is built on."""
    module = importlib.import_module(sdk)
    client = getattr(module, "DefaultAsyncHttpxClient" if asynchronous else "DefaultHttpxClient")
    http = importlib.import_module(client.__mro__[1].__module__.split(".")[0])
    limits = getattr(importlib.import_module(f"{sdk}._constants"), "DEFAULT_CONNECTION_LIMITS", None)
    transport_class = http.AsyncHTTPTransport if asynchronous else http.HTTPTransport
    inner = transport_class(limits=limits) if limits is not None else transport_class()
    return client, inner


def http_client(rp: "Reporter", sdk: str, asynchronous: bool, kwargs: dict) -> Any:
    client_class, inner = _sdk_default_client(sdk, asynchronous)
    transport = (AsyncObservingTransport if asynchronous else ObservingTransport)(rp, inner)
    return client_class(transport=transport, **kwargs)


def genai_http_options(rp: "Reporter", kwargs: dict) -> Any:
    types = importlib.import_module("google.genai.types")
    httpx = importlib.import_module("httpx")
    client_args = dict(kwargs.pop("client_args", None) or {})
    async_client_args = dict(kwargs.pop("async_client_args", None) or {})
    client_args["transport"] = ObservingTransport(rp, client_args.get("transport") or httpx.HTTPTransport())
    # A transport given for async calls is also what keeps the sdk on httpx rather than switching to aiohttp, which this could not see.
    async_client_args["transport"] = AsyncObservingTransport(rp, async_client_args.get("transport") or httpx.AsyncHTTPTransport())
    return types.HttpOptions(client_args=client_args, async_client_args=async_client_args, **kwargs)
