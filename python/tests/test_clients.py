"""Model calls recorded from the client, through the real sdks, against a provider on this machine.

Each test builds a client the way an adopter would and makes an ordinary call. What the provider sends back is scripted, so every shape of reply is covered: whole, streamed, compressed, refused, failed part way. Skipped where the sdk is not installed.
"""

import asyncio

import pytest

from agentpulse import Attribution, Config, Recorder, Reporter, _wire, denied, scope

from fakes import MemorySink, Provider

AGENT = "organisations/acme/agents/research"


@pytest.fixture
def provider():
    p = Provider()
    yield p
    p.close()


class Answers:
    def __init__(self, decision, **response):
        self.response = _wire.DecideResponse(decision=decision, **response)
        self.asked = []

    def decide(self, request, timeout):
        self.asked.append(request)
        return self.response


def reporter(billed_by="", decider=None):
    sink = MemorySink()
    rec = Recorder(Config(sinks=[sink], exit_timeout=0, flush_every=60))
    rp = Reporter(rec, Attribution(agent=AGENT, service="svc", billed_by=billed_by))
    if decider is not None:
        rp = rp.governed(decider, cache_ttl=0)
    return rp, sink


def recorded(rp, sink):
    rp.recorder.flush(timeout=2)
    return sink.activities


def usage(activity):
    return {q.unit: q.quantity for q in activity.reported_usage}


CHAT = {
    "id": "c1",
    "object": "chat.completion",
    "created": 1,
    "model": "gpt-5-2026-08-01",
    "service_tier": "default",
    "choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "SECRET ANSWER"}}],
    "usage": {"prompt_tokens": 100, "completion_tokens": 50, "total_tokens": 150, "prompt_tokens_details": {"cached_tokens": 80}, "completion_tokens_details": {"reasoning_tokens": 20}},
}


def openai_client(rp, provider, asynchronous=False):
    openai = pytest.importorskip("openai")
    cls = openai.AsyncOpenAI if asynchronous else openai.OpenAI
    return cls(api_key="k", base_url=provider.url + "/v1", max_retries=0, http_client=rp.openai_http_client(asynchronous=asynchronous))


def test_an_openai_chat_is_recorded_with_its_own_counts_and_the_caller_is_named(provider):
    rp, sink = reporter()
    client = openai_client(rp, provider)
    provider.json(CHAT)
    with scope(request="req-1"):
        reply = client.chat.completions.create(model="gpt-5", messages=[{"role": "user", "content": "SECRET PROMPT"}])
    assert reply.choices[0].message.content == "SECRET ANSWER"
    [a] = recorded(rp, sink)
    assert (a.model, a.billed_by, a.usage_format, a.status, a.request, a.service_tier, a.attempt) == ("gpt-5-2026-08-01", "OPENAI", "OPENAI_CHAT", _wire.STATUS_OK, "req-1", "default", 1)
    assert usage(a) == {"prompt_tokens": 100, "completion_tokens": 50, "total_tokens": 150, "prompt_tokens_details.cached_tokens": 80, "completion_tokens_details.reasoning_tokens": 20}
    assert a.caller_component == "test_clients.test_an_openai_chat_is_recorded_with_its_own_counts_and_the_caller_is_named"
    assert "SECRET" not in repr(sink.batches)


def test_a_compressed_reply_is_still_read(provider):
    rp, sink = reporter()
    client = openai_client(rp, provider)
    provider.json(CHAT, compress=True)
    assert client.chat.completions.create(model="gpt-5", messages=[]).usage.total_tokens == 150
    assert usage(recorded(rp, sink)[0])["total_tokens"] == 150


