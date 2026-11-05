import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { afterEach, expect, test } from "vitest";
import { DeltaSchema, SnapshotSchema, TicketSummarySchema } from "@/gen/athena/v1/state_pb";
import { connectActivity, MAX_ACTIVITY, useActivity } from "./activity";
import { useAthena } from "./state";

type T = MessageInitShape<typeof TicketSummarySchema>;
const ticket = (init: T): T => ({ title: `title ${init.id}`, stage: "working", updatedAt: timestampFromMs(1000), ...init });

let seq = 1n;
const snapshot = (tickets: T[], epoch = "e1") => {
  seq = 1n;
  useAthena.getState().applySnapshot(create(SnapshotSchema, { cursor: 1n, tickets }), epoch);
};
const delta = (t: T) => {
  seq += 1n;
  useAthena.getState().applyDelta(create(DeltaSchema, { seq, change: { case: "ticket", value: ticket(t) } }));
};
const events = () => useActivity.getState().items.map((i) => `${i.ticketId} ${i.event}`);

let off: (() => void) | undefined;
afterEach(() => {
  off?.();
  useAthena.getState().reset();
  useActivity.setState({ items: [] });
});

test("the first Snapshot, and a new epoch's, add nothing", () => {
  off = connectActivity();
  snapshot([ticket({ id: "T-1" }), ticket({ id: "T-2", stage: "person", action: "review" })]);
  expect(events()).toEqual([]);
  snapshot([ticket({ id: "T-1" }), ticket({ id: "T-3" })], "e2");
  expect(events()).toEqual([]);
});

test("nothing before the store is synced", () => {
  off = connectActivity();
  useAthena.setState({ tickets: new Map([["T-1", create(TicketSummarySchema, ticket({ id: "T-1" }))]]) });
  expect(events()).toEqual([]);
});

test("Deltas add the events that matter, newest first", () => {
  off = connectActivity();
  snapshot([ticket({ id: "T-1" })]);
  delta({ id: "T-2" });
  delta({ id: "T-1", stage: "person", action: "approval", pendingApprovalId: "A-1" });
  delta({ id: "T-1", stage: "working" });
  delta({ id: "T-1", stage: "person", action: "answer", attentionQuestion: "Redis or in-process?" });
  delta({ id: "T-2", prUrl: "https://github.com/acme/api/pull/12" });
  delta({ id: "T-2", stage: "person", action: "review", prUrl: "https://github.com/acme/api/pull/12" });
  delta({ id: "T-2", stage: "ended", outcome: "merged" });
  delta({ id: "T-1", stage: "person", action: "failed", canRetry: true });
  delta({ id: "T-1", stage: "ended", outcome: "stopped" });
  expect(events()).toEqual([
    "T-1 Stopped",
    "T-1 Failed",
    "T-2 Merged",
    "T-2 Ready for review",
    "T-2 Opened PR #12",
    "T-1 Asked you a question",
    "T-1 Needs your approval",
    "T-2 Started work",
  ]);
  const item = useActivity.getState().items.at(-1)!;
  expect(item.title).toBe("title T-2");
  expect(item.at).toBeGreaterThan(0);
  expect(useActivity.getState().items[0].tone).toBe("warn");
});

test("an event already in the feed is not added again", () => {
  off = connectActivity();
  snapshot([ticket({ id: "T-1" })]);
  delta({ id: "T-1", stage: "person", action: "approval", pendingApprovalId: "A-1" });
  // The owner approves here (the Ticket's approval is cleared optimistically), then the command is rolled back.
  const undo = useAthena.getState().markApprovalAnswered("A-1", true);
  undo.undo();
  expect(events()).toEqual(["T-1 Needs your approval"]);
  // A new approval is a new event, counted on the row it repeats.
  delta({ id: "T-1", stage: "person", action: "approval", pendingApprovalId: "A-2" });
  expect(events()).toEqual(["T-1 Needs your approval"]);
  expect(useActivity.getState().items[0].count).toBe(2);
  // Something else in between: a row of its own again.
  delta({ id: "T-2" });
  delta({ id: "T-1", stage: "person", action: "approval", pendingApprovalId: "A-3" });
  expect(events()).toEqual(["T-1 Needs your approval", "T-2 Started work", "T-1 Needs your approval"]);
});

test("a stall without a Needs you change is an event; the ending kinds read as outcomes", () => {
  off = connectActivity();
  snapshot([ticket({ id: "T-1" }), ticket({ id: "T-2" }), ticket({ id: "T-3" })]);
  delta({ id: "T-1", canRetry: true });
  delta({ id: "T-2", stage: "ended", outcome: "completed" });
  delta({ id: "T-3", stage: "ended", outcome: "dropped" });
  expect(events()).toEqual(["T-3 Dropped", "T-2 Finished", "T-1 Stalled"]);
});

test("the feed keeps the newest 30, and a sign-out empties it", () => {
  off = connectActivity();
  snapshot([]);
  for (let i = 0; i < MAX_ACTIVITY + 5; i++) delta({ id: `T-${i}` });
  const items = useActivity.getState().items;
  expect(items).toHaveLength(MAX_ACTIVITY);
  expect(items[0].ticketId).toBe(`T-${MAX_ACTIVITY + 4}`);
  useAthena.getState().reset();
  expect(useActivity.getState().items).toEqual([]);
});
