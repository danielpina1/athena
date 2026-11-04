// Derived lists and counts. A count is always the length of the list it links to: selectCounts measures the very
// lists the other selectors return. A result is kept while its inputs are the same Maps, so a component can subscribe
// to a selector without getting a new array on every store update.
import { timestampMs, type Timestamp } from "@bufbuild/protobuf/wkt";
import type { ApprovalView, TicketSummary } from "@/gen/athena/v1/state_pb";
import type { AthenaData } from "./state";

export type NeedsYouKind = "approval" | "answer" | "review" | "delivery" | "blocked" | "failed";

export type NeedsYouItem = { kind: NeedsYouKind; ticket: TicketSummary; approval?: ApprovalView };

export type TicketFilter = {
  stage?: "open" | "needsYou" | "working" | "idle" | "ended";
  repo?: string;
  model?: string;
  q?: string;
};

export type Counts = { needsYou: number; open: number; working: number; endedToday: number; mergedToday: number };

/** The state the selectors read. Without `answering` nothing is being answered. */
type Slice = Pick<AthenaData, "tickets" | "approvals"> & Partial<Pick<AthenaData, "answering">>;
type TicketsSlice = Pick<AthenaData, "tickets"> & Partial<Pick<AthenaData, "answering">>;

const KINDS = new Set<string>(["approval", "answer", "review", "delivery", "blocked", "failed"]);

const NOTHING_ANSWERED: Answering = new Map();

/**
 * The Needs you kind of a Ticket waiting on the owner, or undefined. A Ticket whose approval the owner answered here
 * waits on no one while the answer is in flight (ruling R30): its approval was cleared from it optimistically, or the
 * answering map still holds it (a Ticket Delta that came meanwhile names it again).
 */
function needsYouKind(t: TicketSummary, answering: Answering): NeedsYouKind | undefined {
  if (t.stage !== "person" || !KINDS.has(t.action)) return undefined;
  if (t.action === "approval" && (t.pendingApprovalId === "" || answering.has(t.pendingApprovalId))) return undefined;
  return t.action as NeedsYouKind;
}

const ms = (ts: Timestamp | undefined) => (ts ? timestampMs(ts) : 0);

/** Approvals first, then answers, then the rest, each oldest first. */
const RANK: Record<NeedsYouKind, number> = { approval: 0, answer: 1, review: 2, delivery: 2, blocked: 2, failed: 2 };

/** When the item started waiting: an approval when it was asked, anything else when its Ticket last changed. */
const since = (i: NeedsYouItem) => (i.approval ? ms(i.approval.createdAt) : ms(i.ticket.updatedAt));

type Tickets = AthenaData["tickets"];
type Approvals = AthenaData["approvals"];
type Answering = AthenaData["answering"];

// Results are cached per tickets Map (and approvals and answering Maps), so a new Map, which every change makes,
// starts afresh and the old results go with the old Map.
const ticketLists = new WeakMap<Tickets, WeakMap<Answering, Map<string, TicketSummary[]>>>();
const needsYouLists = new WeakMap<Tickets, WeakMap<Approvals, WeakMap<Answering, NeedsYouItem[]>>>();
const countsByDay = new WeakMap<Tickets, WeakMap<Approvals, WeakMap<Answering, Map<number, Counts>>>>();

const answeringOf = (s: { answering?: Answering }) => s.answering ?? NOTHING_ANSWERED;

/** The cache of list results for s's tickets and answering. */
function listCache(s: TicketsSlice): Map<string, TicketSummary[]> {
  return cached(cached(ticketLists, s.tickets, () => new WeakMap()), answeringOf(s), () => new Map());
}

function cached<K, V>(m: { get(k: K): V | undefined; set(k: K, v: V): unknown }, key: K, compute: () => V): V {
  let v = m.get(key);
  if (v === undefined) {
    v = compute();
    m.set(key, v);
  }
  return v;
}