def test_a_streamed_chat_is_recorded_from_its_last_counts(provider):
    rp, sink = reporter()
    client = openai_client(rp, provider)
    provider.sse(
        [
            {"id": "c", "object": "chat.completion.chunk", "created": 1, "model": "gpt-5", "choices": [{"index": 0, "delta": {"content": "SECRET"}}]},
            {"id": "c", "object": "chat.completion.chunk", "created": 1, "model": "gpt-5", "choices": [{"index": 0, "delta": {}, "finish_reason": "length"}]},
            {"id": "c", "object": "chat.completion.chunk", "created": 1, "model": "gpt-5", "choices": [], "usage": {"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10}},
        ]
    )
    text = "".join(c.choices[0].delta.content or "" for c in client.chat.completions.create(model="gpt-5", messages=[], stream=True, stream_options={"include_usage": True}) if c.choices)
    assert text == "SECRET"
    [a] = recorded(rp, sink)
    assert usage(a) == {"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10}
    assert (a.status, a.error_code) == (_wire.STATUS_TRUNCATED, "length")


def test_a_responses_call_is_read_in_its_own_convention(provider):
    rp, sink = reporter()
    client = openai_client(rp, provider)
    provider.json(
        {
            "id": "r1",
            "object": "response",
            "created_at": 1,
            "model": "gpt-5",
            "status": "incomplete",
            "incomplete_details": {"reason": "max_output_tokens"},
            "output": [],
            "parallel_tool_calls": True,
            "tool_choice": "auto",
            "tools": [],
            "usage": {"input_tokens": 10, "input_tokens_details": {"cached_tokens": 4}, "output_tokens": 5, "output_tokens_details": {"reasoning_tokens": 2}, "total_tokens": 15},
        }
    )
    client.responses.create(model="gpt-5", input="SECRET")
    [a] = recorded(rp, sink)
    assert a.usage_format == "OPENAI_RESPONSES"
    assert (a.status, a.error_code) == (_wire.STATUS_TRUNCATED, "max_output_tokens")
    assert usage(a)["input_tokens_details.cached_tokens"] == 4


def test_a_refused_openai_call_keeps_what_openai_named_it_and_the_caller_still_gets_its_error(provider):
    openai = pytest.importorskip("openai")
    rp, sink = reporter()
    client = openai_client(rp, provider)
    provider.json({"error": {"message": "SECRET", "type": "requests", "param": None, "code": "rate_limit_exceeded"}}, status=429, headers={"x-request-id": "req_x"})
    with pytest.raises(openai.RateLimitError):
        client.chat.completions.create(model="gpt-5", messages=[])
    [a] = recorded(rp, sink)
    assert (a.status, a.error_code, a.error_format) == (_wire.STATUS_FAILED, "rate_limit_exceeded", "OPENAI")
    assert {f.name: f.value for f in a.reported_error} == {"code": "rate_limit_exceeded", "http_status": "429", "request_id": "req_x", "type": "requests"}


def test_a_call_that_is_not_a_model_call_passes_through_unrecorded(provider):
    rp, sink = reporter()
    client = openai_client(rp, provider)
    provider.json({"object": "list", "data": [{"object": "embedding", "index": 0, "embedding": [0.1]}], "model": "e", "usage": {"prompt_tokens": 1, "total_tokens": 1}})
    client.embeddings.create(model="e", input="x")
    assert recorded(rp, sink) == []


def test_perplexity_through_the_openai_sdk_is_billed_and_read_as_perplexity(provider):
    rp, sink = reporter(billed_by="PERPLEXITY")
    client = openai_client(rp, provider)
    provider.json({**CHAT, "usage": {"prompt_tokens": 5, "completion_tokens": 5, "total_tokens": 10, "citation_tokens": 40, "num_search_queries": 1}})
    client.chat.completions.create(model="sonar", messages=[])
    [a] = recorded(rp, sink)
    assert (a.billed_by, a.usage_format, usage(a)["citation_tokens"]) == ("PERPLEXITY", "PERPLEXITY", 40)


def test_an_async_openai_client_is_recorded_too(provider):
    rp, sink = reporter()
    client = openai_client(rp, provider, asynchronous=True)
    provider.json(CHAT)

    async def call():
        with scope(request="async-1"):
            await client.chat.completions.create(model="gpt-5", messages=[])

    asyncio.run(call())
    [a] = recorded(rp, sink)
    assert (a.request, usage(a)["total_tokens"]) == ("async-1", 150)


def test_a_denied_openai_call_is_never_sent_and_the_sdk_does_not_retry_it(provider):
    openai = pytest.importorskip("openai")
    rp, sink = reporter(decider=Answers(_wire.DECISION_DENY))
    client = openai.OpenAI(api_key="k", base_url=provider.url + "/v1", max_retries=3, http_client=rp.openai_http_client())
    with pytest.raises(openai.PermissionDeniedError) as raised:
        client.chat.completions.create(model="gpt-5", messages=[])
    assert denied(raised.value)
    assert provider.received == []
    [a] = recorded(rp, sink)
    assert (a.status, a.model) == (_wire.STATUS_DENIED, "gpt-5")
    assert rp.recorder.stats().denied == 1


def test_a_downgrade_rewrites_only_the_model_in_the_body(provider):
    rp, sink = reporter(decider=Answers(_wire.DECISION_DOWNGRADE, replacement_provider="OPENAI", replacement_model="gpt-5-mini"))
    client = openai_client(rp, provider)
    provider.json({**CHAT, "model": "gpt-5-mini"})
    client.chat.completions.create(model="gpt-5", messages=[{"role": "user", "content": "SECRET PROMPT"}], temperature=0.2)
    path, body = provider.received[0]
    assert body == {"model": "gpt-5-mini", "messages": [{"role": "user", "content": "SECRET PROMPT"}], "temperature": 0.2}
    assert rp.recorder.stats().downgrade_applied == 1


def test_a_downgrade_to_another_provider_is_not_applied(provider):
    rp, sink = reporter(decider=Answers(_wire.DECISION_DOWNGRADE, replacement_provider="ANTHROPIC", replacement_model="claude-haiku"))
    client = openai_client(rp, provider)
    provider.json(CHAT)
    client.chat.completions.create(model="gpt-5", messages=[])
    assert provider.received[0][1]["model"] == "gpt-5"
    assert rp.recorder.stats().downgrade_not_applied == 1


def anthropic_client(rp, provider, asynchronous=False):
    anthropic = pytest.importorskip("anthropic")
    cls = anthropic.AsyncAnthropic if asynchronous else anthropic.Anthropic
    return cls(api_key="k", base_url=provider.url, max_retries=0, http_client=rp.anthropic_http_client(asynchronous=asynchronous))


MESSAGE = {
    "id": "m1",
    "type": "message",
    "role": "assistant",
    "model": "claude-sonnet-5",
    "content": [{"type": "text", "text": "SECRET"}],
    "stop_reason": "end_turn",
    "usage": {"input_tokens": 10, "output_tokens": 5, "cache_read_input_tokens": 800, "cache_creation_input_tokens": 50, "cache_creation": {"ephemeral_5m_input_tokens": 50, "ephemeral_1h_input_tokens": 0}, "service_tier": "standard"},
}


def test_an_anthropic_message_is_recorded_with_its_cache_counts(provider):
    rp, sink = reporter()
    client = anthropic_client(rp, provider)
    provider.json(MESSAGE)
    client.messages.create(model="claude-sonnet-5", max_tokens=10, messages=[{"role": "user", "content": "SECRET"}])
    [a] = recorded(rp, sink)
    assert (a.model, a.billed_by, a.usage_format, a.service_tier) == ("claude-sonnet-5", "ANTHROPIC", "ANTHROPIC", "standard")
    assert usage(a) == {"input_tokens": 10, "output_tokens": 5, "cache_read_input_tokens": 800, "cache_creation_input_tokens": 50, "cache_creation.ephemeral_5m_input_tokens": 50}


def test_a_streamed_anthropic_message_that_fails_part_way_is_a_failure(provider):
    anthropic = pytest.importorskip("anthropic")
    rp, sink = reporter()
    client = anthropic_client(rp, provider)
    provider.sse(
        [
            {"type": "message_start", "message": {**MESSAGE, "content": [], "stop_reason": None, "usage": {"input_tokens": 10, "output_tokens": 1}}},
            {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}},
            {"type": "error", "error": {"type": "overloaded_error", "message": "SECRET"}},
        ],
        named=True,
    )
    with pytest.raises(anthropic.APIStatusError):
        for _ in client.messages.create(model="claude-sonnet-5", max_tokens=10, messages=[], stream=True):
            pass
    [a] = recorded(rp, sink)
    assert (a.status, a.error_code, a.error_format) == (_wire.STATUS_FAILED, "overloaded_error", "ANTHROPIC")
    assert usage(a) == {"input_tokens": 10, "output_tokens": 1}


def test_a_streamed_anthropic_message_is_recorded_from_its_running_totals(provider):
    rp, sink = reporter()
    client = anthropic_client(rp, provider)
    provider.sse(
        [
            {"type": "message_start", "message": {**MESSAGE, "content": [], "stop_reason": None, "usage": {"input_tokens": 10, "output_tokens": 1, "cache_read_input_tokens": 800}}},
            {"type": "message_delta", "delta": {"stop_reason": "max_tokens"}, "usage": {"output_tokens": 9}},
            {"type": "message_stop"},
        ],
        named=True,
    )
    with client.messages.stream(model="claude-sonnet-5", max_tokens=10, messages=[]) as stream:
        for _ in stream:
            pass
    [a] = recorded(rp, sink)
    assert usage(a) == {"input_tokens": 10, "output_tokens": 9, "cache_read_input_tokens": 800}
    assert (a.status, a.error_code) == (_wire.STATUS_TRUNCATED, "max_tokens")


def test_a_denied_anthropic_call_is_recognised(provider):
    anthropic = pytest.importorskip("anthropic")
    rp, _ = reporter(decider=Answers(_wire.DECISION_DENY))
    client = anthropic_client(rp, provider)
    with pytest.raises(anthropic.PermissionDeniedError) as raised:
        client.messages.create(model="claude-sonnet-5", max_tokens=10, messages=[])
    assert denied(raised.value) and provider.received == []


GENERATE = {
    "candidates": [{"content": {"role": "model", "parts": [{"text": "SECRET"}]}, "finishReason": "STOP"}],
    "modelVersion": "gemini-2.5-pro-002",
    "usageMetadata": {"promptTokenCount": 1000, "cachedContentTokenCount": 800, "candidatesTokenCount": 200, "thoughtsTokenCount": 150, "totalTokenCount": 1350, "trafficType": "ON_DEMAND", "promptTokensDetails": [{"modality": "AUDIO", "tokenCount": 300}]},
}


def genai_client(rp, provider):
    genai = pytest.importorskip("google.genai")
    return genai.Client(api_key="k", http_options=rp.genai_http_options(base_url=provider.url))


def test_a_gemini_call_is_recorded_under_the_rest_names(provider):
    rp, sink = reporter()
    client = genai_client(rp, provider)
    provider.json(GENERATE)
    assert client.models.generate_content(model="gemini-2.5-pro", contents="SECRET").text == "SECRET"
    [a] = recorded(rp, sink)
    assert (a.model, a.billed_by, a.usage_format, a.service_tier) == ("gemini-2.5-pro-002", "VERTEX_AI", "VERTEX", "ON_DEMAND")
    assert usage(a) == {"promptTokenCount": 1000, "cachedContentTokenCount": 800, "candidatesTokenCount": 200, "thoughtsTokenCount": 150, "totalTokenCount": 1350, "promptTokensDetails.AUDIO": 300}


def test_a_gemini_prompt_refused_before_the_model_saw_it_is_a_failure(provider):
    rp, sink = reporter()
    client = genai_client(rp, provider)
    provider.json({"promptFeedback": {"blockReason": "SAFETY"}, "usageMetadata": {"promptTokenCount": 12, "totalTokenCount": 12}})
    client.models.generate_content(model="gemini-2.5-pro", contents="x")
    [a] = recorded(rp, sink)
    assert (a.status, a.error_code, a.model) == (_wire.STATUS_FAILED, "SAFETY", "gemini-2.5-pro")


def test_an_async_streamed_gemini_call_is_recorded(provider):
    rp, sink = reporter()
    client = genai_client(rp, provider)
    provider.sse([{**GENERATE, "usageMetadata": {"promptTokenCount": 5}}, GENERATE], done=False)

    async def call():
        async for _ in await client.aio.models.generate_content_stream(model="gemini-2.5-pro", contents="x"):
            pass

    asyncio.run(call())
    [a] = recorded(rp, sink)
    assert usage(a)["totalTokenCount"] == 1350


def test_a_gemini_failure_keeps_the_reason_beneath_the_status(provider):
    errors = pytest.importorskip("google.genai.errors")
    rp, sink = reporter()
    client = genai_client(rp, provider)
    provider.json({"error": {"code": 429, "message": "SECRET", "status": "RESOURCE_EXHAUSTED", "details": [{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "RATE_LIMIT_EXCEEDED"}]}}, status=429)
    with pytest.raises(errors.APIError):
        client.models.generate_content(model="gemini-2.5-pro", contents="x")
    [a] = recorded(rp, sink)
    assert (a.error_code, a.error_format) == ("RESOURCE_EXHAUSTED", "VERTEX")
    assert {f.name: f.value for f in a.reported_error} == {"http_status": "429", "reason": "RATE_LIMIT_EXCEEDED", "status": "RESOURCE_EXHAUSTED"}


def test_a_denied_gemini_call_raises_spend_denied(provider):
    rp, sink = reporter(decider=Answers(_wire.DECISION_DENY))
    client = genai_client(rp, provider)
    with pytest.raises(Exception) as raised:
        client.models.generate_content(model="gemini-2.5-pro", contents="x")
    assert denied(raised.value) and provider.received == []
    assert recorded(rp, sink)[0].status == _wire.STATUS_DENIED


def test_a_gemini_downgrade_rewrites_the_model_in_the_path(provider):
    answers = Answers(_wire.DECISION_DOWNGRADE, replacement_provider="VERTEX_AI", replacement_model="gemini-2.5-flash")
    rp, sink = reporter(decider=answers)
    client = genai_client(rp, provider)
    provider.sse([GENERATE], done=False)
    for _ in client.models.generate_content_stream(model="gemini-2.5-pro", contents="x"):
        pass
    assert answers.asked[0].requested_model == "gemini-2.5-pro"
    assert provider.received[0][0].startswith("/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse")
    assert rp.recorder.stats().downgrade_applied == 1
