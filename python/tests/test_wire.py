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
        return _wire.batch_create_activities(fields["parent"], [activity(a) for a in fields["activities"]], fields["request_id"])
    if case["message"] == "BatchUpsertUsersRequest":
        return _wire.batch_upsert_users(fields["parent"], [_wire.User(**u) for u in fields["users"]])
    if case["message"] == "DecideRequest":
        return _wire.DecideRequest(**fields).encode()
    raise AssertionError(f"no encoder for {case['message']}")


REPLIES = ("DecideResponse", "ListPriceableUnitsResponse")


@pytest.mark.parametrize("case", [c for c in FIXTURES if c["message"] not in REPLIES], ids=lambda c: c["name"])
def test_encodes_exactly_as_the_official_runtime(case):
    assert encode(case).hex() == case["hex"]


@pytest.mark.parametrize("case", [c for c in FIXTURES if c["message"] == "DecideResponse"], ids=lambda c: c["name"])
def test_decodes_what_the_official_runtime_wrote(case):
    assert _wire.DecideResponse.decode(bytes.fromhex(case["hex"])) == _wire.DecideResponse(**case["fields"])


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
