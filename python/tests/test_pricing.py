"""The running total, priced with the server's own arithmetic.

The rate cases are metering's own, from `metering/v1/internal/pricing/conditions_test.go`, with the same figures, because the local price and the stored price must agree.
"""

import time

from agentpulse import Activity, Config, Recorder, ReportedQuantity, _wire
from agentpulse.pricing import (
    KIND_CACHE_WRITE_TOKENS,
    KIND_CACHED_TOKENS,
    KIND_CANDIDATE_TOKENS,
    KIND_PROMPT_TOKENS,
    GatewayRates,
    PriceableUnit,
    RunningTotals,
    classes_of,
    cost_of,
    price_of,
)

from fakes import FakeGateway, MemorySink, Reply

MODEL = "gemini-3.5-flash"
HOUR_AGO = time.time_ns() - 3600 * 1_000_000_000


def rate(name, kind, nanos, *, model=MODEL, provider="VERTEX_AI", **conditions):
    return PriceableUnit(name=f"priceableUnits/{name}", provider=provider, model=model, kind=kind, unit_cost_nanos=nanos, effective_from_ns=HOUR_AGO, **conditions)


def call(**fields):
    fields.setdefault("billed_by", "VERTEX_AI")
    fields.setdefault("model", MODEL)
    return Activity(**fields)


def test_a_kind_matched_by_two_rates_is_charged_once_at_the_more_specific():
    card = [rate("any", KIND_PROMPT_TOKENS, 1500, model=""), rate("model", KIND_PROMPT_TOKENS, 1000)]
    assert price_of(card, call(prompt_tokens=1000)) == 1000


def test_a_batched_call_is_priced_at_the_batch_rate_and_a_standard_one_is_not():
    card = [rate("std", KIND_PROMPT_TOKENS, 1500), rate("batch", KIND_PROMPT_TOKENS, 750, service_tier="BATCH")]
    assert price_of(card, call(prompt_tokens=1000, service_tier="BATCH")) == 750
    assert price_of(card, call(prompt_tokens=1000)) == 1500


def test_a_call_over_the_context_threshold_is_priced_at_the_higher_rate():
    card = [rate("below", KIND_PROMPT_TOKENS, 1500), rate("above", KIND_PROMPT_TOKENS, 3000, min_prompt_tokens=200_000)]
    assert price_of(card, call(prompt_tokens=150_000, cached_tokens=60_000)) == 450_000
    assert price_of(card, call(prompt_tokens=1000)) == 1500


def test_a_cache_entry_kept_an_hour_costs_more_to_write():
    card = [rate("write", KIND_CACHE_WRITE_TOKENS, 1875), rate("write1hr", KIND_CACHE_WRITE_TOKENS, 3000, min_cache_write_ttl_seconds=3600)]
    assert price_of(card, call(cache_write_tokens=1000, cache_write_ttl_seconds=3600)) == 3000
    assert price_of(card, call(cache_write_tokens=1000, cache_write_ttl_seconds=300)) == 1875


def test_a_rate_with_no_conditions_is_the_fallback():
    card = [rate("any", KIND_PROMPT_TOKENS, 1500)]
    assert price_of(card, call(prompt_tokens=1000, service_tier="PRIORITY", cache_write_ttl_seconds=3600)) == 1500


def test_a_modality_rate_does_not_silently_price_text_tokens():
    card = [rate("text", KIND_PROMPT_TOKENS, 1500), rate("audio", KIND_PROMPT_TOKENS, 15000, modality="AUDIO")]
    assert price_of(card, call(prompt_tokens=1000)) == 1500


def test_the_answer_does_not_depend_on_the_order_the_card_loaded():
    specific, general = rate("batch", KIND_PROMPT_TOKENS, 750, service_tier="BATCH"), rate("std", KIND_PROMPT_TOKENS, 1500)
    c = call(prompt_tokens=1000, service_tier="BATCH")
    assert price_of([specific, general], c) == price_of([general, specific], c)


def test_a_rate_not_yet_or_no_longer_in_force_is_not_used():
    now = time.time_ns()
    card = [
        PriceableUnit(name="priceableUnits/future", provider="VERTEX_AI", kind=KIND_PROMPT_TOKENS, unit_cost_nanos=9000, effective_from_ns=now + 10**12),
        PriceableUnit(name="priceableUnits/past", provider="VERTEX_AI", kind=KIND_PROMPT_TOKENS, unit_cost_nanos=8000, effective_from_ns=HOUR_AGO, effective_to_ns=now - 10**9),
    ]
    assert price_of(card, call(prompt_tokens=1000, occurred_at_ns=now)) == 0
    # Priced against the rates in force when the work happened.
    assert price_of(card, call(prompt_tokens=1000, occurred_at_ns=now - 2 * 10**9)) == 8000


def test_a_small_charge_rounds_to_the_nearest_millionth_and_is_never_free():
    assert cost_of(1, 500) == 1
    assert cost_of(1, 499) == 0
    assert cost_of(-1, 1500) == -1


def test_a_charge_is_priced_by_the_rate_that_names_it():
    card = [PriceableUnit(name="priceableUnits/web-search", provider="VERTEX_AI", kind=6, unit_cost_nanos=14_000_000, effective_from_ns=HOUR_AGO)]
    assert price_of(card, call(charges=[_wire.Charge("priceableUnits/web-search", 2)])) == 28_000


