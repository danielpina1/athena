import { create } from "@bufbuild/protobuf";
import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { stubWidth } from "@/app/testkit";
import { LayoutProvider } from "@/app/Panes";
import type { CommandResult } from "@/gen/athena/v1/commands_pb";
import { DeltaSchema } from "@/gen/athena/v1/state_pb";
import * as actions from "@/lib/commands/actions";
import { useAthena } from "@/lib/store/state";
import { load } from "../tickets/fixtures";
import { AdmissionTable } from "./AdmissionTable";
import { GeneralSettings, GitHubSettings } from "./PolicyForm";

vi.mock("@/lib/commands/actions", async (orig) => ({
  ...(await orig<typeof import("@/lib/commands/actions")>()),
  setPolicy: vi.fn(),
}));
const setPolicy = vi.mocked(actions.setPolicy);
const result = (r: Partial<CommandResult>) => ({ ok: true, code: "", ...r }) as CommandResult;

const admission = {
  issues: { listedUnaddressed: "inferred", unlistedAddressed: "ignore", unlistedUnaddressed: "ignore" },
  pulls: { listedUnaddressed: "inferred", unlistedAddressed: "ignore", unlistedUnaddressed: "ignore" },
  comments: { listedUnaddressed: "inferred", unlistedAddressed: "ignore", unlistedUnaddressed: "ignore" },
  ci: { unlistedUnaddressed: "ignore" },
};
const settings = {
  mode: "observe",
  rules: [{ name: "bugs", instructions: "Fix reported bugs.", enabled: true }],
  sources: { github: { allowed: ["ana"], bots: [], admission } },
};

function policy(revision: number, s: object = settings) {
  return { revision, version: "abc", settingsJson: JSON.stringify(s) };
}

function delta(seq: bigint, revision: number, s: object) {
  useAthena.getState().applyDelta(create(DeltaSchema, { seq, change: { case: "policy", value: policy(revision, s) } }));
}

beforeEach(() => {
  stubWidth(1440);
  load({ policy: policy(3) });
});
afterEach(() => {
  useAthena.getState().reset();
  vi.clearAllMocks();
});

function renderBlock(block: "general" | "github") {
  return render(
    <MemoryRouter>
      <LayoutProvider value="desktop">{block === "general" ? <GeneralSettings /> : <GitHubSettings />}</LayoutProvider>
    </MemoryRouter>,
  );
}

const mode = (name: string) => screen.getByRole("radio", { name: new RegExp(`^${name}`) }) as HTMLInputElement;
const sent = (call = 0) => JSON.parse(setPolicy.mock.calls[call][1]);

test("policy conflict keeps edits and offers reload", async () => {
  const user = userEvent.setup();
  renderBlock("general");
  expect(mode("Observe").checked).toBe(true);

  await user.click(mode("Auto"));
  setPolicy.mockResolvedValueOnce(result({ ok: false, code: "conflict" }));
  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(setPolicy).toHaveBeenCalledWith(3, expect.any(String));

  // Kept: the owner's edit stays, and the bar says what happened.
  expect(await screen.findByText("Changed on another device")).toBeTruthy();
  expect(mode("Auto").checked).toBe(true);
  expect(screen.getByRole("button", { name: "Reload" })).toBeTruthy();

  // The other device's revision arrives; Overwrite resends the edit on top of it.
  act(() => delta(2n, 4, { ...settings, mode: "off" }));
  expect(mode("Auto").checked).toBe(true);
  setPolicy.mockResolvedValueOnce(result({ result: { case: "policyRevision", value: 5 } }));
  await user.click(screen.getByRole("button", { name: "Overwrite" }));
  expect(setPolicy).toHaveBeenLastCalledWith(4, expect.any(String));
  expect(sent(1).mode).toBe("auto");
});

