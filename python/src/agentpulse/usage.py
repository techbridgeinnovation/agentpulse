"""What a provider said a call used, under the provider's own names.

Nothing is added, subtracted or renamed here, and that is the point. Providers disagree about what their own headline numbers contain: Gemini's prompt count already includes the tokens served from cache, OpenAI's does too and counts reasoning inside its output figure besides. Splitting the counts into priced classes happens once, on the server, where a mistake is corrected by one deploy and reapplied to records already written. An interpretation baked in here could only be corrected by every adopter upgrading.

The names match what the Go recorder sends for the same call, because the server reads both with the same reader.
"""

from __future__ import annotations

from typing import Any, Mapping

from ._wire import ReportedQuantity

# The conventions a set of counts can arrive in. Which one decides how the numbers are read, and it is not the same question as who billed: Claude served through Vertex is billed by Google and reports Anthropic-shaped usage.
FORMAT_VERTEX = "VERTEX"
FORMAT_ANTHROPIC = "ANTHROPIC"
FORMAT_OPENAI_CHAT = "OPENAI_CHAT"
FORMAT_OPENAI_RESPONSES = "OPENAI_RESPONSES"
# Every provider's usage as LiteLLM restates it, in OpenAI's shape but with both the cache read and the cache write inside the input count.
FORMAT_LITELLM = "LITELLM"
# OpenAI's chat field names with Perplexity's own counts beside them, such as the citation tokens its search charges for.
FORMAT_PERPLEXITY = "PERPLEXITY"

# Who bills for a call, in the vocabulary the rate card prices against. A plain string on the wire, so calling a provider nobody has called before needs no release of this library.
PROVIDER_VERTEX_AI = "VERTEX_AI"
PROVIDER_ANTHROPIC = "ANTHROPIC"
PROVIDER_OPENAI = "OPENAI"
PROVIDER_PERPLEXITY = "PERPLEXITY"

# Gemini's counts, in the order the Go recorder states them, under the names the REST api gives them. The Python sdk spells them in snake case; the server reads the REST names.
_GENAI_COUNTS = (
    ("prompt_token_count", "promptTokenCount"),
    ("candidates_token_count", "candidatesTokenCount"),
    ("cached_content_token_count", "cachedContentTokenCount"),
    ("thoughts_token_count", "thoughtsTokenCount"),
    ("tool_use_prompt_token_count", "toolUsePromptTokenCount"),
    ("total_token_count", "totalTokenCount"),
)

# The per-modality breakdowns, which are the only place it is visible that some tokens were audio or image rather than text. Those are priced at their own rates, several times the text rate.
_GENAI_DETAILS = (
    ("prompt_tokens_details", "promptTokensDetails"),
    ("candidates_tokens_details", "candidatesTokensDetails"),
    ("cache_tokens_details", "cacheTokensDetails"),
    ("tool_use_prompt_tokens_details", "toolUsePromptTokensDetails"),
)


def reported_quantities(reported: Mapping[str, int] | None) -> list[ReportedQuantity]:
    """What a record carries for a set of counts: sorted by name, with zeros and blank names left out.

    Sorted because a record whose counts land in a different order on every call cannot be compared with another recording of the same call, and comparing them is how a convention gets checked.
    """
    if not reported:
        return []
    return [
        ReportedQuantity(unit=unit, quantity=int(quantity))
        for unit, quantity in sorted(reported.items())
        if unit.strip() and _whole(quantity) and quantity != 0
    ]


def reported_from_genai(usage: Any) -> dict[str, int]:
    """The counts on a Gemini or Vertex reply's usage metadata, from the `google-genai` sdk's object or from the REST reply's `usageMetadata`."""
    if usage is None:
        return {}
    reported: dict[str, int] = {}
    for snake, camel in _GENAI_COUNTS:
        value = _read(usage, snake, camel)
        if _whole(value) and value:
            reported[camel] = int(value)
    for snake, camel in _GENAI_DETAILS:
        for detail in _read(usage, snake, camel) or ():
            modality = _read(detail, "modality", "modality")
            modality = str(getattr(modality, "value", modality) or "").strip()
            count = _read(detail, "token_count", "tokenCount")
            if modality and _whole(count) and count:
                reported[f"{camel}.{modality}"] = int(count)
    return reported


def reported_from(usage: Any) -> dict[str, int]:
    """Every whole-number count on a provider's usage object, each under the provider's own name, with a nested count named by its path, e.g. `prompt_tokens_details.cached_tokens`.

    For OpenAI's and Anthropic's sdks, whose usage objects are the reply's JSON under the JSON's own names, and for a usage dictionary read off any reply. Text, flags and lists are left out; a count reported as zero is too, because a provider reporting nothing and one reporting none are the same thing to a bill.
    """
    counts: dict[str, int] = {}
    _walk(_as_mapping(usage), "", counts, depth=0)
    return counts


reported_from_openai = reported_from
reported_from_anthropic = reported_from


def _walk(mapping: Mapping[str, Any] | None, prefix: str, out: dict[str, int], depth: int) -> None:
    if mapping is None or depth > 4:
        return
    for name, value in mapping.items():
        if not isinstance(name, str):
            continue
        if _whole(value):
            if value:
                out[prefix + name] = int(value)
            continue
        nested = _as_mapping(value)
        if nested is not None:
            _walk(nested, f"{prefix}{name}.", out, depth + 1)


def _as_mapping(value: Any) -> Mapping[str, Any] | None:
    if value is None or isinstance(value, (str, bytes, int, float, list, tuple)):
        return None
    if isinstance(value, Mapping):
        return value
    dump = getattr(value, "model_dump", None)
    if callable(dump):
        try:
            dumped = dump()
        except Exception:
            return None
        return dumped if isinstance(dumped, Mapping) else None
    to_dict = getattr(value, "to_dict", None)
    if callable(to_dict):
        try:
            dumped = to_dict()
        except Exception:
            return None
        return dumped if isinstance(dumped, Mapping) else None
    return None


def _whole(value: Any) -> bool:
    if isinstance(value, bool):
        return False
    if isinstance(value, int):
        return True
    return isinstance(value, float) and value.is_integer()


def _read(source: Any, attribute: str, key: str) -> Any:
    if isinstance(source, Mapping):
        return source.get(key, source.get(attribute))
    return getattr(source, attribute, None)
