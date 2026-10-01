import time

import pytest

from agentpulse import ConfigError, Gateway, MeteringSink, _wire, organisation_of_key
from agentpulse._grpcweb import RPCError, parse_address

from fakes import FakeGateway, Reply

KEY = "organisations/acme/apiKeys/k1"


@pytest.fixture
def fake():
    gateway = FakeGateway()
    yield gateway
    gateway.close()


def test_a_batch_reaches_metering_with_the_key_and_secret_on_the_call(fake):
    sink = MeteringSink(Gateway(fake.address, KEY, "s3cret"))
    sink.send([_wire.Activity(agent="organisations/acme/agents/a", request="r")], "w1", timeout=2)

    call = fake.calls[0]
    assert call.path == "/techbridge.ap.metering.v1.ActivitiesService/BatchCreateActivities"
    assert call.headers["x-api-key"] == KEY
    assert call.headers["x-api-secret"] == "s3cret"
    assert call.headers["content-type"] == "application/grpc-web+proto"
    request_id = _request_id(call.message)
    assert len(request_id) == 36
    assert call.message == _wire.batch_create_activities("organisations/acme/workspaces/w1", [_wire.Activity(agent="organisations/acme/agents/a", request="r")], request_id=request_id)


def _request_id(message):
    return next(value.decode() for number, value in _wire.fields(message) if number == 3)


def _sink(fake, **options):
    sink = MeteringSink(Gateway(fake.address, KEY, "s"), **options)
    sink._sleep = lambda seconds: None
    return sink


@pytest.mark.parametrize("reply", [Reply(header_status=14), Reply(trailer_status=8), Reply(trailer_status=10), Reply(http_status=503, trailer_status=None), Reply(http_status=429, trailer_status=None)])
def test_a_transient_failure_is_tried_again_under_the_same_request_id(fake, reply):
    fake.replies += [reply, Reply()]
    _sink(fake).send([_wire.Activity(request="r")], "", timeout=2)
    assert len(fake.calls) == 2
    first, second = (_request_id(c.message) for c in fake.calls)
    assert first == second


@pytest.mark.parametrize("reply", [Reply(header_status=16), Reply(trailer_status=7), Reply(trailer_status=3)])
def test_a_refusal_is_never_tried_again(fake, reply):
    fake.replies.append(reply)
    with pytest.raises(RPCError):
        _sink(fake).send([_wire.Activity(request="r")], "", timeout=2)
    assert len(fake.calls) == 1


def test_retries_are_bounded_and_each_batch_has_its_own_request_id(fake):
    fake.replies += [Reply(header_status=14)] * 5
    sink = _sink(fake, retries=2)
    with pytest.raises(RPCError):
        sink.send([_wire.Activity(request="r")], "", timeout=2)
    assert len(fake.calls) == 3
    sink.send([_wire.Activity(request="r")], "", timeout=2)
    assert _request_id(fake.calls[3].message) != _request_id(fake.calls[0].message)


def test_retries_never_run_past_the_send_timeout(fake):
    fake.replies += [Reply(header_status=14)] * 5
    sink = MeteringSink(Gateway(fake.address, KEY, "s"))
    started = time.monotonic()
    with pytest.raises(RPCError):
        sink.send([_wire.Activity(request="r")], "", timeout=0.3)
    assert time.monotonic() - started < 1.0


def test_a_batch_with_no_workspace_is_filed_under_the_organisation(fake):
    MeteringSink(Gateway(fake.address, KEY, "s")).send([_wire.Activity(request="r")], "", timeout=2)
    assert fake.calls[0].message.startswith(b"\x0a\x12organisations/acme")


def test_names_go_to_the_directory_of_the_workspace(fake):
    from agentpulse import User

    MeteringSink(Gateway(fake.address, KEY, "s")).send_users([User("u1", "Ada")], "w1", timeout=2)
    assert fake.calls[0].path == "/techbridge.ap.metering.v1.UsersService/BatchUpsertUsers"
    assert b"organisations/acme/workspaces/w1/users/u1" in fake.calls[0].message


