import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayoutProvider } from "@/app/Panes";
import { stubWidth } from "@/app/testkit";
import type { CommandResult } from "@/gen/athena/v1/commands_pb";
import * as actions from "@/lib/commands/actions";
import { useAthena } from "@/lib/store/state";
import { load } from "../tickets/fixtures";
import { Autopilot } from "./AutopilotSettings";

vi.mock("@/lib/commands/actions", async (orig) => ({
  ...(await orig<typeof import("@/lib/commands/actions")>()),
  setPolicy: vi.fn(),
}));
const setPolicy = vi.mocked(actions.setPolicy);
const ok = { ok: true, code: "" } as CommandResult;

const night = { name: "Night shift", instructions: "Tidy up", repo: "acme/web", enabled: true, cron: "0 3 * * *", future: 1 };
const repos = [
  { name: "acme/web", kind: "github" },
  { name: "acme/api", kind: "github" },
  { name: "/srv/local", kind: "dir" },
];

function seed(settings: object, r: object[] = repos) {
  load({ policy: { revision: 3, version: "abc", settingsJson: JSON.stringify(settings) }, repos: r });
}

beforeEach(() => stubWidth(1440));
afterEach(() => {
  useAthena.getState().reset();
  vi.clearAllMocks();
});

function renderIt() {
  return render(
    <MemoryRouter>
      <LayoutProvider value="desktop">
        <Autopilot />
      </LayoutProvider>
    </MemoryRouter>,
  );
}

test("adds an entry and saves the document", async () => {
  seed({ mode: "auto", future: 2 });
  const user = userEvent.setup();
  renderIt();
  setPolicy.mockResolvedValueOnce(ok);
  await user.click(screen.getByRole("button", { name: "Add entry" }));
  await user.type(screen.getByRole("textbox", { name: "Name" }), "Night");
  await user.type(screen.getByRole("textbox", { name: "Instructions" }), "Tidy up");
  await user.selectOptions(screen.getByRole("combobox", { name: "Repository" }), "acme/api");
  await user.selectOptions(screen.getByRole("combobox", { name: "Preset" }), "Every night at 03:00");
  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(setPolicy).toHaveBeenCalledWith(3, expect.any(String));
  expect(JSON.parse(setPolicy.mock.calls[0][1])).toEqual({
    mode: "auto",
    future: 2,
    autopilot: [{ name: "Night", instructions: "Tidy up", repo: "acme/api", enabled: true, cron: "0 3 * * *" }],
  });
  await act(async () => {});
});

test("choosing a preset fills the cron field, and typing one selects Custom", async () => {
  seed({ mode: "auto", autopilot: [{ ...night, cron: "5 4 * * *" }] });
  const user = userEvent.setup();
  renderIt();
  const cron = screen.getByRole("textbox", { name: "Schedule" }) as HTMLInputElement;
  const preset = screen.getByRole("combobox", { name: "Preset" }) as HTMLSelectElement;
  expect(preset.selectedOptions[0].textContent).toBe("Custom");
  await user.selectOptions(preset, "Weekdays at 09:00");
  expect(cron.value).toBe("0 9 * * 1-5");
  expect(preset.selectedOptions[0].textContent).toBe("Weekdays at 09:00");
  expect(screen.getByText("Minute, hour, day, month, weekday, in this server's local time. At least 15 minutes apart.")).toBeTruthy();
});

test("the trigger decides between the schedule field and the hours", async () => {
  seed({ mode: "auto", autopilot: [night] });
  const user = userEvent.setup();
  renderIt();
  const trigger = screen.getByRole("combobox", { name: "Trigger" }) as HTMLSelectElement;
  expect([...trigger.options].map((o) => o.textContent)).toEqual([
    "On a schedule",
    "When the default branch moves",
    "When dependency files change",
    "When CI stays red",
  ]);
  expect(screen.queryByRole("spinbutton", { name: "Hours" })).toBeNull();
  await user.selectOptions(trigger, "When the default branch moves");
  expect(screen.queryByRole("textbox", { name: "Schedule" })).toBeNull();
  expect(screen.queryByRole("spinbutton", { name: "Hours" })).toBeNull();
  await user.selectOptions(trigger, "When CI stays red");
  expect((screen.getByRole("spinbutton", { name: "Hours" }) as HTMLInputElement).value).toBe("4");
  await user.selectOptions(trigger, "On a schedule");
  expect((screen.getByRole("textbox", { name: "Schedule" }) as HTMLInputElement).value).toBe("0 3 * * *");
});

