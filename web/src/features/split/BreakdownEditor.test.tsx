import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { CommandResultSchema } from "@/gen/athena/v1/commands_pb";
import type { BreakdownViewSchema } from "@/gen/athena/v1/state_pb";
import * as actions from "@/lib/commands/actions";
import { useAthena } from "@/lib/store/state";
import { detail, load } from "@/features/tickets/fixtures";
import BreakdownEditor, { LAZY_SENTINEL } from "./BreakdownEditor";

vi.mock("@/lib/commands/actions", async (orig) => ({
  ...(await orig<typeof import("@/lib/commands/actions")>()),
  updateBreakdown: vi.fn(),
  approveBreakdown: vi.fn(),
  rejectBreakdown: vi.fn(),
  replan: vi.fn(),
}));

const ok = create(CommandResultSchema, { ok: true });
const STALE = "The split changed meanwhile; reload it and try again.";

type Plan = MessageInitShape<typeof BreakdownViewSchema>;
const draft = (over: Plan = {}): Plan => ({
  state: "draft",
  revision: 3,
  maxChildren: 4,
  tasks: [
    { key: "api", title: "Add the API", instructions: "Add the endpoint.", areas: ["internal/api"] },
    { key: "ui", title: "Add the page", instructions: "Add the page.", areas: ["web/src"], deps: ["api"] },
    { key: "docs", title: "Document it", instructions: "Write the docs.", areas: ["docs", "web/src"] },
  ],
  overlaps: [{ a: "ui", b: "docs", areas: ["web/src"] }],
  ...over,
});

function plan(init: Plan) {
  const state = init.state === "failed" ? "failed" : "draft";
  detail({ summary: { id: "T-1", title: "Port the parser", stage: "person", action: "plan", planState: state }, breakdown: init });
}

beforeEach(() => {
  useAthena.getState().reset();
  for (const f of [actions.updateBreakdown, actions.approveBreakdown, actions.rejectBreakdown, actions.replan]) vi.mocked(f).mockResolvedValue(ok);
  load({ tickets: [{ id: "T-1", title: "Port the parser", stage: "person", action: "plan", planState: "draft" }] });
  plan(draft());
});
afterEach(() => vi.clearAllMocks());

function setup() {
  const user = userEvent.setup();
  const onClose = vi.fn();
  render(<BreakdownEditor ticketId="T-1" onClose={onClose} />);
  return { user, onClose };
}

const row = (key: string) => screen.getByRole("group", { name: `Sub-task ${key}` });

test("the editor is a lazy chunk with its sentinel", () => {
  expect(LAZY_SENTINEL).toBe("athena-lazy-chunk:breakdown-editor");
});

test("editing a title saves the sub-tasks at the draft's revision", async () => {
  const { user } = setup();
  const title = within(row("ui")).getByRole("textbox", { name: "Title" });
  await user.clear(title);
  await user.type(title, "Add the settings page");
  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(actions.updateBreakdown).toHaveBeenCalledTimes(1);
  const [id, revision, tasks] = vi.mocked(actions.updateBreakdown).mock.calls[0];
  expect([id, revision]).toEqual(["T-1", 3]);
  expect((tasks ?? []).map((t) => [t.key, t.title, t.deps, t.areas])).toEqual([
    ["api", "Add the API", [], ["internal/api"]],
    ["ui", "Add the settings page", ["api"], ["web/src"]],
    ["docs", "Document it", [], ["docs", "web/src"]],
  ]);
});

test("a dep chip toggles", async () => {
  const { user } = setup();
  const chip = within(row("docs")).getByRole("button", { name: "after api" });
  expect(chip.getAttribute("aria-pressed")).toBe("false");
  expect(within(row("docs")).queryByRole("button", { name: "after docs" })).toBeNull();
  await user.click(chip);
  expect(chip.getAttribute("aria-pressed")).toBe("true");
  const ui = within(row("ui")).getByRole("button", { name: "after api" });
  await user.click(ui);
  expect(ui.getAttribute("aria-pressed")).toBe("false");
  await user.click(screen.getByRole("button", { name: "Save" }));
  const tasks = vi.mocked(actions.updateBreakdown).mock.calls[0][2] ?? [];
  expect(tasks.map((t) => t.deps)).toEqual([[], [], ["api"]]);
});

test("an overlap warns under both rows, and does not block", async () => {
  const { user } = setup();
  expect(within(row("ui")).getByText("Overlaps with docs: web/src")).toBeTruthy();
  expect(within(row("docs")).getByText("Overlaps with ui: web/src")).toBeTruthy();
  expect(within(row("api")).queryByText(/Overlaps/)).toBeNull();
  expect(screen.getByRole("button", { name: "Approve and start" }).hasAttribute("disabled")).toBe(false);
  // An edited area is checked as it is typed.
  const areas = within(row("api")).getByRole("textbox", { name: "Areas" });
  await user.clear(areas);
  await user.type(areas, "internal/api, docs/api.md");
  expect(within(row("api")).getByText("Overlaps with docs: docs/api.md")).toBeTruthy();
  expect(within(row("docs")).getByText("Overlaps with api: docs/api.md")).toBeTruthy();
});

