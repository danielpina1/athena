import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { beforeEach, describe, expect, test } from "vitest";
import { DeltaSchema, SnapshotSchema, TicketSummarySchema } from "@/gen/athena/v1/state_pb";
import { useAthena } from "./state";
import { selectCounts, selectEndedToday, selectMergedToday, selectNeedsYou, selectTickets } from "./selectors";

const NOW = new Date(2026, 10, 4, 15, 0, 0).getTime(); // local time: "today" is the local day
const at = (minutesAgo: number) => timestampFromMs(NOW - minutesAgo * 60_000);
type TicketInit = MessageInitShape<typeof TicketSummarySchema>;
const ticket = (id: string, init: TicketInit) => create(TicketSummarySchema, { id, title: `title ${id}`, ...init });

function load(tickets: ReturnType<typeof ticket>[], approvals: MessageInitShape<typeof SnapshotSchema>["approvals"] = []) {
  useAthena.getState().applySnapshot(create(SnapshotSchema, { cursor: 1n, tickets, approvals }), "e1");
  return useAthena.getState();
}

beforeEach(() => useAthena.getState().reset());

describe("selectors", () => {
  test("needs-you order and counts match lists", () => {
    const s = load(
      [
        ticket("T-review-old", { stage: "person", action: "review", updatedAt: at(50) }),
        ticket("T-answer-new", { stage: "person", action: "answer", updatedAt: at(5) }),
        ticket("T-approval-new", { stage: "person", action: "approval", pendingApprovalId: "A-2", updatedAt: at(1) }),
        ticket("T-failed", { stage: "person", action: "failed", updatedAt: at(40) }),
        ticket("T-approval-old", { stage: "person", action: "approval", pendingApprovalId: "A-1", updatedAt: at(2) }),
        ticket("T-answer-old", { stage: "person", action: "answer", updatedAt: at(30) }),
        ticket("T-delivery", { stage: "person", action: "delivery", updatedAt: at(60) }),
        ticket("T-blocked", { stage: "person", action: "blocked", updatedAt: at(45) }),
        ticket("T-working", { stage: "working", updatedAt: at(3), repo: "o/a", model: "opus" }),
        ticket("T-queued", { stage: "working", action: "queued", updatedAt: at(4), repo: "o/b" }),
        ticket("T-idle", { stage: "idle", updatedAt: at(6) }),
        ticket("T-merged", { stage: "ended", outcome: "merged", endedAt: at(20), updatedAt: at(20) }),
        ticket("T-stopped", { stage: "ended", outcome: "stopped", endedAt: at(10), updatedAt: at(10) }),
        ticket("T-merged-yesterday", { stage: "ended", outcome: "merged", endedAt: at(24 * 60), updatedAt: at(24 * 60) }),
      ],
      [
        // Approvals are ordered by when they were asked, not by the Ticket's update time.
        { id: "A-1", ticketId: "T-approval-old", state: "pending", createdAt: at(90) },
        { id: "A-2", ticketId: "T-approval-new", state: "pending", createdAt: at(80) },
      ],
    );
    const items = selectNeedsYou(s);
    expect(items.map((i) => i.ticket.id)).toEqual([
      "T-approval-old",
      "T-approval-new",
      "T-answer-old",
      "T-answer-new",
      "T-delivery",
      "T-review-old",
      "T-blocked",
      "T-failed",
    ]);
    expect(items.map((i) => i.kind)).toEqual(["approval", "approval", "answer", "answer", "delivery", "review", "blocked", "failed"]);
    expect(items[0].approval?.id).toBe("A-1");
    expect(items[2].approval).toBeUndefined();

    const counts = selectCounts(s, NOW);
    expect(counts).toEqual({ needsYou: 8, open: 11, working: 2, endedToday: 2, mergedToday: 1 });
    expect(counts.needsYou).toBe(selectNeedsYou(s).length);
    expect(counts.needsYou).toBe(selectTickets(s, { stage: "needsYou" }).length);
    expect(counts.open).toBe(selectTickets(s, { stage: "open" }).length);
    expect(counts.working).toBe(selectTickets(s, { stage: "working" }).length);
    expect(counts.endedToday).toBe(selectEndedToday(s, NOW).length);
    expect(counts.mergedToday).toBe(selectMergedToday(s, NOW).length);
    expect(selectMergedToday(s, NOW).map((t) => t.id)).toEqual(["T-merged"]);
  });

  test("an approval being answered leaves Needs you and its count (R30)", () => {
    load(
      [
        ticket("T-a", { stage: "person", action: "approval", pendingApprovalId: "A-1", updatedAt: at(5) }),
        ticket("T-b", { stage: "person", action: "answer", updatedAt: at(4) }),
      ],
      [{ id: "A-1", ticketId: "T-a", state: "pending", createdAt: at(5) }],
    );
    const needsYou = () => {
      const s = useAthena.getState();
      const ids = selectNeedsYou(s).map((i) => i.ticket.id);
      expect(selectCounts(s, NOW).needsYou).toBe(ids.length);
      expect(selectTickets(s, { stage: "needsYou" }).length).toBe(ids.length);
      return ids;
    };
    expect(needsYou()).toEqual(["T-a", "T-b"]);

    const change = useAthena.getState().markApprovalAnswered("A-1", true);
    expect(needsYou()).toEqual(["T-b"]);
    // A Ticket Delta that still names the approval, while the answer is in flight, does not bring it back.
    useAthena.getState().applyDelta(
      create(DeltaSchema, {
        seq: 2n,
        change: { case: "ticket", value: ticket("T-a", { stage: "person", action: "approval", pendingApprovalId: "A-1" }) },
      }),
    );
    expect(needsYou()).toEqual(["T-b"]);
    // The answer failed: the approval is back.
    change.undo();
    expect(needsYou()).toEqual(["T-a", "T-b"]);
  });

  test("selectTickets filters by stage, repo, model and text, newest first", () => {
    const s = load([
      ticket("T-1", { stage: "working", repo: "o/a", model: "opus", title: "Fix the login bug", updatedAt: at(10) }),
      ticket("T-2", { stage: "idle", repo: "o/a", model: "sonnet", updatedAt: at(5) }),
      ticket("T-3", { stage: "ended", outcome: "merged", repo: "o/b", model: "opus", updatedAt: at(1), endedAt: at(1) }),
      ticket("T-4", { stage: "person", action: "answer", repo: "o/b", updatedAt: at(20) }),
    ]);
    const ids = (f: Parameters<typeof selectTickets>[1]) => selectTickets(s, f).map((t) => t.id);
    expect(ids({})).toEqual(["T-3", "T-2", "T-1", "T-4"]);
    expect(ids({ stage: "open" })).toEqual(["T-2", "T-1", "T-4"]);
    expect(ids({ stage: "needsYou" })).toEqual(["T-4"]);
    expect(ids({ stage: "working" })).toEqual(["T-1"]);
    expect(ids({ stage: "idle" })).toEqual(["T-2"]);
    expect(ids({ stage: "ended" })).toEqual(["T-3"]);
    expect(ids({ repo: "o/a" })).toEqual(["T-2", "T-1"]);
    expect(ids({ model: "opus" })).toEqual(["T-3", "T-1"]);
    expect(ids({ q: "LOGIN" })).toEqual(["T-1"]);
    expect(ids({ q: "t-4" })).toEqual(["T-4"]);
    expect(ids({ stage: "open", repo: "o/b" })).toEqual(["T-4"]);
  });

  test("selectTickets filters by runtime", () => {
    const s = load([
      ticket("T-1", { runtime: "claude", updatedAt: at(1) }),
      ticket("T-2", { runtime: "codex", runtimeChoice: "auto", updatedAt: at(2) }),
      ticket("T-3", { runtime: "codex", updatedAt: at(3) }),
    ]);
    const ids = (f: Parameters<typeof selectTickets>[1]) => selectTickets(s, f).map((t) => t.id);
    expect(ids({ runtime: "codex" })).toEqual(["T-2", "T-3"]);
    expect(ids({ runtime: "claude" })).toEqual(["T-1"]);
    expect(ids({})).toEqual(["T-1", "T-2", "T-3"]);
  });

  test("selectors return the same list while the state is unchanged", () => {
    const s = load([ticket("T-1", { stage: "person", action: "answer" })]);
    expect(selectNeedsYou(s)).toBe(selectNeedsYou(useAthena.getState()));
    expect(selectTickets(s, { stage: "open" })).toBe(selectTickets(useAthena.getState(), { stage: "open" }));
    expect(selectCounts(s, NOW)).toBe(selectCounts(useAthena.getState(), NOW));
  });
  test("a Ticket whose node is offline is a Needs you item of kind offline, among the stalled ones", () => {
    const s = load([
      ticket("T-answer", { stage: "person", action: "answer", updatedAt: at(5) }),
      ticket("T-offline", { stage: "person", action: "offline", nodeId: "n2", nodeName: "mini", updatedAt: at(30) }),
      ticket("T-failed", { stage: "person", action: "failed", updatedAt: at(10) }),
    ]);
    const items = selectNeedsYou(s);
    expect(items.map((i) => [i.ticket.id, i.kind])).toEqual([
      ["T-answer", "answer"],
      ["T-offline", "offline"],
      ["T-failed", "failed"],
    ]);
    expect(selectCounts(s, NOW).needsYou).toBe(3);
    expect(selectTickets(s, { stage: "needsYou" }).map((t) => t.id)).toContain("T-offline");
  });
  test("a split waiting for the owner is a plan item, and a failed child is a child item on its lead only", () => {
    const s = load([
      ticket("T-plan", { stage: "person", action: "plan", planState: "draft", updatedAt: at(20) }),
      ticket("T-lead", { stage: "person", action: "child", attentionChild: "T-c1", children: ["T-c1", "T-c2"], updatedAt: at(10) }),
      ticket("T-c1", { stage: "person", action: "failed", lead: "T-lead", updatedAt: at(11) }),
      // A failed child its lead does not name (another child is the lead's attention) is still listed on its own.
      ticket("T-c2", { stage: "person", action: "blocked", lead: "T-lead", updatedAt: at(12) }),
    ]);
    expect(selectNeedsYou(s).map((i) => [i.ticket.id, i.kind])).toEqual([
      ["T-plan", "plan"],
      ["T-c2", "blocked"],
      ["T-lead", "child"],
    ]);
    expect(selectCounts(s, NOW).needsYou).toBe(3);
    expect(selectTickets(s, { stage: "needsYou" }).map((t) => t.id)).not.toContain("T-c1");
  });

  test("a child blocked with a question of its own keeps its answer card, even when its lead names it", () => {
    const s = load([
      ticket("T-lead", { stage: "person", action: "child", attentionChild: "T-c1", children: ["T-c1"], updatedAt: at(10) }),
      // Blocked with a question: the server's stage is person/answer, which only the child's own card can answer.
      ticket("T-c1", { stage: "person", action: "answer", attentionQuestion: "Which API key?", lead: "T-lead", updatedAt: at(11) }),
    ]);
    expect(selectNeedsYou(s).map((i) => [i.ticket.id, i.kind])).toEqual([
      ["T-c1", "answer"],
      ["T-lead", "child"],
    ]);
    expect(selectTickets(s, { stage: "needsYou" }).map((t) => t.id)).toContain("T-c1");
  });
});
