import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { stubWidth, Where } from "@/app/testkit";
import type { CommandResult } from "@/gen/athena/v1/commands_pb";
import { at, load } from "@/features/tickets/fixtures";
import * as actions from "@/lib/commands/actions";
import { fakeStreamLink, says, type FakeStreamLink } from "@/lib/stream/testkit";
import { connectStreams } from "@/lib/stream/useSessionStream";
import { useAthena } from "@/lib/store/state";
import { PinnedSessions, PinToggle } from "./PinnedSessions";
import { act } from "react";

vi.mock("@/lib/commands/actions", async (orig) => ({
  ...(await orig<typeof import("@/lib/commands/actions")>()),
  watch: vi.fn(),
  unwatch: vi.fn(),
  onWatchLost: vi.fn(() => () => {}),
}));

const ok = { ok: true, code: "" } as CommandResult;
let link: FakeStreamLink;
let disconnect: () => void;
const t = (id: string, title: string) => ({ id, title, stage: "working", updatedAt: at(0), createdAt: at(0) });

beforeEach(() => {
  vi.mocked(actions.watch).mockResolvedValue(ok);
  vi.mocked(actions.unwatch).mockResolvedValue(ok);
  link = fakeStreamLink("open");
  disconnect = connectStreams(link);
  load({ tickets: [t("T-1", "Ship the API"), t("T-2", "Fix the build"), t("T-3", "Write docs"), t("T-4", "Tidy up")] });
});
afterEach(() => {
  disconnect();
  vi.clearAllMocks();
  useAthena.getState().reset();
});

function renderAt(path: string, ui: React.ReactNode) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      {ui}
      <Where />
    </MemoryRouter>,
  );
}

test("pinned sessions show side by side at 1440 px, up to 3, from the URL", async () => {
  stubWidth(1440);
  renderAt("/tickets?pin=T-1,T-2,T-1,T-3,T-4", <PinnedSessions />);
  const strip = screen.getByRole("region", { name: "Pinned sessions" });
  const pinned = within(strip).getAllByRole("article");
  expect(pinned).toHaveLength(3);
  expect(pinned.map((p) => within(p).getByRole("link").textContent)).toEqual(["Ship the API", "Fix the build", "Write docs"]);
  // Each one is watched and streamed, read-only.
  await waitFor(() => expect(actions.watch).toHaveBeenCalledTimes(3));
  expect(link.opens().map((o) => o.ticketId)).toEqual(["T-1", "T-2", "T-3"]);
  // The stream opens at once; its output shows when the session view's chunk has loaded.
  act(() => link.items("T-2", says(2)));
  expect(await within(pinned[1]).findByText("line 10")).toBeTruthy();
  expect(within(strip).queryByRole("textbox")).toBeNull();

  await userEvent.click(within(pinned[0]).getByRole("button", { name: "Unpin T-1" }));
  expect(screen.getByTestId("where").textContent).toBe("/tickets?pin=T-2,T-3");
});

test("two quick unpins both land", () => {
  stubWidth(1440);
  renderAt("/tickets?pin=T-1,T-2,T-3", <PinnedSessions />);
  const one = screen.getByRole("button", { name: "Unpin T-1" });
  const two = screen.getByRole("button", { name: "Unpin T-2" });
  // Both before the router re-renders.
  act(() => {
    one.click();
    two.click();
  });
  expect(screen.getByTestId("where").textContent).toBe("/tickets?pin=T-3");
});

test("below 1440 px pins are ignored but kept in the URL", () => {
  stubWidth(1439);
  renderAt("/tickets/T-1?pin=T-1", <><PinnedSessions /><PinToggle ticketId="T-1" /></>);
  expect(screen.queryByRole("region", { name: "Pinned sessions" })).toBeNull();
  expect(screen.queryByRole("button", { name: /Pin/ })).toBeNull();
  expect(link.opens()).toEqual([]);
  expect(screen.getByTestId("where").textContent).toBe("/tickets/T-1?pin=T-1");
});

test("the pin control pins up to 3 and keeps the other parameters", async () => {
  stubWidth(1440);
  const user = userEvent.setup();
  const view = renderAt("/tickets/T-4?stage=working&tab=session", <PinToggle ticketId="T-4" />);
  const pin = screen.getByRole("button", { name: "Pin session" });
  expect(pin.getAttribute("aria-pressed")).toBe("false");
  await user.click(pin);
  expect(screen.getByTestId("where").textContent).toBe("/tickets/T-4?stage=working&tab=session&pin=T-4");
  expect(screen.getByRole("button", { name: "Pin session" }).getAttribute("aria-pressed")).toBe("true");
  await user.click(screen.getByRole("button", { name: "Pin session" }));
  expect(screen.getByTestId("where").textContent).toBe("/tickets/T-4?stage=working&tab=session");
  view.unmount();

  renderAt("/tickets/T-4?pin=T-1,T-2,T-3", <PinToggle ticketId="T-4" />);
  const full = screen.getByRole("button", { name: "Pin session" }) as HTMLButtonElement;
  expect(full.disabled).toBe(true);
  expect(full.title).toBe("Up to 3 sessions can be pinned");
});
