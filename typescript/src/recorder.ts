// The queue, the flush, the counters.
//
// The first rule of this library is that it must never degrade its host. Recording never blocks, never throws and never waits on the network: a record goes into a bounded buffer and a background flush delivers it. A full buffer drops the record and counts it, because losing a record is always preferable to delaying the work the agent was asked to do.
//
// Dropped records are counted and the count is readable in `stats()`. That counter is the one thing here that cannot be turned off, because silent loss is worse than visible loss.
//
// The flush timer is unreferenced, so a recorder never keeps a process alive on its own.
//
// A batch the gateway turned away for a reason that passes, such as being unavailable or out of time, is sent again a few times with a growing wait, under the same request id, so the server can tell a retry from new records. What is still waiting when the recorder closes is counted as dropped.

import { randomUUID } from "node:crypto";

import { currentScope, type User } from "./context.ts";
import { RpcError } from "./gateway.ts";
import { Discard, type Sink } from "./sinks.ts";
import type { Activity } from "./wire.ts";

const DEFAULT_QUEUE_SIZE = 2048;
const DEFAULT_BATCH_SIZE = 100;
const DEFAULT_FLUSH_EVERY_MS = 2000;
const DEFAULT_SEND_TIMEOUT_MS = 10_000;
const DEFAULT_EXIT_TIMEOUT_MS = 2000;
const MAX_REMEMBERED_NAMES = 10_000;
const MAX_ATTEMPTS = 3;
const FIRST_RETRY_MS = 250;
const MAX_RETRY_MS = 2000;

// The gRPC codes that say the same call may succeed if sent again. The gateway reports an HTTP 429, 502, 503 or 504 and an unreachable host as unavailable.
const TRANSIENT = new Set([4, 8, 10, 14]);

function transient(err: unknown): boolean {
  return err instanceof RpcError && TRANSIENT.has(err.code);
}

export interface Config {
  /** Each sink receives every record, independently, so one failing does not affect the others. Empty means discard. */
  sinks?: Sink[];
  /** How many records may wait for delivery. Once full, new records are dropped and counted. */
  queueSize?: number;
  /** The most records handed to a sink at once. */
  batchSize?: number;
  /** How long a record may wait before delivery, so a quiet agent still reports. */
  flushEveryMs?: number;
  /** The longest one delivery may take, so a hung destination cannot stall the flush for ever. */
  sendTimeoutMs?: number;
  /** How long the process waits, on SIGTERM, SIGINT or the event loop emptying, for what is still queued. Zero leaves the flush at exit to the host. Neither `process.exit` nor a serverless platform freezing the process gives it a chance: there, await `close()` before returning. */
  exitTimeoutMs?: number;
}

/** What the recorder has done, as a snapshot. */
export interface Stats {
  recorded: number;
  /** Records discarded because the queue was full or the recorder was closed. Above zero means cost data is incomplete. */
  dropped: number;
  delivered: number;
  failed: number;
  /** Batches sent again after a failure that passes. */
  retried: number;
  /** Times something run on the host's behalf threw where it never should. Above zero means this library has a bug; the agent was unaffected. */
  panicked: number;
  notified: number;
  denied: number;
  downgraded: number;
  decisionErrors: number;
  downgradeApplied: number;
  downgradeNotApplied: number;
  named: number;
  namesDropped: number;
  namesFailed: number;
}

export type Counter = keyof Stats;

function emptyStats(): Stats {
  return {
    recorded: 0,
    dropped: 0,
    delivered: 0,
    failed: 0,
    retried: 0,
    panicked: 0,
    notified: 0,
    denied: 0,
    downgraded: 0,
    decisionErrors: 0,
    downgradeApplied: 0,
    downgradeNotApplied: 0,
    named: 0,
    namesDropped: 0,
    namesFailed: 0,
  };
}

function positive(value: number | undefined, fallback: number): number {
  return value !== undefined && value > 0 ? value : fallback;
}

/** Accepts records from an agent and delivers them in the background. */
export class Recorder {
  private readonly sinks: Sink[];
  private readonly queueSize: number;
  private readonly batchSize: number;
  private readonly flushEveryMs: number;
  private readonly sendTimeoutMs: number;
  private readonly counters = emptyStats();
  private records: [Activity, string][] = [];
  private people: [User, string][] = [];
  private seen = new Map<string, string>();
  private timer: ReturnType<typeof setInterval> | undefined;
  private draining: Promise<void> | undefined;
  private closed = false;
  private readonly waiting = new Set<{ timer: ReturnType<typeof setTimeout>; wake: () => void }>();
  private flushing = 0;
  private atExit: (() => void) | undefined;

