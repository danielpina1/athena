import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { beforeEach, expect, test, vi } from "vitest";
import { CommandResultSchema } from "@/gen/athena/v1/commands_pb";
import * as actions from "@/lib/commands/actions";
import { useAthena } from "@/lib/store/state";
import { load } from "@/features/tickets/fixtures";
import { Toaster } from "@/ui/primitives/toast";
import { NeedsYou } from "./NeedsYou";
import { __resetWaitedForTest } from "./OfflineCard";

vi.mock("@/lib/commands/actions", async (orig) => ({
  ...(await orig<typeof import("@/lib/commands/actions")>()),
  retryElsewhere: vi.fn(),
}));

const retryElsewhere = vi.mocked(actions.retryElsewhere);

// 14:02 local time, whatever the machine's zone.
const lastSeen = timestampFromMs(new Date(2026, 10, 4, 14, 2, 0).getTime());

const nodes = [
  { id: "local", name: "studio", local: true, state: "online", slots: 2 },
  { id: "n-mini", name: "mini", state: "offline", lastSeen, slots: 2 },
];
const repoTicket = {
  id: "T-1",
  title: "Fix the flaky test",
  stage: "person",
  action: "offline",
  repo: "acme/api",
  nodeId: "n-mini",
  nodeName: "mini",
};
const dirTicket = {
  id: "T-2",
  title: "Tidy the notes",
  stage: "person",
  action: "offline",
  dir: "/Users/me/notes",
  nodeId: "n-mini",
  nodeName: "mini",
};

function renderPage() {
  return render(
    <MemoryRouter>
      <NeedsYou />
      <Toaster />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  useAthena.getState().reset();
  __resetWaitedForTest();
  vi.clearAllMocks();
  retryElsewhere.mockResolvedValue(create(CommandResultSchema, { ok: true }));
});

test("a repo Ticket on an offline node says since when and offers Retry elsewhere, then Wait", async () => {
  const user = userEvent.setup();
  load({ nodes, tickets: [repoTicket] });
  renderPage();
  const card = screen.getByRole("group", { name: "Fix the flaky test" });
  expect(within(card).getByText((_, el) => el?.tagName === "P" && el.textContent === "mini is offline since 14:02")).toBeTruthy();
  // The stage names the node.
  expect(within(card).getByText("mini is offline")).toBeTruthy();
  const buttons = within(card).getAllByRole("button");
  expect(buttons.map((b) => b.textContent)).toEqual(["Wait", "Retry elsewhere"]);
  await user.click(within(card).getByRole("button", { name: "Retry elsewhere" }));
  expect(retryElsewhere).toHaveBeenCalledWith("T-1");
  await waitFor(() => expect(screen.getByText("Retrying elsewhere")).toBeTruthy());
});

test("Wait hides the card for this view only, leaving the Ticket waiting", async () => {
  const user = userEvent.setup();
  load({ nodes, tickets: [repoTicket] });
  const { unmount } = renderPage();
  await user.click(screen.getByRole("button", { name: "Wait" }));
  expect(screen.queryByRole("group", { name: "Fix the flaky test" })).toBeNull();
  expect(retryElsewhere).not.toHaveBeenCalled();
  unmount();
  // The store still has it waiting on the owner: nothing was sent.
  expect(useAthena.getState().tickets.get("T-1")?.action).toBe("offline");
});

test("a refused Retry elsewhere says the server's sentence", async () => {
  const user = userEvent.setup();
  retryElsewhere.mockResolvedValue(
    create(CommandResultSchema, { ok: false, code: "invalid", message: "this Ticket's node is not offline" }),
  );
  load({ nodes, tickets: [repoTicket] });
  renderPage();
  await user.click(screen.getByRole("button", { name: "Retry elsewhere" }));
  await waitFor(() => expect(screen.getByText("this Ticket's node is not offline")).toBeTruthy());
});

test("a Dir Ticket only says it continues when the node is back", () => {
  load({ nodes, tickets: [dirTicket] });
  renderPage();
  const card = screen.getByRole("group", { name: "Tidy the notes" });
  expect(within(card).getByText("This Ticket works in a folder on mini; it continues when mini is back.")).toBeTruthy();
  expect(within(card).queryByText(/since/)).toBeNull();
  expect(within(card).queryByRole("button")).toBeNull();
});