test("reload after a conflict discards the edit and shows the server's", async () => {
  const user = userEvent.setup();
  renderBlock("general");
  await user.click(mode("Auto"));
  setPolicy.mockResolvedValueOnce(result({ ok: false, code: "conflict" }));
  await user.click(screen.getByRole("button", { name: "Save" }));
  await screen.findByText("Changed on another device");
  act(() => delta(2n, 4, { ...settings, mode: "off" }));

  await user.click(screen.getByRole("button", { name: "Reload" }));
  expect(mode("Off").checked).toBe(true);
  expect(screen.queryByRole("region", { name: "Save General" })).toBeNull();
});

test("unknown policy fields survive a save", async () => {
  const user = userEvent.setup();
  const rich = {
    mode: "observe",
    eval: { timeout: 30, model: "haiku" },
    rules: [{ name: "bugs", instructions: "Fix reported bugs.", enabled: true, schedule: "daily" }],
    sources: {
      github: { allowed: ["ana"], bots: ["dependabot"], admission, sync: { org: "acme" } },
      feishu: { allowed: ["ou_1"] },
    },
  };
  load({ policy: policy(8, rich) });
  renderBlock("general");
  await user.click(mode("Auto"));
  setPolicy.mockResolvedValueOnce(result({ result: { case: "policyRevision", value: 9 } }));
  await user.click(screen.getByRole("button", { name: "Save" }));

  expect(setPolicy.mock.calls[0][0]).toBe(8);
  expect(sent()).toEqual({ ...rich, mode: "auto" });
});

test("the GitHub block edits its lists and keeps the rest", async () => {
  const user = userEvent.setup();
  renderBlock("github");
  const allowed = screen.getByRole("textbox", { name: "Allowed" });
  await user.type(allowed, "{End}{Enter}bo");
  await user.selectOptions(screen.getByRole("combobox", { name: "Pull requests, unlisted addressed" }), "explicit");
  setPolicy.mockResolvedValueOnce(result({ result: { case: "policyRevision", value: 4 } }));
  await user.click(screen.getByRole("button", { name: "Save" }));
  const s = sent();
  expect(s.mode).toBe("observe");
  expect(s.rules).toEqual(settings.rules);
  expect(s.sources.github.allowed).toEqual(["ana", "bo"]);
  expect(s.sources.github.admission.pulls.unlistedAddressed).toBe("explicit");
  expect(s.sources.github.admission.ci).toEqual({ unlistedUnaddressed: "ignore" });
});

test("admission table offers only allowed choices per row", () => {
  render(<AdmissionTable value={admission} onChange={() => {}} layout="desktop" />);
  const options = (name: string) =>
    within(screen.getByRole("combobox", { name })).getAllByRole("option").map((o) => (o as HTMLOptionElement).value);
  for (const row of ["Issues", "Pull requests", "Comments"]) {
    expect(options(`${row}, listed unaddressed`)).toEqual(["inferred", "ignore"]);
    expect(options(`${row}, unlisted addressed`)).toEqual(["explicit", "inferred", "ignore"]);
    expect(options(`${row}, unlisted unaddressed`)).toEqual(["inferred", "ignore"]);
  }
  // A CI result has no sender: only the last cell.
  expect(screen.queryByRole("combobox", { name: "CI, listed unaddressed" })).toBeNull();
  expect(screen.queryByRole("combobox", { name: "CI, unlisted addressed" })).toBeNull();
  expect(options("CI, unlisted unaddressed")).toEqual(["inferred", "ignore"]);
});

test("a rule needs a unique name before it can be saved", async () => {
  const user = userEvent.setup();
  renderBlock("general");
  await user.click(screen.getByRole("button", { name: "Add rule" }));
  expect(screen.getByText("Every rule needs a name.")).toBeTruthy();
  expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
  const names = screen.getAllByRole("textbox", { name: "Name" });
  await user.type(names[1], "bugs");
  expect(screen.getByText("Two rules are named bugs.")).toBeTruthy();
  await user.type(names[1], "2");
  expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(false);
});

