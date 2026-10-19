// The Vercel AI SDK and its providers, where they are installed.
//
// A run that sets AP_AI_SDK_REQUIRED to a major, as CI does, fails here unless that major and its providers are what is installed, so a run cannot pass by skipping every test against the real SDK.

import { createRequire } from "node:module";

export const sdk = await import("ai").catch(() => undefined);
export const openaiSdk = await import("@ai-sdk/openai").catch(() => undefined);
export const anthropicSdk = await import("@ai-sdk/anthropic").catch(() => undefined);
export const googleSdk = await import("@ai-sdk/google").catch(() => undefined);

function installed(name: string): string | undefined {
  try {
    return String(createRequire(import.meta.url)(`${name}/package.json`).version);
  } catch {
    return undefined;
  }
}

/** The installed `ai`, e.g. "7.0.143". */
export const version = installed("ai");

const required = process.env.AP_AI_SDK_REQUIRED?.trim();
if (required) {
  if (version?.split(".")[0] !== required) throw new Error(`AP_AI_SDK_REQUIRED is ${required} and the AI SDK installed is ${version ?? "none"}`);
  const missing = ["@ai-sdk/openai", "@ai-sdk/anthropic", "@ai-sdk/google"].filter((name) => !installed(name));
  if (!sdk || !openaiSdk || !anthropicSdk || !googleSdk || missing.length) throw new Error(`AP_AI_SDK_REQUIRED is ${required} and these are not installed: ${missing.join(", ") || "ai"}`);
}

export const skip = !sdk || !openaiSdk ? "the Vercel AI SDK is not installed" : false;

/** Telemetry integrations, `registerTelemetry` among them, arrived in AI SDK 7. */
export const telemetrySkip = skip || (!("registerTelemetry" in sdk!) && `telemetry integrations need AI SDK 7, and ${version} is installed`);
