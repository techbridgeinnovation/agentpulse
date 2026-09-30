// The protobuf encoding of the few messages the recorder sends, and the reading of the few it receives.
//
// Written by hand rather than generated, because generated code needs a protobuf runtime, and a library running inside somebody else's agent does not get to choose which version of one that agent loads. What is here is small, fixed by the contract's field numbers, and checked byte for byte against the official encoder by the fixtures the Python recorder shares.
//
// Only proto3 scalar rules are needed: a field holding its zero value is left out, a repeated field is one entry per element, and fields are written in field-number order.

const VARINT = 0;
const FIXED64 = 1;
const LENGTH = 2;
const FIXED32 = 5;

// Agent.Kind: how the recorder observed a call.
export const KIND_AGENT = 1;
export const KIND_SERVICE = 2;

// Activity.Status, as the contract numbers it.
export const STATUS_OK = 1;
export const STATUS_FAILED = 2;
export const STATUS_DENIED = 3;
export const STATUS_TRUNCATED = 4;

// DecideResponse.Decision, as the contract numbers it.
export const DECISION_UNSPECIFIED = 0;
export const DECISION_ALLOW = 1;
export const DECISION_NOTIFY = 2;
export const DECISION_DOWNGRADE = 3;
export const DECISION_DENY = 4;

const encoder = new TextEncoder();
const decoder = new TextDecoder();

function varint(value: bigint): number[] {
  // A negative int32 or int64 is written as its 64-bit two's complement, ten bytes long.
  let v = value < 0n ? value + (1n << 64n) : value;
  const out: number[] = [];
  for (;;) {
    const byte = Number(v & 0x7fn);
    v >>= 7n;
    if (v > 0n) {
      out.push(byte | 0x80);
    } else {
      out.push(byte);
      return out;
    }
  }
}

function key(field: number, wireType: number): number[] {
  return varint(BigInt((field << 3) | wireType));
}

function string(field: number, value: string | undefined): number[] {
  if (!value) return [];
  const data = encoder.encode(value);
  return [...key(field, LENGTH), ...varint(BigInt(data.length)), ...data];
}

function integer(field: number, value: number | bigint | undefined): number[] {
  if (!value) return [];
  return [...key(field, VARINT), ...varint(BigInt(value))];
}

function boolean(field: number, value: boolean | undefined): number[] {
  if (!value) return [];
  return [...key(field, VARINT), 1];
}

function message(field: number, data: number[]): number[] {
  return [...key(field, LENGTH), ...varint(BigInt(data.length)), ...data];
}

function timestamp(field: number, nanoseconds: bigint | undefined): number[] {
  if (!nanoseconds) return [];
  const seconds = nanoseconds / 1_000_000_000n;
  const nanos = nanoseconds % 1_000_000_000n;
  return message(field, [...integer(1, seconds), ...integer(2, nanos)]);
}

/** A cost on a call that tokens do not describe, such as one web search. */
export interface Charge {
  /** The rate card entry this is charged against. Format: priceableUnits/{id} */
  priceableUnit: string;
  quantity: number;
}

/** One count a provider reported, under the provider's own name for it. */
export interface ReportedQuantity {
  unit: string;
  quantity: number;
}

/** One thing a provider said about a failure, under the provider's own name for it. */
export interface ReportedField {
  name: string;
  value: string;
}

/**
 * One model call or tool call, as metering stores it.
 *
 * The field names are the contract's. The fields metering writes itself, such as the estimated and billed costs, are absent, because a recorder never states what its own work cost.
 */
export interface Activity {
  agent?: string;
  request?: string;
  session?: string;
  user?: string;
  callerService?: string;
  callerComponent?: string;
  skill?: string;
  model?: string;
  promptTokens?: number;
  candidateTokens?: number;
  cachedTokens?: number;
  cacheWriteTokens?: number;
  reasoningTokens?: number;
  totalTokens?: number;
  charges?: Charge[];
  durationMs?: number;
  status?: number;
  errorCode?: string;
  /** When the work happened, in nanoseconds since the Unix epoch. Absent leaves it for the server to fill in. */
  occurredAtNs?: bigint;
  project?: string;
  tool?: string;
  errorDetail?: string;
  attempt?: number;
  retryOf?: string;
  resultBytes?: number;
  resultTokens?: number;
  emptyResult?: boolean;
  argsBytes?: number;
  argsFingerprint?: string;
  billedBy?: string;
  usageFormat?: string;
  reportedUsage?: ReportedQuantity[];
  serviceTier?: string;
  cacheWriteTtlSeconds?: number;
  region?: string;
  providerCostMicros?: number;
  errorFormat?: string;
  reportedError?: ReportedField[];
  subAgent?: string;
  observedAs?: number;
  framework?: string;
  frameworkVersion?: string;
}

