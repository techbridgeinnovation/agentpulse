"""What each in-flight request has spent, priced here, so a spend ceiling is readable before a model call without a network call to ask.

The figure is exact for what this process did and blind to what another process spent on the same request, so it is a per-request ceiling and never an organisation's budget; that is what governance answers. It is never written onto a record and never billed: metering prices what it stores against the card it holds, and discards whatever a caller claimed.

The arithmetic is the server's own, ported: the same reader per reporting convention turning a provider's counts into priced classes, the same choice of one rate per kind, the most specific that applies, and the same rounding. `tests/test_pricing.py` holds the server's own cases. The two must agree, because two figures for the same work that disagree are worse than one figure and a gap.
"""

from __future__ import annotations

import threading
import time
from dataclasses import dataclass, field
from typing import Callable, Protocol

from . import _wire
from .gateway import Gateway

_LIST_PRICEABLE_UNITS = "/techbridge.ap.metering.v1.PriceableUnitsService/ListPriceableUnits"
_PAGE_SIZE = 1000
_MAX_PAGES = 10

# The rate card's priceable kinds, as the contract numbers them.
KIND_PROMPT_TOKENS = 1
KIND_CANDIDATE_TOKENS = 2
KIND_CACHED_TOKENS = 3
KIND_CACHE_WRITE_TOKENS = 4
KIND_REASONING_TOKENS = 5

# What the running totals hold. An entry expires on its own and the map has a hard cap, neither configurable, so a caller who never finishes a request costs a bounded amount of memory rather than a leak.
_MAX_TOTALS = 4096
_TOTAL_TTL = 15 * 60.0


@dataclass(frozen=True)
class PriceableUnit:
    """One rate on the card."""

    name: str = ""
    provider: str = ""
    model: str = ""
    kind: int = 0
    unit_cost_nanos: int = 0
    service_tier: str = ""
    min_prompt_tokens: int = 0
    min_cache_write_ttl_seconds: int = 0
    modality: str = ""
    # Nanoseconds since the epoch; zero is unbounded.
    effective_from_ns: int = 0
    effective_to_ns: int = 0

    @classmethod
    def decode(cls, data: bytes) -> "PriceableUnit":
        values: dict = {}
        text = {1: "name", 3: "provider", 4: "model", 12: "service_tier", 15: "modality"}
        numbers = {5: "kind", 11: "unit_cost_nanos", 13: "min_prompt_tokens", 14: "min_cache_write_ttl_seconds"}
        for number, value in _wire.fields(data):
            if number in text and isinstance(value, bytes):
                values[text[number]] = value.decode("utf-8", "replace")
            elif number in numbers and isinstance(value, int):
                values[numbers[number]] = _signed(value)
            elif number in (7, 8) and isinstance(value, bytes):
                values["effective_from_ns" if number == 7 else "effective_to_ns"] = _timestamp(value)
        return cls(**values)

    def in_force(self, at_ns: int) -> bool:
        if self.effective_from_ns and self.effective_from_ns > at_ns:
            return False
        if self.effective_to_ns and self.effective_to_ns <= at_ns:
            return False
        return True


def _signed(value: int) -> int:
    return value - (1 << 64) if value >= 1 << 63 else value


def _timestamp(data: bytes) -> int:
    seconds = nanos = 0
    for number, value in _wire.fields(data):
        if number == 1 and isinstance(value, int):
            seconds = _signed(value)
        elif number == 2 and isinstance(value, int):
            nanos = _signed(value)
    return seconds * 1_000_000_000 + nanos


@dataclass
class Classes:
    """A call's counts, split into the classes it is priced on."""

    prompt: int = 0
    candidate: int = 0
    cached: int = 0
    cache_write: int = 0
    reasoning: int = 0
    extra: list[tuple[str, int]] = field(default_factory=list)


def _first(q: dict[str, int], *names: str) -> int:
    for name in names:
        if name in q:
            return q[name]
    return 0


def _without(whole: int, part: int) -> int:
    return 0 if part >= whole else whole - part


def _vertex(q: dict[str, int]) -> Classes:
    # The prompt count already contains what was served from cache, and the thoughts are generated output billed at the output rate.
    prompt, cached, thoughts = _first(q, "promptTokenCount"), _first(q, "cachedContentTokenCount"), _first(q, "thoughtsTokenCount")
    return Classes(
        prompt=_without(prompt, cached) + _first(q, "toolUsePromptTokenCount"),
        candidate=_first(q, "candidatesTokenCount") + thoughts,
        cached=cached,
        reasoning=thoughts,
    )