@pytest.mark.parametrize(
    "reply, code",
    [
        # A refusal before any reply is sent arrives in the headers, still under HTTP 200.
        (Reply(header_status=16), "Unauthenticated"),
        (Reply(trailer_status=7), "PermissionDenied"),
        # No status anywhere is not a success.
        (Reply(trailer_status=None), "Internal"),
        (Reply(http_status=503, trailer_status=None), "Unavailable"),
    ],
)
def test_a_call_succeeds_only_when_a_status_of_zero_was_read(fake, reply, code):
    fake.replies.append(reply)
    with pytest.raises(RPCError) as raised:
        Gateway(fake.address, KEY, "s").call("/x.Y/Z", b"", timeout=2)
    assert raised.value.code_name == code


def test_a_slow_gateway_is_a_deadline_not_a_hang(fake):
    fake.replies.append(Reply(delay=1.0))
    with pytest.raises(RPCError) as raised:
        Gateway(fake.address, KEY, "s").call("/x.Y/Z", b"", timeout=0.2)
    assert raised.value.code_name == "DeadlineExceeded"


def test_an_unreachable_gateway_is_unavailable():
    with pytest.raises(RPCError) as raised:
        Gateway("http://127.0.0.1:1", KEY, "s").call("/x.Y/Z", b"", timeout=1)
    assert raised.value.code_name == "Unavailable"


def test_a_connection_is_reused_and_replaced_after_a_failure(fake):
    gateway = Gateway(fake.address, KEY, "s")
    gateway.call("/x.Y/Z", b"", timeout=2)
    fake.replies.append(Reply(http_status=503, trailer_status=None))
    with pytest.raises(RPCError):
        gateway.call("/x.Y/Z", b"", timeout=2)
    assert gateway.call("/x.Y/Z", b"", timeout=2) == b""


@pytest.mark.parametrize(
    "address, parsed",
    [
        ("gateway.example.com", ("gateway.example.com", 443, True)),
        ("https://gateway.example.com/", ("gateway.example.com", 443, True)),
        ("gateway.example.com:8443", ("gateway.example.com", 8443, True)),
        ("http://localhost:8088", ("localhost", 8088, False)),
        ("http://[::1]:8088", ("::1", 8088, False)),
    ],
)
def test_whatever_a_person_pasted_is_read_as_an_address(address, parsed):
    assert parse_address(address) == parsed


def test_a_key_never_travels_in_clear_to_another_machine():
    with pytest.raises(ConfigError):
        Gateway("http://gateway.example.com", KEY, "s")


@pytest.mark.parametrize("address, key, secret", [("", KEY, "s"), ("g", "", "s"), ("g", KEY, " ")])
def test_a_missing_setting_is_refused_at_startup(address, key, secret):
    with pytest.raises(ConfigError):
        Gateway(address, key, secret)


def test_the_organisation_is_read_off_the_key():
    assert organisation_of_key(KEY) == "organisations/acme"
    assert organisation_of_key("organisations/acme/x/apiKeys/k1") == ""
    assert organisation_of_key("s3cret") == ""


def test_a_sink_with_no_organisation_is_refused():
    with pytest.raises(ConfigError):
        MeteringSink(Gateway("g", "not-a-key-name", "s"))


class NoRates:
    def list_rates(self, timeout):
        return []


def test_connect_refuses_an_agent_outside_the_key_organisation(monkeypatch):
    import agentpulse

    monkeypatch.setenv("AP_GATEWAY", "gateway.example.com")
    monkeypatch.setenv("AP_API_KEY", KEY)
    monkeypatch.setenv("AP_API_SECRET", "s")
    monkeypatch.setenv("AP_AGENT", "organisations/other/agents/a")
    with pytest.raises(ConfigError):
        agentpulse.connect(service="svc")
    monkeypatch.setenv("AP_AGENT", "organisations/acme/agents/a")
    # A rate source of its own, so the test reaches no network.
    reporter = agentpulse.connect(service="svc", config=agentpulse.Config(exit_timeout=0, rates=NoRates()))
    assert reporter.attribution.agent == "organisations/acme/agents/a"
