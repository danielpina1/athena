// Resume is sensitive: the server answers reauth_required, and the real outbox asks the owner for the passkey through
// the real re-auth dialog (boot.ts wires requestReauth the same way).
import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { IDBFactory } from "fake-indexeddb";
import { afterEach, expect, test } from "vitest";
import { CommandResultSchema, type Command } from "@/gen/athena/v1/commands_pb";
import { SnapshotSchema } from "@/gen/athena/v1/state_pb";
import { cancelReauth, ReauthDialog, requestReauth } from "@/features/auth/ReauthDialog";
import { setOutbox } from "@/lib/commands/actions";
import { Outbox } from "@/lib/commands/outbox";
import { useAthena } from "@/lib/store/state";
import { PausedBanner } from "./PausedBanner";

afterEach(() => {
  cancelReauth();
  setOutbox(undefined);
});

test("a Resume the server answers with reauth_required opens the re-auth dialog", async () => {
  const sent: Command[] = [];
  const outbox = new Outbox(new IDBFactory(), (cmd) => (sent.push(cmd), true));
  outbox.setReauthHandler(requestReauth);
  setOutbox(outbox);
  outbox.onOpen();
  useAthena.getState().reset();
  useAthena.getState().applySnapshot(
    create(SnapshotSchema, { cursor: 1n, control: { paused: true, by: "cli", since: timestampFromMs(Date.now() - 60_000) } }),
    "e1",
  );
  render(
    <>
      <PausedBanner />
      <ReauthDialog />
    </>,
  );
  expect(screen.queryByRole("dialog")).toBeNull();

  await userEvent.setup().click(screen.getByRole("button", { name: "Resume" }));
  await outbox.settled();
  expect(sent.map((c) => c.action.case)).toEqual(["resume"]);
  await act(async () => {
    outbox.onResult(create(CommandResultSchema, { id: sent[0].id, ok: false, code: "reauth_required" }));
    await outbox.settled();
  });
  expect(screen.getByRole("dialog", { name: "Confirm it is you" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Confirm with passkey" })).toBeTruthy();
});