def _anthropic(q: dict[str, int]) -> Classes:
    # The input count excludes both the cache read and the cache write, which are reported beside it.
    return Classes(
        prompt=_first(q, "input_tokens"),
        candidate=_first(q, "output_tokens"),
        cached=_first(q, "cache_read_input_tokens"),
        cache_write=_first(q, "cache_creation_input_tokens"),
        reasoning=_first(q, "output_tokens_details.thinking_tokens", "thinking_tokens"),
    )


def _openai(q: dict[str, int]) -> Classes:
    # The input count contains the cached tokens, and the output count contains the reasoning.
    cached = _first(q, "input_tokens_details.cached_tokens", "prompt_tokens_details.cached_tokens")
    return Classes(
        prompt=_without(_first(q, "input_tokens", "prompt_tokens"), cached),
        candidate=_first(q, "output_tokens", "completion_tokens"),
        cached=cached,
        cache_write=_first(q, "input_tokens_details.cache_write_tokens", "prompt_tokens_details.cache_write_tokens"),
        reasoning=_first(q, "output_tokens_details.reasoning_tokens", "completion_tokens_details.reasoning_tokens"),
    )


def _perplexity(q: dict[str, int]) -> Classes:
    classes = Classes(prompt=_first(q, "prompt_tokens") + _first(q, "citation_tokens"), candidate=_first(q, "completion_tokens"), reasoning=_first(q, "reasoning_tokens"))
    searches = _first(q, "num_search_queries")
    if searches > 0:
        classes.extra.append(("num_search_queries", searches))
    return classes


def _litellm(q: dict[str, int]) -> Classes:
    # Both cached parts are inside the input count, so both come out of it before either is priced.
    cached = _first(q, "prompt_tokens_details.cached_tokens", "cache_read_input_tokens")
    cache_write = _first(q, "prompt_tokens_details.cache_write_tokens", "prompt_tokens_details.cache_creation_tokens", "cache_creation_input_tokens")
    return Classes(
        prompt=_without(_first(q, "prompt_tokens"), cached + cache_write),
        candidate=_first(q, "completion_tokens"),
        cached=cached,
        cache_write=cache_write,
        reasoning=_first(q, "completion_tokens_details.reasoning_tokens"),
    )


_READERS: dict[str, Callable[[dict[str, int]], Classes]] = {
    "VERTEX": _vertex,
    "ANTHROPIC": _anthropic,
    "OPENAI_CHAT": _openai,
    "OPENAI_RESPONSES": _openai,
    "PERPLEXITY": _perplexity,
    "LITELLM": _litellm,
}


def classes_of(activity: _wire.Activity) -> Classes:
    """The classes an activity is priced on: read from what the provider reported where a reader claims the convention, as the server does, and otherwise the classes the caller stated."""
    reader = _READERS.get(activity.usage_format)
    if reader is not None:
        quantities: dict[str, int] = {}
        for q in activity.reported_usage:
            if q.unit and q.unit not in quantities:
                quantities[q.unit] = q.quantity
        return reader(quantities)
    return Classes(activity.prompt_tokens, activity.candidate_tokens, activity.cached_tokens, activity.cache_write_tokens, activity.reasoning_tokens)


def _applies(unit: PriceableUnit, activity: _wire.Activity, classes: Classes) -> bool:
    if unit.provider != activity.billed_by:
        return False
    # An empty model, tier or modality on a rate means it holds whatever the call's; a named one holds only for calls that match it.
    if unit.model and unit.model != activity.model:
        return False
    if unit.service_tier and unit.service_tier != activity.service_tier:
        return False
    # A context threshold reacts to the whole prompt in play, the cached part included.
    if unit.min_prompt_tokens > classes.prompt + classes.cached:
        return False
    if unit.min_cache_write_ttl_seconds > activity.cache_write_ttl_seconds:
        return False
    # A record carries no quantity counted by modality, so a modality rate matches nothing rather than pricing audio at the text rate.
    return unit.modality == ""


def _more_specific(unit: PriceableUnit, held: PriceableUnit) -> bool:
    # The rate whose conditions were hardest to meet, in the order a provider's price list is organised. A total order, so the answer never depends on the order the card loaded in.
    if bool(unit.model) != bool(held.model):
        return bool(unit.model)
    if bool(unit.service_tier) != bool(held.service_tier):
        return bool(unit.service_tier)
    if unit.min_prompt_tokens != held.min_prompt_tokens:
        return unit.min_prompt_tokens > held.min_prompt_tokens
    return unit.min_cache_write_ttl_seconds > held.min_cache_write_ttl_seconds


def cost_of(quantity: int, nanos: int) -> int:
    """A quantity at a rate in billionths, in millionths, rounded to the nearest rather than truncated so a small charge is never free."""
    total = quantity * nanos
    if total == 0:
        return 0
    # The server divides as Go does, truncating towards zero, where Python's floor division would round a negative figure the other way.
    shifted = total + 500
    return shifted // 1000 if shifted >= 0 else -(-shifted // 1000)


