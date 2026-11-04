// The task graph's model: lanes, nodes and edges with their positions, computed from the store's Tickets, Threads and
// Decisions. Pure and deterministic (the same records give the same picture), so the screen, the list fallback and
// the replay all draw from one function.
//
//  - Lanes: one per repository, labelled with its host and name (GITHUB · OWNER/NAME; LOCAL · <basename> for a git or
//    dir repository, full path in the title), sorted by label, then "No repository" last. By machine there is one lane
//    per node; today that is this one.
//  - x is the creation time over the visible window (the oldest visible Ticket to `at` or now), on a log scale of the
//    age, so recent Tickets get more room; consecutive Tickets keep a minimum spacing.
//  - y stacks in the lane by greedy interval packing: a node takes the lowest row whose last node ends, with a gap,
//    before it starts. Nodes never overlap.
//  - Edges: `thread` joins the Tickets subscribed to one Thread, oldest to newest; `follow-up` runs from the Thread's
//    earlier Ticket to the one a Decision routed a Trigger to. hand_off, wait_for, ask_agent and overlap come with
//    plan 7; the kinds exist now so the legend stays the same.
import { timestampMs, type Timestamp } from "@bufbuild/protobuf/wkt";
import type { DecisionView, RepoView, ThreadView, TicketSummary } from "@/gen/athena/v1/state_pb";
import { stripAnsi } from "@/lib/text/ansi";
import { parseTicketChip } from "../routing/outcome";

export type GroupBy = "repo" | "machine";

export const EDGE_KINDS = ["thread", "follow-up", "hand_off", "wait_for", "ask_agent", "overlap"] as const;
export type EdgeKind = (typeof EDGE_KINDS)[number];

/** How a node is drawn: by stage, orange for what needs the owner. */
export type NodeLook = "running" | "queued" | "waiting" | "needs-you" | "review" | "ended";

export type Lane = { id: string; label: string; title: string; y: number; height: number };

export type GNode = {
  id: string;
  /** The title without escapes, in full (for the accessible name and the hover). */
  title: string;
  ticket: TicketSummary;
  lane: string;
  x: number;
  y: number;
  w: number;
  h: number;
  look: NodeLook;
  needsYou: boolean;
  created: number;
};

export type GEdge = { id: string; kind: EdgeKind; from: string; to: string; threads: string[] };

export type GraphModel = {
  lanes: Lane[];
  nodes: GNode[];
  edges: GEdge[];
  width: number;
  height: number;
  /** The visible window, in ms: the oldest visible Ticket's creation to `at` or now. */
  start: number;
  end: number;
};

export type LayoutOptions = {
  groupBy: GroupBy;
  /** Shows the graph as it was then: only what existed by `at`. */
  at?: Date;
  now?: Date;
  /** The Tickets the Needs you rule selects (selectNeedsYou). */
  needsYou?: ReadonlySet<string>;
  needsYouOnly?: boolean;
  /** The configured repositories, for each lane's host. */
  repos?: readonly RepoView[];
  /** The node's name, for the machine lane. */
  machine?: string;
};

export const NODE_W = 176;
export const NODE_H = 52;
const GAP_X = 16;
const ROW_GAP = 12;
/** Room for the lane's label above its first row. */
const LANE_HEAD = 32;
const LANE_PAD = 16;
const PAD_X = 16;
/** The time axis's width at 1×, before minimum spacing widens it. */
const AXIS = 960;
const MIN_DX = 24;
/** The log scale's unit: ages are counted in minutes. */
const UNIT = 60_000;

const NO_REPO: Omit<Lane, "y" | "height"> = { id: "none", label: "No repository", title: "No repository" };

const ms = (ts: Timestamp | undefined) => (ts ? timestampMs(ts) : 0);

function basename(path: string): string {
  const parts = path.replace(/[\\/]+$/, "").split(/[\\/]/);
  return parts[parts.length - 1] || path;
}

const LOCAL_KINDS = new Set(["git", "dir"]);

/** The lane a Ticket's repository puts it in. */
function repoLane(t: TicketSummary, kinds: ReadonlyMap<string, string>): Omit<Lane, "y" | "height"> {
  if (t.repo) {
    const kind = kinds.get(t.repo) ?? "github";
    const host = LOCAL_KINDS.has(kind) ? "LOCAL" : kind.toUpperCase();
    return { id: `repo:${kind}:${t.repo.toLowerCase()}`, label: `${host} · ${t.repo.toUpperCase()}`, title: t.repo };
  }
  if (t.dir) return { id: `local:${t.dir}`, label: `LOCAL · ${basename(t.dir)}`, title: t.dir };
  return NO_REPO;
}