def reported(format, **counts):
    return call(usage_format=format, reported_usage=[ReportedQuantity(k.replace("__", "."), v) for k, v in counts.items()])


def test_gemini_counts_are_split_as_the_server_splits_them():
    c = classes_of(reported("VERTEX", promptTokenCount=1000, cachedContentTokenCount=800, thoughtsTokenCount=150, candidatesTokenCount=200, toolUsePromptTokenCount=5))
    assert (c.prompt, c.cached, c.candidate, c.reasoning) == (205, 800, 350, 150)


def test_anthropic_and_openai_counts_are_split_as_the_server_splits_them():
    a = classes_of(reported("ANTHROPIC", input_tokens=10, output_tokens=5, cache_read_input_tokens=800, cache_creation_input_tokens=50))
    assert (a.prompt, a.candidate, a.cached, a.cache_write) == (10, 5, 800, 50)
    o = classes_of(reported("OPENAI_CHAT", prompt_tokens=100, completion_tokens=50, prompt_tokens_details__cached_tokens=80, completion_tokens_details__reasoning_tokens=20))
    assert (o.prompt, o.candidate, o.cached, o.reasoning) == (20, 50, 80, 20)


def test_a_cached_gemini_call_is_charged_once_for_its_cached_tokens():
    card = [rate("in", KIND_PROMPT_TOKENS, 1000), rate("cached", KIND_CACHED_TOKENS, 100), rate("out", KIND_CANDIDATE_TOKENS, 4000)]
    # 200 fresh input at 1000, 800 cached at 100, 200 output at 4000.
    assert price_of(card, reported("VERTEX", promptTokenCount=1000, cachedContentTokenCount=800, candidatesTokenCount=200)) == 200 + 80 + 800


class Card:
    def __init__(self, units=None, error=None):
        self.units, self.error, self.reads = units or [], error, 0

    def list_rates(self, timeout):
        self.reads += 1
        if self.error:
            raise self.error
        return self.units


def wait_for(condition, seconds=2.0):
    deadline = time.monotonic() + seconds
    while not condition() and time.monotonic() < deadline:
        time.sleep(0.01)
    return condition()


def test_a_request_spend_is_the_sum_of_its_calls_and_a_dropped_record_still_counts():
    card = Card([rate("in", KIND_PROMPT_TOKENS, 1000)])
    rec = Recorder(Config(sinks=[MemorySink(delay=5)], queue_size=1, batch_size=1, exit_timeout=0, rates=card))
    assert wait_for(lambda: rec._card is not None)
    for _ in range(5):
        rec.record(call(request="req-1", prompt_tokens=1000))
    assert rec.stats().dropped >= 1
    assert rec.spent_on("req-1") == 5000
    rec.finish_request("req-1")
    assert rec.spent_on("req-1") == 0


def test_without_a_rate_source_nothing_is_counted():
    rec = Recorder(Config(exit_timeout=0))
    rec.record(call(request="req-1", prompt_tokens=1000))
    assert rec.spent_on("req-1") == 0


def test_a_rate_card_that_cannot_be_read_is_counted_and_the_card_in_hand_is_kept():
    card = Card([rate("in", KIND_PROMPT_TOKENS, 1000)])
    rec = Recorder(Config(exit_timeout=0, rates=card, rate_refresh=0.05))
    assert wait_for(lambda: rec._card is not None)
    card.error = RuntimeError("unreachable")
    assert wait_for(lambda: rec.stats().rate_fetch_errors >= 1)
    rec.record(call(request="r", prompt_tokens=10))
    assert rec.spent_on("r") == 10
    rec.close(timeout=1)


def test_the_running_totals_cannot_grow_without_bound_and_an_eviction_is_counted():
    totals = RunningTotals()
    for i in range(5000):
        totals.add(f"r{i}", 1)
    assert len(totals._totals) <= 4096 and totals.evicted > 0


def test_a_running_total_expires_on_its_own():
    clock = [0.0]
    totals = RunningTotals(now=lambda: clock[0])
    totals.add("r", 10)
    clock[0] = 15 * 60
    assert totals.spent_on("r") == 0


def test_the_rate_card_is_read_page_by_page_through_the_gateway():
    from agentpulse import Gateway

    def page(units, token):
        body = b"".join(_wire._message(1, _wire._string(1, n) + _wire._string(3, "VERTEX_AI") + _wire._integer(5, 1) + _wire._integer(11, c)) for n, c in units)
        return Reply(message=body + _wire._string(2, token))

    fake = FakeGateway()
    try:
        fake.replies += [page([("priceableUnits/a", 1000)], "next"), page([("priceableUnits/b", 2000)], "")]
        units = GatewayRates(Gateway(fake.address, "organisations/acme/apiKeys/k1", "s")).list_rates(2)
        assert [(u.name, u.unit_cost_nanos) for u in units] == [("priceableUnits/a", 1000), ("priceableUnits/b", 2000)]
        assert fake.calls[1].message == _wire._integer(1, 1000) + _wire._string(2, "next")
        assert fake.calls[0].path == "/techbridge.ap.metering.v1.PriceableUnitsService/ListPriceableUnits"
    finally:
        fake.close()
