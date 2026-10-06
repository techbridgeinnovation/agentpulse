import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { test } from "node:test";

import { scope } from "../src/context.ts";
import { RpcError } from "../src/gateway.ts";
import { Recorder } from "../src/recorder.ts";
import type { Sink } from "../src/sinks.ts";
import { VERSION } from "../src/version.ts";
import { type Activity, anyLosses, type RecorderLosses } from "../src/wire.ts";
import { MemorySink } from "./fakes.ts";

/** A sink that refuses with each code in turn, then accepts, remembering the request id of every attempt. */
class Flaky implements Sink {
  readonly name = "flaky";
  readonly ids: (string | undefined)[] = [];
  private readonly codes: number[];
  constructor(codes: number[]) {
    this.codes = codes;
  }
  async send(_activities: Activity[], _workspace: string, _timeoutMs: number, requestId?: string): Promise<void> {
    this.ids.push(requestId);
    const code = this.codes.shift();
    if (code !== undefined) throw new RpcError(code);
  }
}

test("a full queue drops the record and counts it, and never throws or waits", async () => {
  const sink = new MemorySink();
  const rec = new Recorder({ sinks: [sink], queueSize: 2, batchSize: 100, exitTimeoutMs: 0 });
  for (let i = 0; i < 5; i++) rec.record({ model: `m${i}` });
  assert.deepEqual([rec.stats().recorded, rec.stats().dropped], [2, 3]);
  assert.equal(await rec.flush(1000), true);
  assert.deepEqual(sink.activities.map((a) => a.model), ["m0", "m1"]);
  await rec.close();
});

test("a background flush that finds the queue empty does not stop later records being delivered", async () => {
  const sink = new MemorySink();
  const rec = new Recorder({ sinks: [sink], flushEveryMs: 20, exitTimeoutMs: 0 });
  rec.record({ model: "m0" });
  await new Promise((resolve) => setTimeout(resolve, 100));
  rec.record({ model: "m1" });
  await new Promise((resolve) => setTimeout(resolve, 100));
  assert.deepEqual(sink.activities.map((a) => a.model), ["m0", "m1"]);
  rec.record({ model: "m2" });
  assert.equal(await rec.flush(1000), true);
  assert.equal(rec.stats().delivered, 3);
  await rec.close();
});

test("records are delivered per workspace, the one in force on the scope", async () => {
  const sink = new MemorySink();
  const rec = new Recorder({ sinks: [sink], exitTimeoutMs: 0 });
  scope({ workspace: "acme" }, () => rec.record({ model: "a" }));
  rec.record({ model: "b" });
  await rec.flush(1000);
  assert.deepEqual(sink.batches.map(([w, batch]) => [w, batch.length]).sort(), [["", 1], ["acme", 1]]);
  await rec.close();
});

test("a sink that refuses is counted and the others still receive", async () => {
  const good = new MemorySink();
  const bad = new MemorySink();
  bad.fail = true;
  const rec = new Recorder({ sinks: [bad, good], exitTimeoutMs: 0 });
  rec.record({ model: "x" });
  await rec.flush(1000);
  assert.equal(good.activities.length, 1);
  assert.deepEqual([rec.stats().delivered, rec.stats().failed], [1, 1]);
  await rec.close();
});

test("a closed recorder drops what it is handed", async () => {
  const rec = new Recorder({ exitTimeoutMs: 0 });
  await rec.close();
  rec.record({ model: "late" });
  assert.equal(rec.stats().dropped, 1);
});

test("a person is named once, never with a slash in their identifier", async () => {
  const sink = new MemorySink();
  const rec = new Recorder({ sinks: [sink], exitTimeoutMs: 0 });
  rec.noteUser({ id: "u1", name: "Ada" });
  rec.noteUser({ id: "u1", name: "Ada" });
  rec.noteUser({ id: "users/u2", name: "Bo" });
  rec.noteUser({ id: "u3" });
  await rec.flush(1000);
  assert.deepEqual(sink.named.flatMap(([, users]) => users.map((u) => u.id)), ["u1"]);
  assert.deepEqual([rec.stats().named, rec.stats().namesFailed], [1, 1]);
  await rec.close();
});