test("a server error shows in the save bar and keeps the edit", async () => {
  seed({ mode: "auto", autopilot: [night] });
  const user = userEvent.setup();
  renderIt();
  setPolicy.mockResolvedValueOnce({ ok: false, code: "invalid" } as CommandResult);
  const cron = screen.getByRole("textbox", { name: "Schedule" }) as HTMLInputElement;
  await user.clear(cron);
  await user.type(cron, "* * * * *");
  await user.click(screen.getByRole("button", { name: "Save" }));
  const bar = await screen.findByRole("region", { name: "Save Autopilot" });
  expect(within(bar).getByRole("status").textContent).not.toBe("Unsaved changes");
  expect(within(bar).getByRole("button", { name: "Try again" })).toBeTruthy();
  expect(cron.value).toBe("* * * * *");
});

test("the mode line follows the Policy", () => {
  const line = (mode: string) => {
    seed({ mode, autopilot: [] });
    const r = renderIt();
    const found = {
      off: screen.queryByText("Accept mode is off, so these entries do nothing yet."),
      observe: screen.queryByText("Accept mode is observe, so Athena only records what it would have started."),
    };
    r.unmount();
    useAthena.getState().reset();
    return found;
  };
  expect(line("off").off).toBeTruthy();
  expect(line("observe").observe).toBeTruthy();
  const auto = line("auto");
  expect(auto.off ?? auto.observe).toBeNull();
  // A Policy that never set a mode is off.
  expect(line("").off).toBeTruthy();
});

test("See its runs links a saved entry to its Triggers, and an unsaved one has none", async () => {
  seed({ mode: "auto", autopilot: [night] });
  const user = userEvent.setup();
  renderIt();
  const link = screen.getByRole("link", { name: "See its runs" });
  expect(link.getAttribute("href")).toBe("/routing?q=schedule:night-shift");
  await user.click(screen.getByRole("button", { name: "Add entry" }));
  expect(screen.getAllByRole("link", { name: "See its runs" })).toHaveLength(1);
});

test("with no GitHub repository known a line replaces the select", async () => {
  seed({ mode: "auto" }, [{ name: "/srv/local", kind: "dir" }]);
  const user = userEvent.setup();
  renderIt();
  await user.click(screen.getByRole("button", { name: "Add entry" }));
  expect(screen.getByText("No GitHub repository is known yet.")).toBeTruthy();
  expect(screen.queryByRole("combobox", { name: "Repository" })).toBeNull();
});

test("an incomplete entry cannot be saved and the bar says why", async () => {
  seed({ mode: "auto", autopilot: [night] });
  const user = userEvent.setup();
  renderIt();
  await user.click(screen.getByRole("button", { name: "Add entry" }));
  const bar = screen.getByRole("region", { name: "Save Autopilot" });
  expect(within(bar).getByText("Every entry needs a name.")).toBeTruthy();
  expect((within(bar).getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
  // Removing the new entry is no change at all.
  await user.click(screen.getByRole("button", { name: "Remove entry 2" }));
  expect(screen.queryByRole("region", { name: "Save Autopilot" })).toBeNull();
});

test("removing the last entry saves the Policy without the key", async () => {
  seed({ mode: "auto", future: 1, autopilot: [night] });
  const user = userEvent.setup();
  renderIt();
  setPolicy.mockResolvedValueOnce(ok);
  await user.click(screen.getByRole("button", { name: "Remove Night shift" }));
  expect(document.activeElement).toBe(screen.getByRole("button", { name: "Add entry" }));
  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(JSON.parse(setPolicy.mock.calls[0][1])).toEqual({ mode: "auto", future: 1 });
  await act(async () => {});
});
