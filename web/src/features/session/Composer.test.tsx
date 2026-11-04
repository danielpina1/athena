import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { CommandResult } from "@/gen/athena/v1/commands_pb";
import { create } from "@bufbuild/protobuf";
import { TicketSummarySchema } from "@/gen/athena/v1/state_pb";
import * as actions from "@/lib/commands/actions";
import { Toaster } from "@/ui/primitives/toast";
import { Composer, interruptBusy } from "./Composer";

vi.mock("@/lib/commands/actions", async (orig) => ({
  ...(await orig<typeof import("@/lib/commands/actions")>()),
  reply: vi.fn(),
  interrupt: vi.fn(),
}));

const ok = { ok: true, code: "" } as CommandResult;
const reply = vi.mocked(actions.reply);
const interrupt = vi.mocked(actions.interrupt);
const ticket = (stage: string, outcome = "") => create(TicketSummarySchema, { id: "T-1", title: "Ship it", stage, outcome });

beforeEach(() => {
  reply.mockResolvedValue(ok);
  interrupt.mockResolvedValue(ok);
});
afterEach(() => vi.clearAllMocks());

test("composer enter replies, cmd-enter interrupts", async () => {
  const user = userEvent.setup();
  render(
    <>
      <Composer ticket={ticket("working")} />
      <Toaster />
    </>,
  );
  const box = screen.getByRole("textbox", { name: "Reply to the session" });
  await user.type(box, "Use the v2 endpoint");
  await user.keyboard("{Meta>}{Enter}{/Meta}");
  expect(interrupt).toHaveBeenCalledWith("T-1");
  expect(reply).not.toHaveBeenCalled();
  expect((box as HTMLTextAreaElement).value).toBe("Use the v2 endpoint");
  await user.keyboard("{Control>}{Enter}{/Control}");
  expect(interrupt).toHaveBeenCalledTimes(2);

  // Shift+Enter is a new line; Enter sends.
  await user.keyboard("{Shift>}{Enter}{/Shift}please");
  expect(reply).not.toHaveBeenCalled();
  await user.keyboard("{Enter}");
  expect(reply).toHaveBeenCalledWith("T-1", "Use the v2 endpoint\nplease");
  await waitFor(() => expect((box as HTMLTextAreaElement).value).toBe(""));
});

test("text stays in the field until the result is ok", async () => {
  const user = userEvent.setup();
  reply.mockResolvedValueOnce({ ok: false, code: "unavailable" } as CommandResult);
  render(
    <>
      <Composer ticket={ticket("working")} />
      <Toaster />
    </>,
  );
  const box = screen.getByRole("textbox", { name: "Reply to the session" }) as HTMLTextAreaElement;
  await user.type(box, "Try again{Enter}");
  expect(await screen.findByText("Reply not sent")).toBeTruthy();
  expect(box.value).toBe("Try again");
  // A send in flight is not sent twice.
  let finish!: (r: CommandResult) => void;
  reply.mockReturnValueOnce(new Promise((r) => (finish = r)));
  await user.keyboard("{Enter}");
  await user.keyboard("{Enter}");
  expect(reply).toHaveBeenCalledTimes(2);
  expect(box.value).toBe("Try again");
  finish(ok);
  await waitFor(() => expect(box.value).toBe(""));
});

test("composer disabled after end with reason", () => {
  render(<Composer ticket={ticket("ended", "completed")} />);
  const box = screen.getByRole("textbox", { name: "Reply to the session" }) as HTMLTextAreaElement;
  expect(box.disabled).toBe(true);
  expect(screen.getByText("This Ticket has ended, so it takes no more replies.")).toBeTruthy();
  expect((screen.getByRole("button", { name: "Send" }) as HTMLButtonElement).disabled).toBe(true);
});

test("cmd-enter interrupts only while the Ticket works, once at a time", async () => {
  const user = userEvent.setup();
  const view = render(<Composer ticket={ticket("idle")} />);
  // The header has the one Interrupt button.
  expect(screen.queryByRole("button", { name: "Interrupt" })).toBeNull();
  await user.type(screen.getByRole("textbox"), "x");
  await user.keyboard("{Meta>}{Enter}{/Meta}");
  expect(interrupt).not.toHaveBeenCalled();
  view.rerender(<Composer ticket={ticket("working")} />);
  expect(screen.queryByRole("button", { name: "Interrupt" })).toBeNull();
  let finish!: (r: CommandResult) => void;
  interrupt.mockReturnValueOnce(new Promise((r) => (finish = r)));
  await user.keyboard("{Meta>}{Enter}{/Meta}");
  await user.keyboard("{Meta>}{Enter}{/Meta}");
  expect(interrupt).toHaveBeenCalledTimes(1);
  finish(ok);
  await waitFor(() => expect(interruptBusy("T-1")).toBe(false));
  await user.keyboard("{Meta>}{Enter}{/Meta}");
  expect(interrupt).toHaveBeenCalledTimes(2);
});

test("an Enter that commits an IME candidate does not send", async () => {
  const user = userEvent.setup();
  render(<Composer ticket={ticket("working")} />);
  const box = screen.getByRole("textbox", { name: "Reply to the session" });
  await user.type(box, "日本");
  // Safari ends the composition before this keydown, so only keyCode 229 tells.
  fireEvent.keyDown(box, { key: "Enter", keyCode: 229 });
  fireEvent.keyDown(box, { key: "Enter", isComposing: true });
  expect(reply).not.toHaveBeenCalled();
  fireEvent.keyDown(box, { key: "Enter", keyCode: 13 });
  expect(reply).toHaveBeenCalledWith("T-1", "日本");
});