test("a slow sink does not hold flush past its timeout", async () => {
  const slow = { name: "slow", send: () => new Promise<void>((r) => setTimeout(r, 1000)) };
  const rec = new Recorder({ sinks: [slow], exitTimeoutMs: 0 });
  rec.record({ model: "x" });
  const started = performance.now();
  assert.equal(await rec.flush(50), false);
  assert.ok(performance.now() - started < 500);
});

test("a batch refused for a reason that passes is sent again under the same request id", async () => {
  const sink = new Flaky([14, 4]);
  const rec = new Recorder({ sinks: [sink], exitTimeoutMs: 0 });
  rec.record({ model: "x" });
  assert.equal(await rec.flush(5000), true);
  assert.equal(sink.ids.length, 3);
  assert.ok(sink.ids[0] && sink.ids.every((id) => id === sink.ids[0]));
  assert.deepEqual([rec.stats().delivered, rec.stats().retried, rec.stats().failed], [1, 2, 0]);
  await rec.close();
});

test("a batch refused for a reason that does not pass is not sent again", async () => {
  const sink = new Flaky([16]);
  const rec = new Recorder({ sinks: [sink], exitTimeoutMs: 0 });
  rec.record({ model: "x" });
  await rec.flush(1000);
  assert.deepEqual([sink.ids.length, rec.stats().failed, rec.stats().retried], [1, 1, 0]);
  await rec.close();
});

test("retries are bounded, and what is still waiting at close is counted as dropped", async () => {
  const bounded = new Flaky([14, 14, 14, 14]);
  const rec = new Recorder({ sinks: [bounded], exitTimeoutMs: 0 });
  rec.record({ model: "x" });
  await rec.flush(5000);
  assert.deepEqual([bounded.ids.length, rec.stats().failed], [3, 1]);
  await rec.close();

  const waiting = new Recorder({ sinks: [new Flaky([14, 14, 14])], exitTimeoutMs: 0 });
  waiting.record({ model: "y" });
  await new Promise((r) => setTimeout(r, 20));
  assert.equal(await waiting.close(1), false);
  await new Promise((r) => setTimeout(r, 10));
  assert.deepEqual([waiting.stats().dropped, waiting.stats().failed], [1, 0]);
});

// A child process, so the signal reaches nothing but the recorder under test.
function child(code: string): Promise<{ signal: NodeJS.Signals | null; status: number | null; out: string }> {
  return new Promise((resolve) => {
    const p = spawn(process.execPath, ["--input-type=module", "-e", code], { cwd: new URL("..", import.meta.url), stdio: ["ignore", "pipe", "inherit"] });
    let out = "";
    p.stdout.on("data", (d: Buffer) => (out += d.toString()));
    p.on("exit", (status, signal) => resolve({ signal, status, out }));
  });
}

const RECORDING_CHILD = `
  import { Recorder } from "./src/recorder.ts";
  const sink = { name: "slow", send: async () => { await new Promise((r) => setTimeout(r, 100)); process.stdout.write("delivered;"); } };
  const rec = new Recorder({ sinks: [sink], exitTimeoutMs: 2000 });
  rec.record({ model: "x" });
`;

test("SIGTERM delivers what is queued, then ends the process as the signal would have", async () => {
  const { signal, out } = await child(RECORDING_CHILD + `setInterval(() => {}, 1000); process.kill(process.pid, "SIGTERM");`);
  assert.equal(out, "delivered;");
  assert.equal(signal, "SIGTERM");
});

test("a host with its own SIGINT handler keeps it, and the recorder flushes beside it", async () => {
  const { signal, status, out } = await child(
    RECORDING_CHILD + `process.on("SIGINT", () => setTimeout(() => { process.stdout.write("host;"); process.exit(7); }, 300)); setInterval(() => {}, 1000); process.kill(process.pid, "SIGINT");`,
  );
  assert.deepEqual([signal, status, out], [null, 7, "delivered;host;"]);
});

test("a host's own once handler, added before the recorder, still decides what SIGTERM does", async () => {
  const { signal, status, out } = await child(
    `process.once("SIGTERM", () => setTimeout(() => { process.stdout.write("host;"); process.exit(7); }, 300));` + RECORDING_CHILD + `setInterval(() => {}, 1000); process.kill(process.pid, "SIGTERM");`,
  );
  assert.deepEqual([signal, status, out], [null, 7, "delivered;host;"]);
});

