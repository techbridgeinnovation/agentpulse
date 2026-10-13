"""The hand-written encoder against the official protobuf runtime, byte for byte.

The fixtures were written by `scripts/wire_fixtures.py` from the contract itself, so a difference here is this library disagreeing with what metering and governance will parse.
"""

import json
import pathlib

import pytest

from agentpulse import _wire

FIXTURES = json.loads((pathlib.Path(__file__).parent / "wire_fixtures.json").read_text())


def activity(fields):
    fields = dict(fields)
    fields["charges"] = [_wire.Charge(**c) for c in fields.get("charges", [])]
    fields["reported_usage"] = [_wire.ReportedQuantity(**q) for q in fields.get("reported_usage", [])]
    fields["reported_error"] = [_wire.ReportedField(**f) for f in fields.get("reported_error", [])]
    return _wire.Activity(**fields)


def encode(case):
    fields = case["fields"]
    if case["message"] == "BatchCreateActivitiesRequest":
        losses = _wire.RecorderLosses(**fields["recorder_losses"]) if "recorder_losses" in fields else None
        return _wire.batch_create_activities(fields["parent"], [activity(a) for a in fields["activities"]], fields["request_id"], losses)
    if case["message"] == "BatchUpsertUsersRequest":
        return _wire.batch_upsert_users(fields["parent"], [_wire.User(**u) for u in fields["users"]])
    if case["message"] == "DecideRequest":
        return _wire.DecideRequest(**fields).encode()
    raise AssertionError(f"no encoder for {case['message']}")


REPLIES = ("DecideResponse", "ListPriceableUnitsResponse")


@pytest.mark.parametrize("case", [c for c in FIXTURES if c["message"] not in REPLIES], ids=lambda c: c["name"])
def test_encodes_exactly_as_the_official_runtime(case):
    assert encode(case).hex() == case["hex"]


def decide_response(fields):
    fields = dict(fields)
    fields["budget_statuses"] = [_wire.BudgetStatus(**s) for s in fields.get("budget_statuses", [])]
    return _wire.DecideResponse(**fields)


@pytest.mark.parametrize("case", [c for c in FIXTURES if c["message"] == "DecideResponse"], ids=lambda c: c["name"])
def test_decodes_what_the_official_runtime_wrote(case):
    assert _wire.DecideResponse.decode(bytes.fromhex(case["hex"])) == decide_response(case["fields"])


def test_budget_statuses_and_the_evaluate_time_are_read_from_the_official_runtime_s_bytes():
    [case] = [c for c in FIXTURES if c["name"] == "decide response with budget statuses"]
    response = _wire.DecideResponse.decode(bytes.fromhex(case["hex"]))
    assert [s.scope for s in response.budget_statuses] == [_wire.SCOPE_WORKSPACE, _wire.SCOPE_RESERVE, _wire.SCOPE_PERSONAL, 9]
    assert [s.level for s in response.budget_statuses] == [_wire.LEVEL_PASSED_95_PERCENT, _wire.LEVEL_EXHAUSTED, _wire.LEVEL_WITHIN, 7]
    assert response.budget_statuses[1].limit_micros == 0
    assert response.evaluate_time_ns == 1791892800_123456789


def test_a_reply_from_a_governance_without_statuses_has_none():
    [case] = [c for c in FIXTURES if c["name"] == "decide response"]
    response = _wire.DecideResponse.decode(bytes.fromhex(case["hex"]))
    assert response.budget_statuses == [] and response.evaluate_time_ns == 0


def test_an_unknown_field_inside_a_budget_status_is_skipped_and_a_negative_int64_is_read_as_signed():
    # Field 5, spent_micros, as -1 in ten bytes, then an unknown field 15, inside status field 6.
    status = b"\x28" + b"\xff" * 9 + b"\x01" + b"\x7a\x01x" + b"\x48\x02"
    response = _wire.DecideResponse.decode(b"\x32" + bytes([len(status)]) + status)
    [read] = response.budget_statuses
    assert (read.spent_micros, read.level) == (-1, _wire.LEVEL_PASSED_75_PERCENT)


def test_an_unknown_field_in_a_reply_is_skipped():
    # A newer server may add fields; an older recorder must still read the ones it knows.
    known = _wire.DecideResponse(decision=_wire.DECISION_DENY).decode(bytes.fromhex("0804") + b"\x7a\x03abc" + b"\xa0\x06\x01")
    assert known.decision == _wire.DECISION_DENY


def test_a_truncated_reply_is_refused_rather_than_misread():
    with pytest.raises(_wire.DecodeError):
        _wire.DecideResponse.decode(b"\x12\x05ab")


@pytest.mark.parametrize("case", [c for c in FIXTURES if c["message"] == "ListPriceableUnitsResponse"], ids=lambda c: c["name"])
def test_reads_a_rate_card_page_the_official_runtime_wrote(case):
    from agentpulse.pricing import PriceableUnit

    units = [PriceableUnit.decode(v) for n, v in _wire.fields(bytes.fromhex(case["hex"])) if n == 1]
    wanted = [{k: v for k, v in u.items() if k not in ("display_name", "rate_card_version")} for u in case["fields"]["priceable_units"]]
    assert [PriceableUnit(**w) for w in wanted] == units


def test_the_framework_and_its_version_are_fields_49_and_50():
    # Keys 49 << 3 | 2 = 394 and 50 << 3 | 2 = 402, written as the varints 8a 03 and 92 03.
    assert _wire.Activity(framework="f", framework_version="1").encode() == b"\x8a\x03\x01f\x92\x03\x011"
    assert _wire.Activity().encode() == b""


def test_losses_that_are_all_zero_are_left_off_the_batch():
    zero = _wire.RecorderLosses(recorder="python/0.4.0")
    assert _wire.batch_create_activities("organisations/acme", [], losses=zero) == _wire.batch_create_activities("organisations/acme", [])
