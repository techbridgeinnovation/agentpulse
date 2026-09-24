"""A gateway on this machine that speaks gRPC-web the way the real one does, and a sink that remembers what it was given."""

from __future__ import annotations

import gzip
import http.server
import json
import threading
import time
from dataclasses import dataclass, field

from agentpulse import _wire


@dataclass
class Call:
    path: str
    headers: dict
    message: bytes


@dataclass
class Reply:
    """How the fake gateway answers the next call."""

    message: bytes = b""
    # The status in the trailer frame; None sends no trailer.
    trailer_status: int | None = 0
    # A status in the headers, as a refusal before any reply is sent.
    header_status: int | None = None
    http_status: int = 200
    delay: float = 0.0
    # For a streaming call: each message sent as its own frame, with a pause of `delay` before the trailer.
    messages: list | None = None


class FakeGateway:
    def __init__(self):
        self.calls: list[Call] = []
        self.replies: list[Reply] = []
        gateway = self

        class Handler(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *args):
                pass

            def do_POST(self):
                body = self.rfile.read(int(self.headers["content-length"]))
                assert body[0] == 0
                length = int.from_bytes(body[1:5], "big")
                gateway.calls.append(Call(self.path, {k.lower(): v for k, v in self.headers.items()}, body[5 : 5 + length]))
                reply = gateway.replies.pop(0) if gateway.replies else Reply()
                if reply.messages is not None:
                    self.send_response(200)
                    self.send_header("content-type", "application/grpc-web+proto")
                    self.send_header("transfer-encoding", "chunked")
                    self.end_headers()
                    trailer = b"grpc-status:0\r\n"
                    frames = [b"\x00" + len(m).to_bytes(4, "big") + m for m in reply.messages]
                    for frame in frames:
                        self.wfile.write(f"{len(frame):x}\r\n".encode() + frame + b"\r\n")
                        self.wfile.flush()
                    time.sleep(reply.delay)
                    frame = b"\x80" + len(trailer).to_bytes(4, "big") + trailer
                    self.wfile.write(f"{len(frame):x}\r\n".encode() + frame + b"\r\n0\r\n\r\n")
                    return
                if reply.delay:
                    time.sleep(reply.delay)
                payload = b""
                if reply.header_status is None:
                    payload += b"\x00" + len(reply.message).to_bytes(4, "big") + reply.message
                    if reply.trailer_status is not None:
                        trailer = f"grpc-status:{reply.trailer_status}\r\ngrpc-message:\r\n".encode()
                        payload += b"\x80" + len(trailer).to_bytes(4, "big") + trailer
                self.send_response(reply.http_status)
                self.send_header("content-type", "application/grpc-web+proto")
                if reply.header_status is not None:
                    self.send_header("grpc-status", str(reply.header_status))
                self.send_header("content-length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.address = f"http://127.0.0.1:{self.server.server_address[1]}"
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def close(self):
        self.server.shutdown()
        self.server.server_close()


@dataclass
class MemorySink:
    name: str = "memory"
    batches: list[tuple[str, list[_wire.Activity]]] = field(default_factory=list)
    named: list[tuple[str, list]] = field(default_factory=list)
    fail: bool = False
    delay: float = 0.0

    def send(self, activities, workspace, timeout):
        if self.delay:
            time.sleep(self.delay)
        if self.fail:
            raise RuntimeError("refused")
        self.batches.append((workspace, list(activities)))

    def send_users(self, users, workspace, timeout):
        if self.fail:
            raise RuntimeError("refused")
        self.named.append((workspace, list(users)))

    @property
    def activities(self):
        return [a for _, batch in self.batches for a in batch]


class Provider:
    """Answers each request with the next scripted reply and remembers what it was sent."""

    def __init__(self):
        self.replies = []
        self.received = []
        provider = self

        class Handler(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *args):
                pass

            def do_POST(self):
                length = int(self.headers.get("content-length") or 0)
                provider.received.append((self.path, json.loads(self.rfile.read(length) or b"{}")))
                status, headers, chunks = provider.replies.pop(0)
                self.send_response(status)
                for name, value in headers.items():
                    self.send_header(name, value)
                if isinstance(chunks, bytes):
                    self.send_header("content-length", str(len(chunks)))
                    self.end_headers()
                    self.wfile.write(chunks)
                    return
                self.send_header("transfer-encoding", "chunked")
                self.end_headers()
                for chunk in chunks:
                    self.wfile.write(f"{len(chunk):x}\r\n".encode() + chunk + b"\r\n")
                    self.wfile.flush()
                self.wfile.write(b"0\r\n\r\n")

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.server.server_address[1]}"
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def json(self, body, status=200, compress=False, headers=None):
        data = json.dumps(body).encode()
        extra = {"content-type": "application/json", **(headers or {})}
        if compress:
            data = gzip.compress(data)
            extra["content-encoding"] = "gzip"
        self.replies.append((status, extra, data))

    def sse(self, events, named=False, done=True):
        # Anthropic names each event on its own line, as the real api does, and its sdk reads the name.
        chunks = [(f"event: {e['type']}\n" if named else "").encode() + f"data: {json.dumps(e)}\n\n".encode() for e in events] + ([b"data: [DONE]\n\n"] if done else [])
        # Split one event across two writes, as a network does.
        chunks[0:1] = [chunks[0][:7], chunks[0][7:]]
        self.replies.append((200, {"content-type": "text/event-stream"}, chunks))

    def close(self):
        self.server.shutdown()
        self.server.server_close()