test("closing removes the exit listeners, however many recorders there were", async () => {
  const before = [process.listenerCount("SIGTERM"), process.listenerCount("SIGINT"), process.listenerCount("beforeExit")];
  const a = new Recorder({ exitTimeoutMs: 100 });
  const b = new Recorder({ exitTimeoutMs: 100 });
  assert.ok(process.listenerCount("SIGTERM") <= before[0]! + 1);
  await a.close();
  await b.close();
  assert.deepEqual([process.listenerCount("SIGTERM"), process.listenerCount("SIGINT"), process.listenerCount("beforeExit")], before);
});

/** Remembers the losses each delivered batch carried, and refuses while told to. */
class LossCarrier implements Sink {
  readonly name = "losses";
  readonly carriesLosses = true;
  readonly carried: (RecorderLosses | undefined)[] = [];
  readonly attempts: (RecorderLosses | undefined)[] = [];
  codes: number[] = [];
  fail = false;
  async send(_activities: Activity[], _workspace: string, _timeoutMs: number, _requestId?: string, losses?: RecorderLosses): Promise<void> {
    this.attempts.push(losses);
    const code = this.codes.shift();
    if (code !== undefined) throw new RpcError(code);
    if (this.fail) throw new Error("refused");
    this.carried.push(anyLosses(losses) ? losses : undefined);
  }
}

const counts = (l: RecorderLosses | undefined): unknown[] => [l?.dropped, l?.undelivered, l?.panicked];

test("a dropped record is reported on the next batch delivered", async () => {
  const sink = new LossCarrier();
  const rec = new Recorder({ sinks: [sink], queueSize: 1, exitTimeoutMs: 0 });
  rec.record({ model: "a" });
  rec.record({ model: "dropped" });
  assert.equal(await rec.flush(1000), true);
  assert.deepEqual(counts(sink.carried[0]), [1, 0, 0]);
  assert.equal(sink.carried[0]?.recorder, `typescript/${VERSION}`);
  await rec.close();
});

test("losses survive a failed batch and are reported once", async () => {
  const sink = new LossCarrier();
  const rec = new Recorder({ sinks: [sink], queueSize: 1, exitTimeoutMs: 0 });
  rec.record({ model: "a" });
  rec.record({ model: "dropped" });
  sink.fail = true;
  await rec.flush(1000);
  assert.deepEqual(sink.carried, []);

  sink.fail = false;
  rec.note("panicked");
  rec.record({ model: "b" });
  await rec.flush(1000);
  // The failed batch's own record is undelivered, and the drop it carried comes forward.
  assert.deepEqual(counts(sink.carried[0]), [1, 1, 1]);

  rec.record({ model: "c" });
  await rec.flush(1000);
  assert.equal(sink.carried[1], undefined);
  const stats = rec.stats();
  assert.deepEqual([stats.dropped, stats.failed, stats.panicked], [1, 1, 1]);
  await rec.close();
});

test("a recorder that lost nothing reports no losses", async () => {
  const sink = new LossCarrier();
  const rec = new Recorder({ sinks: [sink], exitTimeoutMs: 0 });
  rec.record({ model: "a" });
  await rec.flush(1000);
  assert.deepEqual(sink.carried, [undefined]);
  await rec.close();
});

test("a retried batch carries the same losses, and batches sent at once never report one twice", async () => {
  const sink = new LossCarrier();
  sink.codes = [14];
  const rec = new Recorder({ sinks: [sink], exitTimeoutMs: 0 });
  rec.note("dropped", 3);
  scope({ workspace: "w1" }, () => rec.record({ model: "a" }));
  scope({ workspace: "w2" }, () => rec.record({ model: "b" }));
  await rec.flush(5000);
  const reported = sink.carried.filter((l) => l !== undefined).map((l) => l.dropped);
  assert.deepEqual(reported, [3]);
  const withDrops = sink.attempts.filter((l) => anyLosses(l));
  assert.ok(withDrops.length >= 1 && withDrops.every((l) => l?.dropped === 3));
  await rec.close();
});
