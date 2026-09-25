"""Writes tests/wire_fixtures.json: messages encoded by the official protobuf runtime, compiled from the contract, for the hand-written encoder to be checked against byte for byte.

Run after a contract release that touches a message the recorder sends, from an environment with grpcio-tools and googleapis-common-protos, pointing DEFINE at a checkout of the define repository:

    DEFINE=~/alis.build/techbridge/define python scripts/wire_fixtures.py
"""

from __future__ import annotations

import json
import os
import pathlib
import sys
import tempfile


CONTRACTS = [
    "techbridge/ap/metering/v1/activity.proto",
    "techbridge/ap/metering/v1/user.proto",
    "techbridge/ap/governance/v1/decision.proto",
    "techbridge/ap/metering/v1/priceable_unit.proto",
]

FULL_ACTIVITY = {
    "agent": "organisations/acme/agents/research",
    "request": "req-1",
    "session": "sess-1",
    "user": "8c21e0b4",
    "caller_service": "sources-service",
    "caller_component": "asset_summary",
    "skill": "summaries",
    "model": "gemini-2.5-pro",
    "prompt_tokens": -1,
    "candidate_tokens": 2147483647,
    "cached_tokens": 3,
    "cache_write_tokens": 4,
    "reasoning_tokens": 5,
    "total_tokens": 300,
    "charges": [{"priceable_unit": "priceableUnits/web-search", "quantity": 2}, {"priceable_unit": "priceableUnits/x", "quantity": -7}],
    "duration_ms": 1234,
    "status": 2,
    "error_code": "RESOURCE_EXHAUSTED",
    "occurred_at_ns": 1790167065123456789,
    "project": "matter-1183",
    "tool": "search",
    "error_detail": "",
    "attempt": 2,
    "retry_of": "activities/prev",
    "result_bytes": 9007199254740993,
    "result_tokens": 17,
    "empty_result": True,
    "args_bytes": 64,
    "args_fingerprint": "ab12",
    "billed_by": "ANTHROPIC",
    "usage_format": "ANTHROPIC",
    "reported_usage": [{"unit": "cache_read_input_tokens", "quantity": 21847}, {"unit": "input_tokens", "quantity": 4211}],
    "service_tier": "standard",
    "cache_write_ttl_seconds": 3600,
    "provider_cost_micros": 1500,
    "error_format": "ANTHROPIC",
    "reported_error": [{"name": "http_status", "value": "429"}, {"name": "type", "value": "rate_limit_error"}],
}

UNICODE_ACTIVITY = {"agent": "organisations/acme/agents/é", "request": "请求-𝄞", "occurred_at_ns": 1_000_000_000}

CASES = [
    {"name": "full activity", "message": "BatchCreateActivitiesRequest", "fields": {"parent": "organisations/acme/workspaces/w1", "activities": [FULL_ACTIVITY], "request_id": ""}},
    {"name": "several activities and a request id", "message": "BatchCreateActivitiesRequest", "fields": {"parent": "organisations/acme", "activities": [UNICODE_ACTIVITY, {}], "request_id": "batch-1"}},
    {"name": "users", "message": "BatchUpsertUsersRequest", "fields": {"parent": "organisations/acme/workspaces/w1", "users": [{"name": "organisations/acme/workspaces/w1/users/u1", "display_name": "Ada", "email": "ada@example.com"}, {"name": "organisations/acme/workspaces/w1/users/u2"}]}},
    {"name": "decide request", "message": "DecideRequest", "fields": {"parent": "organisations/acme", "agent": "organisations/acme/agents/research", "user": "u1", "workspace": "organisations/acme/workspaces/w1", "project": "p", "requested_provider": "VERTEX_AI", "requested_model": "gemini-2.5-pro"}},
    {
        "name": "rate card page",
        "message": "ListPriceableUnitsResponse",
        "fields": {
            "priceable_units": [
                {"name": "priceableUnits/gemini-in", "display_name": "Gemini input", "provider": "VERTEX_AI", "model": "gemini-2.5-pro", "kind": 1, "unit_cost_nanos": 1250, "effective_from_ns": 1767225600000000000, "rate_card_version": "2026-01"},
                {"name": "priceableUnits/batch-long", "provider": "VERTEX_AI", "kind": 1, "unit_cost_nanos": 3000, "service_tier": "BATCH", "min_prompt_tokens": 200000, "min_cache_write_ttl_seconds": 3600, "modality": "AUDIO", "effective_from_ns": 1767225600000000000, "effective_to_ns": 1798761600500000000},
            ],
            "next_page_token": "page-2",
        },
    },
    {"name": "decide response", "message": "DecideResponse", "fields": {"decision": 3, "reason": "over budget", "policy_version": "v7", "replacement_provider": "VERTEX_AI", "replacement_model": "gemini-2.5-flash"}},
]


def compile_contracts(define: str, out: str) -> None:
    import grpc_tools
    from grpc_tools import protoc

    includes = [define, os.path.join(os.path.dirname(grpc_tools.__file__), "_proto")]
    import google.api  # noqa: F401  googleapis-common-protos, for the annotations the contracts import

    includes.append(str(pathlib.Path(sys.modules["google.api"].__path__[0]).parent.parent))
    args = ["protoc", *(f"-I{i}" for i in includes), f"--python_out={out}", *(os.path.join(define, c) for c in CONTRACTS)]
    if protoc.main(args) != 0:
        raise SystemExit("protoc failed")


def to_message(cls, fields: dict):
    message = cls()
    for name, value in fields.items():
        if name.endswith("_ns"):
            getattr(message, name[: -len("_ns")]).FromNanoseconds(value)
            continue
        target = getattr(message, name)
        if isinstance(value, list):
            for item in value:
                element = target.add() if hasattr(target, "add") else None
                if element is None:
                    target.append(item)
                else:
                    element.CopyFrom(to_message(type(element), item))
        else:
            setattr(message, name, value)
    return message


def main() -> None:
    define = os.environ.get("DEFINE") or str(pathlib.Path.home() / "alis.build/techbridge/define")
    with tempfile.TemporaryDirectory() as out:
        compile_contracts(define, out)
        sys.path.insert(0, out)
        from techbridge.ap.governance.v1 import decision_pb2
        from techbridge.ap.metering.v1 import activity_pb2, priceable_unit_pb2, user_pb2

        classes = {
            "BatchCreateActivitiesRequest": activity_pb2.BatchCreateActivitiesRequest,
            "BatchUpsertUsersRequest": user_pb2.BatchUpsertUsersRequest,
            "DecideRequest": decision_pb2.DecideRequest,
            "DecideResponse": decision_pb2.DecideResponse,
            "ListPriceableUnitsResponse": priceable_unit_pb2.ListPriceableUnitsResponse,
        }
        fixtures = []
        for case in CASES:
            message = to_message(classes[case["message"]], case["fields"])
            fixtures.append({**case, "hex": message.SerializeToString().hex()})

    target = pathlib.Path(__file__).resolve().parent.parent / "tests" / "wire_fixtures.json"
    target.write_text(json.dumps(fixtures, indent=2, ensure_ascii=False) + "\n")
    print(f"wrote {len(fixtures)} fixtures to {target}")


if __name__ == "__main__":
    main()
