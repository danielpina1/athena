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
  expect(submitTask).toHaveBeenLastCalledWith({ text: "Fix the bug", repo: "acme/web" });

  await user.clear(repoBox());
  await user.type(repoBox(), "/srv/app");
  await user.click(start());
  expect(submitTask).toHaveBeenLastCalledWith({ text: "Fix the bug", dir: "/srv/app" });

  // A listed repo goes by its kind: GitHub as repo, local repositories and folders as dir.
  await user.clear(repoBox());
  await user.click(repoBox());
  expect(screen.getByText("GitHub")).toBeTruthy();
  expect(screen.getByText("Local repositories")).toBeTruthy();
  expect(screen.getByText("Folders")).toBeTruthy();
  await user.click(screen.getByRole("option", { name: "acme/api" }));
  await user.click(start());
  expect(submitTask).toHaveBeenLastCalledWith({ text: "Fix the bug", repo: "acme/api" });
  await user.click(repoBox());
  await user.click(screen.getByRole("option", { name: "/Users/me/code/web" }));
  await user.click(start());
  expect(submitTask).toHaveBeenLastCalledWith({ text: "Fix the bug", dir: "/Users/me/code/web" });
});

test("no repository sends neither", async () => {
  const user = setup();
  expect(repoBox().getAttribute("placeholder")).toBe("No repository");
  await user.type(task(), "Just think");
  await user.click(start());
  expect(submitTask).toHaveBeenCalledWith({ text: "Just think" });
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

test("model suggestions are the recent models, and an empty model sends none", async () => {
  const user = setup();
  const options = Array.from(document.querySelectorAll("datalist option")).map((o) => o.getAttribute("value"));
  expect(options).toEqual(["opus", "sonnet"]);
  await user.type(task(), "x");
  await user.type(screen.getByRole("combobox", { name: "Model" }), "haiku");
  await user.click(start());
  expect(submitTask).toHaveBeenCalledWith({ text: "x", model: "haiku" });
});
