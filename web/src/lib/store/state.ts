// The dashboard's state: a hub Snapshot, then the Deltas after it. Every screen reads it through useAthena and the
// selectors; connectStore feeds it from a LinkClient. Records live in Maps keyed by id, and a change replaces the
// Map it touches, so a subscriber sees a new reference exactly when its slice changed.
import { create } from "zustand";
import { Channel } from "@/gen/athena/v1/link_pb";
import type {
  ApprovalView,
  DecisionView,
  Delta,
  Health,
  NodeStatus,
  NotificationPrefsView,
  PolicyView,
  PromptsView,
  RepoView,
  Snapshot,
  ThreadView,
  TicketDetail,
  TicketSummary,
  TriggerView,
} from "@/gen/athena/v1/state_pb";
import type { LinkClient } from "@/lib/link/client";

export type AthenaData = {
  /** The epoch of the Welcome whose Snapshot the state came from. */
  epoch: string;
  /** The highest Delta seq applied, or the Snapshot's cursor. */
  cursor: bigint;
  tickets: Map<string, TicketSummary>;
  approvals: Map<string, ApprovalView>;
  triggers: Map<string, TriggerView>;
  /** By thread key. */
  threads: Map<string, ThreadView>;
  decisions: Map<string, DecisionView>;
  node?: NodeStatus;
  health?: Health;
  policy?: PolicyView;
  repos: RepoView[];
  notifications?: NotificationPrefsView;
  prompts?: PromptsView;
  /** The watched Tickets' details (seq 0 Deltas), by Ticket id. A Snapshot keeps them: they are not part of it. */
  details: Map<string, TicketDetail>;
};

export type AthenaState = AthenaData & {
  /** Replaces the state with s; epoch is the Welcome's that s follows (omitted after a Resync: the epoch stays). */
  applySnapshot(s: Snapshot, epoch?: string): void;
  /**
   * Applies d. A "gap" (d.seq above cursor + 1) is not applied: the caller asks for a Snapshot with Resync. A seq 0
   * Delta (a ticket_detail) never moves the cursor; one at or below the cursor is old and ignored.
   */
  applyDelta(d: Delta): "ok" | "gap";
  reset(): void;
};

function empty(): AthenaData {
  return {
    epoch: "",
    cursor: 0n,
    tickets: new Map(),
    approvals: new Map(),
    triggers: new Map(),
    threads: new Map(),
    decisions: new Map(),
    node: undefined,
    health: undefined,
    policy: undefined,
    repos: [],
    notifications: undefined,
    prompts: undefined,
    details: new Map(),
  };
}

function byKey<T>(list: T[], key: (v: T) => string): Map<string, T> {
  const m = new Map<string, T>();
  for (const v of list) m.set(key(v), v);
  return m;
}

function upsert<T>(m: Map<string, T>, k: string, v: T): Map<string, T> {
  const next = new Map(m);
  next.set(k, v);
  return next;
}

/** The part of the state one Delta changes. */
function change(s: AthenaData, d: Delta): Partial<AthenaData> {
  const c = d.change;
  switch (c.case) {
    case "ticket":
      return { tickets: upsert(s.tickets, c.value.id, c.value) };
    case "ticketDetail": {
      const id = c.value.summary?.id;
      return id ? { details: upsert(s.details, id, c.value) } : {};
    }
    case "approval":
      return { approvals: upsert(s.approvals, c.value.id, c.value) };
    case "trigger":
      return { triggers: upsert(s.triggers, c.value.id, c.value) };
    case "thread":
      return { threads: upsert(s.threads, c.value.key, c.value) };
    case "decision":
      return { decisions: upsert(s.decisions, c.value.id, c.value) };
    case "node":
      return { node: c.value };
    case "health":
      return { health: c.value };
    case "policy":
      return { policy: c.value };
    case "repos":
      return { repos: c.value.repos };
    case "notifications":
      return { notifications: c.value };
    case "prompts":
      return { prompts: c.value };
    default:
      return {}; // a change this client does not know yet
  }
}

export const useAthena = create<AthenaState>()((set, get) => ({
  ...empty(),
  applySnapshot(s, epoch) {
    set({
      epoch: epoch ?? get().epoch,
      cursor: s.cursor,
      tickets: byKey(s.tickets, (t) => t.id),
      approvals: byKey(s.approvals, (a) => a.id),
      triggers: byKey(s.triggers, (t) => t.id),
      threads: byKey(s.threads, (t) => t.key),
      decisions: byKey(s.decisions, (d) => d.id),
      node: s.node,
      health: s.health,
      policy: s.policy,
      repos: s.repos,
      notifications: s.notifications,
      prompts: s.prompts,
    });
  },
  applyDelta(d) {
    const s = get();
    if (d.seq === 0n) {
      set(change(s, d));
      return "ok";
    }
    if (d.seq <= s.cursor) return "ok";
    if (d.seq > s.cursor + 1n) return "gap";
    set({ ...change(s, d), cursor: d.seq });
    return "ok";
  },
  reset() {
    set(empty());
  },
}));

/**
 * Feeds the store from client: Snapshots and Deltas go in, a gap asks for a Snapshot with Resync (once, until it
 * arrives), and the client's next Hello resumes from the store's epoch and cursor. A Welcome's epoch counts only once
 * its Snapshot is applied, so a drop in between resumes from the old state, not a mix. Returns the unsubscribe.
 */
export function connectStore(client: LinkClient, store: typeof useAthena = useAthena): () => void {
  let welcomeEpoch: string | undefined;
  let resyncAsked = false;
  const resume = () => {
    const s = store.getState();
    client.setResume(s.epoch, s.cursor);
  };
  resume();
  // Any change to the store, a reset at sign-out included, moves the resume point: a reset store resumes from
  // nothing and gets a Snapshot, never Deltas onto an empty state.
  const offStore = store.subscribe(resume);
  const offFrame = client.onFrame((f) => {
    switch (f.body.case) {
      case "welcome":
        welcomeEpoch = f.body.value.epoch;
        resyncAsked = false;
        // No Snapshot follows only when the epoch matched: the Deltas after the cursor come next.
        if (!f.body.value.snapshotFollows) store.setState({ epoch: welcomeEpoch });
        break;
      case "snapshot":
        store.getState().applySnapshot(f.body.value, welcomeEpoch);
        resyncAsked = false;
        break;
      case "delta": {
        const d = f.body.value;
        // After a gap the Snapshot replaces everything: the Deltas before it are not applied.
        if (resyncAsked && d.seq !== 0n) return;
        if (store.getState().applyDelta(d) === "gap") {
          resyncAsked = client.send(Channel.CONTROL, false, { case: "resync", value: {} });
        }
        break;
      }
    }
  });
  return () => {
    offFrame();
    offStore();
  };
}
