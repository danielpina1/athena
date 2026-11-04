import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, test } from "vitest";
import { Button } from "@/ui/primitives/button";
import { Dialog, DialogContent, DialogDescription, DialogTitle, DialogTrigger } from "@/ui/primitives/dialog";

const setup = () =>
  render(
    <Dialog>
      <DialogTrigger asChild>
        <Button>Rename Ticket</Button>
      </DialogTrigger>
      <DialogContent>
        <DialogTitle>Rename Ticket</DialogTitle>
        <DialogDescription>The new title shows everywhere.</DialogDescription>
      </DialogContent>
    </Dialog>,
  );

test("a dialog is a native modal named by its title, and Esc returns focus to the opener", async () => {
  const user = userEvent.setup();
  setup();
  const trigger = screen.getByRole("button", { name: "Rename Ticket" });
  await user.click(trigger);

  const dialog = screen.getByRole("dialog", { name: "Rename Ticket" }) as HTMLDialogElement;
  expect(dialog.tagName).toBe("DIALOG");
  expect(dialog.open).toBe(true);
  expect(dialog.getAttribute("aria-describedby")).toBe(screen.getByText("The new title shows everywhere.").id);

  // The browser fires `cancel` on Esc.
  fireEvent(dialog, new Event("cancel", { cancelable: true }));
  expect(dialog.open).toBe(false);
  expect(document.activeElement).toBe(trigger);
});

test("a click on the backdrop closes the dialog", async () => {
  const user = userEvent.setup();
  setup();
  await user.click(screen.getByRole("button", { name: "Rename Ticket" }));
  const dialog = screen.getByRole("dialog") as HTMLDialogElement;
  await user.click(screen.getByText("The new title shows everywhere."));
  expect(dialog.open).toBe(true);
  fireEvent.click(dialog);
  expect(dialog.open).toBe(false);
});