class RateCard:
    """The rates in hand, indexed so pricing a record on the caller's thread looks only at the rates for its provider. Immutable: a refresh replaces the whole card, so a record is never priced against one halfway through a change."""

    def __init__(self, units: list[PriceableUnit]):
        self.units = list(units)
        self.by_provider: dict[str, list[PriceableUnit]] = {}
        self.by_name: dict[str, list[PriceableUnit]] = {}
        for unit in self.units:
            self.by_provider.setdefault(unit.provider, []).append(unit)
            self.by_name.setdefault(unit.name, []).append(unit)


def price_of(card: RateCard | list[PriceableUnit], activity: _wire.Activity) -> int:
    """What an activity is expected to cost, in millionths of a dollar, against the rates in force when it happened."""
    if not isinstance(card, RateCard):
        card = RateCard(card)
    at = activity.occurred_at_ns or time.time_ns()
    in_force = [u for u in card.by_provider.get(activity.billed_by, ()) if u.in_force(at)]
    classes = classes_of(activity)
    by_kind = {
        KIND_PROMPT_TOKENS: classes.prompt,
        KIND_CANDIDATE_TOKENS: classes.candidate,
        KIND_CACHED_TOKENS: classes.cached,
        KIND_CACHE_WRITE_TOKENS: classes.cache_write,
        KIND_REASONING_TOKENS: classes.reasoning,
    }
    best: dict[int, PriceableUnit] = {}
    for unit in in_force:
        if not by_kind.get(unit.kind) or not _applies(unit, activity, classes):
            continue
        held = best.get(unit.kind)
        if held is None or _more_specific(unit, held):
            best[unit.kind] = unit
    total = sum(cost_of(by_kind[kind], unit.unit_cost_nanos) for kind, unit in best.items())

    # Charges not measured in tokens, such as a per-request search fee, each priced by the rate that names it.
    charges = [(c.priceable_unit, c.quantity) for c in activity.charges] + classes.extra
    for name, quantity in charges:
        for unit in card.by_name.get(name, ()):
            if unit.in_force(at):
                total += cost_of(quantity, unit.unit_cost_nanos)
                break
    return total


class RateSource(Protocol):
    """Supplies the whole rate card. Called on a background schedule and never on the path of a model call. Raises on failure."""

    def list_rates(self, timeout: float) -> list[PriceableUnit]: ...


class GatewayRates:
    """The metering rate card, read through the gateway on the same connection records travel on."""

    def __init__(self, gateway: Gateway):
        self._gateway = gateway

    def list_rates(self, timeout: float) -> list[PriceableUnit]:
        units: list[PriceableUnit] = []
        token = ""
        for _ in range(_MAX_PAGES):
            request = _wire._integer(1, _PAGE_SIZE) + _wire._string(2, token)
            token = ""
            for number, value in _wire.fields(self._gateway.call(_LIST_PRICEABLE_UNITS, request, timeout)):
                if number == 1 and isinstance(value, bytes):
                    units.append(PriceableUnit.decode(value))
                elif number == 2 and isinstance(value, bytes):
                    token = value.decode("utf-8", "replace")
            if not token:
                break
        return units


class RunningTotals:
    """What each request has spent in this process, bounded by a cap and a lifetime."""

    def __init__(self, now: Callable[[], float] = time.monotonic):
        self._lock = threading.Lock()
        self._totals: dict[str, tuple[int, float]] = {}
        self._now = now
        self.evicted = 0

    def add(self, request: str, micros: int) -> None:
        if not request or not micros:
            return
        with self._lock:
            now = self._now()
            spent, touched = self._totals.get(request, (0, now))
            if now - touched >= _TOTAL_TTL:
                spent = 0
            if request not in self._totals and len(self._totals) >= _MAX_TOTALS:
                self._make_room(now)
            self._totals[request] = (spent + micros, now)

    def spent_on(self, request: str) -> int:
        with self._lock:
            entry = self._totals.get(request)
            if entry is None:
                return 0
            if self._now() - entry[1] >= _TOTAL_TTL:
                del self._totals[request]
                return 0
            return entry[0]

    def finish(self, request: str) -> None:
        with self._lock:
            self._totals.pop(request, None)

    def _make_room(self, now: float) -> None:
        for stale in [r for r, (_, touched) in self._totals.items() if now - touched >= _TOTAL_TTL]:
            del self._totals[stale]
        if len(self._totals) < _MAX_TOTALS:
            return
        # An eviction under the cap is an undercount for a request still running, so it is counted.
        oldest = sorted(self._totals, key=lambda r: self._totals[r][1])[: max(1, _MAX_TOTALS // 8)]
        for request in oldest:
            del self._totals[request]
        self.evicted += len(oldest)
