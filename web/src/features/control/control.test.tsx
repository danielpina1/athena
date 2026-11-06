import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { beforeEach, expect, test, vi } from "vitest";
import type { CommandResult } from "@/gen/athena/v1/commands_pb";
import { DeltaSchema, SnapshotSchema } from "@/gen/athena/v1/state_pb";
import * as actions from "@/lib/commands/actions";
import { useAthena } from "@/lib/store/state";
import { Toaster } from "@/ui/primitives/toast";
import { BrakeNotice } from "@/features/needs-you/BrakeNotice";
import { PauseControl } from "./PauseControl";
import { PausedBanner } from "./PausedBanner";
import { isRestricted, TrustBadge, trustText } from "./trust";

vi.mock("@/lib/commands/actions", async (orig) => ({
  ...(await orig<typeof import("@/lib/commands/actions")>()),
  pause: vi.fn(),
  resume: vi.fn(),
}));
const pause = vi.mocked(actions.pause);
const resume = vi.mocked(actions.resume);
const ok = { ok: true, code: "" } as CommandResult;

function load(control?: { paused: boolean; since?: number; by?: string }, extra = {}) {
  useAthena.getState().applySnapshot(
    create(SnapshotSchema, {
      cursor: 1n,
      control: control && { paused: control.paused, by: control.by ?? "web", since: timestampFromMs(control.since ?? Date.now() - 4 * 60_000) },
      ...extra,
    }),
    "e1",
  );
}

beforeEach(() => {
  useAthena.getState().reset();
  vi.clearAllMocks();
});

test("the store takes control from the Snapshot and from the Delta", () => {
  load({ paused: false });
  expect(useAthena.getState().control?.paused).toBe(false);
  act(() => {
    useAthena.getState().applyDelta(
      create(DeltaSchema, { seq: 2n, change: { case: "control", value: { paused: true, by: "cli" } } }),
    );
  });
  expect(useAthena.getState().control?.paused).toBe(true);
});

test("Pause Athena asks first, and Pause calls pause()", async () => {
  const user = userEvent.setup();
  pause.mockResolvedValue(ok);
  load({ paused: false });
  render(<PauseControl variant="sidebar" />);
  await user.click(screen.getByRole("button", { name: "Pause Athena" }));
  expect(pause).not.toHaveBeenCalled();
  const dialog = screen.getByRole("dialog");
  expect(dialog.textContent).toContain(
    "Pause Athena?Running sessions stop at the end of their current turn, no new work starts, and replies wait. Nothing queued is lost.",
  );
  await user.click(screen.getByRole("button", { name: "Pause" }));
  expect(pause).toHaveBeenCalledTimes(1);
});

test("Cancel does not pause", async () => {
  const user = userEvent.setup();
  load({ paused: false });
  render(<PauseControl variant="phone" />);
  await user.click(screen.getByRole("button", { name: "Pause Athena" }));
  await user.click(screen.getByRole("button", { name: "Cancel" }));
  expect(pause).not.toHaveBeenCalled();
});

test("the banner shows only while paused, and Resume calls resume()", async () => {
  const user = userEvent.setup();
  resume.mockResolvedValue(ok);
  load({ paused: false });
  const { container } = render(<PausedBanner />);
  expect(container.textContent).toBe("");
  act(() => {
    useAthena.getState().applyDelta(
      create(DeltaSchema, { seq: 2n, change: { case: "control", value: { paused: true, since: timestampFromMs(Date.now() - 4 * 60_000) } } }),
    );
  });
  expect(screen.getByRole("status").textContent).toBe("Athena is paused since 4m ago. Nothing new starts and replies wait.Resume");
  await user.click(screen.getByRole("button", { name: "Resume" }));
  expect(resume).toHaveBeenCalledTimes(1);
});

test("a Resume that stays reauth_required says so", async () => {
  const user = userEvent.setup();
  resume.mockResolvedValue({ ok: false, code: "reauth_required" } as CommandResult);
  load({ paused: true });
  render(
    <MemoryRouter>
      <PausedBanner />
      <Toaster />
    </MemoryRouter>,
  );
  await user.click(screen.getByRole("button", { name: "Resume" }));
  await waitFor(() => expect(screen.getByText(actions.errorMessage("reauth_required"))).toBeTruthy());
});

test("the brake notice says the exact sentence, and nothing at braked 0", () => {
  const repo = { name: "acme/web", kind: "github", owed: 3, ceiling: 3, braked: 2 };
  const { container, rerender } = render(<BrakeNotice repos={[repo as never]} />);
  expect(container.textContent).toBe("Holding 2 new items in acme/web until you catch up: 3 already wait on you (limit 3).");
  rerender(<BrakeNotice repos={[{ ...repo, braked: 1 } as never]} />);
  expect(container.textContent).toContain("Holding 1 new item in acme/web");
  rerender(<BrakeNotice repos={[{ ...repo, braked: 0 } as never]} />);
  expect(container.textContent).toBe("");
});

test("the trust badge maps all six classes and the owner shows nothing", () => {
  const words: Record<string, string> = {
    owner: "",
    allowed: "From your allow list",
    collaborator: "From a collaborator",
    stranger: "From outside your allow list",
    bot: "From a bot",
    ci: "From CI",
  };
  for (const [c, w] of Object.entries(words)) {
    expect(trustText(c)).toBe(w);
    const { container, unmount } = render(<TrustBadge trust={c} />);
    expect(container.textContent).toBe(w);
    unmount();
  }
  expect(render(<TrustBadge trust="" />).container.textContent).toBe("");
});

test("a stranger's Ticket says restricted tools, a collaborator's does not, and the Policy overrides", () => {
  load(undefined);
  const a = render(<TrustBadge trust="stranger" ticket />);
  expect(a.container.textContent).toBe("From outside your allow list · restricted tools");
  a.unmount();
  const b = render(<TrustBadge trust="collaborator" ticket />);
  expect(b.container.textContent).toBe("From a collaborator");
  b.unmount();
  expect(isRestricted("collaborator", JSON.stringify({ trust: { collaborator: "restricted" } }))).toBe(true);
  expect(isRestricted("stranger", JSON.stringify({ trust: { stranger: "standard" } }))).toBe(false);
  expect(isRestricted("bot", "not json")).toBe(false);
});
