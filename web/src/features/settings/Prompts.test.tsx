import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { CommandResult } from "@/gen/athena/v1/commands_pb";
import { DeltaSchema } from "@/gen/athena/v1/state_pb";
import * as actions from "@/lib/commands/actions";
import { useAthena } from "@/lib/store/state";
import { load } from "../tickets/fixtures";
import Prompts from "./Prompts";

vi.mock("@/lib/commands/actions", async (orig) => ({
  ...(await orig<typeof import("@/lib/commands/actions")>()),
  getPrompt: vi.fn(),
  setPrompt: vi.fn(),
  resetPrompt: vi.fn(),
}));
const getPrompt = vi.mocked(actions.getPrompt);
const setPrompt = vi.mocked(actions.setPrompt);
const resetPrompt = vi.mocked(actions.resetPrompt);
const ok = { ok: true, code: "" } as CommandResult;

const NOW = Date.parse("2026-11-04T12:00:00Z");
const details: Record<string, object> = {
  worker: {
    name: "worker",
    defaultText: "You are a careful worker.\nRun the tests.",
    text: "My own worker text.",
    previousDefaultText: "You are a worker.\nRun the tests.",
    fullWorker: "FULL WORKER PROMPT",
    fullRouter: "FULL ROUTER PROMPT",
    history: [
      { revision: 6, at: timestampFromMs(NOW - 60_000), origin: "web", reset: false },
      { revision: 2, at: timestampFromMs(NOW - 86_400_000), origin: "cli", reset: true },
    ],
  },
  router: { name: "router", defaultText: "Route it.", text: "", fullWorker: "FW", fullRouter: "FR", history: [] },
};
const detail = (part: string) =>
  ({ ok: true, code: "", result: { case: "prompt", value: details[part] } }) as unknown as CommandResult;

beforeEach(() => {
  load({
    prompts: {
      revision: 7,
      parts: [
        { name: "worker", overridden: true, defaultUpdated: true },
        { name: "router", overridden: false },
      ],
    },
  });
  getPrompt.mockImplementation(async (part) => detail(part));
  setPrompt.mockResolvedValue(ok);
  resetPrompt.mockResolvedValue(ok);
});
afterEach(() => {
  useAthena.getState().reset();
  vi.clearAllMocks();
});

function renderPrompts(path = "/settings/prompts?part=worker") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Prompts />
    </MemoryRouter>,
  );
}

