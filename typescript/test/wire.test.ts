// The hand-written encoder against the official protobuf runtime, byte for byte, through the fixtures the Python recorder's script wrote from the contract.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

import { type Activity, batchCreateActivities, batchUpsertUsers, DECISION_DENY, DecodeError, decodeDecideResponse, encodeDecideRequest } from "../src/wire.ts";

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
      assert.equal(hex(batchCreateActivities(f.parent, f.activities.map(activity), f.request_id)), fixture.hex);
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
      assert.deepEqual(got, { decision: f.decision, reason: f.reason, policyVersion: f.policy_version, replacementProvider: f.replacement_provider, replacementModel: f.replacement_model });
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

test("a truncated reply is refused rather than misread", () => {
  assert.throws(() => decodeDecideResponse(Buffer.from([0x12, 0x05, 97, 98])), DecodeError);
});
