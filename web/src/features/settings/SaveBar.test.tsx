import { render, screen } from "@testing-library/react";
import { expect, test, vi } from "vitest";
import { SaveBar, type SaveStatus } from "./SaveBar";

function bar(dirty: boolean, status: SaveStatus, error?: string) {
  return (
    <SaveBar
      label="General"
      dirty={dirty}
      status={status}
      error={error}
      onSave={vi.fn()}
      onDiscard={vi.fn()}
      onReload={vi.fn()}
      onOverwrite={vi.fn()}
    />
  );
}

const region = () => screen.queryByRole("region", { name: "Save General" });

test("save bar only while dirty, saving or failed", () => {
  const { rerender } = render(bar(false, "idle"));
  expect(region()).toBeNull();

  rerender(bar(true, "idle"));
  expect(region()).not.toBeNull();
  expect(screen.getByText("Unsaved changes")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Save" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Discard" })).toBeTruthy();

  rerender(bar(true, "saving"));
  expect(screen.getByText("Saving…")).toBeTruthy();
  expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);

  rerender(bar(true, "failed", "The server is busy. Try again in a moment."));
  expect(screen.getByText("The server is busy. Try again in a moment.")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Try again" })).toBeTruthy();

  rerender(bar(true, "conflict"));
  expect(screen.getByText("Changed on another device")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Reload" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Overwrite" })).toBeTruthy();

  // Saved, the Delta not here yet: nothing to say.
  rerender(bar(false, "saved"));
  expect(region()).toBeNull();
  // Saved and then edited again: dirty once more.
  rerender(bar(true, "saved"));
  expect(region()).not.toBeNull();
});