  constructor(config: Config = {}) {
    this.sinks = config.sinks?.length ? [...config.sinks] : [new Discard()];
    this.queueSize = positive(config.queueSize, DEFAULT_QUEUE_SIZE);
    this.batchSize = positive(config.batchSize, DEFAULT_BATCH_SIZE);
    this.flushEveryMs = positive(config.flushEveryMs, DEFAULT_FLUSH_EVERY_MS);
    this.sendTimeoutMs = positive(config.sendTimeoutMs, DEFAULT_SEND_TIMEOUT_MS);
    const exitTimeoutMs = config.exitTimeoutMs ?? DEFAULT_EXIT_TIMEOUT_MS;
    // Where there is no Node process, such as a browser or an edge runtime, there is no exit to flush at.
    if (exitTimeoutMs > 0 && typeof process !== "undefined" && typeof process.on === "function") {
      // beforeExit fires when the event loop has emptied, so a last flush can still send; it does not fire on process.exit, which a host owns.
      this.atExit = () => void this.flush(exitTimeoutMs);
      process.once("beforeExit", this.atExit);
      flushOnSignal(this, exitTimeoutMs);
    }
  }

  /** Hands over one record and returns immediately. Never blocks, never throws. Filed under the workspace in force on the current context unless the caller states one. */
  record(activity: Activity, workspace?: string): void {
    try {
      if (this.closed || this.records.length >= this.queueSize) {
        this.counters.dropped++;
        return;
      }
      this.records.push([activity, workspace ?? currentScope().workspace ?? ""]);
      this.counters.recorded++;
      this.start();
      if (this.records.length >= this.batchSize) void this.drain();
    } catch {
      this.note("panicked");
    }
  }

  /** Hands over a person to name and returns immediately. Someone already named with the same name costs a map lookup. */
  noteUser(user: User | undefined, workspace?: string): void {
    try {
      if (!user?.id || !(user.name || user.email)) return;
      if (user.id.includes("/")) {
        this.note("namesFailed");
        return;
      }
      const ws = workspace ?? currentScope().workspace ?? "";
      const key = `${ws}\u0000${user.id}`;
      const said = `${user.name ?? ""}\u0000${user.email ?? ""}`;
      if (this.seen.get(key) === said) return;
      if (this.closed || this.people.length >= this.queueSize) {
        this.counters.namesDropped++;
        return;
      }
      this.people.push([user, ws]);
      if (this.seen.size >= MAX_REMEMBERED_NAMES) this.seen.clear();
      this.seen.set(key, said);
      this.counters.named++;
      this.start();
    } catch {
      this.note("panicked");
    }
  }

  stats(): Stats {
    return { ...this.counters };
  }

  /** @internal */
  note(counter: Counter, n = 1): void {
    this.counters[counter] += n;
  }

  /** Delivers everything queued so far and resolves whether that finished within `timeoutMs`. For a process that may be frozen as soon as it answers, such as a serverless function: await it before returning. */
  async flush(timeoutMs = 5000): Promise<boolean> {
    this.flushing++;
    // A retry's wait holds the process open while someone waits on a flush, and never otherwise.
    for (const w of this.waiting) w.timer.ref?.();
    try {
      if (this.records.length === 0 && this.people.length === 0 && !this.draining) return true;
      let timer: ReturnType<typeof setTimeout> | undefined;
      const late = new Promise<false>((resolve) => {
        timer = setTimeout(() => resolve(false), Math.max(0, timeoutMs));
        timer.unref?.();
      });
      const done = (async () => {
        while (this.records.length > 0 || this.people.length > 0 || this.draining) await this.drain();
        return true as const;
      })();
      const finished = await Promise.race([done, late]);
      clearTimeout(timer);
      return finished;
    } catch {
      this.note("panicked");
      return false;
    } finally {
      if (--this.flushing === 0) for (const w of this.waiting) w.timer.unref?.();
    }
  }

  /** Stops the recorder after a final attempt to deliver what is queued, within `timeoutMs`. What is still queued or waiting to be retried then, and every record handed over afterwards, is dropped and counted. For a process that may be frozen as soon as it answers, such as a serverless function, await it, or `flush`, before returning. */
  async close(timeoutMs = 5000): Promise<boolean> {
    const flushed = await this.flush(timeoutMs);
    this.closed = true;
    if (this.timer) clearInterval(this.timer);
    this.timer = undefined;
    forgetOnSignal(this);
    if (this.atExit) process.removeListener("beforeExit", this.atExit);
    this.atExit = undefined;
    for (const w of this.waiting) w.wake();
    this.note("dropped", this.records.length);
    this.note("namesDropped", this.people.length);
    this.records = [];
    this.people = [];
    return flushed;
  }

  private start(): void {
    if (this.timer || this.closed) return;
    this.timer = setInterval(() => void this.drain(), this.flushEveryMs);
    this.timer.unref?.();
  }

