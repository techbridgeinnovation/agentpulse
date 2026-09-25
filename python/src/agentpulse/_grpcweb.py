"""One gRPC call to the gateway, made as gRPC-web over the standard library's HTTP client.

The gateway serves gRPC-web beside native gRPC, and the same api-key checks run for both. Speaking it from here means no grpcio, which is the dependency most likely to hurt a host: it is a large native wheel, it is not safe across a fork without settings the host would have to make, and it has hung servers that held a client as a global.

gRPC-web puts a call's outcome in a trailer frame at the end of the body, and a refusal before any reply puts it in the headers instead, still under HTTP 200. So a call succeeds only when a status of zero was actually read from one of the two, and HTTP 200 on its own means nothing.
"""

from __future__ import annotations

import http.client
import os
import socket
import ssl
import threading
from typing import Iterator

# The canonical gRPC status codes, named as grpc-go names them, which is how the Go recorder writes a transport failure's code onto a record. The two recorders must name the same failure the same way.
CODE_NAMES = {
    0: "OK",
    1: "Canceled",
    2: "Unknown",
    3: "InvalidArgument",
    4: "DeadlineExceeded",
    5: "NotFound",
    6: "AlreadyExists",
    7: "PermissionDenied",
    8: "ResourceExhausted",
    9: "FailedPrecondition",
    10: "Aborted",
    11: "OutOfRange",
    12: "Unimplemented",
    13: "Internal",
    14: "Unavailable",
    15: "DataLoss",
    16: "Unauthenticated",
}

CANCELLED = 1
UNKNOWN = 2
DEADLINE_EXCEEDED = 4
PERMISSION_DENIED = 7
UNIMPLEMENTED = 12
INTERNAL = 13
UNAVAILABLE = 14
UNAUTHENTICATED = 16

# What an HTTP status means when the reply carried no gRPC status at all, as the gRPC-over-HTTP mapping has it.
_HTTP_TO_CODE = {
    400: INTERNAL,
    401: UNAUTHENTICATED,
    403: PERMISSION_DENIED,
    404: UNIMPLEMENTED,
    429: UNAVAILABLE,
    502: UNAVAILABLE,
    503: UNAVAILABLE,
    504: UNAVAILABLE,
}

# A reply is a small message and a trailer. Anything larger is not a reply from the gateway, and is not read.
_MAX_REPLY = 4 << 20

_LOOPBACK = {"localhost", "127.0.0.1", "::1"}


class RPCError(Exception):
    """A call that did not succeed, by its gRPC code.

    The status message is deliberately not kept. It is the gateway's, not a provider's, so it quotes no prompt, but a library that never holds error text cannot be the one that leaks it.
    """

    def __init__(self, code: int):
        super().__init__(CODE_NAMES.get(code, "Unknown"))
        self.code = code

    @property
    def code_name(self) -> str:
        return CODE_NAMES.get(self.code, "Unknown")


def parse_address(address: str) -> tuple[str, int, bool]:
    """The host, port and whether to use TLS, from whatever a person pasted.

    A scheme and a trailing slash are dropped and 443 is assumed, because the gateway serves on 443 and a bare host is what its URL shows. Plain HTTP is accepted only for this machine, so a test can reach a local gateway and a key can never travel in clear to anywhere else.
    """
    target = address.strip()
    secure = True
    if target.startswith("https://"):
        target = target[len("https://") :]
    elif target.startswith("http://"):
        target = target[len("http://") :]
        secure = False
    target = target.rstrip("/")
    if not target:
        raise ValueError("the gateway address is empty")

    host, port = target, 443
    if target.startswith("["):
        end = target.find("]")
        if end < 0:
            raise ValueError("the gateway address has an unclosed bracket")
        host = target[1:end]
        if target[end + 1 :].startswith(":"):
            port = int(target[end + 2 :])
    elif target.count(":") == 1:
        host, _, raw_port = target.partition(":")
        port = int(raw_port)

    if not secure and host not in _LOOPBACK:
        raise ValueError("plain http is only allowed to this machine; use https for the gateway")
    return host, port, secure


