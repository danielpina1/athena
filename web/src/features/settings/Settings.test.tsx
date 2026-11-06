import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useNavigate } from "react-router";
import { guardedNavigate } from "@/app/guardedNavigate";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayoutProvider } from "@/app/Panes";
import { stubWidth, Where } from "@/app/testkit";
import { useAthena } from "@/lib/store/state";
import { load } from "../tickets/fixtures";
import { Settings } from "./Settings";

vi.mock("@/lib/commands/actions", async (orig) => ({
  ...(await orig<typeof import("@/lib/commands/actions")>()),
  listPasskeys: vi.fn(async () => ({ ok: true, code: "", result: { case: "passkeys", value: { passkeys: [] } } })),
}));
vi.mock("@/lib/auth/session", async (orig) => ({
  ...(await orig<typeof import("@/lib/auth/session")>()),
  getSession: vi.fn(async () => ({ signedIn: true })),
}));

const settings = {
  mode: "observe",
  rules: [],
  sources: { github: { allowed: ["ana"], bots: [], admission: {} } },
};

beforeEach(() => {
  load({
    policy: { revision: 1, settingsJson: JSON.stringify(settings) },
    node: { name: "mac-mini", slots: 4, slotsInUse: 1, claudeVersion: "2.1.3" },
  });
  useAthena.setState({ serverVersion: "0.4.0" });
});
afterEach(() => {
  useAthena.getState().reset();
  vi.clearAllMocks();
});

/** What the palette and the g shortcuts do: navigate through the guard. */
function GoTickets() {
  const navigate = useNavigate();
  return (
    <button type="button" onClick={() => guardedNavigate(navigate, "/tickets")}>
      Go to Tickets
    </button>
  );
}

function renderSettings(path: string, layout: "phone" | "desktop") {
  stubWidth(layout === "phone" ? 390 : 1440);
  return render(
    <MemoryRouter initialEntries={[path]}>
      <LayoutProvider value={layout}>
        <Routes>
          <Route path="settings/:group?" element={<Settings />} />
          <Route path="*" element={<p>Elsewhere</p>} />
        </Routes>
        <Where />
        <GoTickets />
      </LayoutProvider>
    </MemoryRouter>,
  );
}
const where = () => screen.getByTestId("where").textContent;
const sections = () => screen.getByRole("navigation", { name: "Settings sections" });

test("desktop shows the section list beside General by default", () => {
  renderSettings("/settings", "desktop");
  expect(within(sections()).getAllByRole("link").map((l) => l.textContent)).toEqual([
    "General",
    "Sources › GitHub",
    "Notifications",
    "Devices",
    "Prompts",
    "Execution",
    "System",
  ]);
  expect(within(sections()).getByRole("link", { name: "General" }).getAttribute("aria-current")).toBe("page");
  expect(screen.getByRole("heading", { name: "General" })).toBeTruthy();
});

test("the phone shows the list first and a section on tap, with a back link", async () => {
  const user = userEvent.setup();
  renderSettings("/settings", "phone");
  expect(screen.getByRole("heading", { level: 1, name: "Settings" })).toBeTruthy();
  expect(screen.queryByRole("heading", { name: "General" })).toBeNull();

  await user.click(within(sections()).getByRole("link", { name: "System" }));
  expect(where()).toBe("/settings/system");
  expect(screen.getByRole("heading", { level: 1, name: "System" })).toBeTruthy();
  expect(screen.getByText("0.4.0")).toBeTruthy();
  await user.click(screen.getByRole("link", { name: "Settings" }));
  expect(where()).toBe("/settings");
});

test("execution shows the node's slots and Claude version, read-only", () => {
  renderSettings("/settings/execution", "desktop");
  const facts = screen.getByRole("region", { name: "Execution" });
  expect(within(facts).getByText("4")).toBeTruthy();
  expect(within(facts).getByText("2.1.3")).toBeTruthy();
  expect(within(facts).queryByRole("textbox")).toBeNull();
  expect(within(facts).getByText(/athena server --slots/)).toBeTruthy();
});

test("execution lists each runtime's version", () => {
  load({
    policy: { revision: 1, settingsJson: JSON.stringify(settings) },
    node: {
      name: "mac-mini",
      slots: 4,
      runtimes: [
        { name: "claude", version: "", ready: true },
        { name: "codex", version: "0.161.0", ready: true, tested: false },
      ],
    },
  });
  renderSettings("/settings/execution", "desktop");
  const facts = screen.getByRole("region", { name: "Execution" });
  // An empty version shows nothing (R70): the runtime has no version row.
  expect(within(facts).queryByText("Claude Code version")).toBeNull();
  expect(within(facts).queryByText("Not reported")).toBeNull();
  const codex = within(facts).getByText("Codex version").nextElementSibling;
  expect(codex?.textContent).toBe("0.161.0 untested");
  // One gap before "untested": a space, no margin besides.
  expect(codex?.querySelector("span")?.className).not.toMatch(/\bml-/);
});

test("system links to Routing and Nodes and switches the theme", async () => {
  const user = userEvent.setup();
  renderSettings("/settings/system", "desktop");
  const system = screen.getByRole("region", { name: "System" });
  expect(within(system).getByRole("link", { name: "Routing" }).getAttribute("href")).toBe("/routing");
  expect(within(system).getByRole("link", { name: "Nodes" }).getAttribute("href")).toBe("/nodes");
  await user.click(within(system).getByRole("radio", { name: "Dark" }));
  expect(document.documentElement.dataset.theme).toBe("dark");
  await user.click(within(system).getByRole("radio", { name: "Light" }));
  expect(document.documentElement.dataset.theme).toBe("light");
});

test("leaving with unsaved changes asks first", async () => {
  const user = userEvent.setup();
  renderSettings("/settings/general", "desktop");
  await user.click(screen.getByRole("radio", { name: /^Auto/ }));

  await user.click(within(sections()).getByRole("link", { name: "Devices" }));
  expect(where()).toBe("/settings/general");
  const dialog = screen.getByRole("dialog", { name: "Discard unsaved changes?" });
  await user.click(within(dialog).getByRole("button", { name: "Keep editing" }));
  expect(where()).toBe("/settings/general");
  expect((screen.getByRole("radio", { name: /^Auto/ }) as HTMLInputElement).checked).toBe(true);

  await user.click(within(sections()).getByRole("link", { name: "Devices" }));
  await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Discard changes" }));
  expect(where()).toBe("/settings/devices");

  // Nothing unsaved: links go straight through.
  await user.click(within(sections()).getByRole("link", { name: "System" }));
  expect(where()).toBe("/settings/system");
});

test("an unknown section goes back to the list", () => {
  renderSettings("/settings/nowhere", "desktop");
  expect(where()).toBe("/settings");
});

test("the palette and shortcuts ask first too, and go straight through when nothing is unsaved", async () => {
  const user = userEvent.setup();
  renderSettings("/settings/general", "desktop");
  await user.click(screen.getByRole("radio", { name: /^Auto/ }));
  await user.click(screen.getByRole("button", { name: "Go to Tickets" }));
  expect(where()).toBe("/settings/general");
  await user.click(within(screen.getByRole("dialog", { name: "Discard unsaved changes?" })).getByRole("button", { name: "Discard changes" }));
  expect(where()).toBe("/tickets");
});

test("guarded navigation goes straight through without Settings open", async () => {
  const user = userEvent.setup();
  renderSettings("/elsewhere", "desktop");
  await user.click(screen.getByRole("button", { name: "Go to Tickets" }));
  expect(where()).toBe("/tickets");
});