/** The Tickets waiting on the owner, each with its one action: approvals first, then answers, then the rest. */
export function selectNeedsYou(s: Slice): NeedsYouItem[] {
  const answering = answeringOf(s);
  const byApprovals = cached(cached(needsYouLists, s.tickets, () => new WeakMap()), s.approvals, () => new WeakMap());
  return cached(byApprovals, answering, () => {
    const items: NeedsYouItem[] = [];
    for (const ticket of s.tickets.values()) {
      const kind = needsYouKind(ticket, answering);
      if (!kind) continue;
      const approval = kind === "approval" ? s.approvals.get(ticket.pendingApprovalId) : undefined;
      items.push(approval ? { kind, ticket, approval } : { kind, ticket });
    }
    return items.sort((a, b) => RANK[a.kind] - RANK[b.kind] || since(a) - since(b) || cmp(a.ticket.id, b.ticket.id));
  });
}

function cmp(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0;
}

function matchesStage(t: TicketSummary, stage: TicketFilter["stage"], answering: Answering): boolean {
  switch (stage) {
    case undefined:
      return true;
    case "open":
      return t.stage !== "ended";
    case "needsYou":
      return needsYouKind(t, answering) !== undefined;
    default:
      return t.stage === stage;
  }
}

/** The Tickets the filter selects, most recently changed first. `q` matches the id or title, ignoring case. */
export function selectTickets(s: TicketsSlice, f: TicketFilter): TicketSummary[] {
  const key = JSON.stringify([f.stage, f.repo, f.model, f.q]);
  const answering = answeringOf(s);
  return cached(listCache(s), key, () => {
    const q = f.q?.trim().toLowerCase();
    const out: TicketSummary[] = [];
    for (const t of s.tickets.values()) {
      if (!matchesStage(t, f.stage, answering)) continue;
      if (f.repo !== undefined && t.repo !== f.repo) continue;
      if (f.model !== undefined && t.model !== f.model) continue;
      if (q && !t.id.toLowerCase().includes(q) && !t.title.toLowerCase().includes(q)) continue;
      out.push(t);
    }
    return out.sort((a, b) => ms(b.updatedAt) - ms(a.updatedAt) || cmp(a.id, b.id));
  });
}

/** Midnight of now's local day. */
function startOfDay(now: number): number {
  const d = new Date(now);
  d.setHours(0, 0, 0, 0);
  return d.getTime();
}

/** The Tickets that ended today (local time), most recently changed first. */
export function selectEndedToday(s: TicketsSlice, now: number = Date.now()): TicketSummary[] {
  const from = startOfDay(now);
  return cached(listCache(s), `endedSince:${from}`, () =>
    selectTickets(s, { stage: "ended" }).filter((t) => ms(t.endedAt) >= from),
  );
}

/** The Tickets merged today (local time), most recently changed first. */
export function selectMergedToday(s: TicketsSlice, now: number = Date.now()): TicketSummary[] {
  const from = startOfDay(now);
  return cached(listCache(s), `mergedSince:${from}`, () =>
    selectEndedToday(s, now).filter((t) => t.outcome === "merged"),
  );
}

/** The counts the sidebar and tabs show; each is the length of the list it links to. */
export function selectCounts(s: Slice, now: number = Date.now()): Counts {
  const byApprovals = cached(cached(countsByDay, s.tickets, () => new WeakMap()), s.approvals, () => new WeakMap());
  const byDay = cached(byApprovals, answeringOf(s), () => new Map<number, Counts>());
  return cached(byDay, startOfDay(now), () => ({
    needsYou: selectNeedsYou(s).length,
    open: selectTickets(s, { stage: "open" }).length,
    working: selectTickets(s, { stage: "working" }).length,
    endedToday: selectEndedToday(s, now).length,
    mergedToday: selectMergedToday(s, now).length,
  }));
}

/**
 * The owner's answer to an approval while its command has no result yet: true approve, false deny, undefined when not
 * being answered. Needs you leaves an answered approval out with it (ruling R30).
 */
export function selectAnswering(s: Pick<AthenaData, "answering">, approvalId: string): boolean | undefined {
  return s.answering.get(approvalId);
}

/** Whether the first Snapshot has arrived: until then counts and lists are unknown, not zero or empty. */
export function selectSynced(s: Pick<AthenaData, "synced">): boolean {
  return s.synced;
}
