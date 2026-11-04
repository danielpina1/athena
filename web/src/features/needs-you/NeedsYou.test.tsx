import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { CommandResult } from "@/gen/athena/v1/commands_pb";
import { DeltaSchema, NodeStatusSchema, SnapshotSchema } from "@/gen/athena/v1/state_pb";
import * as actions from "@/lib/commands/actions";
import { useAthena } from "@/lib/store/state";
import { Toaster } from "@/ui/primitives/toast";
import { NeedsYou } from "./NeedsYou";

vi.mock("@/lib/commands/actions", async (orig) => ({
  ...(await orig<typeof import("@/lib/commands/actions")>()),
  answerApproval: vi.fn(),
  reply: vi.fn(),
  retry: vi.fn(),
  stop: vi.fn(),
}));

const answerApproval = vi.mocked(actions.answerApproval);
const reply = vi.mocked(actions.reply);
const retry = vi.mocked(actions.retry);
const stop = vi.mocked(actions.stop);

type SnapshotInit = MessageInitShape<typeof SnapshotSchema>;
const ok = { ok: true, code: "" } as CommandResult;
const at = (ms: number) => timestampFromMs(Date.parse("2026-11-04T09:00:00Z") + ms);

function load(init: SnapshotInit) {
  useAthena.getState().applySnapshot(create(SnapshotSchema, { cursor: 1n, ...init }), "e1");
}

const approvalTicket = {
  id: "T-1",
  title: "Deploy the staging build",
  stage: "person",
  action: "approval",
  repo: "acme/api",
  model: "opus",
  pendingApprovalId: "A-1",
  updatedAt: at(0),
};
const approval = {
  id: "A-1",
  ticketId: "T-1",
  tool: "Bash",
  summary: "Run make deploy",
  rule: "bash:deploy",
  inputRedacted: '{"command":"make deploy"}',
  state: "pending",
  createdAt: at(0),
};
const answerTicket = {
  id: "T-2",
  title: "Pick a database",
  stage: "person",
  action: "answer",
  dir: "/work/shop",
  model: "sonnet",
  attentionQuestion: "Postgres or SQLite?",
  updatedAt: at(1000),
};

function renderPage() {
  return render(
    <MemoryRouter>
      <NeedsYou />
      <Toaster />
    </MemoryRouter>,
  );
}

/** The command's optimistic change is made by the real store; the result is the test's to settle. */
function answerWith(result?: (undo: () => void) => CommandResult) {
  answerApproval.mockImplementation((id, approve) => {
    const change = useAthena.getState().markApprovalAnswered(id, approve);
    return Promise.resolve().then(() => (result ? result(change.undo) : new Promise<CommandResult>(() => {})));
  });
}

beforeEach(() => {
  useAthena.getState().reset();
  vi.clearAllMocks();
  retry.mockResolvedValue(ok);
  stop.mockResolvedValue(ok);
  reply.mockResolvedValue(ok);
});
afterEach(() => vi.useRealTimers());

test("approval approve calls answerApproval once and shows Approved", async () => {
  const user = userEvent.setup();
  load({ tickets: [approvalTicket, answerTicket], approvals: [approval] });
  answerWith();
  renderPage();

  const card = screen.getByRole("group", { name: "Deploy the staging build" });
  expect(within(card).getByText("Bash")).toBeTruthy();
  expect(within(card).getByText("Run make deploy")).toBeTruthy();
  expect(within(card).getByText(/bash:deploy/)).toBeTruthy();
  expect(within(card).getByText('{"command":"make deploy"}')).toBeTruthy();
  expect(screen.getByText(/^2 need you/)).toBeTruthy();

  await user.click(within(card).getByRole("button", { name: "Approve" }));
  expect(answerApproval).toHaveBeenCalledTimes(1);
  expect(answerApproval).toHaveBeenCalledWith("A-1", true, "");

  // R40: the header drops at once; the card stays, answered, and cannot be answered again.
  expect(screen.getByText(/^1 need you/)).toBeTruthy();
  expect(within(card).getByText("Approved")).toBeTruthy();
  expect(within(card).queryByRole("button", { name: "Approve" })).toBeNull();
  expect(within(card).queryByRole("button", { name: "Deny" })).toBeNull();
});

