import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { stubWidth } from "@/app/testkit";
import { LayoutProvider } from "@/app/Panes";
import type { CommandResult } from "@/gen/athena/v1/commands_pb";
import * as actions from "@/lib/commands/actions";
import { useAthena } from "@/lib/store/state";
import { load } from "../tickets/fixtures";
import { Trust } from "./Trust";

vi.mock("@/lib/commands/actions", async (orig) => ({
  ...(await orig<typeof import("@/lib/commands/actions")>()),
  setPolicy: vi.fn(),
}));
const setPolicy = vi.mocked(actions.setPolicy);

const settings = {
  mode: "observe",
  allow: [
    { tool: "Bash", rule: "make test", repo: "acme/web", added: "2026-11-01T10:00:00Z" },
    { tool: "Edit", rule: "docs/**", repo: "acme/api", added: "2026-11-02T10:00:00Z" },
  ],
};

beforeEach(() => {
  stubWidth(1440);
  load({ policy: { revision: 3, version: "abc", settingsJson: JSON.stringify(settings) } });
});
afterEach(() => {
  useAthena.getState().reset();
  vi.clearAllMocks();
});

function renderIt() {
  return render(
    <MemoryRouter>
      <LayoutProvider value="desktop">
        <Trust />
      </LayoutProvider>
    </MemoryRouter>,
  );
}

test("the owner row is fixed to Standard and strangers start Restricted", () => {
  renderIt();
  const owner = screen.getByRole("combobox", { name: "You" }) as HTMLSelectElement;
  expect(owner.disabled).toBe(true);
  expect(owner.value).toBe("standard");
  expect((screen.getByRole("combobox", { name: "Strangers" }) as HTMLSelectElement).value).toBe("restricted");
  expect((screen.getByRole("combobox", { name: "Bots" }) as HTMLSelectElement).disabled).toBe(false);
  const after = screen.getByRole("combobox", { name: /Offer a standing rule after/ }) as HTMLSelectElement;
  expect(after.selectedOptions[0].textContent).toBe("5 (default)");
  expect(after.options[0].value).toBe("2");
  expect(after.options[after.options.length - 1].value).toBe("50");
});

test("Remove drops one standing rule and saves the other", async () => {
  const user = userEvent.setup();
  renderIt();
  const rules = within(screen.getByRole("region", { name: "Standing rules" }));
  expect(rules.getAllByRole("listitem")).toHaveLength(2);
  expect(rules.getAllByRole("listitem")[0].textContent).toBe("Bash · make test in acme/web, added 1 Nov 2026Remove");
  setPolicy.mockResolvedValueOnce({ ok: true, code: "" } as CommandResult);
  await user.click(rules.getByRole("button", { name: "Remove Bash · make test in acme/web" }));
  expect(rules.getAllByRole("listitem")).toHaveLength(1);
  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(JSON.parse(setPolicy.mock.calls[0][1]).allow).toEqual([settings.allow[1]]);
});
