import { create } from "@bufbuild/protobuf";
import { IDBFactory } from "fake-indexeddb";
import { afterEach, beforeEach, describe, expect, test } from "vitest";
import { CommandResultSchema, type Command } from "@/gen/athena/v1/commands_pb";
import { DeltaSchema, SnapshotSchema } from "@/gen/athena/v1/state_pb";
import { selectAnswering } from "@/lib/store/selectors";
import { useAthena } from "@/lib/store/state";
import { stageLabel } from "@/ui/athena/StageBadge";
import * as actions from "./actions";
import { Outbox, type Action } from "./outbox";

let outbox: Outbox;
let sent: Command[];
const st = () => useAthena.getState();
const last = () => sent.at(-1)!;
const answer = (code?: string) =>
  outbox.onResult(create(CommandResultSchema, { id: last().id, ok: code === undefined, code: code ?? "" }));

beforeEach(() => {
  st().reset();
  sent = [];
  outbox = new Outbox(new IDBFactory(), (c) => {
    sent.push(c);
    return true;
  });
  outbox.onOpen();
  actions.setOutbox(outbox);
});

afterEach(() => actions.setOutbox(undefined));

describe("actions", () => {
  test("each helper sends its command", async () => {
    const sub = { endpoint: "https://push.example/1", keys: { p256dh: "pk", auth: "au" } };
    const calls: [() => Promise<unknown>, NonNullable<Action>][] = [
      [() => actions.submitTask({ text: "fix it", repo: "o/r" }), { case: "submitTask", value: { text: "fix it", repo: "o/r", dir: "", model: "" } }],
      [() => actions.reply("T-1", "yes"), { case: "reply", value: { ticketId: "T-1", text: "yes" } }],
      [() => actions.stop("T-1", "wrong repo"), { case: "stop", value: { ticketId: "T-1", reason: "wrong repo" } }],
      [() => actions.retry("T-1"), { case: "retry", value: { ticketId: "T-1" } }],
      [() => actions.interrupt("T-1"), { case: "interrupt", value: { ticketId: "T-1" } }],
      [() => actions.answerApproval("A-1", false, "no"), { case: "answerApproval", value: { approvalId: "A-1", approve: false, note: "no" } }],
      [() => actions.reevaluate("G-1"), { case: "reevaluate", value: { triggerId: "G-1" } }],
      [() => actions.dismiss("D-1"), { case: "dismiss", value: { decisionId: "D-1" } }],
      [() => actions.unsubscribe("github:o/r#1"), { case: "unsubscribe", value: { thread: "github:o/r#1" } }],
      [() => actions.setPolicy(4, '{"a":1}'), { case: "setPolicy", value: { baseRevision: 4, settingsJson: '{"a":1}' } }],
      [() => actions.watch("T-1"), { case: "watchTicket", value: { ticketId: "T-1" } }],
      [() => actions.unwatch("T-1"), { case: "unwatchTicket", value: { ticketId: "T-1" } }],
      [() => actions.pushSubscribe(sub), { case: "pushSubscribe", value: { endpoint: sub.endpoint, p256dh: "pk", auth: "au" } }],
      [() => actions.pushUnsubscribe(sub.endpoint), { case: "pushUnsubscribe", value: { endpoint: sub.endpoint } }],
      [() => actions.setMuted(["review"]), { case: "setNotificationPrefs", value: { muted: ["review"] } }],
      [() => actions.visibility(true), { case: "visibility", value: { visible: true } }],
      [() => actions.listPasskeys(), { case: "listPasskeys", value: {} }],
      [() => actions.revokePasskey("P-1"), { case: "revokePasskey", value: { id: "P-1" } }],
      [() => actions.getPrompt("worker"), { case: "getPrompt", value: { part: "worker" } }],
      [() => actions.setPrompt("worker", 2, "be brief"), { case: "setPrompt", value: { part: "worker", baseRevision: 2, text: "be brief" } }],
      [() => actions.resetPrompt("worker", 3), { case: "resetPrompt", value: { part: "worker", baseRevision: 3 } }],
    ];
    for (const [call, want] of calls) {
      const p = call();
      await outbox.settled();
      expect(last().action.case).toBe(want?.case);
      expect(last().action.value).toMatchObject(want?.value ?? {});
      answer();
      await expect(p).resolves.toMatchObject({ ok: true, id: last().id });
    }
    expect(new Set(sent.map((c) => c.id)).size).toBe(sent.length);
  });

  describe("answerApproval", () => {
    beforeEach(() => {
      st().applySnapshot(
        create(SnapshotSchema, {
          cursor: 1n,
          tickets: [{ id: "T-1", stage: "person", action: "approval", pendingApprovalId: "A-1" }],
          approvals: [{ id: "A-1", ticketId: "T-1", state: "pending" }],
        }),
        "e1",
      );
    });

    test("marks the approval answered at once", async () => {
      const p = actions.answerApproval("A-1", true);
      expect(st().approvals.get("A-1")?.state).toBe("approved");
      expect(st().approvals.get("A-1")?.answeredAt).toBeDefined();
      expect(st().tickets.get("T-1")?.pendingApprovalId).toBe("");
      await outbox.settled();
      answer();
      await p;
      expect(st().approvals.get("A-1")?.state).toBe("approved");
    });

    test("failure rolls back the optimistic change", async () => {
      const p = actions.answerApproval("A-1", false);
      expect(st().approvals.get("A-1")?.state).toBe("denied");
      await outbox.settled();
      answer("conflict");
      await expect(p).resolves.toMatchObject({ ok: false, code: "conflict" });
      expect(st().approvals.get("A-1")?.state).toBe("pending");
      expect(st().approvals.get("A-1")?.answeredAt).toBeUndefined();
      expect(st().tickets.get("T-1")?.pendingApprovalId).toBe("A-1");
    });

    const prior = () => {
      expect(st().approvals.get("A-1")?.state).toBe("pending");
      expect(st().approvals.get("A-1")?.answeredAt).toBeUndefined();
      expect(st().tickets.get("T-1")?.pendingApprovalId).toBe("A-1");
      expect(selectAnswering(st(), "A-1")).toBeUndefined();
    };

    test("two answers to one approval that both fail restore the prior state", async () => {
      const p1 = actions.answerApproval("A-1", true);
      const p2 = actions.answerApproval("A-1", true);
      await outbox.settled();
      const [c1, c2] = sent.slice(-2);
      outbox.onResult(create(CommandResultSchema, { id: c1.id, ok: false, code: "conflict" }));
      outbox.onResult(create(CommandResultSchema, { id: c2.id, ok: false, code: "conflict" }));
      await Promise.all([p1, p2]);
      prior();
    });

    test("two answers that fail by clear restore the prior state", async () => {
      void actions.answerApproval("A-1", true);
      void actions.answerApproval("A-1", false);
      await outbox.settled();
      outbox.clear();
      prior();
    });

    test("an answer in flight survives a new-epoch Snapshot that still says pending", async () => {
      const p = actions.answerApproval("A-1", false);
      expect(selectAnswering(st(), "A-1")).toBe(false);
      st().applySnapshot(
        create(SnapshotSchema, {
          cursor: 1n,
          tickets: [{ id: "T-1", stage: "person", action: "approval", pendingApprovalId: "A-1" }],
          approvals: [{ id: "A-1", ticketId: "T-1", state: "pending" }],
        }),
        "e2",
      );
      expect(st().approvals.get("A-1")?.state).toBe("denied");
      expect(st().tickets.get("T-1")?.pendingApprovalId).toBe("");
      expect(selectAnswering(st(), "A-1")).toBe(false);
      await outbox.settled();
      answer("conflict");
      await p;
      prior(); // the rollback undoes the re-applied guess too
    });

    test("a Snapshot that says answered ends the answer in flight", () => {
      void actions.answerApproval("A-1", true);
      st().applySnapshot(
        create(SnapshotSchema, { cursor: 4n, approvals: [{ id: "A-1", ticketId: "T-1", state: "denied" }] }),
        "e2",
      );
      expect(selectAnswering(st(), "A-1")).toBeUndefined();
      expect(st().approvals.get("A-1")?.state).toBe("denied");
    });

    test("the result clears the answer in flight and keeps the guess", async () => {
      const p = actions.answerApproval("A-1", true);
      await outbox.settled();
      answer();
      await p;
      expect(selectAnswering(st(), "A-1")).toBeUndefined();
      expect(st().approvals.get("A-1")?.state).toBe("approved");
    });

    test("a Delta that came first is the truth: the rollback leaves it", async () => {
      const p = actions.answerApproval("A-1", true);
      await outbox.settled();
      // Another device answered: the server says denied, and the Ticket moved on.
      st().applyDelta(create(DeltaSchema, { seq: 2n, change: { case: "approval", value: { id: "A-1", ticketId: "T-1", state: "denied" } } }));
      st().applyDelta(create(DeltaSchema, { seq: 3n, change: { case: "ticket", value: { id: "T-1", stage: "working" } } }));
      answer("conflict");
      await p;
      expect(st().approvals.get("A-1")?.state).toBe("denied");
      expect(st().tickets.get("T-1")?.stage).toBe("working");
      expect(st().tickets.get("T-1")?.pendingApprovalId).toBe("");
    });
  });

  describe("stop", () => {
    beforeEach(() => {
      st().applySnapshot(create(SnapshotSchema, { cursor: 1n, tickets: [{ id: "T-1", stage: "working" }] }), "e1");
    });

    const label = () => {
      const t = st().tickets.get("T-1")!;
      return stageLabel({ stage: t.stage, action: t.action, outcome: t.outcome, stopping: st().stopping.has("T-1") });
    };

    test("shows the Ticket as Stopping until a Delta ends it", async () => {
      const p = actions.stop("T-1", "done");
      expect(label()).toBe("Stopping…");
      await outbox.settled();
      answer();
      await p;
      expect(label()).toBe("Stopping…"); // the result is not the truth; the Ticket's Delta is
      st().applyDelta(create(DeltaSchema, { seq: 2n, change: { case: "ticket", value: { id: "T-1", stage: "ended", outcome: "stopped" } } }));
      expect(st().stopping.has("T-1")).toBe(false);
      expect(label()).toBe("Stopped");
    });

    test("a refused stop goes back to the Ticket's stage", async () => {
      const p = actions.stop("T-1", "done");
      await outbox.settled();
      answer("not_found");
      await p;
      expect(label()).toBe("Working");
    });

    test("a Snapshot clears stopping for Tickets that ended or went away", () => {
      void actions.stop("T-1", "done");
      st().applySnapshot(create(SnapshotSchema, { cursor: 5n, tickets: [{ id: "T-1", stage: "ended", outcome: "stopped" }] }));
      expect(st().stopping.size).toBe(0);
    });
  });

  test("without an outbox a helper throws", () => {
    actions.setOutbox(undefined);
    expect(() => actions.retry("T-1")).toThrow(/outbox/);
  });
});

describe("errorMessage", () => {
  test("says each stable code in one plain sentence", () => {
    expect(actions.errorMessage("conflict")).toBe("Changed on another device. Reload to see the latest.");
    for (const code of ["unauthenticated", "reauth_required", "invalid", "not_found", "unavailable", "internal", "expired"]) {
      const m = actions.errorMessage(code);
      expect(m).toMatch(/^[A-Z].*\.$/);
      expect(m).not.toMatch(/_/);
      expect(m).not.toBe(actions.errorMessage("something_new"));
    }
    expect(actions.errorMessage("something_new")).toMatch(/^[A-Z].*\.$/);
  });
});
