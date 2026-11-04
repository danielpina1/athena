import { act, fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { expect, test } from "vitest";
import { Confirm } from "@/ui/athena/Confirm";
import { Button } from "@/ui/primitives/button";
import { Dialog, DialogContent, DialogDescription, DialogTitle, DialogTrigger } from "@/ui/primitives/dialog";
import { Sheet, SheetContent, SheetTitle, SheetTrigger } from "@/ui/primitives/sheet";
import { toast, Toaster } from "@/ui/primitives/toast";

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

const cancel = (dialog: HTMLElement) => fireEvent(dialog, new Event("cancel", { cancelable: true }));

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
  cancel(dialog);
  expect(dialog.open).toBe(false);
  expect(document.activeElement).toBe(trigger);
});

test("a click on the backdrop closes the dialog; a click inside does not", async () => {
  const user = userEvent.setup();
  setup();
  await user.click(screen.getByRole("button", { name: "Rename Ticket" }));
  const dialog = screen.getByRole("dialog") as HTMLDialogElement;
  await user.click(screen.getByText("The new title shows everywhere."));
  expect(dialog.open).toBe(true);
  fireEvent.pointerDown(dialog);
  fireEvent.click(dialog);
  expect(dialog.open).toBe(false);
});

test("a drag from inside the box onto the backdrop does not close the dialog", async () => {
  const user = userEvent.setup();
  setup();
  await user.click(screen.getByRole("button", { name: "Rename Ticket" }));
  const dialog = screen.getByRole("dialog") as HTMLDialogElement;
  // A selection starts on the text and is released over the backdrop: the click lands on the <dialog>.
  fireEvent.pointerDown(screen.getByText("The new title shows everywhere."));
  fireEvent.click(dialog);
  expect(dialog.open).toBe(true);
});

test("closing a dialog nested in a sheet leaves the sheet open", async () => {
  const user = userEvent.setup();
  render(
    <Sheet>
      <SheetTrigger asChild>
        <Button>Answer</Button>
      </SheetTrigger>
      <SheetContent>
        <SheetTitle>Answer the question</SheetTitle>
        <Dialog>
          <DialogTrigger asChild>
            <Button>Rename Ticket</Button>
          </DialogTrigger>
          <DialogContent>
            <DialogTitle>Rename Ticket</DialogTitle>
          </DialogContent>
        </Dialog>
      </SheetContent>
    </Sheet>,
  );
  await user.click(screen.getByRole("button", { name: "Answer" }));
  await user.click(screen.getByRole("button", { name: "Rename Ticket" }));
  const sheet = screen.getByRole("dialog", { name: "Answer the question" }) as HTMLDialogElement;
  const inner = screen.getByRole("dialog", { name: "Rename Ticket" }) as HTMLDialogElement;

  cancel(inner);
  expect(inner.open).toBe(false);
  expect(sheet.open).toBe(true);

  await user.click(screen.getByRole("button", { name: "Rename Ticket" }));
  const again = screen.getByRole("dialog", { name: "Rename Ticket" }) as HTMLDialogElement;
  fireEvent.pointerDown(again);
  fireEvent.click(again);
  expect(again.open).toBe(false);
  expect(sheet.open).toBe(true);

  // A close the browser makes on its own (it fires `close`) is also contained.
  await user.click(screen.getByRole("button", { name: "Rename Ticket" }));
  act(() => (screen.getByRole("dialog", { name: "Rename Ticket" }) as HTMLDialogElement).close());
  expect(sheet.open).toBe(true);
});

test("a dialog unmounted while open gives focus back to its opener and leaves the modal stack", async () => {
  const user = userEvent.setup();
  function Host() {
    const [open, setOpen] = useState(false);
    const [mounted, setMounted] = useState(true);
    return (
      <>
        <Button onClick={() => setOpen(true)}>Open</Button>
        {mounted && (
          <Dialog open={open} onOpenChange={setOpen}>
            <DialogContent>
              <DialogTitle>Gone soon</DialogTitle>
              <Button onClick={() => setMounted(false)}>Remove me</Button>
            </DialogContent>
          </Dialog>
        )}
      </>
    );
  }
  render(
    <>
      <Host />
      <Toaster />
    </>,
  );
  const opener = screen.getByRole("button", { name: "Open" });
  await user.click(opener);
  expect(screen.getByRole("dialog", { name: "Gone soon" })).toBeTruthy();
  await user.click(screen.getByRole("button", { name: "Remove me" }));
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(document.activeElement).toBe(opener);
  // The Toaster no longer targets the removed dialog.
  act(() => {
    toast({ title: "Still here" });
  });
  expect(screen.getByRole("region", { name: "Notifications" }).isConnected).toBe(true);
});

test("a toast raised while a sheet is open renders inside the sheet and is clickable", async () => {
  const user = userEvent.setup();
  let undone = 0;
  render(
    <>
      <Sheet>
        <SheetTrigger asChild>
          <Button>Answer</Button>
        </SheetTrigger>
        <SheetContent>
          <SheetTitle>Answer the question</SheetTitle>
          <Button
            onClick={() => toast({ title: "Answer queued", action: { label: "Undo", onClick: () => undone++ } })}
          >
            Send answer
          </Button>
        </SheetContent>
      </Sheet>
      <Toaster />
    </>,
  );
  await user.click(screen.getByRole("button", { name: "Answer" }));
  await user.click(screen.getByRole("button", { name: "Send answer" }));
  const sheet = screen.getByRole("dialog", { name: "Answer the question" });
  const region = screen.getByRole("region", { name: "Notifications" });
  // Inside the open modal <dialog>, so in the top layer, above the backdrop and not inert.
  expect(sheet.contains(region)).toBe(true);
  await user.click(within(region).getByRole("button", { name: "Undo" }));
  expect(undone).toBe(1);
  expect((sheet as HTMLDialogElement).open).toBe(true);

  cancel(sheet);
  expect(document.body.contains(screen.getByRole("region", { name: "Notifications" }))).toBe(true);
  expect(sheet.contains(screen.getByRole("region", { name: "Notifications" }))).toBe(false);
});

test("Esc on an armed Confirm inside a dialog disarms without cancelling the dialog", async () => {
  const user = userEvent.setup();
  render(
    <Dialog defaultOpen>
      <DialogContent>
        <DialogTitle>Ticket</DialogTitle>
        <Confirm label="Stop" confirmLabel="Stop this Ticket" onConfirm={() => {}} />
      </DialogContent>
    </Dialog>,
  );
  await user.click(screen.getByRole("button", { name: "Stop" }));
  const armed = screen.getByRole("button", { name: "Stop this Ticket" });
  // The browser cancels a modal only when Esc's keydown keeps its default.
  const keydown = new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true });
  act(() => {
    armed.dispatchEvent(keydown);
  });
  expect(keydown.defaultPrevented).toBe(true);
  expect(screen.getByRole("button", { name: "Stop" })).toBeTruthy();
});