function lookOf(t: TicketSummary, needsYou: boolean): NodeLook {
  if (needsYou) return t.action === "review" ? "review" : "needs-you";
  if (t.stage === "working") return t.action === "queued" ? "queued" : "running";
  if (t.stage === "ended") return "ended";
  return "waiting";
}

const byCreated = (a: { created: number; id: string }, b: { created: number; id: string }) =>
  a.created - b.created || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0);

/** The whole history's window, for the scrubber: the oldest Ticket's creation to now (or the newest, if later). */
export function timeRange(tickets: Iterable<TicketSummary>, now: number): { start: number; end: number } {
  let start = Infinity;
  let end = now;
  for (const t of tickets) {
    const c = ms(t.createdAt);
    start = Math.min(start, c);
    end = Math.max(end, c);
  }
  return { start: Number.isFinite(start) ? start : now, end };
}

/** A title cut to `max` characters (whole code points) with an ellipsis; escapes and control characters removed. */
export function truncateTitle(title: string, max: number): string {
  const clean = stripAnsi(title).replace(/\s+/g, " ").trim();
  if (!clean) return "Untitled";
  const chars = Array.from(clean);
  return chars.length <= max ? clean : `${chars.slice(0, max - 1).join("")}…`;
}

/** The Ticket a Decision routed its Trigger to, or "". */
function routedTo(d: DecisionView): string {
  return d.reasons.find((r) => r.code === "routed")?.values.ticket ?? "";
}

export function layout(
  tickets: Iterable<TicketSummary>,
  threads: Iterable<ThreadView>,
  decisions: Iterable<DecisionView>,
  opts: LayoutOptions,
): GraphModel {
  const now = (opts.now ?? new Date()).getTime();
  const at = opts.at?.getTime();
  const needsYou = opts.needsYou ?? new Set<string>();
  const kinds = new Map((opts.repos ?? []).map((r) => [r.name, r.kind]));

  // The visible Tickets, oldest first.
  const shown: Array<{ id: string; created: number; t: TicketSummary }> = [];
  for (const t of tickets) {
    const created = ms(t.createdAt);
    if (at !== undefined && created > at) continue;
    if (opts.needsYouOnly && !needsYou.has(t.id)) continue;
    shown.push({ id: t.id, created, t });
  }
  shown.sort(byCreated);

  const end = at ?? Math.max(now, shown.length ? shown[shown.length - 1].created : now);
  const start = shown.length ? shown[0].created : end;
  const span = end - start;

  // x: the log of the age, newest at the right, then the minimum spacing in creation order.
  const xs: number[] = [];
  for (const s of shown) {
    const age = Math.max(0, end - s.created);
    const pos = span > 0 ? 1 - Math.log1p(age / UNIT) / Math.log1p(span / UNIT) : 1;
    const x = PAD_X + pos * AXIS;
    xs.push(xs.length ? Math.max(x, xs[xs.length - 1] + MIN_DX) : x);
  }

  // Lanes.
  const machine: Omit<Lane, "y" | "height"> = {
    id: "machine",
    label: opts.machine || "This machine",
    title: opts.machine || "This machine",
  };
  const laneDefs = new Map<string, Omit<Lane, "y" | "height">>();
  const laneOfTicket = new Map<string, string>();
  for (const s of shown) {
    const def = opts.groupBy === "machine" ? machine : repoLane(s.t, kinds);
    laneDefs.set(def.id, def);
    laneOfTicket.set(s.id, def.id);
  }
  const ordered =
    opts.groupBy === "machine"
      ? [machine]
      : [
          ...[...laneDefs.values()]
            .filter((l) => l.id !== NO_REPO.id)
            .sort((a, b) => (a.label < b.label ? -1 : a.label > b.label ? 1 : a.id < b.id ? -1 : 1)),
          NO_REPO,
        ];

  // Rows: greedy interval packing per lane, in x order.
  const rowOf = new Map<string, number>();
  const rowsIn = new Map<string, number>();
  const rowEnds = new Map<string, number[]>();
  shown.forEach((s, i) => {
    const lane = laneOfTicket.get(s.id) ?? NO_REPO.id;
    const ends = rowEnds.get(lane) ?? [];
    let r = ends.findIndex((e) => e + GAP_X <= xs[i]);
    if (r < 0) r = ends.length;
    ends[r] = xs[i] + NODE_W;
    rowEnds.set(lane, ends);
    rowOf.set(s.id, r);
    rowsIn.set(lane, ends.length);
  });

  const lanes: Lane[] = [];
  let y = 0;
  for (const def of ordered) {
    const rows = Math.max(1, rowsIn.get(def.id) ?? 0);
    const height = LANE_HEAD + rows * (NODE_H + ROW_GAP) - ROW_GAP + LANE_PAD;
    lanes.push({ ...def, y, height });
    y += height;
  }
  const laneY = new Map(lanes.map((l) => [l.id, l.y]));

  const nodes: GNode[] = shown.map((s, i) => {
    const lane = laneOfTicket.get(s.id) ?? NO_REPO.id;
    const isNeedsYou = needsYou.has(s.id);
    return {
      id: s.id,
      title: stripAnsi(s.t.title),
      ticket: s.t,
      lane,
      x: xs[i],
      y: (laneY.get(lane) ?? 0) + LANE_HEAD + (rowOf.get(s.id) ?? 0) * (NODE_H + ROW_GAP),
      w: NODE_W,
      h: NODE_H,
      look: lookOf(s.t, isNeedsYou),
      needsYou: isNeedsYou,
      created: s.created,
    };
  });

  const edges = edgesOf(shown, threads, decisions, at);
  const right = xs.length ? xs[xs.length - 1] + NODE_W + PAD_X : 0;
  return { lanes, nodes, edges, width: Math.max(PAD_X * 2 + AXIS + NODE_W, right), height: y, start, end };
}

