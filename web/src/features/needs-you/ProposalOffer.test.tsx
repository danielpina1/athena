import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { beforeEach, expect, test, vi } from "vitest";
import type { CommandResult } from "@/gen/athena/v1/commands_pb";
import { DeltaSchema, SnapshotSchema } from "@/gen/athena/v1/state_pb";
import * as actions from "@/lib/commands/actions";
import { selectNeedsYou } from "@/lib/store/selectors";
import { useAthena } from "@/lib/store/state";
import { NeedsYou } from "./NeedsYou";

vi.mock("@/lib/commands/actions", async (orig) => ({
  ...(await orig<typeof import("@/lib/commands/actions")>()),
  applyProposal: vi.fn(),
  discardProposal: vi.fn(),
  allowAction: vi.fn(),
  dismissOffer: vi.fn(),
}));

const ok = { ok: true, code: "" } as CommandResult;
const at = (ms: number) => timestampFromMs(Date.parse("2026-11-04T09:00:00Z") + ms);

const approvalTicket = { id: "T-1", title: "Deploy", stage: "person", action: "approval", pendingApprovalId: "A-1", updatedAt: at(0) };
const holdTicket = { id: "T-2", title: "Fix login", stage: "person", action: "proposal", repo: "acme/web", updatedAt: at(1000) };
const answerTicket = { id: "T-3", title: "Pick", stage: "person", action: "answer", attentionQuestion: "Which?", updatedAt: at(500) };
const approval = { id: "A-1", ticketId: "T-1", tool: "Bash", summary: "Run it", state: "pending", createdAt: at(0) };
const proposal = { id: "P-1", ticketId: "T-2", repo: "acme/web", kind: "reply", state: "pending", summary: "Reply on #4", detail: "Thanks, fixed.", createdAt: at(1000) };
const offer = { class: "bash|rule|acme/web", tool: "Bash", rule: "bash:make", repo: "acme/web", approved: 5 };

function load(init: object) {
  useAthena.getState().applySnapshot(create(SnapshotSchema, { cursor: 1n, ...init }), "e1");
}
const page = () =>
  render(
    <MemoryRouter>
      <NeedsYou />
    </MemoryRouter>,
  );

beforeEach(() => {
  useAthena.getState().reset();
  vi.clearAllMocks();
  for (const f of [actions.applyProposal, actions.discardProposal, actions.allowAction, actions.dismissOffer]) {
    vi.mocked(f).mockResolvedValue(ok);
  }
});

test("each kind of Proposal has its title, and Apply and Discard act on its id", async () => {
  const user = userEvent.setup();
  load({
    tickets: [holdTicket],
    proposals: [proposal, { ...proposal, id: "P-2", kind: "push", detail: "main @ abc1234" }, { ...proposal, id: "P-3", kind: "pull_request" }],
  });
  page();
  expect(screen.getByRole("group", { name: "Reply held for you" })).toBeTruthy();
  expect(screen.getByRole("group", { name: "Push held for you" })).toBeTruthy();
  expect(screen.getByRole("group", { name: "Pull request held for you" })).toBeTruthy();
  const card = screen.getByRole("group", { name: "Reply held for you" });
  expect(within(card).getByText("Thanks, fixed.")).toBeTruthy();
  expect(within(card).getAllByText("acme/web").length).toBeGreaterThan(0);
  expect(within(card).getByRole("link", { name: "T-2" })).toBeTruthy();
  await user.click(within(card).getByRole("button", { name: "Apply" }));
  expect(actions.applyProposal).toHaveBeenCalledWith("P-1");
  await user.click(within(card).getByRole("button", { name: "Discard" }));
  expect(actions.discardProposal).toHaveBeenCalledWith("P-1");
});

test("a failed Proposal shows its error and Try again applies it again", async () => {
  const user = userEvent.setup();
  load({ tickets: [holdTicket], proposals: [{ ...proposal, state: "failed", error: "push rejected" }] });
  page();
  expect(screen.getByText("push rejected")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Apply" })).toBeNull();
  await user.click(screen.getByRole("button", { name: "Try again" }));
  expect(actions.applyProposal).toHaveBeenCalledWith("P-1");
});

test("a Proposal Delta that applies it removes the card; offers replace the list", () => {
  load({ tickets: [holdTicket], proposals: [proposal] });
  page();
  expect(screen.getByRole("group", { name: "Reply held for you" })).toBeTruthy();
  act(() => {
    useAthena.getState().applyDelta(create(DeltaSchema, { seq: 2n, change: { case: "proposal", value: { ...proposal, state: "applied" } } }));
  });
  expect(screen.queryByRole("group", { name: "Reply held for you" })).toBeNull();
  act(() => {
    useAthena.getState().applyDelta(create(DeltaSchema, { seq: 3n, change: { case: "offers", value: { offers: [offer] } } }));
  });
  expect(screen.getByText(/Always allow it here\?/)).toBeTruthy();
});

test("the offer card reads as specified; Always allow and Not now send the class", async () => {
  const user = userEvent.setup();
  load({ offers: { offers: [offer] } });
  page();
  expect(screen.getByText(/You approved/).textContent).toBe("You approved Bash · bash:make in acme/web 5 times in a row. Always allow it here?");
  expect(screen.getByText("Writes a standing rule to your Policy. Never applies to work from outside your allow list.")).toBeTruthy();
  await user.click(screen.getByRole("button", { name: "Always allow" }));
  expect(actions.allowAction).toHaveBeenCalledWith("bash|rule|acme/web");
  await user.click(screen.getByRole("button", { name: "Not now" }));
  expect(actions.dismissOffer).toHaveBeenCalledWith("bash|rule|acme/web");
});

test("order: approvals, proposals, then the rest; offers last", () => {
  load({ tickets: [answerTicket, holdTicket, approvalTicket], approvals: [approval], proposals: [proposal], offers: { offers: [offer] } });
  expect(selectNeedsYou(useAthena.getState()).map((i) => i.kind)).toEqual(["approval", "proposal", "answer"]);
  page();
  const text = document.body.textContent ?? "";
  expect(text.indexOf("Reply held for you")).toBeLessThan(text.indexOf("Which?"));
  expect(text.indexOf("Which?")).toBeLessThan(text.indexOf("Always allow it here?"));
});
