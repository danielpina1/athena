import { create } from "@bufbuild/protobuf";
import { act, render, renderHook, screen } from "@testing-library/react";
import { expect, test, vi } from "vitest";
import { CommandResultSchema } from "@/gen/athena/v1/commands_pb";
import { SaveBar, type SaveStatus, useSaveBlock } from "./SaveBar";

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

test("a decision that keeps a value builds the next edit on its own revision, not an older save's", async () => {
  const ok = (rev?: number) =>
    create(CommandResultSchema, { ok: true, result: rev === undefined ? { case: undefined } : { case: "policyRevision", value: rev } });
  const send = vi.fn<(v: string, base: number) => Promise<ReturnType<typeof ok>>>(async () => ok(5));
  const { result, rerender } = renderHook((p: { server: string; revision: number }) =>
    useSaveBlock({ label: "Block", server: p.server, revision: p.revision, equal: (a, b) => a === b, send }),
  { initialProps: { server: "a", revision: 1 } });

  // A save that made revision 5, whose Delta came; later changes moved the server to revision 7.
  act(() => result.current.edit("b"));
  await act(async () => result.current.bar.onSave());
  rerender({ server: "b", revision: 5 });
  rerender({ server: "b", revision: 7 });
  // A decision on revision 7 that keeps the owner's text (held): saved, its Delta not here yet.
  await act(async () => {
    await result.current.act(async () => ok(), "c");
  });
  // An edit now builds on revision 7, where the decision was made: never on the older save's 5.
  send.mockClear();
  act(() => result.current.edit("d"));
  await act(async () => result.current.bar.onSave());
  expect(send).toHaveBeenCalledWith("d", 7);
});
