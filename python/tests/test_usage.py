"""A provider's counts arrive untouched, under its own names, and under the same names the Go recorder uses."""

import pytest

from agentpulse import reported_from, reported_from_genai
from agentpulse.usage import reported_quantities


def test_nested_counts_are_named_by_their_path_and_everything_else_is_left_out():
    usage = {
        "prompt_tokens": 100,
        "completion_tokens": 50,
        "prompt_tokens_details": {"cached_tokens": 80, "audio_tokens": None},
        "completion_tokens_details": {"reasoning_tokens": 20, "rejected_prediction_tokens": 0},
        "service_tier": "default",
        "flagged": True,
        "breakdown": [1, 2],
        "whole_float": 3.0,
        "fraction": 0.5,
    }
    assert reported_from(usage) == {
        "prompt_tokens": 100,
        "completion_tokens": 50,
        "prompt_tokens_details.cached_tokens": 80,
        "completion_tokens_details.reasoning_tokens": 20,
        "whole_float": 3,
    }


def test_a_record_carries_its_counts_sorted_with_zeros_left_out():
    quantities = reported_quantities({"b": 2, "a": 1, "z": 0, " ": 5})
    assert [(q.unit, q.quantity) for q in quantities] == [("a", 1), ("b", 2)]


def test_gemini_counts_from_the_rest_reply_keep_the_rest_names():
    usage = {"promptTokenCount": 1000, "cachedContentTokenCount": 800, "trafficType": "ON_DEMAND", "promptTokensDetails": [{"modality": "AUDIO", "tokenCount": 300}]}
    assert reported_from_genai(usage) == {"promptTokenCount": 1000, "cachedContentTokenCount": 800, "promptTokensDetails.AUDIO": 300}


def test_the_gemini_sdk_object_is_named_the_way_the_rest_reply_is():
    types = pytest.importorskip("google.genai.types")
    usage = types.GenerateContentResponseUsageMetadata(
        prompt_token_count=1000,
        cached_content_token_count=800,
        candidates_token_count=200,
        thoughts_token_count=150,
        tool_use_prompt_token_count=0,
        total_token_count=1350,
        prompt_tokens_details=[
            types.ModalityTokenCount(modality=types.MediaModality.AUDIO, token_count=300),
            types.ModalityTokenCount(modality=types.MediaModality.TEXT, token_count=700),
        ],
    )
    assert reported_from_genai(usage) == {
        "promptTokenCount": 1000,
        "cachedContentTokenCount": 800,
        "candidatesTokenCount": 200,
        "thoughtsTokenCount": 150,
        "totalTokenCount": 1350,
        "promptTokensDetails.AUDIO": 300,
        "promptTokensDetails.TEXT": 700,
    }


def test_the_openai_sdk_usage_is_its_json():
    types = pytest.importorskip("openai.types")
    from openai.types.completion_usage import CompletionTokensDetails, PromptTokensDetails

    usage = types.CompletionUsage(
        prompt_tokens=100,
        completion_tokens=50,
        total_tokens=150,
        prompt_tokens_details=PromptTokensDetails(cached_tokens=80),
        completion_tokens_details=CompletionTokensDetails(reasoning_tokens=20),
    )
    assert reported_from(usage) == {
        "prompt_tokens": 100,
        "completion_tokens": 50,
        "total_tokens": 150,
        "prompt_tokens_details.cached_tokens": 80,
        "completion_tokens_details.reasoning_tokens": 20,
    }


def test_the_anthropic_sdk_usage_keeps_the_cache_lifetimes_and_the_searches():
    types = pytest.importorskip("anthropic.types")
    usage = types.Usage(
        input_tokens=10,
        output_tokens=5,
        cache_read_input_tokens=800,
        cache_creation_input_tokens=50,
        cache_creation=types.CacheCreation(ephemeral_5m_input_tokens=20, ephemeral_1h_input_tokens=30),
        server_tool_use=types.ServerToolUsage(web_search_requests=2, web_fetch_requests=0),
        service_tier="standard",
    )
    assert reported_from(usage) == {
        "input_tokens": 10,
        "output_tokens": 5,
        "cache_read_input_tokens": 800,
        "cache_creation_input_tokens": 50,
        "cache_creation.ephemeral_5m_input_tokens": 20,
        "cache_creation.ephemeral_1h_input_tokens": 30,
        "server_tool_use.web_search_requests": 2,
    }
