import assert from "node:assert/strict";
import { test } from "node:test";

import { scope } from "../src/context.ts";
import { Recorder } from "../src/recorder.ts";
import { MemorySink } from "./fakes.ts";

test("a full queue drops the record and counts it, and never throws or waits", async () => {
  const sink = new MemorySink();
  const rec = new Recorder({ sinks: [sink], queueSize: 2, batchSize: 100, exitTimeoutMs: 0 });
  for (let i = 0; i < 5; i++) rec.record({ model: `m${i}` });
  assert.deepEqual([rec.stats().recorded, rec.stats().dropped], [2, 3]);
  assert.equal(await rec.flush(1000), true);
  assert.deepEqual(sink.activities.map((a) => a.model), ["m0", "m1"]);
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