test("the parts list marks edited parts and updated defaults", async () => {
  renderPrompts();
  const parts = screen.getByRole("navigation", { name: "Prompt parts" });
  const worker = within(parts).getByRole("link", { name: /worker/ });
  expect(within(worker).getByText("Edited")).toBeTruthy();
  expect(within(worker).getByText("Default updated")).toBeTruthy();
  expect(within(within(parts).getByRole("link", { name: /router/ })).queryByText("Edited")).toBeNull();
  expect(screen.getByText("Edits apply to sessions that start after you save.")).toBeTruthy();
  // History and the full prompts come from the part's detail.
  expect(await screen.findByText("FULL WORKER PROMPT")).toBeTruthy();
  expect(screen.getByText("FULL ROUTER PROMPT")).toBeTruthy();
  const history = screen.getByRole("list", { name: "History" });
  expect(within(history).getAllByRole("listitem").map((li) => li.textContent)).toEqual([
    expect.stringMatching(/^#6.*Saved.*web/),
    expect.stringMatching(/^#2.*Reset to the default.*cli/),
  ]);
});

test("prompts default-updated keep and use-new", async () => {
  const user = userEvent.setup();
  renderPrompts();
  const panel = await screen.findByRole("region", { name: "The default changed" });
  // The old and new defaults, compared, above the owner's text.
  expect(within(panel).getByText("You are a worker.")).toBeTruthy();
  expect(within(panel).getByText("You are a careful worker.")).toBeTruthy();
  const editor = screen.getByRole("textbox", { name: "worker" });
  expect(panel.compareDocumentPosition(editor) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  expect((editor as HTMLTextAreaElement).value).toBe("My own worker text.");

  await user.click(within(panel).getByRole("button", { name: "Keep my text" }));
  expect(setPrompt).toHaveBeenCalledWith("worker", 7, "My own worker text.");

  await user.click(within(panel).getByRole("button", { name: "Use the new default" }));
  expect(resetPrompt).not.toHaveBeenCalled();
  await new Promise((r) => setTimeout(r, 350));
  await user.click(within(panel).getByRole("button", { name: "Replace my text with the new default" }));
  expect(resetPrompt).toHaveBeenCalledWith("worker", 7);
});

test("save sends the text against the revision, and a conflict keeps it", async () => {
  const user = userEvent.setup();
  renderPrompts("/settings/prompts?part=router");
  const editor = (await screen.findByDisplayValue("Route it.")) as HTMLTextAreaElement;
  await user.type(editor, " Carefully.");
  setPrompt.mockResolvedValueOnce({ ok: false, code: "conflict" } as CommandResult);
  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(setPrompt).toHaveBeenCalledWith("router", 7, "Route it. Carefully.");
  expect(await screen.findByText("Changed on another device")).toBeTruthy();
  expect(editor.value).toBe("Route it. Carefully.");

  act(() => {
    useAthena.getState().applyDelta(
      create(DeltaSchema, { seq: 2n, change: { case: "prompts", value: { revision: 8, parts: [{ name: "router" }] } } }),
    );
  });
  await user.click(screen.getByRole("button", { name: "Overwrite" }));
  expect(setPrompt).toHaveBeenLastCalledWith("router", 8, "Route it. Carefully.");
});

test("reset is confirmed and only for an edited part", async () => {
  const user = userEvent.setup();
  const { unmount } = renderPrompts("/settings/prompts?part=router");
  await screen.findByDisplayValue("Route it.");
  expect(screen.queryByRole("button", { name: "Reset" })).toBeNull();
  unmount();

  renderPrompts();
  await screen.findByDisplayValue("My own worker text.");
  await user.click(screen.getByRole("button", { name: "Reset" }));
  await new Promise((r) => setTimeout(r, 350));
  await user.click(screen.getByRole("button", { name: "Reset worker to its default" }));
  expect(resetPrompt).toHaveBeenCalledWith("worker", 7);
});

const promptsDelta = (seq: bigint, revision: number) =>
  act(() => {
    useAthena.getState().applyDelta(
      create(DeltaSchema, { seq, change: { case: "prompts", value: { revision, parts: [{ name: "router" }] } } }),
    );
  });

test("an edit right after a save is based on that save, not reported as another device's change", async () => {
  const user = userEvent.setup();
  renderPrompts("/settings/prompts?part=router");
  const editor = (await screen.findByDisplayValue("Route it.")) as HTMLTextAreaElement;
  await user.type(editor, " A");
  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(setPrompt).toHaveBeenLastCalledWith("router", 7, "Route it. A");
  // Edited again before this save's own Delta arrives (setPrompt returns no revision).
  await user.type(editor, "B");
  details.router = { ...details.router, text: "Route it. A" };
  promptsDelta(2n, 8);
  await vi.waitFor(() => expect(getPrompt).toHaveBeenCalledTimes(2));
  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(setPrompt).toHaveBeenLastCalledWith("router", 8, "Route it. AB");
  details.router = { ...details.router, text: "" };
});

test("keep my text sends once, and a conflict on it offers Reload and Overwrite", async () => {
  const user = userEvent.setup();
  let answer!: (r: CommandResult) => void;
  setPrompt.mockImplementationOnce(() => new Promise((r) => (answer = r)));
  renderPrompts();
  const panel = await screen.findByRole("region", { name: "The default changed" });
  const keep = within(panel).getByRole("button", { name: "Keep my text" });
  await user.click(keep);
  expect((keep as HTMLButtonElement).disabled).toBe(true);
  expect((within(panel).getByRole("button", { name: "Use the new default" }) as HTMLButtonElement).disabled).toBe(true);
  expect((screen.getByRole("button", { name: "Reset" }) as HTMLButtonElement).disabled).toBe(true);
  await user.click(keep);
  expect(setPrompt).toHaveBeenCalledTimes(1);

  await act(async () => answer({ ok: false, code: "conflict" } as CommandResult));
  const bar = screen.getByRole("region", { name: "Save worker" });
  expect(within(bar).getByText("Changed on another device")).toBeTruthy();
  act(() => {
    useAthena.getState().applyDelta(
      create(DeltaSchema, { seq: 2n, change: { case: "prompts", value: { revision: 9, parts: [{ name: "worker", overridden: true, defaultUpdated: true }] } } }),
    );
  });
  await vi.waitFor(() => expect(getPrompt).toHaveBeenCalledTimes(2));
  await user.click(within(bar).getByRole("button", { name: "Overwrite" }));
  // Overwrite repeats the same decision on the fresh revision.
  expect(setPrompt).toHaveBeenLastCalledWith("worker", 9, "My own worker text.");
});
