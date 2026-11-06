import { create } from "@bufbuild/protobuf";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { CommandResultSchema } from "@/gen/athena/v1/commands_pb";
import * as actions from "@/lib/commands/actions";
import { useAthena } from "@/lib/store/state";
import { load } from "@/features/tickets/fixtures";
import { NewTask } from "./NewTask";

vi.mock("@/lib/commands/actions", async (orig) => ({
  ...(await orig<typeof import("@/lib/commands/actions")>()),
  submitTask: vi.fn(),
}));

const submitTask = vi.mocked(actions.submitTask);
const made = (id: string) => create(CommandResultSchema, { ok: true, result: { case: "ticketId", value: id } });

beforeEach(() => {
  useAthena.getState().reset();
  load({
    repos: [
      { name: "acme/api", kind: "github" },
      { name: "/Users/me/code/web", kind: "git" },
      { name: "/Users/me/notes", kind: "dir" },
    ],
    tickets: [
      { id: "T-1", title: "a", model: "opus" },
      { id: "T-2", title: "b", model: "sonnet" },
      { id: "T-3", title: "c", model: "opus" },
    ],
  });
  submitTask.mockResolvedValue(made("T-9"));
});
afterEach(() => vi.clearAllMocks());

function setup() {
  const user = userEvent.setup();
  render(
    <MemoryRouter initialEntries={["/new"]}>
      <Routes>
        <Route path="/new" element={<NewTask />} />
        <Route path="/tickets/:id" element={<p>detail of the ticket</p>} />
      </Routes>
    </MemoryRouter>,
  );
  return user;
}
const task = () => screen.getByRole("textbox", { name: "What should Athena do?" });
const repoBox = () => screen.getByRole("combobox", { name: "Repository" });
const start = () => screen.getByRole("button", { name: "Start" });

test("repo picker sends repo for owner/name and dir for a path", async () => {
  const user = setup();
  // Failures keep the form, so one test can send several times.
  submitTask.mockResolvedValue(create(CommandResultSchema, { ok: false, code: "unavailable" }));
  await user.type(task(), "Fix the bug");
  await user.type(repoBox(), "acme/web");
  await user.click(start());
  expect(submitTask).toHaveBeenLastCalledWith({ text: "Fix the bug", repo: "acme/web", runtime: "auto" });

  await user.clear(repoBox());
  await user.type(repoBox(), "/srv/app");
  await user.click(start());
  expect(submitTask).toHaveBeenLastCalledWith({ text: "Fix the bug", dir: "/srv/app", runtime: "auto" });

  // A listed repo goes by its kind: GitHub as repo, local repositories and folders as dir.
  await user.clear(repoBox());
  await user.click(repoBox());
  expect(screen.getByText("GitHub")).toBeTruthy();
  expect(screen.getByText("Local repositories")).toBeTruthy();
  expect(screen.getByText("Folders")).toBeTruthy();
  await user.click(screen.getByRole("option", { name: "acme/api" }));
  await user.click(start());
  expect(submitTask).toHaveBeenLastCalledWith({ text: "Fix the bug", repo: "acme/api", runtime: "auto" });
  await user.click(repoBox());
  await user.click(screen.getByRole("option", { name: "/Users/me/code/web" }));
  await user.click(start());
  expect(submitTask).toHaveBeenLastCalledWith({ text: "Fix the bug", dir: "/Users/me/code/web", runtime: "auto" });
});

test("no repository sends neither", async () => {
  const user = setup();
  expect(repoBox().getAttribute("placeholder")).toBe("No repository");
  await user.type(task(), "Just think");
  await user.click(start());
  expect(submitTask).toHaveBeenCalledWith({ text: "Just think", runtime: "auto" });
});

test("invalid owner/name is refused inline", async () => {
  const user = setup();
  await user.type(task(), "Fix the bug");
  await user.type(repoBox(), "acme");
  expect(screen.getByRole("alert").textContent).toMatch(/owner\/name/);
  expect((start() as HTMLButtonElement).disabled).toBe(true);
  fireEvent.keyDown(task(), { key: "Enter", metaKey: true });
  expect(submitTask).not.toHaveBeenCalled();
});

test("success navigates to the new ticket", async () => {
  const user = setup();
  await user.type(task(), "Fix the bug");
  await user.click(start());
  await waitFor(() => expect(screen.getByText("detail of the ticket")).toBeTruthy());
});

test("failure keeps the text", async () => {
  const user = setup();
  submitTask.mockResolvedValueOnce(create(CommandResultSchema, { ok: false, code: "unavailable" }));
  await user.type(task(), "Fix the bug");
  await user.click(start());
  await waitFor(() => expect(screen.getByRole("alert").textContent).toMatch(/busy/));
  expect((task() as HTMLTextAreaElement).value).toBe("Fix the bug");
  expect(screen.queryByText("detail of the ticket")).toBeNull();
});