test("rules move up and down and are removed", async () => {
  const user = userEvent.setup();
  load({
    policy: policy(3, {
      ...settings,
      rules: [
        { name: "a", instructions: "", enabled: true },
        { name: "b", instructions: "", enabled: false },
      ],
    }),
  });
  renderBlock("general");
  await user.click(screen.getByRole("button", { name: "Move b up" }));
  setPolicy.mockResolvedValue(result({ result: { case: "policyRevision", value: 4 } }));
  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(sent().rules.map((r: { name: string }) => r.name)).toEqual(["b", "a"]);

  await user.click(screen.getByRole("button", { name: "Remove a" }));
  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(sent(1).rules.map((r: { name: string }) => r.name)).toEqual(["b"]);
});

test("waiting for the server before the first Snapshot", () => {
  useAthena.getState().reset();
  renderBlock("general");
  expect(screen.getByText("Waiting for the server.")).toBeTruthy();
});

test("overwrite can conflict again and still keeps the edit", async () => {
  const user = userEvent.setup();
  renderBlock("general");
  await user.click(mode("Auto"));
  setPolicy.mockResolvedValueOnce(result({ ok: false, code: "conflict" }));
  await user.click(screen.getByRole("button", { name: "Save" }));
  await screen.findByText("Changed on another device");
  act(() => delta(2n, 4, { ...settings, mode: "off" }));
  setPolicy.mockResolvedValueOnce(result({ ok: false, code: "conflict" }));
  await user.click(screen.getByRole("button", { name: "Overwrite" }));
  expect(setPolicy).toHaveBeenLastCalledWith(4, expect.any(String));
  expect(await screen.findByText("Changed on another device")).toBeTruthy();
  expect(mode("Auto").checked).toBe(true);
  act(() => delta(3n, 5, { ...settings, mode: "off" }));
  setPolicy.mockResolvedValueOnce(result({ result: { case: "policyRevision", value: 6 } }));
  await user.click(screen.getByRole("button", { name: "Overwrite" }));
  expect(setPolicy).toHaveBeenLastCalledWith(5, expect.any(String));
  expect(sent(2).mode).toBe("auto");
});

test("a conflict focuses Reload, and a finished save focuses the heading", async () => {
  const user = userEvent.setup();
  renderBlock("general");
  await user.click(mode("Auto"));
  setPolicy.mockResolvedValueOnce(result({ ok: false, code: "conflict" }));
  await user.click(screen.getByRole("button", { name: "Save" }));
  await screen.findByText("Changed on another device");
  expect(document.activeElement).toBe(screen.getByRole("button", { name: "Reload" }));

  setPolicy.mockResolvedValueOnce(result({ result: { case: "policyRevision", value: 4 } }));
  await user.click(screen.getByRole("button", { name: "Overwrite" }));
  expect(screen.queryByRole("region", { name: "Save General" })).toBeNull();
  expect(document.activeElement).toBe(screen.getByRole("heading", { name: "General" }));
});

test("moving and removing rules keeps the keyboard's place", async () => {
  const user = userEvent.setup();
  load({
    policy: policy(3, {
      ...settings,
      rules: [
        { name: "a", instructions: "", enabled: true },
        { name: "b", instructions: "", enabled: true },
        { name: "c", instructions: "", enabled: true },
      ],
    }),
  });
  renderBlock("general");
  const focused = () => document.activeElement?.getAttribute("aria-label") ?? document.activeElement?.textContent;
  await user.click(screen.getByRole("button", { name: "Move b down" }));
  // a, c, b: b is last, so its down arrow is disabled and the up arrow takes the focus.
  expect(focused()).toBe("Move b up");
  await user.click(screen.getByRole("button", { name: "Move b up" }));
  expect(focused()).toBe("Move b up");
  await user.click(screen.getByRole("button", { name: "Move a down" }));
  expect(focused()).toBe("Move a down");
  // b, a, c: removing b moves the focus to the next rule.
  await user.click(screen.getByRole("button", { name: "Remove b" }));
  expect(document.activeElement).toBe(screen.getAllByRole("textbox", { name: "Name" })[0]);
  await user.click(screen.getByRole("button", { name: "Remove c" }));
  await user.click(screen.getByRole("button", { name: "Remove a" }));
  expect(document.activeElement).toBe(screen.getByRole("button", { name: "Add rule" }));
});
