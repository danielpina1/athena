import { create } from "@bufbuild/protobuf";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { CommandResultSchema } from "@/gen/athena/v1/commands_pb";
import * as actions from "@/lib/commands/actions";
import { useAthena } from "@/lib/store/state";
import { load } from "@/features/tickets/fixtures";
import { ChildCard } from "./ChildCard";

vi.mock("@/lib/commands/actions", async (orig) => ({
  ...(await orig<typeof import("@/lib/commands/actions")>()),
  retry: vi.fn(),
  stop: vi.fn(),
  continueWithout: vi.fn(),
}));

const ok = create(CommandResultSchema, { ok: true });
const QUESTION = "Child T-2 (Lexer) failed: tests fail. Retry it, drop it, or continue without it.";

beforeEach(() => {
  useAthena.getState().reset();
  for (const f of [actions.retry, actions.stop, actions.continueWithout]) vi.mocked(f).mockResolvedValue(ok);
  load({
    tickets: [
      { id: "T-1", title: "Split the parser", stage: "person", action: "child", attentionChild: "T-2", attentionQuestion: QUESTION, children: ["T-2"] },
      { id: "T-2", title: "Lexer", stage: "person", action: "failed", lead: "T-1" },
    ],
  });
});
afterEach(() => vi.clearAllMocks());

function setup() {
  const user = userEvent.setup();
  render(
    <MemoryRouter>
      <ChildCard ticket={useAthena.getState().tickets.get("T-1")!} />
    </MemoryRouter>,
  );
  return user;
}

test("the card on the lead asks the lead's question", () => {
  setup();
  expect(screen.getByText(QUESTION)).toBeTruthy();
});

test("Retry retries the child, and Continue without it goes on without the child", async () => {
  const user = setup();
  await user.click(screen.getByRole("button", { name: "Retry" }));
  expect(actions.retry).toHaveBeenCalledWith("T-2");
  await user.click(screen.getByRole("button", { name: "Continue without it" }));
  expect(actions.continueWithout).toHaveBeenCalledWith("T-2");
  expect(actions.stop).not.toHaveBeenCalled();
});

test("Drop stops the child only once confirmed", async () => {
  const user = setup();
  await user.click(screen.getByRole("button", { name: "Drop" }));
  expect(actions.stop).not.toHaveBeenCalled();
  const confirm = screen.getByRole("button", { name: "Drop T-2? Child tickets that wait for it are dropped too." });
  // A click within the arming delay is a double-click, not the confirmation.
  await new Promise((r) => setTimeout(r, 350));
  await user.click(confirm);
  expect(actions.stop).toHaveBeenCalledWith("T-2", expect.any(String));
});
