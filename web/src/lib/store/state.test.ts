import { create } from "@bufbuild/protobuf";
import { beforeEach, describe, expect, test } from "vitest";
import { DeltaSchema, SnapshotSchema, type Delta } from "@/gen/athena/v1/state_pb";
import type { MessageInitShape } from "@bufbuild/protobuf";
import { useAthena } from "./state";
import { selectCounts, selectNeedsYou, selectSynced, selectTickets } from "./selectors";

const snap = (init: MessageInitShape<typeof SnapshotSchema>) => create(SnapshotSchema, init);
const delta = (init: MessageInitShape<typeof DeltaSchema>) => create(DeltaSchema, init);
const st = () => useAthena.getState();

beforeEach(() => st().reset());

describe("useAthena", () => {
  test("a snapshot replaces the state and keeps details", () => {
    st().applyDelta(delta({ seq: 0n, change: { case: "ticketDetail", value: { summary: { id: "T-1" } } } }));
    st().applySnapshot(snap({ cursor: 3n, tickets: [{ id: "T-0" }], approvals: [{ id: "A-0" }] }), "e1");
    st().applySnapshot(
      snap({
        cursor: 9n,
        tickets: [{ id: "T-1", title: "one" }, { id: "T-2" }],
        approvals: [{ id: "A-1", ticketId: "T-1", state: "pending" }],
        triggers: [{ id: "G-1" }],
        threads: [{ key: "github:o/r#1" }],
        decisions: [{ id: "D-1" }],
        node: { name: "n1", slots: 4 },
        health: { items: [{ name: "tls", ok: true }] },
        policy: { revision: 2, settingsJson: "{}" },
        repos: [{ name: "o/r", kind: "github" }],
        notifications: { kinds: ["approval"], muted: [] },
        prompts: { revision: 1, parts: [{ name: "worker" }] },
      }),
      "e2",
    );
    const s = st();
    expect(s.epoch).toBe("e2");
    expect(s.cursor).toBe(9n);
    expect([...s.tickets.keys()]).toEqual(["T-1", "T-2"]);
    expect(s.tickets.get("T-1")?.title).toBe("one");
    expect([...s.approvals.keys()]).toEqual(["A-1"]);
    expect([...s.triggers.keys()]).toEqual(["G-1"]);
    expect([...s.threads.keys()]).toEqual(["github:o/r#1"]);
    expect([...s.decisions.keys()]).toEqual(["D-1"]);
    expect(s.node?.slots).toBe(4);
    expect(s.health?.items[0].name).toBe("tls");
    expect(s.policy?.revision).toBe(2);
    expect(s.repos.map((r) => r.name)).toEqual(["o/r"]);
    expect(s.notifications?.kinds).toEqual(["approval"]);
    expect(s.prompts?.parts[0].name).toBe("worker");
    expect(s.details.has("T-1")).toBe(true);
  });

  test("a snapshot without an epoch keeps the current one", () => {
    st().applySnapshot(snap({ cursor: 1n }), "e1");
    st().applySnapshot(snap({ cursor: 2n }));
    expect(st().epoch).toBe("e1");
  });

  test("deltas upsert every kind and move the cursor", () => {
    st().applySnapshot(snap({ cursor: 0n }), "e1");
    const changes: MessageInitShape<typeof DeltaSchema>["change"][] = [
      { case: "ticket", value: { id: "T-1", stage: "working" } },
      { case: "approval", value: { id: "A-1" } },
      { case: "trigger", value: { id: "G-1" } },
      { case: "thread", value: { key: "k" } },
      { case: "decision", value: { id: "D-1" } },
      { case: "node", value: { name: "n" } },
      { case: "health", value: { items: [{ name: "push" }] } },
      { case: "policy", value: { revision: 5 } },
      { case: "repos", value: { repos: [{ name: "/srv/x", kind: "dir" }] } },
      { case: "notifications", value: { kinds: ["a", "b"] } },
      { case: "prompts", value: { revision: 7 } },
      { case: "ticket", value: { id: "T-1", stage: "idle" } },
    ];
    changes.forEach((change, i) => expect(st().applyDelta(delta({ seq: BigInt(i + 1), change }))).toBe("ok"));
    const s = st();
    expect(s.cursor).toBe(BigInt(changes.length));
    expect(s.tickets.get("T-1")?.stage).toBe("idle");
    expect(s.approvals.has("A-1")).toBe(true);
    expect(s.triggers.has("G-1")).toBe(true);
    expect(s.threads.has("k")).toBe(true);
    expect(s.decisions.has("D-1")).toBe(true);
    expect(s.node?.name).toBe("n");
    expect(s.health?.items[0].name).toBe("push");
    expect(s.policy?.revision).toBe(5);
    expect(s.repos.map((r) => r.name)).toEqual(["/srv/x"]);
    expect(s.notifications?.kinds).toEqual(["a", "b"]);
    expect(s.prompts?.revision).toBe(7);
  });

  test("a changed record is a new Map, so subscribers see it", () => {
    st().applySnapshot(snap({ cursor: 0n, tickets: [{ id: "T-1" }] }), "e1");
    const before = st().tickets;
    const approvals = st().approvals;
    st().applyDelta(delta({ seq: 1n, change: { case: "ticket", value: { id: "T-1", title: "new" } } }));
    expect(st().tickets).not.toBe(before);
    expect(before.get("T-1")?.title).toBe("");
    expect(st().approvals).toBe(approvals);
  });

  test("a gap is reported and not applied", () => {
    st().applySnapshot(snap({ cursor: 5n }), "e1");
    expect(st().applyDelta(delta({ seq: 7n, change: { case: "ticket", value: { id: "T-7" } } }))).toBe("gap");
    expect(st().cursor).toBe(5n);
    expect(st().tickets.size).toBe(0);
  });

  test("a delta at or below the cursor is ignored", () => {
    st().applySnapshot(snap({ cursor: 5n, tickets: [{ id: "T-1", title: "fresh" }] }), "e1");
    expect(st().applyDelta(delta({ seq: 5n, change: { case: "ticket", value: { id: "T-1", title: "stale" } } }))).toBe("ok");
    expect(st().tickets.get("T-1")?.title).toBe("fresh");
    expect(st().cursor).toBe(5n);
  });

  test("seq 0 details never move the cursor", () => {
    st().applySnapshot(snap({ cursor: 5n }), "e1");
    expect(st().applyDelta(delta({ seq: 0n, change: { case: "ticketDetail", value: { summary: { id: "T-1" }, threads: ["k"] } } }))).toBe("ok");
    expect(st().cursor).toBe(5n);
    expect(st().details.get("T-1")?.threads).toEqual(["k"]);
    expect(st().applyDelta(delta({ seq: 6n, change: { case: "ticket", value: { id: "T-1" } } }))).toBe("ok");
    expect(st().cursor).toBe(6n);
  });

  test("reset empties the store", () => {
    st().applySnapshot(snap({ cursor: 5n, tickets: [{ id: "T-1" }], node: { name: "n" } }), "e1");
    st().reset();
    const s = st();
    expect(s.epoch).toBe("");
    expect(s.cursor).toBe(0n);
    expect(s.tickets.size).toBe(0);
    expect(s.node).toBeUndefined();
    expect(s.details.size).toBe(0);
  });

  test("500 tickets apply under budget", () => {
    const tickets = Array.from({ length: 500 }, (_, i) => ({ id: `T-${i}`, title: `ticket ${i}`, stage: "working" }));
    st().applySnapshot(snap({ cursor: 0n, tickets }), "e1");
    const deltas: Delta[] = tickets.map((t, i) =>
      delta({ seq: BigInt(i + 1), change: { case: "ticket", value: { ...t, stage: i % 2 ? "idle" : "person", action: "answer" } } }),
    );
    // Warm up the JIT on a copy so the timed run measures steady state, then reset to the same starting point.
    for (const d of deltas) {
      st().applyDelta(d);
      selectCounts(st(), Date.now());
    }
    st().applySnapshot(snap({ cursor: 0n, tickets }), "e1");
    // Per Delta: apply it, then derive what a screen shows from it (the Needs you list, the counts, a Tickets list).
    const now = Date.now();
    const start = performance.now();
    for (const d of deltas) {
      expect(st().applyDelta(d)).toBe("ok");
      const s = st();
      selectNeedsYou(s);
      selectCounts(s, now);
      selectTickets(s, { stage: "open" });
    }
    const mean = (performance.now() - start) / deltas.length;
    // The budget is 16 ms to apply and render a Delta; a shared CI runner gets four times that.
    const budget = 16 * (process.env.CI ? 4 : 1);
    expect(mean).toBeLessThan(budget);
    expect(st().tickets.size).toBe(500);
    expect(st().cursor).toBe(500n);
    expect(selectNeedsYou(st())).toHaveLength(250);
  });
});

test("synced after a Snapshot, not before and not after reset", () => {
  useAthena.getState().reset();
  expect(selectSynced(useAthena.getState())).toBe(false);
  // A Delta alone (seq 0, a watched detail) is not the state: still not synced.
  useAthena.getState().applyDelta(create(DeltaSchema, { seq: 0n }));
  expect(selectSynced(useAthena.getState())).toBe(false);
  useAthena.getState().applySnapshot(create(SnapshotSchema, { cursor: 1n }), "e1");
  expect(selectSynced(useAthena.getState())).toBe(true);
  useAthena.getState().reset();
  expect(selectSynced(useAthena.getState())).toBe(false);
});