test("a stale revision says so, and the draft reloads from the store", async () => {
  vi.mocked(actions.updateBreakdown).mockResolvedValue(create(CommandResultSchema, { ok: false, code: "invalid", message: STALE }));
  const { user } = setup();
  const title = within(row("ui")).getByRole("textbox", { name: "Title" });
  await user.clear(title);
  await user.type(title, "Mine");
  // The draft changed meanwhile (the planner ran again, or another screen saved).
  plan(draft({ revision: 4, tasks: [{ key: "api", title: "Theirs" }, { key: "ui", title: "Their page" }], overlaps: [] }));
  expect((within(row("ui")).getByRole("textbox", { name: "Title" }) as HTMLInputElement).value).toBe("Mine");
  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(await screen.findByText(STALE)).toBeTruthy();
  expect((within(row("ui")).getByRole("textbox", { name: "Title" }) as HTMLInputElement).value).toBe("Their page");
  expect(screen.queryByRole("group", { name: "Sub-task docs" })).toBeNull();
  vi.mocked(actions.updateBreakdown).mockResolvedValue(ok);
  await user.type(within(row("ui")).getByRole("textbox", { name: "Title" }), "!");
  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(vi.mocked(actions.updateBreakdown).mock.calls[1][1]).toBe(4);
});

test("Approve on a dirty draft saves first, then approves the saved revision", async () => {
  const { user, onClose } = setup();
  await user.type(within(row("api")).getByRole("textbox", { name: "Title" }), " endpoint");
  await user.click(screen.getByRole("button", { name: "Approve and start" }));
  expect(actions.updateBreakdown).toHaveBeenCalledWith("T-1", 3, expect.any(Array));
  expect(actions.approveBreakdown).toHaveBeenCalledWith("T-1", 4);
  expect(vi.mocked(actions.updateBreakdown).mock.invocationCallOrder[0]).toBeLessThan(
    vi.mocked(actions.approveBreakdown).mock.invocationCallOrder[0],
  );
  expect(onClose).toHaveBeenCalled();
});

test("Approve on a clean draft approves at once", async () => {
  const { user } = setup();
  await user.click(screen.getByRole("button", { name: "Approve and start" }));
  expect(actions.updateBreakdown).not.toHaveBeenCalled();
  expect(actions.approveBreakdown).toHaveBeenCalledWith("T-1", 3);
});

test("rows are added up to max_children, and removed with the deps on them", async () => {
  const { user } = setup();
  const add = screen.getByRole("button", { name: "Add a sub-task" });
  await user.click(add);
  expect(screen.getAllByRole("group", { name: /^Sub-task / })).toHaveLength(4);
  expect(add.hasAttribute("disabled")).toBe(true);
  expect(screen.getByText("At most 4 sub-tasks.")).toBeTruthy();
  await user.click(within(row("api")).getByRole("button", { name: "Remove api" }));
  expect(screen.getAllByRole("group", { name: /^Sub-task / })).toHaveLength(3);
  expect(add.hasAttribute("disabled")).toBe(false);
  expect(within(row("ui")).queryByRole("button", { name: "after api" })).toBeNull();
  await user.click(screen.getByRole("button", { name: "Save" }));
  const tasks = vi.mocked(actions.updateBreakdown).mock.calls[0][2] ?? [];
  expect(tasks.map((t) => [t.key, t.deps])).toEqual([
    ["ui", []],
    ["docs", []],
    ["sub-4", []],
  ]);
});

test("the runtime and model hints are sent", async () => {
  const { user } = setup();
  await user.selectOptions(within(row("api")).getByRole("combobox", { name: "Runtime" }), "codex");
  await user.type(within(row("api")).getByRole("textbox", { name: "Model" }), "gpt-5");
  await user.click(screen.getByRole("button", { name: "Save" }));
  const api = vi.mocked(actions.updateBreakdown).mock.calls[0][2]?.[0];
  expect([api?.runtime, api?.model]).toEqual(["codex", "gpt-5"]);
});

test("Run as one ticket rejects the split", async () => {
  const { user, onClose } = setup();
  await user.click(screen.getByRole("button", { name: "Run as one ticket" }));
  expect(actions.rejectBreakdown).toHaveBeenCalledWith("T-1");
  expect(onClose).toHaveBeenCalled();
});

test("a failed plan offers Try again and Run as one ticket", async () => {
  plan({ state: "failed", error: "the planner timed out" });
  const { user } = setup();
  expect(screen.getByText("Planning failed: the planner timed out")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Approve and start" })).toBeNull();
  await user.click(screen.getByRole("button", { name: "Try again" }));
  expect(actions.replan).toHaveBeenCalledWith("T-1");
  expect(screen.getByRole("button", { name: "Run as one ticket" })).toBeTruthy();
});
