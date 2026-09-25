"""Failures reduced to a code and the fields a provider names a failure by, and never to a message.

The expected codes and fields are the ones the Go recorder writes for the same failure, because the server reads both with one reader. Tests against the real sdks run where the sdk is installed and are skipped where it is not; the library itself never imports any of them.
"""

import asyncio

import pytest

from agentpulse import blocked_finish, error_code, reported_error_for, reported_error_of
from agentpulse._grpcweb import RPCError

SECRET = "the prompt said: my card number is 4111"


def fields(reported):
    return {f.name: f.value for f in reported.fields}


def test_a_deadline_and_a_cancellation_are_named_as_grpc_names_them():
    assert error_code(TimeoutError(SECRET)) == "DeadlineExceeded"
    assert error_code(asyncio.TimeoutError()) == "DeadlineExceeded"
    assert error_code(asyncio.CancelledError()) == "Canceled"
    assert fields(reported_error_for(TimeoutError(SECRET))) == {"code": "DeadlineExceeded"}


def test_an_error_nobody_named_is_unknown_and_says_nothing_more():
    assert error_code(ValueError(SECRET)) == "Unknown"
    assert reported_error_for(ValueError(SECRET)).format == ""


def test_a_gateway_failure_keeps_its_grpc_code():
    assert error_code(RPCError(14)) == "Unavailable"


def test_a_cause_is_read_through_what_wrapped_it():
    try:
        try:
            raise TimeoutError(SECRET)
        except TimeoutError as inner:
            raise RuntimeError("wrapped") from inner
    except RuntimeError as outer:
        assert error_code(outer) == "DeadlineExceeded"


def test_a_message_given_by_hand_is_dropped_whatever_it_is_called():
    reported = reported_error_of("ANTHROPIC", {"type": "overloaded_error", "message": SECRET, "error": SECRET, "Details": SECRET})
    assert fields(reported) == {"type": "overloaded_error"}


def test_a_value_longer_than_a_name_is_cut_on_a_character_boundary():
    reported = reported_error_of("OPENAI", {"code": "é" * 150})
    value = reported.fields[0].value
    assert len(value.encode()) <= 200 and value == "é" * 100


def test_finish_reasons_are_read_whatever_their_case():
    assert blocked_finish("content_filter") and blocked_finish(" SAFETY ")
    assert not blocked_finish("stop") and not blocked_finish(None)


class TestGenAI:
    # Skipped at test time rather than collection, so a missing sdk skips its own tests and not the whole file.
    genai_errors = property(lambda self: pytest.importorskip("google.genai.errors"))

    def error(self, code=429, status="RESOURCE_EXHAUSTED"):
        return self.genai_errors.ClientError(
            code,
            {
                "error": {
                    "code": code,
                    "message": SECRET,
                    "status": status,
                    "details": [
                        {"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "RATE_LIMIT_EXCEEDED", "domain": "googleapis.com"},
                        {"@type": "type.googleapis.com/google.rpc.BadRequest", "fieldViolations": [{"field": "contents[0]", "description": SECRET}]},
                    ],
                }
            },
        )

    def test_the_status_google_named_is_the_code(self):
        assert error_code(self.error()) == "RESOURCE_EXHAUSTED"

    def test_an_http_phrase_in_place_of_a_status_falls_back_to_the_http_code(self):
        assert error_code(self.error(status="429 Too Many Requests")) == "HTTP_429"

    def test_the_details_beneath_the_status_are_kept_and_their_descriptions_are_not(self):
        reported = reported_error_for(self.error())
        assert reported.format == "VERTEX"
        assert fields(reported) == {
            "domain": "googleapis.com",
            "fieldViolations.field": "contents[0]",
            "http_status": "429",
            "reason": "RATE_LIMIT_EXCEEDED",
            "status": "RESOURCE_EXHAUSTED",
        }
        assert SECRET not in repr(reported)


def _response(httpx, status, headers):
    return httpx.Response(status, headers=headers, request=httpx.Request("POST", "https://api.example.com/v1/x"))


class TestOpenAI:
    openai = property(lambda self: pytest.importorskip("openai"))

    def error(self):
        openai = self.openai
        httpx2 = pytest.importorskip("httpx2")
        response = _response(httpx2, 429, {"x-request-id": "req_abc", "retry-after": "20"})
        body = {"message": SECRET, "type": "requests", "param": None, "code": "rate_limit_exceeded"}
        return openai.RateLimitError(SECRET, response=response, body=body)

    def test_the_specific_code_is_the_code(self):
        assert error_code(self.error()) == "rate_limit_exceeded"

    def test_the_fields_openai_names_a_failure_by_are_kept(self):
        reported = reported_error_for(self.error(), "OPENAI")
        assert reported.format == "OPENAI"
        assert fields(reported) == {"code": "rate_limit_exceeded", "http_status": "429", "request_id": "req_abc", "retry_after": "20", "type": "requests"}

    def test_the_same_shape_billed_by_perplexity_is_stated_in_its_convention(self):
        assert reported_error_for(self.error(), "PERPLEXITY").format == "PERPLEXITY"

    def test_a_timeout_in_the_sdk_is_a_deadline(self):
        openai = self.openai
        httpx2 = pytest.importorskip("httpx2")
        error = openai.APITimeoutError(request=httpx2.Request("POST", "https://api.openai.com/v1/chat/completions"))
        assert error_code(error) == "DeadlineExceeded"


class TestAnthropic:
    anthropic = property(lambda self: pytest.importorskip("anthropic"))

    def error(self):
        anthropic = self.anthropic
        httpx2 = pytest.importorskip("httpx2")
        response = _response(httpx2, 529, {"request-id": "req_hdr"})
        body = {"type": "error", "error": {"type": "overloaded_error", "message": SECRET}, "request_id": "req_body"}
        return anthropic.OverloadedError(SECRET, response=response, body=body)

    def test_the_type_is_the_code(self):
        assert error_code(self.error()) == "overloaded_error"

    def test_the_envelope_is_read_in_anthropic_convention(self):
        reported = reported_error_for(self.error(), "ANTHROPIC")
        assert reported.format == "ANTHROPIC"
        assert fields(reported) == {"http_status": "529", "request_id": "req_body", "type": "overloaded_error"}
