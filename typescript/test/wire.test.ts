// The hand-written encoder against the official protobuf runtime, byte for byte, through the fixtures the Python recorder's script wrote from the contract.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

import {
  type Activity,
  batchCreateActivities,
  batchUpsertUsers,
  DECISION_DENY,
  DecodeError,
  decodeDecideResponse,
  encodeDecideRequest,
  LEVEL_EXHAUSTED,
  LEVEL_PASSED_75_PERCENT,
  LEVEL_PASSED_95_PERCENT,
  LEVEL_WITHIN,
  SCOPE_PERSONAL,
  SCOPE_RESERVE,
  SCOPE_WORKSPACE,
} from "../src/wire.ts";

interface Fixture {
  name: string;
  message: string;
  fields: Record<string, any>;
  hex: string;
}

// Integers past 2^53 are read from the JSON's own text, since a JavaScript number would round them.
const exact = (_key: string, value: unknown, context?: { source?: string }): unknown =>
  typeof value === "number" && !Number.isSafeInteger(value) && context?.source && /^-?\d+$/.test(context.source) ? BigInt(context.source) : value;
const FIXTURES: Fixture[] = JSON.parse(readFileSync(new URL("../../python/tests/wire_fixtures.json", import.meta.url), "utf8"), exact as any);

const camel = (name: string): string => name.replace(/_([a-z])/g, (_, c: string) => c.toUpperCase());

function activity(fields: Record<string, any>): Activity {
  const out: Record<string, unknown> = {};
  for (const [name, value] of Object.entries(fields)) {
    if (name === "occurred_at_ns") out.occurredAtNs = BigInt(value);
    else if (name === "charges") out.charges = value.map((c: any) => ({ priceableUnit: c.priceable_unit, quantity: c.quantity }));
    else out[camel(name)] = value;
  }
  return out as Activity;
}

const hex = (bytes: Uint8Array): string => Buffer.from(bytes).toString("hex");

for (const fixture of FIXTURES) {
  const f = fixture.fields;
  if (fixture.message === "BatchCreateActivitiesRequest") {
    test(`encodes ${fixture.name} exactly as the official runtime`, () => {
      const losses = f.recorder_losses && Object.fromEntries(Object.entries(f.recorder_losses).map(([k, v]) => [camel(k), v]));
      assert.equal(hex(batchCreateActivities(f.parent, f.activities.map(activity), f.request_id, losses)), fixture.hex);
    });
  } else if (fixture.message === "BatchUpsertUsersRequest") {
    test(`encodes ${fixture.name} exactly as the official runtime`, () => {
      assert.equal(hex(batchUpsertUsers(f.parent, f.users.map((u: any) => ({ name: u.name, displayName: u.display_name, email: u.email })))), fixture.hex);
    });
  } else if (fixture.message === "DecideRequest") {
    test(`encodes ${fixture.name} exactly as the official runtime`, () => {
      const request = Object.fromEntries(Object.entries(f).map(([k, v]) => [camel(k), v]));
      assert.equal(hex(encodeDecideRequest(request as any)), fixture.hex);
    });
  } else if (fixture.message === "DecideResponse") {
    test(`decodes ${fixture.name} the official runtime wrote`, () => {
      const got = decodeDecideResponse(Buffer.from(fixture.hex, "hex"));
      const budgetStatuses = (f.budget_statuses ?? []).map((s: any) => ({
        budget: s.budget ?? "",
        displayName: s.display_name ?? "",
        scope: s.scope ?? 0,
        workspace: s.workspace ?? "",
        spentMicros: s.spent_micros ?? 0,
        limitMicros: s.limit_micros ?? 0,
        periodEndTimeNs: BigInt(s.period_end_time_ns ?? 0),
        warnOnly: s.warn_only ?? false,
        level: s.level ?? 0,
      }));
      assert.deepEqual(got, {
        decision: f.decision,
        reason: f.reason,
        policyVersion: f.policy_version,
        replacementProvider: f.replacement_provider ?? "",
        replacementModel: f.replacement_model ?? "",
        budgetStatuses,
        evaluateTimeNs: BigInt(f.evaluate_time_ns ?? 0),
      });
    });
  }
}

test("every fixture that is not a rate card page is exercised", () => {
  assert.ok(FIXTURES.filter((f) => f.message !== "ListPriceableUnitsResponse").length >= 5);
});

test("an unknown field in a reply is skipped", () => {
  const data = Buffer.concat([Buffer.from("0804", "hex"), Buffer.from([0x7a, 3, 97, 98, 99]), Buffer.from([0xa0, 0x06, 0x01])]);
  assert.equal(decodeDecideResponse(data).decision, DECISION_DENY);
});

test("budget statuses and the evaluate time are read from the official runtime's bytes", () => {
  const fixture = FIXTURES.find((f) => f.name === "decide response with budget statuses")!;
  const got = decodeDecideResponse(Buffer.from(fixture.hex, "hex"));
  assert.deepEqual(got.budgetStatuses?.map((s) => [s.scope, s.level]), [[SCOPE_WORKSPACE, LEVEL_PASSED_95_PERCENT], [SCOPE_RESERVE, LEVEL_EXHAUSTED], [SCOPE_PERSONAL, LEVEL_WITHIN], [9, 7]]);
  assert.equal(got.budgetStatuses?.[1]?.limitMicros, 0);
  assert.equal(got.evaluateTimeNs, 1791892800_123456789n);
});

test("a reply from a governance without statuses has none", () => {
  const fixture = FIXTURES.find((f) => f.name === "decide response")!;
  const got = decodeDecideResponse(Buffer.from(fixture.hex, "hex"));
  assert.deepEqual([got.budgetStatuses, got.evaluateTimeNs], [[], 0n]);
});

test("an unknown field inside a budget status is skipped and a negative int64 is read as signed", () => {
  // Field 5, spent_micros, as -1 in ten bytes, then an unknown field 15, inside status field 6.
  const status = Buffer.from([0x28, ...new Array(9).fill(0xff), 0x01, 0x7a, 0x01, 0x78, 0x48, 0x02]);
  const [read] = decodeDecideResponse(Buffer.concat([Buffer.from([0x32, status.length]), status])).budgetStatuses ?? [];
  assert.deepEqual([read?.spentMicros, read?.level], [-1, LEVEL_PASSED_75_PERCENT]);
});

test("a truncated reply is refused rather than misread", () => {
  assert.throws(() => decodeDecideResponse(Buffer.from([0x12, 0x05, 97, 98])), DecodeError);
});

test("losses that are all zero are left off the batch", () => {
  assert.deepEqual(batchCreateActivities("organisations/acme", [], "", { recorder: "typescript/0.1.0" }), batchCreateActivities("organisations/acme", []));
});
