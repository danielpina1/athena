import { create } from "@bufbuild/protobuf";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { IDBFactory } from "fake-indexeddb";
import { afterEach, expect, test, vi } from "vitest";
import { CommandResultSchema, type Command } from "@/gen/athena/v1/commands_pb";
import { bindSession } from "@/lib/auth/session";
import { ASSERTION, fakeCredentials, fakeFetch } from "@/lib/auth/testkit";
import { Outbox } from "@/lib/commands/outbox";
import { cancelReauth, ReauthDialog, requestReauth } from "./ReauthDialog";

afterEach(() => {
  cancelReauth();
  vi.unstubAllGlobals();
});

const reauthRoutes = {
  "POST /auth/reauth/begin": { json: ASSERTION },
  "POST /auth/reauth/finish": { json: { reauthValidUntil: "2026-11-04T12:30:00Z" } },
};

test("reauth dialog resends the held command", async () => {
  bindSession({ outbox: { clear: vi.fn() } });
  const sent: Command[] = [];
  const outbox = new Outbox(new IDBFactory(), (cmd) => (sent.push(cmd), true));
  outbox.setReauthHandler(requestReauth);
  outbox.onOpen();
  const { calls } = fakeFetch(reauthRoutes);
  fakeCredentials();
  const user = userEvent.setup();
  render(<ReauthDialog />);
  expect(screen.queryByRole("dialog")).toBeNull();

  const result = outbox.submit({ case: "revokePasskey", value: { id: "P-1" } });
  await outbox.settled();
  const id = sent[0].id;
  outbox.onResult(create(CommandResultSchema, { id, ok: false, code: "reauth_required" }));

  // One sentence on why, and nothing is resent while the owner is at the prompt.
  const dialog = await screen.findByRole("dialog", { name: "Confirm it is you" });
  expect(dialog.textContent).toContain("asks for your passkey again");
  expect(sent).toHaveLength(1);

  await user.click(screen.getByRole("button", { name: "Confirm with passkey" }));
  await waitFor(() => expect(sent).toHaveLength(2));
  expect(sent[1].id).toBe(id);
  expect(calls.map((c) => c.path)).toEqual(["/auth/reauth/begin", "/auth/reauth/finish"]);
  await waitFor(() => expect((screen.queryByRole("dialog") as HTMLDialogElement | null)?.open ?? false).toBe(false));

  outbox.onResult(create(CommandResultSchema, { id, ok: true }));
  expect((await result).ok).toBe(true);
});

test("cancelling the step-up fails the held command, a refused ceremony can be tried again", async () => {
  bindSession({ outbox: { clear: vi.fn() } });
  fakeFetch(reauthRoutes);
  let n = 0;
  fakeCredentials({
    get: () => {
      if (n++ === 0) throw new Error("boom");
      return { id: "Y3JlZA", rawId: new ArrayBuffer(1), type: "public-key", response: { clientDataJSON: new ArrayBuffer(1) }, getClientExtensionResults: () => ({}) };
    },
  });
  const user = userEvent.setup();
  render(<ReauthDialog />);
  const first = requestReauth();
  // A second ask while the first is open shares its answer.
  expect(requestReauth()).toBe(first);
  await screen.findByRole("dialog", { name: "Confirm it is you" });
  await user.click(screen.getByRole("button", { name: "Confirm with passkey" }));
  expect((await screen.findByRole("alert")).textContent).toContain("Could not confirm");
  await user.click(screen.getByRole("button", { name: "Confirm with passkey" }));
  expect(await first).toBe(true);

  const second = requestReauth();
  await screen.findByRole("dialog", { name: "Confirm it is you" });
  await user.click(screen.getByRole("button", { name: "Cancel" }));
  expect(await second).toBe(false);
});
