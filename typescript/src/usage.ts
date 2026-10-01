// What a provider said a call used, under the provider's own names.
//
// Nothing is added, subtracted or renamed here. Splitting the counts into priced classes happens once, on the server, where a mistake is corrected by one deploy and reapplied to records already written.

import type { ReportedQuantity } from "./wire.ts";

// The conventions a set of counts can arrive in. Which one decides how the numbers are read, and it is not the same question as who billed.
export const FORMAT_VERTEX = "VERTEX";
export const FORMAT_ANTHROPIC = "ANTHROPIC";
export const FORMAT_OPENAI_CHAT = "OPENAI_CHAT";
export const FORMAT_OPENAI_RESPONSES = "OPENAI_RESPONSES";
export const FORMAT_PERPLEXITY = "PERPLEXITY";
/** Every model's usage as the Vercel AI SDK restates it, for a provider whose own counts the SDK does not pass on. */
export const FORMAT_AI_SDK = "AI_SDK";

// Who bills for a call, in the vocabulary the rate card prices against. A plain string on the wire, so a provider nobody has called before needs no release of this library.
export const PROVIDER_VERTEX_AI = "VERTEX_AI";
export const PROVIDER_ANTHROPIC = "ANTHROPIC";
export const PROVIDER_OPENAI = "OPENAI";
export const PROVIDER_PERPLEXITY = "PERPLEXITY";

function isWhole(value: unknown): value is number {
  return typeof value === "number" && Number.isInteger(value);
}

/**
 * Every whole-number count on a usage object, each under the provider's own name, with a nested count named by its path, e.g. `prompt_tokens_details.cached_tokens`. Text, flags, lists and zeros are left out.
 */
export function reportedFrom(usage: unknown): Record<string, number> {
  const out: Record<string, number> = {};
  const walk = (value: unknown, prefix: string, depth: number): void => {
    if (value === null || typeof value !== "object" || Array.isArray(value) || depth > 4) return;
    for (const [name, v] of Object.entries(value as Record<string, unknown>)) {
      if (isWhole(v)) {
        if (v !== 0) out[prefix + name] = v;
      } else if (v !== null && typeof v === "object" && !Array.isArray(v)) {
        walk(v, `${prefix}${name}.`, depth + 1);
      }
    }
  };
  walk(usage, "", 0);
  return out;
}

/** What a record carries for a set of counts: sorted by name, with zeros and blank names left out, so two recordings of the same call compare equal. */
export function reportedQuantities(reported: Record<string, number> | undefined): ReportedQuantity[] {
  if (!reported) return [];
  return Object.keys(reported)
    .sort()
    .filter((unit) => unit.trim() && isWhole(reported[unit]) && reported[unit] !== 0)
    .map((unit) => ({ unit, quantity: reported[unit]! }));
}

/** How large a tool's result is, as the JSON it reaches the model in, and whether it was empty. A result that cannot be serialised is unmeasured, which is not empty: zero bytes would claim the tool returned nothing when it may have returned plenty. */
export function resultSize(result: unknown): { bytes: number; empty: boolean } {
  if (result === undefined || result === null) return { bytes: 0, empty: true };
  let json: string | undefined;
  try {
    json = JSON.stringify(result);
  } catch {
    return { bytes: 0, empty: false };
  }
  if (json === undefined) return { bytes: 0, empty: false };
  if (json === "{}" || json === "[]" || json === '""' || json === "null") return { bytes: 0, empty: true };
  return { bytes: Buffer.byteLength(json, "utf8"), empty: false };
}