  /** Delivers everything queued, a batch at a time. One drain runs at once; a second call waits on the first. */
  private drain(): Promise<void> {
    if (!this.draining) {
      this.draining = (async () => {
        try {
          for (;;) {
            const records = this.records.splice(0, this.batchSize);
            const people = this.people.splice(0, this.batchSize);
            if (records.length === 0 && people.length === 0) return;
            await Promise.all([this.deliver(records), this.deliverNames(people)]);
          }
        } catch {
          this.note("panicked");
        } finally {
          this.draining = undefined;
        }
      })();
    }
    return this.draining;
  }

  private async deliver(batch: [Activity, string][]): Promise<void> {
    const jobs: Promise<void>[] = [];
    for (const [workspace, activities] of byWorkspace(batch)) {
      for (const sink of this.sinks) {
        const requestId = randomUUID();
        jobs.push(
          this.attempt(() => sink.send(activities, workspace, this.sendTimeoutMs, requestId)).then((outcome) =>
            this.note(outcome === "sent" ? "delivered" : outcome, activities.length),
          ),
        );
      }
    }
    await Promise.all(jobs);
  }

  private async deliverNames(batch: [User, string][]): Promise<void> {
    const jobs: Promise<void>[] = [];
    for (const [workspace, users] of byWorkspace(batch)) {
      for (const sink of this.sinks) {
        const sendUsers = sink.sendUsers?.bind(sink);
        if (!sendUsers) continue;
        jobs.push(
          this.attempt(() => sendUsers(users, workspace, this.sendTimeoutMs)).then((outcome) => {
            if (outcome !== "sent") this.note(outcome === "failed" ? "namesFailed" : "namesDropped", users.length);
          }),
        );
      }
    }
    await Promise.all(jobs);
  }

  /** One delivery, sent again after a failure that passes, until it is sent, refused for good, or the recorder closes while it waits. Never rejects. */
  private async attempt(send: () => Promise<void>): Promise<"sent" | "failed" | "dropped"> {
    for (let n = 1; ; n++) {
      try {
        await send();
        return "sent";
      } catch (err) {
        if (!transient(err) || n >= MAX_ATTEMPTS) return "failed";
      }
      // Jittered, so many processes refused by one outage do not all return at the same moment.
      const wait = Math.min(MAX_RETRY_MS, FIRST_RETRY_MS * 2 ** (n - 1)) * (0.5 + Math.random());
      if (this.closed || !(await this.pause(wait))) return "dropped";
      this.note("retried");
    }
  }

  /** Waits, and resolves false where the recorder closed first. */
  private pause(ms: number): Promise<boolean> {
    return new Promise((resolve) => {
      const w = {
        timer: setTimeout(() => {
          this.waiting.delete(w);
          resolve(!this.closed);
        }, ms),
        wake: () => {
          clearTimeout(w.timer);
          this.waiting.delete(w);
          resolve(false);
        },
      };
      if (this.flushing === 0) w.timer.unref?.();
      this.waiting.add(w);
    });
  }
}

function byWorkspace<T>(batch: [T, string][]): Map<string, T[]> {
  const out = new Map<string, T[]>();
  for (const [item, workspace] of batch) {
    const list = out.get(workspace);
    if (list) list.push(item);
    else out.set(workspace, [item]);
  }
  return out;
}

// One listener for each signal, however many recorders there are, so the count of other listeners says whether the host handles the signal itself.
const SIGNALS = ["SIGTERM", "SIGINT"] as const;
const onSignal = new Map<Recorder, number>();
let stopping = false;

// Put first, so a host's own `process.once` listener is still counted when this runs: one that ran earlier would have removed itself, and the signal would be raised under the host's own shutdown.
function flushOnSignal(recorder: Recorder, timeoutMs: number): void {
  if (onSignal.size === 0) for (const s of SIGNALS) process.prependListener(s, signalled);
  onSignal.set(recorder, timeoutMs);
}

function forgetOnSignal(recorder: Recorder): void {
  if (onSignal.delete(recorder) && onSignal.size === 0) for (const s of SIGNALS) process.removeListener(s, signalled);
}

// Flushes, then does what the signal would have done without this listener. A host with a listener of its own decides what the signal does, and this only flushes beside it.
function signalled(signal: NodeJS.Signals): void {
  const hostHandles = process.listenerCount(signal) > 1;
  if (stopping) {
    // A second signal while the first is still flushing is not kept waiting.
    if (!hostHandles) raise(signal);
    return;
  }
  stopping = true;
  const flushed = Promise.allSettled([...onSignal].map(([recorder, timeoutMs]) => recorder.flush(timeoutMs)));
  void flushed.then(() => {
    stopping = false;
    if (!hostHandles) raise(signal);
  });
}

function raise(signal: NodeJS.Signals): void {
  for (const s of SIGNALS) process.removeListener(s, signalled);
  onSignal.clear();
  process.kill(process.pid, signal);
}
