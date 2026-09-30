// Where records go: the metering service, or nothing at all.
//
// A sink runs on the recorder's background flush and never on the caller's turn. It is handed one workspace's records at a time and signals a failure by rejecting; the recorder counts it and carries on, so a sink never has to protect its host.

import type { User } from "./context.ts";
import { workspaceName } from "./context.ts";
import { ConfigError, type Gateway } from "./gateway.ts";
import { type Activity, batchCreateActivities, batchUpsertUsers } from "./wire.ts";

const BATCH_CREATE_ACTIVITIES = "/techbridge.ap.metering.v1.ActivitiesService/BatchCreateActivities";
const BATCH_UPSERT_USERS = "/techbridge.ap.metering.v1.UsersService/BatchUpsertUsers";

/** A destination for records. */
export interface Sink {
  /** Identifies the sink, so a team can tell which destination is failing. */
  readonly name: string;
  /** Delivers one workspace's records. An empty workspace is the ordinary case for a product with one tenant. */
  send(activities: Activity[], workspace: string, timeoutMs: number): Promise<void>;
  /** Delivers one workspace's people to name. A sink without it receives records and no names. */
  sendUsers?(users: User[], workspace: string, timeoutMs: number): Promise<void>;
}

/** Accepts everything and keeps nothing: the default, so a recorder with nowhere to send is inert rather than broken. */
export class Discard implements Sink {
  readonly name = "discard";
  async send(): Promise<void> {}
}

/** Records, and the names behind them, to the metering service through the gateway. The organisation is fixed because one process bills one organisation; the workspace is not, because one process can serve many tenants. */
export class MeteringSink implements Sink {
  readonly name = "metering";
  private readonly gateway: Gateway;
  private readonly organisation: string;

  constructor(gateway: Gateway, organisation = "") {
    this.gateway = gateway;
    this.organisation = (organisation || gateway.organisation).trim();
    if (!this.organisation) throw new ConfigError("agentpulse: the metering sink needs the organisation the records are filed under");
  }

  private parent(workspace: string): string {
    // A batch naming no workspace is filed under the organisation, which lands it in the organisation's default workspace.
    return workspaceName(this.organisation, workspace) || this.organisation;
  }

  /** One call per batch. A rejected batch is not retried: a duplicate costs more than a gap. */
  async send(activities: Activity[], workspace: string, timeoutMs: number): Promise<void> {
    if (activities.length === 0) return;
    await this.gateway.call(BATCH_CREATE_ACTIVITIES, batchCreateActivities(this.parent(workspace), activities), timeoutMs);
  }

  async sendUsers(users: User[], workspace: string, timeoutMs: number): Promise<void> {
    if (users.length === 0) return;
    const parent = this.parent(workspace);
    const rows = users.map((u) => ({ name: `${parent}/users/${u.id}`, displayName: u.name, email: u.email }));
    await this.gateway.call(BATCH_UPSERT_USERS, batchUpsertUsers(parent, rows), timeoutMs);
  }
}
