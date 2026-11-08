import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { stubWidth } from "@/app/testkit";
import { LayoutProvider } from "@/app/Panes";
import type { CommandResult } from "@/gen/athena/v1/commands_pb";
import * as actions from "@/lib/commands/actions";
import { useAthena } from "@/lib/store/state";
import { load } from "../tickets/fixtures";
import { Repositories } from "./Repositories";

vi.mock("@/lib/commands/actions", async (orig) => ({
  ...(await orig<typeof import("@/lib/commands/actions")>()),
  setPolicy: vi.fn(),
}));
const setPolicy = vi.mocked(actions.setPolicy);

const settings = { mode: "observe", future: 1 };

beforeEach(() => {
  stubWidth(1440);
  load({
    policy: { revision: 3, version: "abc", settingsJson: JSON.stringify(settings) },
    repos: [
      { name: "acme/web", kind: "github", owed: 2, ceiling: 3, braked: 1, mode: "live" },
      { name: "/srv/local", kind: "dir" },
    ],
  });
});
afterEach(() => {
  useAthena.getState().reset();
  vi.clearAllMocks();
});

function renderIt() {
  return render(
    <MemoryRouter>
      <LayoutProvider value="desktop">
        <Repositories />
      </LayoutProvider>
    </MemoryRouter>,
  );
}

test("lists the GitHub repositories with the default ceiling and the live numbers", () => {
  renderIt();
  expect(screen.getByText("acme/web")).toBeTruthy();
  expect(screen.queryByText("/srv/local")).toBeNull();
  expect(screen.getByText("2 of 3 waiting on you, 1 held back")).toBeTruthy();
  const own = screen.getByRole("combobox", { name: /Hold work in acme\/web/ }) as HTMLSelectElement;
  expect(own.selectedOptions[0].textContent).toBe("3 (default)");
  const all = screen.getByRole("combobox", { name: /^Hold self-chosen work/ }) as HTMLSelectElement;
  expect(all.selectedOptions[0].textContent).toBe("3 (default)");
  expect(all.options).toHaveLength(20);
});

test("switching a repository to shadow saves its mode and nothing else", async () => {
  const user = userEvent.setup();
  renderIt();
  setPolicy.mockResolvedValueOnce({ ok: true, code: "" } as CommandResult);
  await user.selectOptions(screen.getByRole("combobox", { name: "Mode of acme/web" }), "shadow");
  await user.selectOptions(screen.getByRole("combobox", { name: /Hold work in acme\/web/ }), "5");
  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(setPolicy).toHaveBeenCalledWith(3, expect.any(String));
  expect(JSON.parse(setPolicy.mock.calls[0][1])).toEqual({ ...settings, repos: { "acme/web": { mode: "shadow", wipCeiling: 5 } } });
  await act(async () => {});
});
