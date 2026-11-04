// The dashboard's state: a hub Snapshot, then the Deltas after it. Every screen reads it through useAthena and the
// selectors; connectStore feeds it from a LinkClient. Records live in Maps keyed by id, and a change replaces the
// Map it touches, so a subscriber sees a new reference exactly when its slice changed.
import { timestampNow } from "@bufbuild/protobuf/wkt";
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
  /**
   * Whether a Snapshot has arrived since the start or the last reset. Until then the lists are not empty, they are
   * unknown: views say "Waiting for the server." rather than "nothing" or "0" (the installed app opened offline).
   */
  synced: boolean;
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
  /**
   * Tickets the owner asked to stop that have not ended yet: shown as "Stopping…". This client's own optimistic
   * mark, not the server's state; the Delta that ends the Ticket clears it.
   */
  stopping: ReadonlySet<string>;
  /**
   * Approvals the owner answered whose command has no result yet, by approval id: true for approve. This client's
   * own optimistic mark; a Snapshot that still says pending gets it applied again, and the result, a rollback, or a
   * Delta that answers the approval clears it.
   */
  answering: ReadonlyMap<string, boolean>;
};

/** An optimistic change: undo puts back what is still this client's guess; commit keeps it once the server agreed. */
export type Optimistic = { undo(): void; commit(): void };

const NO_CHANGE: Optimistic = { undo() {}, commit() {} };

export type AthenaState = AthenaData & {
  /** Replaces the state with s; epoch is the Welcome's that s follows (omitted after a Resync: the epoch stays). */
  applySnapshot(s: Snapshot, epoch?: string): void;
  /**
   * Applies d. A "gap" (d.seq above cursor + 1) is not applied: the caller asks for a Snapshot with Resync. A seq 0
   * Delta (a ticket_detail) never moves the cursor; one at or below the cursor is old and ignored.
   */
  applyDelta(d: Delta): "ok" | "gap";
  reset(): void;
  /**
   * Optimistically marks an approval answered and clears it from its Tickets' pending_approval_id. The undo restores
   * only what a Delta has not replaced since: the Deltas are the truth. An approval already answered, or being
   * answered, is left alone (a no-op), so two quick taps cannot tangle their rollbacks.
   */
  markApprovalAnswered(id: string, approve: boolean): Optimistic;
  /** Optimistically marks a Ticket as stopping. A Ticket already stopping or ended is left alone (a no-op). */
  markStopping(ticketId: string): Optimistic;
};

