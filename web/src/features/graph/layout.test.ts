import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { describe, expect, test } from "vitest";
import {
  DecisionViewSchema,
  RepoViewSchema,
  ThreadViewSchema,
  TicketSummarySchema,
  type DecisionView,
  type ThreadView,
  type TicketSummary,
} from "@/gen/athena/v1/state_pb";
import { EDGE_KINDS, type GNode, layout, timeRange, truncateTitle } from "./layout";

const NOW = Date.parse("2026-11-04T14:00:00Z");
const ago = (min: number) => timestampFromMs(NOW - min * 60_000);
const now = new Date(NOW);

const ticket = (id: string, init: MessageInitShape<typeof TicketSummarySchema> = {}): TicketSummary =>
  create(TicketSummarySchema, { id, title: `title ${id}`, stage: "working", createdAt: ago(10), ...init });
const thread = (key: string, tickets: string[]): ThreadView => create(ThreadViewSchema, { key, tickets });
const decision = (init: MessageInitShape<typeof DecisionViewSchema>): DecisionView => create(DecisionViewSchema, init);

function overlaps(a: GNode, b: GNode): boolean {
  return a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h;
}

describe("layout", () => {
  test("lanes per repo plus no-repo", () => {
    const tickets = [
      ticket("T-1", { repo: "acme/web" }),
      ticket("T-2", { repo: "acme/api" }),
      ticket("T-3", { repo: "acme/api" }),
      ticket("T-4", { dir: "/Users/me/src/notes" }),
      ticket("T-5"),
    ];
    const repos = [create(RepoViewSchema, { name: "/Users/me/src/notes", kind: "git" })];
    const m = layout(tickets, [], [], { groupBy: "repo", now, repos });
    expect(m.lanes.map((l) => l.label)).toEqual([
      "GITHUB · ACME/API",
      "GITHUB · ACME/WEB",
      "LOCAL · notes",
      "No repository",
    ]);
    // The local lane's title carries the full path.
    expect(m.lanes[2].title).toBe("/Users/me/src/notes");
    const laneOf = (id: string) => m.lanes.find((l) => l.id === m.nodes.find((n) => n.id === id)?.lane)?.label;
    expect(laneOf("T-2")).toBe("GITHUB · ACME/API");
    expect(laneOf("T-3")).toBe("GITHUB · ACME/API");
    expect(laneOf("T-1")).toBe("GITHUB · ACME/WEB");
    expect(laneOf("T-4")).toBe("LOCAL · notes");
    expect(laneOf("T-5")).toBe("No repository");
    // Lanes stack top to bottom without gaps, and every node sits inside its lane.
    for (let i = 1; i < m.lanes.length; i++) expect(m.lanes[i].y).toBe(m.lanes[i - 1].y + m.lanes[i - 1].height);
    for (const n of m.nodes) {
      const lane = m.lanes.find((l) => l.id === n.lane)!;
      expect(n.y).toBeGreaterThanOrEqual(lane.y);
      expect(n.y + n.h).toBeLessThanOrEqual(lane.y + lane.height);
    }
    // "No repository" is there even with nothing in it.
    expect(layout([ticket("T-9", { repo: "o/n" })], [], [], { groupBy: "repo", now }).lanes.map((l) => l.label)).toEqual([
      "GITHUB · O/N",
      "No repository",
    ]);
  });

  test("machine mode is one lane named after the node", () => {
    const tickets = [ticket("T-1", { repo: "acme/web" }), ticket("T-2")];
    const named = layout(tickets, [], [], { groupBy: "machine", now, machine: "mac-studio" });
    expect(named.lanes.map((l) => l.label)).toEqual(["mac-studio"]);
    expect(new Set(named.nodes.map((n) => n.lane))).toEqual(new Set([named.lanes[0].id]));
    expect(layout(tickets, [], [], { groupBy: "machine", now }).lanes.map((l) => l.label)).toEqual(["This machine"]);
  });

  test("no overlapping nodes in a lane", () => {
    // 50 Tickets in one lane, some created at the same instant, most of them close together.
    const tickets = Array.from({ length: 50 }, (_, i) =>
      ticket(`T-${String(i).padStart(4, "0")}`, { repo: "acme/api", createdAt: ago(i < 10 ? 0 : i < 30 ? 1 + i / 10 : i * 7) }),
    );
    const m = layout(tickets, [], [], { groupBy: "repo", now });
    expect(m.nodes).toHaveLength(50);
    for (let i = 0; i < m.nodes.length; i++) {
      for (let j = i + 1; j < m.nodes.length; j++) {
        expect(overlaps(m.nodes[i], m.nodes[j]), `${m.nodes[i].id} overlaps ${m.nodes[j].id}`).toBe(false);
      }
    }
    // Deterministic: the same input gives the same positions.
    const again = layout([...tickets].reverse(), [], [], { groupBy: "repo", now });
    const pos = (g: typeof m) => Object.fromEntries(g.nodes.map((n) => [n.id, [n.x, n.y]]));
    expect(pos(again)).toEqual(pos(m));
  });

  test("x is creation time, log-compressed: older is left and recent Tickets get more room", () => {
    const tickets = [
      ticket("T-old", { createdAt: ago(600) }),
      ticket("T-mid", { createdAt: ago(300) }),
      ticket("T-new2", { createdAt: ago(20) }),
      ticket("T-new1", { createdAt: ago(10) }),
    ];
    const m = layout(tickets, [], [], { groupBy: "repo", now });
    const x = (id: string) => m.nodes.find((n) => n.id === id)!.x;
    expect(x("T-old")).toBeLessThan(x("T-mid"));
    expect(x("T-mid")).toBeLessThan(x("T-new2"));
    expect(x("T-new2")).toBeLessThan(x("T-new1"));
    // Per minute of age, the newest Tickets get more room than the oldest.
    expect((x("T-mid") - x("T-old")) / 300).toBeLessThan((x("T-new1") - x("T-new2")) / 10);
    expect(m.start).toBe(NOW - 600 * 60_000);
    expect(m.end).toBe(NOW);
  });

  test("thread edges join co-subscribed tickets", () => {
    const tickets = [
      ticket("T-1", { createdAt: ago(30) }),
      ticket("T-2", { createdAt: ago(20) }),
      ticket("T-3", { createdAt: ago(10) }),
      ticket("T-4", { createdAt: ago(5) }),
    ];
    const threads = [
      thread("github:acme/api#12", ["T-2(context)", "T-1(own,bound)", "T-3"]),
      thread("github:acme/api#13", ["T-1(own)", "T-2"]),
      // A Thread with one Ticket, and one naming a Ticket the store does not have, join nothing.
      thread("github:acme/api#14", ["T-4(own)"]),
      thread("github:acme/api#15", ["T-4", "T-99"]),
    ];
    const m = layout(tickets, threads, [], { groupBy: "repo", now });
    const thr = m.edges.filter((e) => e.kind === "thread");
    expect(thr.map((e) => [e.from, e.to, e.threads])).toEqual([
      ["T-1", "T-2", ["github:acme/api#12", "github:acme/api#13"]],
      ["T-2", "T-3", ["github:acme/api#12"]],
    ]);
  });

  test("follow-up edges run from the Thread's earlier Ticket to the one a Decision routed to", () => {
    const T = "github:acme/api#12";
    const tickets = [ticket("T-1", { createdAt: ago(30) }), ticket("T-2", { createdAt: ago(20) })];
    const decisions = [
      decision({ id: "d-1", at: ago(29), kind: "accept", thread: T, ticketId: "T-1", triggers: ["tr-1"] }),
      decision({ id: "d-2", at: ago(5), kind: "ignore", thread: T, triggers: ["tr-2"], reasons: [{ code: "routed", values: { ticket: "T-2" } }] }),
      // Routed back to the Ticket the Thread started: no edge to itself.
      decision({ id: "d-3", at: ago(4), kind: "ignore", thread: T, triggers: ["tr-3"], reasons: [{ code: "routed", values: { ticket: "T-1" } }] }),
    ];
    const m = layout(tickets, [thread(T, ["T-1(own)", "T-2"])], decisions, { groupBy: "repo", now });
    expect(m.edges.filter((e) => e.kind === "follow-up").map((e) => [e.from, e.to, e.threads])).toEqual([["T-1", "T-2", [T]]]);
    // Without an accept on the Thread, the Ticket that owns it is the earlier one.
    const owned = layout(
      [...tickets, ticket("T-3", { createdAt: ago(15) })],
      [thread("github:acme/api#9", ["T-3", "T-2(own)"])],
      [decision({ id: "d-9", at: ago(2), kind: "ignore", thread: "github:acme/api#9", reasons: [{ code: "routed", values: { ticket: "T-3" } }] })],
      { groupBy: "repo", now },
    );
    expect(owned.edges.filter((e) => e.kind === "follow-up").map((e) => [e.from, e.to])).toEqual([["T-2", "T-3"]]);
    expect(EDGE_KINDS).toEqual(["thread", "follow-up", "hand_off", "wait_for", "ask_agent", "overlap"]);
  });

  test("at filters to the past", () => {
    const T = "github:acme/api#12";
    const tickets = [
      ticket("T-1", { createdAt: ago(60) }),
      ticket("T-2", { createdAt: ago(40) }),
      ticket("T-3", { createdAt: ago(10) }),
    ];
    const decisions = [
      decision({ id: "d-1", at: ago(59), kind: "accept", thread: T, ticketId: "T-1" }),
      decision({ id: "d-2", at: ago(30), kind: "ignore", thread: T, reasons: [{ code: "routed", values: { ticket: "T-2" } }] }),
    ];
    const threads = [thread(T, ["T-1", "T-2", "T-3"])];
    const past = new Date(NOW - 45 * 60_000);
    const m = layout(tickets, threads, decisions, { groupBy: "repo", now, at: past });
    expect(m.nodes.map((n) => n.id)).toEqual(["T-1"]);
    expect(m.edges).toEqual([]);
    expect(m.end).toBe(past.getTime());

    const later = layout(tickets, threads, decisions, { groupBy: "repo", now, at: new Date(NOW - 35 * 60_000) });
    expect(later.nodes.map((n) => n.id).sort()).toEqual(["T-1", "T-2"]);
    // The routing Decision came at 30 minutes ago: not yet.
    expect(later.edges.map((e) => e.kind)).toEqual(["thread"]);

    const all = layout(tickets, threads, decisions, { groupBy: "repo", now });
    expect(all.edges.map((e) => e.kind).sort()).toEqual(["follow-up", "thread", "thread"]);
    expect(timeRange(tickets, NOW)).toEqual({ start: NOW - 60 * 60_000, end: NOW });
  });

  test("needs-you filter", () => {
    const tickets = [
      ticket("T-1", { stage: "person", action: "answer" }),
      ticket("T-2", { stage: "working" }),
      ticket("T-3", { stage: "person", action: "review", repo: "acme/api" }),
    ];
    const needsYou = new Set(["T-1", "T-3"]);
    const threads = [thread("github:acme/api#1", ["T-1", "T-2", "T-3"])];
    const all = layout(tickets, threads, [], { groupBy: "repo", now, needsYou });
    expect(all.nodes.find((n) => n.id === "T-1")?.look).toBe("needs-you");
    expect(all.nodes.find((n) => n.id === "T-3")?.look).toBe("review");
    expect(all.nodes.find((n) => n.id === "T-2")?.look).toBe("running");
    expect(all.nodes.filter((n) => n.needsYou).map((n) => n.id).sort()).toEqual(["T-1", "T-3"]);

    const only = layout(tickets, threads, [], { groupBy: "repo", now, needsYou, needsYouOnly: true });
    expect(only.nodes.map((n) => n.id).sort()).toEqual(["T-1", "T-3"]);
    // Edges only join what is shown.
    expect(only.edges.every((e) => e.from !== "T-2" && e.to !== "T-2")).toBe(true);
    // A person stage the Needs you rule leaves out (an approval being answered) is not orange.
    const answered = layout([ticket("T-4", { stage: "person", action: "approval" })], [], [], { groupBy: "repo", now, needsYou: new Set() });
    expect(answered.nodes[0].look).not.toBe("needs-you");
    expect(answered.nodes[0].needsYou).toBe(false);
  });

  test("looks by stage", () => {
    const look = (init: MessageInitShape<typeof TicketSummarySchema>) =>
      layout([ticket("T-1", init)], [], [], { groupBy: "repo", now, needsYou: new Set() }).nodes[0].look;
    expect(look({ stage: "working" })).toBe("running");
    expect(look({ stage: "working", action: "queued" })).toBe("queued");
    expect(look({ stage: "idle" })).toBe("waiting");
    expect(look({ stage: "ended", outcome: "merged" })).toBe("ended");
  });

  test("long titles are cut to fit a node, by characters, never inside a surrogate pair", () => {
    expect(truncateTitle("Short", 20)).toBe("Short");
    const long = "W".repeat(2000);
    expect(truncateTitle(long, 20)).toBe(`${"W".repeat(19)}…`);
    expect(Array.from(truncateTitle("😀".repeat(30), 10))).toHaveLength(10);
    // Escapes and control characters never reach the SVG.
    expect(truncateTitle("\u001b[31mred\u001b[0m", 20)).toBe("red");
    expect(truncateTitle("", 20)).toBe("Untitled");
  });
});