class Channel:
    """Unary gRPC-web calls to one host, with the same headers on every call.

    Each thread keeps its own connection and reuses it between calls, since the worker makes one call every few seconds and a new TLS handshake each time would be the slowest part of it. A connection that fails in any way is thrown away and the next call opens another. A process that forks gets fresh connections in the child, because a socket inherited from the parent is still the parent's.
    """

    def __init__(self, host: str, port: int, secure: bool, headers: dict[str, str]):
        self._host = host
        self._port = port
        self._secure = secure
        self._headers = dict(headers)
        self._local = threading.local()
        self._context: ssl.SSLContext | None = None
        if secure:
            self._context = ssl.create_default_context()
            self._context.minimum_version = ssl.TLSVersion.TLSv1_2

    def unary(self, method: str, message: bytes, timeout: float) -> bytes:
        """Calls `method`, e.g. `/techbridge.ap.metering.v1.ActivitiesService/BatchCreateActivities`, and returns the encoded reply.

        Raises RPCError for every way a call can fail, a network failure included, so a caller handles one exception.
        """
        body = b"\x00" + len(message).to_bytes(4, "big") + message
        headers = {
            **self._headers,
            "content-type": "application/grpc-web+proto",
            "accept": "application/grpc-web+proto",
            "x-grpc-web": "1",
            "grpc-timeout": f"{max(1, int(timeout * 1000))}m",
        }
        try:
            connection = self._connection(timeout)
            connection.request("POST", method, body=body, headers=headers)
            response = connection.getresponse()
            status_header = response.getheader("grpc-status")
            payload = response.read(_MAX_REPLY + 1)
        except (socket.timeout, TimeoutError):
            self._discard()
            raise RPCError(DEADLINE_EXCEEDED) from None
        except (OSError, http.client.HTTPException):
            self._discard()
            raise RPCError(UNAVAILABLE) from None

        if response.status != 200:
            self._discard()
            if status_header is not None:
                raise RPCError(_code(status_header))
            raise RPCError(_HTTP_TO_CODE.get(response.status, UNKNOWN))
        if status_header is not None and _code(status_header) != 0:
            raise RPCError(_code(status_header))
        if len(payload) > _MAX_REPLY:
            self._discard()
            raise RPCError(INTERNAL)
        if response.getheader("connection", "").lower() == "close":
            self._discard()

        reply, trailer_status = _frames(payload)
        status = trailer_status if trailer_status is not None else _code(status_header) if status_header is not None else None
        if status is None:
            raise RPCError(INTERNAL)
        if status != 0:
            raise RPCError(status)
        return reply

    def server_stream(self, method: str, message: bytes, idle: float, stopped: threading.Event) -> Iterator[bytes]:
        """Calls a server-streaming `method` and yields each message as it arrives, until the stream ends, sits silent for `idle` seconds, or `stopped` is set.

        On a connection of its own, since a stream holds one open for as long as it lasts. Ends quietly when the server ends it with a status of zero or goes silent; raises RPCError for any other outcome.
        """
        body = b"\x00" + len(message).to_bytes(4, "big") + message
        headers = {**self._headers, "content-type": "application/grpc-web+proto", "accept": "application/grpc-web+proto", "x-grpc-web": "1"}
        connection = self._new_connection(idle)
        try:
            try:
                connection.request("POST", method, body=body, headers=headers)
                response = connection.getresponse()
            except (socket.timeout, TimeoutError):
                raise RPCError(DEADLINE_EXCEEDED) from None
            except (OSError, http.client.HTTPException):
                raise RPCError(UNAVAILABLE) from None
            status_header = response.getheader("grpc-status")
            if response.status != 200:
                raise RPCError(_code(status_header) if status_header is not None else _HTTP_TO_CODE.get(response.status, UNKNOWN))
            if status_header is not None and _code(status_header) != 0:
                raise RPCError(_code(status_header))
            while not stopped.is_set():
                try:
                    head = _read_exactly(response, 5)
                    if len(head) < 5:
                        return
                    length = int.from_bytes(head[1:5], "big")
                    if length > _MAX_REPLY:
                        raise RPCError(INTERNAL)
                    frame = _read_exactly(response, length)
                except (socket.timeout, TimeoutError):
                    return
                except (OSError, http.client.HTTPException):
                    raise RPCError(UNAVAILABLE) from None
                if len(frame) < length:
                    return
                if head[0] & 0x80:
                    for line in frame.decode("latin-1").split("\r\n"):
                        name, _, value = line.partition(":")
                        if name.strip().lower() == "grpc-status" and _code(value) != 0:
                            raise RPCError(_code(value))
                    return
                yield frame
        finally:
            try:
                connection.close()
            except Exception:
                pass

    def _new_connection(self, timeout: float) -> http.client.HTTPConnection:
        if self._secure:
            return http.client.HTTPSConnection(self._host, self._port, timeout=timeout, context=self._context)
        return http.client.HTTPConnection(self._host, self._port, timeout=timeout)

    def _connection(self, timeout: float) -> http.client.HTTPConnection:
        local = self._local
        connection = getattr(local, "connection", None)
        if connection is not None and local.pid == os.getpid():
            connection.timeout = timeout
            if connection.sock is not None:
                connection.sock.settimeout(timeout)
            return connection
        connection = self._new_connection(timeout)
        local.connection = connection
        local.pid = os.getpid()
        return connection

    def _discard(self) -> None:
        local = self._local
        connection = getattr(local, "connection", None)
        local.connection = None
        # A connection inherited across a fork is left alone: closing it here would send the TLS close on a session the parent is still using.
        if connection is not None and getattr(local, "pid", None) == os.getpid():
            try:
                connection.close()
            except Exception:
                pass


def _read_exactly(response: http.client.HTTPResponse, n: int) -> bytes:
    data = b""
    while len(data) < n:
        chunk = response.read(n - len(data))
        if not chunk:
            break
        data += chunk
    return data


def _code(value: str) -> int:
    try:
        return int(value.strip())
    except ValueError:
        return UNKNOWN


def _frames(payload: bytes) -> tuple[bytes, int | None]:
    """The first message in a gRPC-web body and the status its trailer frame states, if it states one."""
    message = b""
    status: int | None = None
    have_message = False
    at = 0
    while at + 5 <= len(payload):
        flag = payload[at]
        length = int.from_bytes(payload[at + 1 : at + 5], "big")
        frame = payload[at + 5 : at + 5 + length]
        if len(frame) < length:
            raise RPCError(INTERNAL)
        at += 5 + length
        if flag & 0x80:
            for line in frame.decode("latin-1").split("\r\n"):
                name, _, value = line.partition(":")
                if name.strip().lower() == "grpc-status":
                    status = _code(value)
        elif not have_message:
            if flag & 0x01:
                # A compressed message; the gateway never compresses and this does not ask it to.
                raise RPCError(INTERNAL)
            message, have_message = frame, True
    return message, status
