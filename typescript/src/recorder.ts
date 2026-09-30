// The queue, the flush, the counters.
//
// The first rule of this library is that it must never degrade its host. Recording never blocks, never throws and never waits on the network: a record goes into a bounded buffer and a background flush delivers it. A full buffer drops the record and counts it, because losing a record is always preferable to delaying the work the agent was asked to do.
//
// Dropped records are counted and the count is readable in `stats()`. That counter is the one thing here that cannot be turned off, because silent loss is worse than visible loss.
//
// The flush timer is unreferenced, so a recorder never keeps a process alive on its own.

import { currentScope, type User } from "./context.ts";
import { Discard, type Sink } from "./sinks.ts";
import type { Activity } from "./wire.ts";

const DEFAULT_QUEUE_SIZE = 2048;
const DEFAULT_BATCH_SIZE = 100;
const DEFAULT_FLUSH_EVERY_MS = 2000;
const DEFAULT_SEND_TIMEOUT_MS = 10_000;
const DEFAULT_EXIT_TIMEOUT_MS = 2000;
const MAX_REMEMBERED_NAMES = 10_000;

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
  /** How long the process waits before exiting for what is still queued. Zero leaves the flush at exit to the host. */
  exitTimeoutMs?: number;
}

/** What the recorder has done, as a snapshot. */
export interface Stats {
  recorded: number;
  /** Records discarded because the queue was full or the recorder was closed. Above zero means cost data is incomplete. */
  dropped: number;
  delivered: number;
  failed: number;
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

  constructor(config: Config = {}) {
    this.sinks = config.sinks?.length ? [...config.sinks] : [new Discard()];
    this.queueSize = positive(config.queueSize, DEFAULT_QUEUE_SIZE);
    this.batchSize = positive(config.batchSize, DEFAULT_BATCH_SIZE);
    this.flushEveryMs = positive(config.flushEveryMs, DEFAULT_FLUSH_EVERY_MS);
    this.sendTimeoutMs = positive(config.sendTimeoutMs, DEFAULT_SEND_TIMEOUT_MS);
    const exitTimeoutMs = config.exitTimeoutMs ?? DEFAULT_EXIT_TIMEOUT_MS;
    if (exitTimeoutMs > 0) {
      // beforeExit fires when the event loop has emptied, so a last flush can still send; it does not fire on process.exit or a signal, which a host owns.
      process.once("beforeExit", () => void this.flush(exitTimeoutMs));
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
    }
  }

  /** Stops the recorder after a final attempt to deliver what is queued. Records handed over afterwards are dropped and counted. */
  async close(timeoutMs = 5000): Promise<boolean> {
    const flushed = await this.flush(timeoutMs);
    this.closed = true;
    if (this.timer) clearInterval(this.timer);
    this.timer = undefined;
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
        jobs.push(
          sink.send(activities, workspace, this.sendTimeoutMs).then(
            () => this.note("delivered", activities.length),
            () => this.note("failed", activities.length),
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
        if (!sink.sendUsers) continue;
        jobs.push(sink.sendUsers(users, workspace, this.sendTimeoutMs).then(undefined, () => this.note("namesFailed", users.length)));
      }
    }
    await Promise.all(jobs);
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