export function encodeActivity(a: Activity): number[] {
  return [
    ...string(2, a.agent),
    ...string(3, a.request),
    ...string(4, a.session),
    ...string(5, a.user),
    ...string(6, a.callerService),
    ...string(7, a.callerComponent),
    ...string(8, a.skill),
    ...string(9, a.model),
    ...integer(11, a.promptTokens),
    ...integer(12, a.candidateTokens),
    ...integer(13, a.cachedTokens),
    ...integer(14, a.cacheWriteTokens),
    ...integer(15, a.reasoningTokens),
    ...integer(16, a.totalTokens),
    ...(a.charges ?? []).flatMap((c) => message(20, [...string(1, c.priceableUnit), ...integer(2, c.quantity)])),
    ...integer(21, a.durationMs),
    ...integer(22, a.status),
    ...string(23, a.errorCode),
    ...timestamp(24, a.occurredAtNs),
    ...string(25, a.project),
    ...string(26, a.tool),
    ...string(28, a.errorDetail),
    ...integer(29, a.attempt),
    ...string(30, a.retryOf),
    ...integer(31, a.resultBytes),
    ...integer(32, a.resultTokens),
    ...boolean(33, a.emptyResult),
    ...integer(34, a.argsBytes),
    ...string(35, a.argsFingerprint),
    ...string(36, a.billedBy),
    ...string(37, a.usageFormat),
    ...(a.reportedUsage ?? []).flatMap((q) => message(38, [...string(1, q.unit), ...integer(2, q.quantity)])),
    ...string(39, a.serviceTier),
    ...integer(40, a.cacheWriteTtlSeconds),
    ...integer(41, a.providerCostMicros),
    ...string(42, a.errorFormat),
    ...(a.reportedError ?? []).flatMap((f) => message(43, [...string(1, f.name), ...string(2, f.value)])),
    ...integer(45, a.observedAs),
    ...string(46, a.subAgent),
    ...string(48, a.region),
    ...string(49, a.framework),
    ...string(50, a.frameworkVersion),
  ];
}

/** A BatchCreateActivitiesRequest, encoded. */
export function batchCreateActivities(parent: string, activities: Activity[], requestId = ""): Uint8Array {
  return Uint8Array.from([...string(1, parent), ...activities.flatMap((a) => message(2, encodeActivity(a))), ...string(3, requestId)]);
}

/** A directory row: the person behind an identifier. */
export interface UserRow {
  /** Format: organisations/{organisation}/workspaces/{workspace}/users/{user}, or organisations/{organisation}/users/{user} */
  name: string;
  displayName?: string;
  email?: string;
}

/** A BatchUpsertUsersRequest, encoded. */
export function batchUpsertUsers(parent: string, users: UserRow[]): Uint8Array {
  return Uint8Array.from([
    ...string(1, parent),
    ...users.flatMap((u) => message(2, [...string(1, u.name), ...string(2, u.displayName), ...string(3, u.email)])),
  ]);
}

export interface DecideRequest {
  parent: string;
  agent: string;
  user?: string;
  workspace?: string;
  project?: string;
  requestedProvider?: string;
  requestedModel?: string;
}

export function encodeDecideRequest(r: DecideRequest): Uint8Array {
  return Uint8Array.from([
    ...string(1, r.parent),
    ...string(2, r.agent),
    ...string(4, r.user),
    ...string(5, r.workspace),
    ...string(6, r.project),
    ...string(7, r.requestedProvider),
    ...string(8, r.requestedModel),
  ]);
}

export interface DecideResponse {
  decision: number;
  reason: string;
  policyVersion: string;
  replacementProvider: string;
  replacementModel: string;
}

export function decodeDecideResponse(data: Uint8Array): DecideResponse {
  const out: DecideResponse = { decision: DECISION_UNSPECIFIED, reason: "", policyVersion: "", replacementProvider: "", replacementModel: "" };
  for (const [field, value] of fields(data)) {
    if (field === 1 && typeof value === "bigint") out.decision = Number(value);
    else if (typeof value !== "bigint") {
      const text = decoder.decode(value);
      if (field === 2) out.reason = text;
      else if (field === 3) out.policyVersion = text;
      else if (field === 4) out.replacementProvider = text;
      else if (field === 5) out.replacementModel = text;
    }
  }
  return out;
}

/** A message that is not valid protobuf. */
export class DecodeError extends Error {}

function readVarint(data: Uint8Array, at: number): [bigint, number] {
  let value = 0n;
  let shift = 0n;
  for (;;) {
    if (at >= data.length || shift > 63n) throw new DecodeError("truncated varint");
    const byte = data[at++]!;
    value |= BigInt(byte & 0x7f) << shift;
    if (!(byte & 0x80)) return [value, at];
    shift += 7n;
  }
}

/**
 * Every field in an encoded message, as its number and its raw value: a bigint for a varint or a fixed-width field, bytes for a length-delimited one.
 *
 * An unknown field is yielded like any other and the caller skips it, which keeps a reply from a newer server readable by an older recorder.
 */
export function* fields(data: Uint8Array): Generator<[number, bigint | Uint8Array]> {
  let at = 0;
  while (at < data.length) {
    const [k, next] = readVarint(data, at);
    at = next;
    const field = Number(k >> 3n);
    const wireType = Number(k & 7n);
    if (wireType === VARINT) {
      const [value, after] = readVarint(data, at);
      at = after;
      yield [field, value];
    } else if (wireType === LENGTH) {
      const [length, after] = readVarint(data, at);
      at = after;
      const end = at + Number(length);
      if (end > data.length) throw new DecodeError("truncated field");
      yield [field, data.subarray(at, end)];
      at = end;
    } else if (wireType === FIXED64) {
      if (at + 8 > data.length) throw new DecodeError("truncated field");
      yield [field, new DataView(data.buffer, data.byteOffset + at, 8).getBigUint64(0, true)];
      at += 8;
    } else if (wireType === FIXED32) {
      if (at + 4 > data.length) throw new DecodeError("truncated field");
      yield [field, BigInt(new DataView(data.buffer, data.byteOffset + at, 4).getUint32(0, true))];
      at += 4;
    } else {
      throw new DecodeError(`unsupported wire type ${wireType}`);
    }
  }
}
