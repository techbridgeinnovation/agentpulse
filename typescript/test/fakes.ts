// A gateway on this machine that speaks gRPC-web the way the real one does, a sink that remembers what it was given, and a provider that answers with scripted replies.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";

import type { User } from "../src/context.ts";
import type { Sink } from "../src/sinks.ts";
import type { Activity } from "../src/wire.ts";

export interface Call {
  path: string;
  headers: IncomingMessage["headers"];
  message: Uint8Array;
}

export interface Reply {
  message?: Uint8Array;
  /** The status in the trailer frame; null sends no trailer. */
  trailerStatus?: number | null;
  /** A status in the headers, as a refusal before any reply is sent. */
  headerStatus?: number;
  httpStatus?: number;
  delayMs?: number;
}

async function body(req: IncomingMessage): Promise<Uint8Array> {
  const chunks: Buffer[] = [];
  for await (const chunk of req) chunks.push(chunk as Buffer);
  return new Uint8Array(Buffer.concat(chunks));
}

function frame(flags: number, data: Uint8Array): Buffer {
  const head = Buffer.alloc(5);
  head[0] = flags;
  head.writeUInt32BE(data.length, 1);
  return Buffer.concat([head, Buffer.from(data)]);
}

export class FakeGateway {
  readonly calls: Call[] = [];
  readonly replies: Reply[] = [];
  private server: Server;
  url = "";

  constructor() {
    this.server = createServer((req: IncomingMessage, res: ServerResponse) => void this.handle(req, res));
  }

  async start(): Promise<this> {
    await new Promise<void>((resolve) => this.server.listen(0, "127.0.0.1", resolve));
    this.url = `http://127.0.0.1:${(this.server.address() as AddressInfo).port}`;
    return this;
  }

  close(): Promise<void> {
    this.server.closeAllConnections();
    return new Promise((resolve) => this.server.close(() => resolve()));
  }

  private async handle(req: IncomingMessage, res: ServerResponse): Promise<void> {
    const data = await body(req);
    const length = data.length >= 5 ? Buffer.from(data).readUInt32BE(1) : 0;
    this.calls.push({ path: req.url ?? "", headers: req.headers, message: data.subarray(5, 5 + length) });
    const reply = this.replies.shift() ?? {};
    if (reply.delayMs) await new Promise((r) => setTimeout(r, reply.delayMs));
    const headers: Record<string, string> = { "content-type": "application/grpc-web+proto" };
    if (reply.headerStatus !== undefined) headers["grpc-status"] = String(reply.headerStatus);
    res.writeHead(reply.httpStatus ?? 200, headers);
    if (reply.headerStatus !== undefined) {
      res.end();
      return;
    }
    const parts = [frame(0, reply.message ?? new Uint8Array())];
    const trailer = reply.trailerStatus === undefined ? 0 : reply.trailerStatus;
    if (trailer !== null) parts.push(frame(0x80, new TextEncoder().encode(`grpc-status:${trailer}\r\n`)));
    res.end(Buffer.concat(parts));
  }
}

export class MemorySink implements Sink {
  readonly name = "memory";
  readonly batches: [string, Activity[]][] = [];
  readonly named: [string, User[]][] = [];
  fail = false;
  async send(activities: Activity[], workspace: string): Promise<void> {
    if (this.fail) throw new Error("refused");
    this.batches.push([workspace, [...activities]]);
  }
  async sendUsers(users: User[], workspace: string): Promise<void> {
    this.named.push([workspace, [...users]]);
  }
  get activities(): Activity[] {
    return this.batches.flatMap(([, batch]) => batch);
  }
}

/** A provider on this machine: answers each request with the next scripted reply and remembers what it was sent. */
export class Provider {
  readonly received: { path: string; body: unknown }[] = [];
  readonly replies: { status: number; headers: Record<string, string>; body: string | string[] }[] = [];
  private server: Server;
  url = "";

  constructor() {
    this.server = createServer(async (req, res) => {
      const raw = Buffer.from(await body(req)).toString("utf8");
      this.received.push({ path: req.url ?? "", body: raw ? JSON.parse(raw) : {} });
      const reply = this.replies.shift() ?? { status: 500, headers: {}, body: "{}" };
      res.writeHead(reply.status, reply.headers);
      if (Array.isArray(reply.body)) {
        for (const chunk of reply.body) res.write(chunk);
        res.end();
      } else {
        res.end(reply.body);
      }
    });
  }

  async start(): Promise<this> {
    await new Promise<void>((resolve) => this.server.listen(0, "127.0.0.1", resolve));
    this.url = `http://127.0.0.1:${(this.server.address() as AddressInfo).port}`;
    return this;
  }

  json(body: unknown, status = 200): void {
    this.replies.push({ status, headers: { "content-type": "application/json" }, body: JSON.stringify(body) });
  }

  sse(events: unknown[]): void {
    this.replies.push({ status: 200, headers: { "content-type": "text/event-stream" }, body: [...events.map((e) => `data: ${JSON.stringify(e)}\n\n`), "data: [DONE]\n\n"] });
  }

  close(): Promise<void> {
    this.server.closeAllConnections();
    return new Promise((resolve) => this.server.close(() => resolve()));
  }
}