test("the answered card leaves when its Delta lands", async () => {
  const user = userEvent.setup();
  load({ tickets: [approvalTicket], approvals: [approval] });
  answerWith();
  renderPage();
  await user.click(screen.getByRole("button", { name: "Approve" }));
  expect(screen.getByText("Approved")).toBeTruthy();

  act(() => {
    useAthena.getState().applyDelta(
      create(DeltaSchema, {
        seq: 2n,
        change: { case: "approval", value: { ...approval, state: "approved", answeredAt: at(5000) } },
      }),
    );
  });
  expect(screen.queryByText("Approved")).toBeNull();
  act(() => {
    useAthena.getState().applyDelta(
      create(DeltaSchema, {
        seq: 3n,
        change: { case: "ticket", value: { ...approvalTicket, stage: "working", action: "", pendingApprovalId: "" } },
      }),
    );
  });
  expect(screen.queryByRole("group")).toBeNull();
  expect(screen.getByText("Nothing needs you.")).toBeTruthy();
});

test("a failed approval brings the card back with the error", async () => {
  const user = userEvent.setup();
  load({ tickets: [approvalTicket], approvals: [approval] });
  answerWith((undo) => {
    undo();
    return { ok: false, code: "unavailable" } as CommandResult;
  });
  renderPage();
  await user.click(screen.getByRole("button", { name: "Approve" }));

  await waitFor(() => expect(screen.getByText("Could not approve")).toBeTruthy());
  expect(screen.getByText("The server is busy. Try again in a moment.")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Approve" })).toBeTruthy();
  expect(screen.queryByText("Approved")).toBeNull();
  expect(screen.getByText(/^1 need you/)).toBeTruthy();
});

test("deny with note sends the note", async () => {
  const user = userEvent.setup();
  load({ tickets: [approvalTicket], approvals: [approval] });
  answerWith();
  renderPage();

  await user.type(screen.getByRole("textbox", { name: "Note (optional)" }), "not on a Friday");
  await user.click(screen.getByRole("button", { name: "Deny" }));
  expect(answerApproval).toHaveBeenCalledWith("A-1", false, "not on a Friday");
  expect(screen.getByText("Denied")).toBeTruthy();
});

test("answer composer sends on Enter, newline on Shift+Enter", async () => {
  const user = userEvent.setup();
  load({ tickets: [answerTicket] });
  renderPage();

  expect(screen.getByText("Postgres or SQLite?")).toBeTruthy();
  const box = screen.getByRole("textbox", { name: "Your answer" }) as HTMLTextAreaElement;
  await user.type(box, "SQLite{Shift>}{Enter}{/Shift}for now");
  expect(reply).not.toHaveBeenCalled();
  expect(box.value).toBe("SQLite\nfor now");

  await user.type(box, "{Enter}");
  expect(reply).toHaveBeenCalledTimes(1);
  expect(reply).toHaveBeenCalledWith("T-2", "SQLite\nfor now");
  await waitFor(() => expect(screen.getByText("Answer sent")).toBeTruthy());
});

test("an empty answer is not sent", async () => {
  const user = userEvent.setup();
  load({ tickets: [answerTicket] });
  renderPage();
  await user.type(screen.getByRole("textbox", { name: "Your answer" }), "   {Enter}");
  expect(reply).not.toHaveBeenCalled();
});

test("a failed answer keeps the text and says why", async () => {
  const user = userEvent.setup();
  load({ tickets: [answerTicket] });
  reply.mockResolvedValue({ ok: false, code: "not_found" } as CommandResult);
  renderPage();
  const box = screen.getByRole("textbox", { name: "Your answer" }) as HTMLTextAreaElement;
  await user.type(box, "SQLite{Enter}");
  await waitFor(() => expect(screen.getByText("Answer not sent")).toBeTruthy());
  expect(box.value).toBe("SQLite");
});

test("retry only when can_retry", async () => {
  const user = userEvent.setup();
  load({
    tickets: [
      { id: "T-3", title: "Fails retryably", stage: "person", action: "failed", canRetry: true, updatedAt: at(0) },
      { id: "T-4", title: "Fails for good", stage: "person", action: "blocked", canRetry: false, updatedAt: at(1) },
    ],
  });
  renderPage();

  const retryable = screen.getByRole("group", { name: "Fails retryably" });
  const final = screen.getByRole("group", { name: "Fails for good" });
  expect(within(final).queryByRole("button", { name: "Retry" })).toBeNull();
  expect(within(final).getByRole("button", { name: "Stop" })).toBeTruthy();
  await user.click(within(retryable).getByRole("button", { name: "Retry" }));
  expect(retry).toHaveBeenCalledWith("T-3");
});

test("stop asks for confirmation", async () => {
  const user = userEvent.setup();
  load({
    tickets: [{ id: "T-5", title: "Ready one", stage: "person", action: "review", updatedAt: at(0) }],
  });
  renderPage();

  await user.click(screen.getByRole("button", { name: "Stop" }));
  expect(stop).not.toHaveBeenCalled();
  await new Promise((r) => setTimeout(r, 350));
  await user.click(screen.getByRole("button", { name: "Stop this Ticket" }));
  expect(stop).toHaveBeenCalledTimes(1);
  expect(stop.mock.calls[0][0]).toBe("T-5");
});

test("review links to the pull request or to the local diff", () => {
  load({
    tickets: [
      { id: "T-6", title: "With a PR", stage: "person", action: "review", prUrl: "https://github.com/acme/api/pull/7", updatedAt: at(0) },
      { id: "T-7", title: "Local only", stage: "person", action: "review", updatedAt: at(1) },
    ],
  });
  renderPage();

  const pr = within(screen.getByRole("group", { name: "With a PR" })).getByRole("link", { name: /pull request/i });
  expect(pr.getAttribute("href")).toBe("https://github.com/acme/api/pull/7");
  expect(pr.getAttribute("target")).toBe("_blank");
  expect(pr.getAttribute("rel")).toContain("noopener");

  const localCard = screen.getByRole("group", { name: "Local only" });
  expect(within(localCard).getByRole("link", { name: /changes/i }).getAttribute("href")).toBe("/tickets/T-7#diff");
  expect(within(localCard).getByText("Ready for review", { selector: "p" })).toBeTruthy();
});

test("keyboard a approves the focused card", async () => {
  const user = userEvent.setup();
  load({ tickets: [approvalTicket, answerTicket], approvals: [approval] });
  answerWith();
  renderPage();

  // Nothing is focused: a does nothing.
  await user.keyboard("a");
  expect(answerApproval).not.toHaveBeenCalled();

  await user.keyboard("j");
  expect(document.activeElement).toBe(screen.getByRole("group", { name: "Deploy the staging build" }));
  await user.keyboard("a");
  expect(answerApproval).toHaveBeenCalledTimes(1);
  expect(answerApproval).toHaveBeenCalledWith("A-1", true, "");
});

test("keyboard j and k move between cards, d denies and r focuses the composer", async () => {
  const user = userEvent.setup();
  load({ tickets: [approvalTicket, answerTicket], approvals: [approval] });
  answerWith();
  renderPage();

  const first = screen.getByRole("group", { name: "Deploy the staging build" });
  const second = screen.getByRole("group", { name: "Pick a database" });
  await user.keyboard("j");
  expect(document.activeElement).toBe(first);
  await user.keyboard("j");
  expect(document.activeElement).toBe(second);
  await user.keyboard("j");
  expect(document.activeElement).toBe(second);
  await user.keyboard("k");
  expect(document.activeElement).toBe(first);
  await user.keyboard("j");
  await user.keyboard("r");
  expect(document.activeElement).toBe(screen.getByRole("textbox", { name: "Your answer" }));
  // Typing in the composer fires no shortcut.
  await user.keyboard("jkad");
  expect((screen.getByRole("textbox", { name: "Your answer" }) as HTMLTextAreaElement).value).toBe("jkad");
  expect(answerApproval).not.toHaveBeenCalled();

  first.focus();
  await user.keyboard("d");
  expect(answerApproval).toHaveBeenCalledWith("A-1", false, "");
});

test("keys typed in the note field do not answer", async () => {
  const user = userEvent.setup();
  load({ tickets: [approvalTicket], approvals: [approval] });
  answerWith();
  renderPage();
  await user.type(screen.getByRole("textbox", { name: "Note (optional)" }), "add");
  expect(answerApproval).not.toHaveBeenCalled();
});

test("header line names the count, node and load", () => {
  load({
    tickets: [approvalTicket, answerTicket],
    approvals: [approval],
    node: create(NodeStatusSchema, { name: "mac-studio", slots: 4, slotsInUse: 2 }),
  });
  renderPage();
  expect(screen.getByRole("heading", { level: 1, name: "Needs you" })).toBeTruthy();
  expect(screen.getByText("2 need you · mac-studio · 2 of 4 running")).toBeTruthy();
});

test("empty state copy", () => {
  const now = Date.now();
  load({
    tickets: [
      { id: "T-8", title: "Busy", stage: "working" },
      { id: "T-9", title: "Done", stage: "ended", outcome: "merged", endedAt: timestampFromMs(now), updatedAt: timestampFromMs(now) },
    ],
    node: create(NodeStatusSchema, { name: "mac-studio", slots: 4, slotsInUse: 1 }),
  });
  renderPage();
  expect(screen.getByText("Nothing needs you.")).toBeTruthy();
  expect(screen.getByText("Today: 1 working, 1 ended, 1 merged.")).toBeTruthy();
  expect(screen.getByText("0 need you · mac-studio · 1 of 4 running")).toBeTruthy();
});

test("a pr_url that is not an https GitHub address is not linked", () => {
  load({
    tickets: [
      { id: "T-10", title: "Script url", stage: "person", action: "review", prUrl: "javascript:alert(1)", updatedAt: at(0) },
      { id: "T-11", title: "Other host", stage: "person", action: "review", prUrl: "https://evil.example/acme/api/pull/7", updatedAt: at(1) },
      { id: "T-12", title: "Plain http", stage: "person", action: "review", prUrl: "http://github.com/acme/api/pull/7", updatedAt: at(2) },
    ],
  });
  renderPage();
  for (const [title, id] of [["Script url", "T-10"], ["Other host", "T-11"], ["Plain http", "T-12"]]) {
    const card = screen.getByRole("group", { name: title });
    expect(within(card).queryByRole("link", { name: /pull request/i })).toBeNull();
    expect(within(card).getByRole("link", { name: /changes/i }).getAttribute("href")).toBe(`/tickets/${id}#diff`);
  }
});

test("two Enters before a re-render send one answer", async () => {
  load({ tickets: [answerTicket] });
  reply.mockImplementation(() => new Promise(() => {}));
  renderPage();
  const box = screen.getByRole("textbox", { name: "Your answer" });
  const user = userEvent.setup();
  await user.type(box, "SQLite");
  act(() => {
    fireEvent.keyDown(box, { key: "Enter" });
    fireEvent.keyDown(box, { key: "Enter" });
  });
  expect(reply).toHaveBeenCalledTimes(1);
});

test("a held Enter does not send", async () => {
  load({ tickets: [answerTicket] });
  renderPage();
  const box = screen.getByRole("textbox", { name: "Your answer" });
  await userEvent.setup().type(box, "SQLite");
  fireEvent.keyDown(box, { key: "Enter", repeat: true });
  expect(reply).not.toHaveBeenCalled();
});

test("an answering approval and a new pending one on the same Ticket both render", async () => {
  const user = userEvent.setup();
  const err = vi.spyOn(console, "error").mockImplementation(() => {});
  load({ tickets: [approvalTicket], approvals: [approval] });
  answerWith();
  renderPage();
  await user.click(screen.getByRole("button", { name: "Approve" }));
  act(() => {
    useAthena.getState().applyDelta(
      create(DeltaSchema, {
        seq: 2n,
        change: { case: "approval", value: { ...approval, id: "A-2", summary: "Run make deploy again", createdAt: at(9000) } },
      }),
    );
    useAthena.getState().applyDelta(
      create(DeltaSchema, {
        seq: 3n,
        change: { case: "ticket", value: { ...approvalTicket, pendingApprovalId: "A-2" } },
      }),
    );
  });
  expect(screen.getAllByRole("group")).toHaveLength(2);
  expect(screen.getByText("Approved")).toBeTruthy();
  expect(screen.getByText("Run make deploy again")).toBeTruthy();
  expect(err.mock.calls.filter((c) => String(c[0]).includes("same key"))).toEqual([]);
  err.mockRestore();
});

test("a stalled card labels the last message and claims no cause otherwise", () => {
  load({
    tickets: [
      { id: "T-13", title: "Has text", stage: "person", action: "failed", attentionQuestion: "tests are red", updatedAt: at(0) },
      { id: "T-14", title: "No text", stage: "person", action: "delivery", updatedAt: at(1) },
    ],
  });
  renderPage();
  const withText = screen.getByRole("group", { name: "Has text" });
  expect(within(withText).getByText("Last message")).toBeTruthy();
  expect(within(withText).getByText("tests are red")).toBeTruthy();
  const bare = screen.getByRole("group", { name: "No text" });
  expect(within(bare).queryByText("Last message")).toBeNull();
  expect(within(bare).getByText("Delivery failed. Open the Ticket to see what happened.")).toBeTruthy();
});