function edgesOf(
  shown: ReadonlyArray<{ id: string; created: number }>,
  threads: Iterable<ThreadView>,
  decisions: Iterable<DecisionView>,
  at: number | undefined,
): GEdge[] {
  const created = new Map(shown.map((s) => [s.id, s.created]));
  const order = (ids: Iterable<string>) =>
    [...new Set(ids)]
      .filter((id) => created.has(id))
      .map((id) => ({ id, created: created.get(id) ?? 0 }))
      .sort(byCreated)
      .map((s) => s.id);

  const edges = new Map<string, GEdge>();
  const add = (kind: EdgeKind, from: string, to: string, thread: string) => {
    const id = `${kind}:${from}->${to}`;
    const e = edges.get(id) ?? { id, kind, from, to, threads: [] };
    if (!e.threads.includes(thread)) e.threads.push(thread);
    edges.set(id, e);
  };

  // Thread: each subscribed Ticket to the next one, oldest first.
  const subscribed = new Map<string, string[]>();
  const owners = new Map<string, string>();
  for (const th of [...threads].sort((a, b) => (a.key < b.key ? -1 : a.key > b.key ? 1 : 0))) {
    const chips = th.tickets.map(parseTicketChip);
    const ids = chips.map((c) => c.id);
    const own = chips.find((c) => c.flags.includes("own"));
    if (own) owners.set(th.key, own.id);
    subscribed.set(th.key, ids);
    const chain = order(ids);
    for (let i = 1; i < chain.length; i++) add("thread", chain[i - 1], chain[i], th.key);
  }

  // Follow-up: from the Ticket that started the Thread (its latest accept before the Decision; without one, the
  // Ticket that owns the Thread, else its oldest subscribed one) to the Ticket the Decision routed to.
  const byTime = [...decisions].sort((a, b) => ms(a.at) - ms(b.at) || (a.id < b.id ? -1 : 1));
  for (const d of byTime) {
    const to = routedTo(d);
    if (!to || !d.thread || !created.has(to)) continue;
    const when = ms(d.at);
    if (at !== undefined && when > at) continue;
    const started = byTime.filter((p) => p.kind === "accept" && p.thread === d.thread && p.ticketId && ms(p.at) <= when);
    let from = started.length ? started[started.length - 1].ticketId : "";
    if (!from) from = owners.get(d.thread) ?? order(subscribed.get(d.thread) ?? [])[0] ?? "";
    if (from && from !== to && created.has(from)) add("follow-up", from, to, d.thread);
  }

  return [...edges.values()].sort(
    (a, b) =>
      EDGE_KINDS.indexOf(a.kind) - EDGE_KINDS.indexOf(b.kind) ||
      (created.get(a.from) ?? 0) - (created.get(b.from) ?? 0) ||
      (a.id < b.id ? -1 : 1),
  );
}