function empty(): AthenaData {
  return {
    synced: false,
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
    stopping: new Set(),
    answering: new Map(),
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

function without(set: ReadonlySet<string>, k: string): ReadonlySet<string> {
  const next = new Set(set);
  next.delete(k);
  return next;
}

function withoutKey<V>(m: ReadonlyMap<string, V>, k: string): ReadonlyMap<string, V> {
  const next = new Map(m);
  next.delete(k);
  return next;
}

/** The stopping marks that still apply to tickets: a Ticket that ended, or is gone, is no longer stopping. */
function stillStopping(stopping: ReadonlySet<string>, tickets: Map<string, TicketSummary>): ReadonlySet<string> {
  if (stopping.size === 0) return stopping;
  const next = new Set<string>();
  for (const id of stopping) {
    const t = tickets.get(id);
    if (t && t.stage !== "ended") next.add(id);
  }
  return next.size === stopping.size ? stopping : next;
}

/**
 * The optimistic copies this client made, each mapped to the record it replaced. An undo restores a record only while
 * the store still holds one of these copies; a record from the server is never in it.
 */
const guessed = new WeakMap<object, object>();

/** approvals and tickets with approval id answered: the approval's state set, its Tickets' pending_approval_id cleared. */
function answered(
  approvals: Map<string, ApprovalView>,
  tickets: Map<string, TicketSummary>,
  id: string,
  approve: boolean,
): Pick<AthenaData, "approvals" | "tickets"> {
  const before = approvals.get(id);
  if (before) {
    const after: ApprovalView = { ...before, state: approve ? "approved" : "denied", answeredAt: timestampNow() };
    guessed.set(after, before);
    approvals = upsert(approvals, id, after);
  }
  let changed: Map<string, TicketSummary> | undefined;
  for (const t of tickets.values()) {
    if (t.pendingApprovalId !== id) continue;
    const after: TicketSummary = { ...t, pendingApprovalId: "" };
    guessed.set(after, t);
    changed ??= new Map(tickets);
    changed.set(t.id, after);
  }
  return { approvals, tickets: changed ?? tickets };
}

/** The part of the state one Delta changes. */
function change(s: AthenaData, d: Delta): Partial<AthenaData> {
  const c = d.change;
  switch (c.case) {
    case "ticket": {
      const tickets = upsert(s.tickets, c.value.id, c.value);
      return c.value.stage === "ended" && s.stopping.has(c.value.id)
        ? { tickets, stopping: without(s.stopping, c.value.id) }
        : { tickets };
    }
    case "ticketDetail": {
      const id = c.value.summary?.id;
      return id ? { details: upsert(s.details, id, c.value) } : {};
    }
    case "approval": {
      const approvals = upsert(s.approvals, c.value.id, c.value);
      return c.value.state !== "pending" && s.answering.has(c.value.id)
        ? { approvals, answering: withoutKey(s.answering, c.value.id) }
        : { approvals };
    }
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
    let tickets = byKey(s.tickets, (t) => t.id);
    let approvals = byKey(s.approvals, (a) => a.id);
    // An answer still in flight stays shown while the server still says pending; any other state is the truth.
    const answering = new Map(get().answering);
    for (const [id, approve] of answering) {
      if (approvals.get(id)?.state !== "pending") {
        answering.delete(id);
        continue;
      }
      ({ approvals, tickets } = answered(approvals, tickets, id, approve));
    }
    set({
      synced: true,
      epoch: epoch ?? get().epoch,
      cursor: s.cursor,
      tickets,
      stopping: stillStopping(get().stopping, tickets),
      answering: answering.size === get().answering.size ? get().answering : answering,
      approvals,
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
  markApprovalAnswered(id, approve) {
    const s = get();
    const approval = s.approvals.get(id);
    if (s.answering.has(id) || (approval && approval.state !== "pending")) return NO_CHANGE;
    set({ ...answered(s.approvals, s.tickets, id, approve), answering: new Map(s.answering).set(id, approve) });
    const settle = () => {
      const now = get();
      return now.answering.has(id) ? { answering: withoutKey(now.answering, id) } : {};
    };
    return {
      undo() {
        const now = get();
        const undo: Partial<AthenaData> = settle();
        const a = now.approvals.get(id);
        const base = a && guessed.get(a);
        if (base) undo.approvals = upsert(now.approvals, id, base as ApprovalView);
        let tickets: Map<string, TicketSummary> | undefined;
        for (const t of now.tickets.values()) {
          const prev = guessed.get(t) as TicketSummary | undefined;
          if (prev?.pendingApprovalId !== id) continue;
          tickets ??= new Map(now.tickets);
          tickets.set(t.id, prev);
        }
        if (tickets) undo.tickets = tickets;
        set(undo);
      },
      commit() {
        set(settle());
      },
    };
  },
  markStopping(ticketId) {
    const s = get();
    if (s.stopping.has(ticketId) || s.tickets.get(ticketId)?.stage === "ended") return NO_CHANGE;
    set({ stopping: new Set(s.stopping).add(ticketId) });
    return {
      undo() {
        const now = get();
        if (now.stopping.has(ticketId)) set({ stopping: without(now.stopping, ticketId) });
      },
      // The Delta that ends the Ticket clears it, not the result: stopping takes the session a moment.
      commit() {},
    };
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
