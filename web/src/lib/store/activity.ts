// The live activity feed: the important events across every Ticket, as they happen. It is built here, on the client,
// by diffing the Tickets map on each Delta (diffTickets): a Ticket created, waiting on the owner, opening a pull
// request, ending, or stalling. The first Snapshot, and a new epoch's, are the state as it is, not news: they add
// nothing, so a reload does not flood the feed. The newest MAX_ACTIVITY items are kept, in memory only.
import { create } from "zustand";
import type { TicketSummary } from "@/gen/athena/v1/state_pb";
import type { Tone } from "@/ui/athena/Dot";
import { needsYouKindOf, type NeedsYouKind } from "./selectors";
import { type AthenaState, useAthena } from "./state";

export type ActivityItem = {
  /** Unique within the session, for React keys. */
  id: number;
  /** What the event is (a Ticket, what happened, and which approval, question or pull request): an event already in the feed is not added again. */
  key: string;
  ticketId: string;
  /** The Ticket's title when it happened. */
  title: string;
  /** What happened, in plain words. */
  event: string;
  tone: Tone;
  /** When this client saw it (the latest time, for a repeated event), in ms. */
  at: number;
  /** How many times in a row the Ticket had this event: repeats are one row. */
  count: number;
};

export const MAX_ACTIVITY = 30;

type Event = Pick<ActivityItem, "key" | "event" | "tone">;

const NEEDS_YOU: Record<NeedsYouKind, [string, Tone]> = {
  approval: ["Needs your approval", "attention"],
  answer: ["Asked you a question", "attention"],
  review: ["Ready for review", "attention"],
  delivery: ["Delivery failed", "danger"],
  blocked: ["Blocked", "warn"],
  failed: ["Failed", "danger"],
};

const ENDED: Record<string, [string, Tone]> = {
  merged: ["Merged", "ok"],
  completed: ["Finished", "ok"],
  stopped: ["Stopped", "warn"],
  dropped: ["Dropped", "neutral"],
  closed: ["Closed", "neutral"],
  failed: ["Failed", "danger"],
};

const prNumber = (url: string) => /\/pull\/(\d+)/.exec(url)?.[1];

/** What changed on one Ticket that the owner would want to see, oldest first. */
export function ticketEvents(prev: TicketSummary | undefined, next: TicketSummary): Event[] {
  const id = next.id;
  const out: Event[] = [];
  if (!prev) {
    // A Ticket first seen already ended is an old one the Snapshot left out, not news.
    if (next.stage !== "ended") out.push({ key: `${id}:started`, event: "Started work", tone: "ok" });
  }
  const kind = needsYouKindOf(next);
  const was = prev && needsYouKindOf(prev);
  if (next.prUrl && next.prUrl !== prev?.prUrl) {
    const n = prNumber(next.prUrl);
    out.push({ key: `${id}:pr:${next.prUrl}`, event: n ? `Opened PR #${n}` : "Opened a pull request", tone: "neutral" });
  }
  const turned =
    kind !== was ||
    (kind === "approval" && next.pendingApprovalId !== prev?.pendingApprovalId) ||
    (kind === "answer" && next.attentionQuestion !== prev?.attentionQuestion);
  if (kind && turned) {
    const [event, tone] = NEEDS_YOU[kind];
    const what = kind === "approval" ? next.pendingApprovalId : kind === "answer" ? next.attentionQuestion : String(Number(next.updatedAt?.seconds ?? 0));
    out.push({ key: `${id}:${kind}:${what}`, event, tone });
  } else if (next.canRetry && !prev?.canRetry && next.stage !== "ended") {
    out.push({ key: `${id}:stalled:${String(next.updatedAt?.seconds ?? 0)}`, event: "Stalled", tone: "warn" });
  }
  if (next.stage === "ended" && prev && prev.stage !== "ended") {
    const [event, tone] = ENDED[next.outcome] ?? ["Ended", "neutral"];
    out.push({ key: `${id}:ended`, event, tone });
  }
  return out;
}

type NewItem = Omit<ActivityItem, "id" | "count">;

/** The events between two Tickets maps, oldest first. */
export function diffTickets(prev: ReadonlyMap<string, TicketSummary>, next: ReadonlyMap<string, TicketSummary>, at: number): NewItem[] {
  const out: NewItem[] = [];
  for (const t of next.values()) {
    const before = prev.get(t.id);
    if (before === t) continue;
    for (const e of ticketEvents(before, t)) out.push({ ...e, ticketId: t.id, title: t.title, at });
  }
  return out;
}

export const useActivity = create<{ items: ActivityItem[] }>()(() => ({ items: [] }));

let nextId = 0;

/**
 * Adds events to the feed, newest first: an event the feed already had is left out, and one that repeats the newest
 * row (the same Ticket, the same words: a run of approvals) is counted on that row.
 */
export function addEvents(items: readonly ActivityItem[], events: readonly NewItem[], seen: Set<string>): ActivityItem[] {
  let out = [...items];
  for (const e of events) {
    if (seen.has(e.key)) continue;
    seen.add(e.key);
    const top = out[0];
    if (top && top.ticketId === e.ticketId && top.event === e.event) {
      out[0] = { ...top, at: e.at, title: e.title, count: top.count + 1 };
      continue;
    }
    out = [{ ...e, id: ++nextId, count: 1 }, ...out];
  }
  return out.slice(0, MAX_ACTIVITY);
}

/** Feeds the activity from the store's Deltas. Returns the unsubscribe. */
export function connectActivity(store: typeof useAthena = useAthena, activity: typeof useActivity = useActivity): () => void {
  // Every event this session added, so one is never added twice (a rolled-back answer brings an approval back).
  const seen = new Set<string>();
  return store.subscribe((s: AthenaState, prev: AthenaState) => {
    if (!s.synced) {
      // Signed out (a reset): what happened belongs to that session.
      seen.clear();
      if (activity.getState().items.length > 0) activity.setState({ items: [] });
      return;
    }
    // The first Snapshot and a new epoch's are the state as it is, not events.
    if (s.tickets === prev.tickets || !prev.synced || s.epoch !== prev.epoch) return;
    const events = diffTickets(prev.tickets, s.tickets, Date.now());
    if (events.length === 0) return;
    const items = activity.getState().items;
    const next = addEvents(items, events, seen);
    if (next.length !== items.length || next.some((i, k) => i !== items[k])) activity.setState({ items: next });
  });
}