test("cmd-enter submits once, enter adds a line, composition does not submit", async () => {
  const user = setup();
  let finish!: (r: ReturnType<typeof made>) => void;
  submitTask.mockReturnValueOnce(new Promise((r) => (finish = r)));
  await user.type(task(), "one{Enter}two");
  expect((task() as HTMLTextAreaElement).value).toBe("one\ntwo");
  expect(submitTask).not.toHaveBeenCalled();
  fireEvent.keyDown(task(), { key: "Enter", metaKey: true, isComposing: true });
  fireEvent.keyDown(task(), { key: "Enter", metaKey: true, keyCode: 229 });
  expect(submitTask).not.toHaveBeenCalled();
  fireEvent.keyDown(task(), { key: "Enter", metaKey: true });
  fireEvent.keyDown(task(), { key: "Enter", metaKey: true });
  fireEvent.keyDown(task(), { key: "Enter", ctrlKey: true, repeat: true });
  expect(submitTask).toHaveBeenCalledTimes(1);
  await act(async () => finish(made("T-9")));
  await waitFor(() => expect(screen.getByText("detail of the ticket")).toBeTruthy());
});

const suggested = () => Array.from(document.querySelectorAll("datalist option")).map((o) => o.getAttribute("value"));

test("model suggestions are the recent models, and an empty model sends none", async () => {
  const user = setup();
  expect(suggested()).toEqual(["opus", "sonnet"]);
  await user.type(task(), "x");
  await user.type(screen.getByRole("combobox", { name: "Model" }), "haiku");
  await user.click(start());
  expect(submitTask).toHaveBeenCalledWith({ text: "x", model: "haiku", runtime: "auto" });
});

test("a bare slash is refused as a directory", async () => {
  const user = setup();
  await user.type(task(), "x");
  await user.type(repoBox(), "/");
  expect(screen.getByRole("alert").textContent).toMatch(/owner\/name/);
  expect((start() as HTMLButtonElement).disabled).toBe(true);
  await user.type(repoBox(), "srv");
  expect(screen.queryByRole("alert")).toBeNull();
});

test("the list closes on a pointerdown outside and on picking", async () => {
  const user = setup();
  const list = () => document.querySelector("[cmdk-list]")?.parentElement as HTMLElement;
  await user.click(repoBox());
  expect(list().hidden).toBe(false);
  // A press outside takes the list out of the way at once, and the click collapses it: Start never moves mid-tap.
  fireEvent.pointerDown(task());
  expect(list().className).toContain("invisible");
  expect(list().hidden).toBe(false);
  fireEvent.click(task());
  expect(list().hidden).toBe(true);
  await user.click(repoBox());
  expect(list().hidden).toBe(false);
  await user.click(screen.getByRole("option", { name: "acme/api" }));
  expect(list().hidden).toBe(true);
  await user.clear(repoBox());
  await user.type(repoBox(), "o/r");
  expect(list().hidden).toBe(false);
  await user.click(screen.getByRole("option", { name: "Use o/r" }));
  expect(list().hidden).toBe(true);
});

test("a pointercancel never collapses the list", async () => {
  const user = setup();
  const list = () => document.querySelector("[cmdk-list]")?.parentElement as HTMLElement;
  await user.click(repoBox());
  // A touch scroll inside the list.
  fireEvent.pointerDown(screen.getByRole("option", { name: "No repository" }));
  fireEvent.pointerCancel(screen.getByRole("option", { name: "No repository" }));
  expect(list().hidden).toBe(false);
  expect(list().className).not.toContain("invisible");
  // A touch scroll that began outside it: the list is as it was.
  fireEvent.pointerDown(task());
  expect(list().className).toContain("invisible");
  fireEvent.pointerCancel(task());
  expect(list().hidden).toBe(false);
  expect(list().className).not.toContain("invisible");
});

const both = [
  { name: "claude", ready: true, tested: true },
  { name: "codex", ready: true, tested: true, version: "0.160.1" },
];

function withRuntimes(runtimes: Array<{ name: string; ready: boolean; tested?: boolean; why?: string }>, settingsJson = "{}") {
  load({
    repos: [],
    tickets: [],
    node: { name: "mac-mini", slots: 2, runtimes },
    policy: { revision: 1, settingsJson },
  });
}

test("new task sends runtime and review", async () => {
  withRuntimes(both);
  const user = setup();
  submitTask.mockResolvedValue(create(CommandResultSchema, { ok: false, code: "unavailable" }));
  await user.type(task(), "Port the parser");
  await user.click(screen.getByRole("radio", { name: "Codex" }));
  // The switch untouched: the Policy's default decides, so no review is sent.
  await user.click(start());
  expect(submitTask).toHaveBeenLastCalledWith({ text: "Port the parser", runtime: "codex" });
  const review = screen.getByRole("switch", { name: "Review each ready report" });
  expect(review.getAttribute("aria-checked")).toBe("false");
  await user.click(review);
  await user.click(start());
  expect(submitTask).toHaveBeenLastCalledWith({ text: "Port the parser", runtime: "codex", review: true });
  // Turned off again by the person: an explicit no.
  await user.click(review);
  await user.click(screen.getByRole("radio", { name: "Auto" }));
  await user.click(start());
  expect(submitTask).toHaveBeenLastCalledWith({ text: "Port the parser", runtime: "auto", review: false });
});

test("the review switch starts at the Policy's default and sends only when toggled", async () => {
  withRuntimes(both, '{"pairing":{"review":"on","maxRounds":2}}');
  const user = setup();
  submitTask.mockResolvedValue(create(CommandResultSchema, { ok: false, code: "unavailable" }));
  const review = screen.getByRole("switch", { name: "Review each ready report" });
  expect(review.getAttribute("aria-checked")).toBe("true");
  await user.type(task(), "x");
  await user.click(start());
  expect(submitTask).toHaveBeenLastCalledWith({ text: "x", runtime: "auto" });
  await user.click(review);
  await user.click(start());
  expect(submitTask).toHaveBeenLastCalledWith({ text: "x", runtime: "auto", review: false });
});

test("model suggestions follow the chosen runtime; under Auto the model is Claude Code's", async () => {
  load({
    repos: [],
    tickets: [
      { id: "T-1", title: "a", model: "opus", runtime: "claude" },
      { id: "T-2", title: "b", model: "gpt-5.5", runtime: "codex" },
      { id: "T-3", title: "c", model: "sonnet" },
    ],
    node: { name: "mac-mini", slots: 2, runtimes: both },
    policy: { revision: 1, settingsJson: "{}" },
  });
  const user = setup();
  expect(suggested()).toEqual(["opus", "sonnet"]);
  expect(screen.getByText("Under Auto, the model applies on Claude Code; Codex runs its own default.")).toBeTruthy();
  await user.click(screen.getByRole("radio", { name: "Codex" }));
  expect(suggested()).toEqual(["gpt-5.5"]);
  expect(screen.queryByText(/Under Auto/)).toBeNull();
  await user.click(screen.getByRole("radio", { name: "Claude Code" }));
  expect(suggested()).toEqual(["opus", "sonnet"]);
});

test("review switch with two ready runtimes, or whenever the Policy pairs", async () => {
  withRuntimes([both[0]], '{"pairing":{"review":"on"}}');
  const one = render(
    <MemoryRouter>
      <NewTask />
    </MemoryRouter>,
  );
  // One runtime, the Policy pairing every task: the owner can still opt out of this one.
  const review = screen.getByRole("switch", { name: "Review each ready report" });
  expect(review.getAttribute("aria-checked")).toBe("true");
  expect(screen.getByText(/on the other runtime when it can review/)).toBeTruthy();
  submitTask.mockResolvedValue(create(CommandResultSchema, { ok: false, code: "unavailable" }));
  const user = userEvent.setup();
  await user.type(task(), "x");
  await user.click(review);
  await user.click(start());
  expect(submitTask).toHaveBeenLastCalledWith({ text: "x", runtime: "auto", review: false });
  one.unmount();

  withRuntimes([both[0], { name: "codex", ready: false, why: "Codex is not logged in" }]);
  const { unmount } = render(
    <MemoryRouter>
      <NewTask />
    </MemoryRouter>,
  );
  expect(screen.queryByRole("switch", { name: "Review each ready report" })).toBeNull();
  unmount();
  withRuntimes([both[0]]);
  const again = render(
    <MemoryRouter>
      <NewTask />
    </MemoryRouter>,
  );
  expect(screen.queryByRole("switch", { name: "Review each ready report" })).toBeNull();
  again.unmount();
  withRuntimes(both);
  render(
    <MemoryRouter>
      <NewTask />
    </MemoryRouter>,
  );
  expect(screen.getByRole("switch", { name: "Review each ready report" })).toBeTruthy();
});

test("a chosen runtime that stops being ready sends auto", async () => {
  withRuntimes(both);
  const user = setup();
  await user.type(task(), "x");
  await user.click(screen.getByRole("radio", { name: "Codex" }));
  act(() => withRuntimes([both[0], { name: "codex", ready: false, why: "Codex is not logged in" }]));
  expect(screen.getByRole("radio", { name: "Auto" }).getAttribute("aria-checked")).toBe("true");
  await user.click(start());
  expect(submitTask).toHaveBeenLastCalledWith({ text: "x", runtime: "auto" });
});
