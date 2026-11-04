import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { Where } from "@/app/testkit";
import type { CommandResult } from "@/gen/athena/v1/commands_pb";
import * as actions from "@/lib/commands/actions";
import { useAthena } from "@/lib/store/state";
import { Toaster } from "@/ui/primitives/toast";
import { at, load } from "../tickets/fixtures";
import { canReevaluate, filterInWords, filteredText, parseOutcome, parseTicketChip } from "./outcome";
import { Routing } from "./Routing";

vi.mock("@/lib/commands/actions", async (orig) => ({
  ...(await orig<typeof import("@/lib/commands/actions")>()),
  reevaluate: vi.fn(),
  unsubscribe: vi.fn(),
  dismiss: vi.fn(),
}));
const reevaluate = vi.mocked(actions.reevaluate);
const unsubscribe = vi.mocked(actions.unsubscribe);
const dismiss = vi.mocked(actions.dismiss);
const ok = { ok: true, code: "" } as CommandResult;

const T = "github:acme/api#12";
const triggers = [
  { id: "tr-filtered", at: at(9000), source: "github", actor: "ana", kind: "comment", thread: "github:acme/web#3", text: "Filtered one", outcome: "filtered:not_listening" },
  { id: "tr-reached", at: at(8000), source: "github", actor: "bob", kind: "comment", addressed: "mention", thread: T, text: "Reached one", outcome: "reached:T-1" },
  { id: "tr-attached", at: at(7000), source: "github", actor: "cy", kind: "issue", thread: T, text: "Attached one", outcome: "attached:T-2" },
  { id: "tr-unavailable", at: at(6000), source: "github", actor: "di", kind: "comment", thread: T, text: "Unavailable one", outcome: "" },
  { id: "tr-new", at: at(5000), source: "github", actor: "ed", kind: "comment", thread: T, text: "Undecided one", outcome: "" },
  { id: "tr-held", at: at(4000), source: "github", actor: "fy", kind: "comment", thread: "github:acme/held#1", text: "Held by an eval", outcome: "filtered:not_listening requeued" },
  { id: "tr-requeued", at: at(3000), source: "github", actor: "gus", kind: "comment", thread: T, text: "Requeued one", outcome: "requeued" },
];
const decisions = [
  { id: "d-unavailable", at: at(6500), kind: "unavailable", thread: T, triggers: ["tr-unavailable"], reasons: [{ code: "timeout", values: {} }] },
  { id: "d-unavailable-done", at: at(6400), kind: "unavailable", thread: T, triggers: ["x"], reasons: [{ code: "timeout", values: {} }], dismissedAt: at(6450) },
  { id: "d-would", at: at(7500), kind: "ignore", thread: T, ticketId: "", triggers: ["tr-new"], wouldAccept: true, reasons: [{ code: "would_accept", values: { rule: "bug", text: "Fix it" } }] },
  { id: "d-accept", at: at(7100), kind: "accept", thread: T, ticketId: "T-2", triggers: ["tr-attached"], reasons: [{ code: "routed", values: { ticket: "T-2" } }] },
];
const threads = [
  { key: T, queued: 2, decisions: 3, tickets: ["T-1(own,bound)", "T-2(context)"] },
  { key: "github:acme/web#3", queued: 0, decisions: 0, tickets: [], closed: "merged" },
  { key: "github:acme/held#1", queued: 0, decisions: 1, tickets: ["T-3"], eval: { id: "e-1", strength: "explicit", startedAt: at(1000) } },
];

beforeEach(() => {
  load({ triggers, decisions, threads, tickets: [{ id: "T-1", title: "One" }] });
  reevaluate.mockResolvedValue(ok);
  unsubscribe.mockResolvedValue(ok);
  dismiss.mockResolvedValue(ok);
});
afterEach(() => {
  useAthena.getState().reset();
  vi.clearAllMocks();
});

function renderRouting(path = "/routing") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routing />
      <Where />
      <Toaster />
    </MemoryRouter>,
  );
}
const where = () => screen.getByTestId("where").textContent;
const triggerRow = (id: string) => document.querySelector<HTMLElement>(`[data-trigger-row="${id}"]`) as HTMLElement;
const tabLabels = () => screen.getAllByRole("tab").map((t) => t.textContent);

test("evaluate again only on allowed outcomes", () => {
  renderRouting();
  const offered = (id: string) => !!within(triggerRow(id)).queryByRole("button", { name: "Evaluate again" });
  // Filtered by its Source: offered.
  expect(offered("tr-filtered")).toBe(true);
  // Delivered to a Ticket, no judgement of it: offered.
  expect(offered("tr-reached")).toBe(true);
  // Eval was unavailable: offered (no outcome of its own, an unavailable Decision lists it).
  expect(offered("tr-unavailable")).toBe(true);
  // A non-stop Decision attached it: judged, not offered.
  expect(offered("tr-attached")).toBe(false);
  // Nothing happened to it yet, or it was only requeued: no judgement to redo.
  expect(offered("tr-new")).toBe(false);
  expect(offered("tr-requeued")).toBe(false);
  // The server refuses a Trigger an Eval holds: its Thread has an Eval in progress.
  expect(offered("tr-held")).toBe(false);
});

test("evaluate again follows the reference outcome by outcome", () => {
  const t = (outcome: string, thread = T) => ({ id: "t", thread, outcome }) as never;
  const none = { threads: new Map(), decisions: new Map() };
  expect(canReevaluate(t("filtered:not_listening"), none)).toBe(true);
  expect(canReevaluate(t("reached:T-1"), none)).toBe(true);
  expect(canReevaluate(t("attached:T-1"), none)).toBe(false);
  expect(canReevaluate(t("handedBack:T-1"), none)).toBe(false);
  expect(canReevaluate(t("filtered:not_listening", ""), none)).toBe(false);
  const d = (kind: string) => new Map([["d", { id: "d", kind, triggers: ["t"] } as never]]);
  expect(canReevaluate(t("reached:T-1"), { threads: new Map(), decisions: d("ignore") })).toBe(false);
  expect(canReevaluate(t("reached:T-1"), { threads: new Map(), decisions: d("stop") })).toBe(true);
  expect(canReevaluate(t(""), { threads: new Map(), decisions: d("unavailable") })).toBe(true);
  expect(canReevaluate(t(""), { threads: new Map([[T, { eval: { id: "e" } } as never]]), decisions: d("unavailable") })).toBe(false);
});

test("evaluate again sends the command once, disables while in flight and says a failure", async () => {
  const user = userEvent.setup();
  let finish!: (r: CommandResult) => void;
  reevaluate.mockReturnValueOnce(new Promise((res) => (finish = res)));
  renderRouting();
  const button = within(triggerRow("tr-filtered")).getByRole("button", { name: "Evaluate again" });
  await user.dblClick(button);
  expect(reevaluate).toHaveBeenCalledTimes(1);
  expect(reevaluate).toHaveBeenCalledWith("tr-filtered");
  expect((button as HTMLButtonElement).disabled).toBe(true);
  await act(async () => finish({ ok: false, code: "not_found" } as CommandResult));
  expect(await screen.findByText("Could not evaluate again")).toBeTruthy();
  expect((button as HTMLButtonElement).disabled).toBe(false);
});

test("triggers read as words: outcome, to agent mark, Thread and Ticket links, newest first", () => {
  renderRouting();
  const rows = [...document.querySelectorAll("[data-trigger-row]")].map((r) => r.getAttribute("data-trigger-row"));
  expect(rows).toEqual(["tr-filtered", "tr-reached", "tr-attached", "tr-unavailable", "tr-new", "tr-held", "tr-requeued"]);
  expect(within(triggerRow("tr-filtered")).getByText("Filtered: no source listens to this thread")).toBeTruthy();
  expect(within(triggerRow("tr-reached")).getByText("to agent")).toBeTruthy();
  expect(within(triggerRow("tr-filtered")).queryByText("to agent")).toBeNull();
  expect(within(triggerRow("tr-reached")).getByRole("link", { name: "T-1" }).getAttribute("href")).toBe("/tickets/T-1");
  expect(within(triggerRow("tr-attached")).getByRole("link", { name: "T-2" }).getAttribute("href")).toBe("/tickets/T-2");
  expect(within(triggerRow("tr-requeued")).getByText("Requeued")).toBeTruthy();
  expect(within(triggerRow("tr-new")).getByText("No outcome yet")).toBeTruthy();
  expect(within(triggerRow("tr-reached")).getByRole("link", { name: T }).getAttribute("href")).toContain("tab=threads");
  expect(filteredText("some_new_code")).toBe("Filtered: some new code");
  expect(filterInWords("not_listening")).toBe("no source listens to this thread");
  expect(parseOutcome("filtered:x reached:T-1,T-2 requeued")).toEqual([
    { kind: "filtered", code: "x" },
    { kind: "reached", tickets: ["T-1", "T-2"] },
    { kind: "requeued" },
  ]);
});

test("unsubscribe confirms", async () => {
  const user = userEvent.setup();
  renderRouting("/routing?tab=threads");
  const row = document.querySelector<HTMLElement>(`[data-thread-row="${T}"]`) as HTMLElement;
  await user.click(within(row).getByRole("button", { name: "Unsubscribe" }));
  // The first click only arms it and says what will happen.
  expect(unsubscribe).not.toHaveBeenCalled();
  const confirm = within(row).getByRole("button", { name: "Unsubscribe these 2 Tickets" });
  await new Promise((r) => setTimeout(r, 350));
  await user.click(confirm);
  expect(unsubscribe).toHaveBeenCalledTimes(1);
  expect(unsubscribe).toHaveBeenCalledWith(T);
  // A Thread nobody follows has nothing to unsubscribe.
  const web = document.querySelector<HTMLElement>('[data-thread-row="github:acme/web#3"]') as HTMLElement;
  expect(within(web).queryByRole("button", { name: /unsubscribe/i })).toBeNull();
});

test("threads show the key, queued count, Evaluating strength, Ticket flags, Decisions and closed", () => {
  renderRouting("/routing?tab=threads");
  const row = (k: string) => document.querySelector<HTMLElement>(`[data-thread-row="${k}"]`) as HTMLElement;
  const main = within(row(T));
  expect(main.getByText("2 queued")).toBeTruthy();
  expect(main.getByText("3 Decisions")).toBeTruthy();
  expect(main.getByRole("link", { name: /T-1.*own.*bound/ }).getAttribute("href")).toBe("/tickets/T-1");
  expect(main.getByRole("link", { name: /T-2.*context/ })).toBeTruthy();
  expect(main.queryByText(/Evaluating/)).toBeNull();
  expect(within(row("github:acme/held#1")).getByText("Evaluating · explicit")).toBeTruthy();
  expect(within(row("github:acme/web#3")).getByText("Closed: merged")).toBeTruthy();
  expect(within(row("github:acme/web#3")).getByText("0 Decisions")).toBeTruthy();
  expect(parseTicketChip("T-0001(own,bound,context)")).toEqual({ id: "T-0001", flags: ["own", "bound", "context"] });
  expect(parseTicketChip("T-9")).toEqual({ id: "T-9", flags: [] });
});

test("dismiss only undismissed would-accept (the server dismisses only unavailable Decisions)", async () => {
  const user = userEvent.setup();
  renderRouting("/routing?tab=decisions");
  const row = (id: string) => document.querySelector<HTMLElement>(`[data-decision-row="${id}"]`) as HTMLElement;
  const has = (id: string) => !!within(row(id)).queryByRole("button", { name: "Dismiss" });
  // core.Engine.Dismiss refuses anything but an unavailable Decision, so a would-accept ignore gets no button.
  expect(has("d-would")).toBe(false);
  expect(within(row("d-would")).getByText("would accept")).toBeTruthy();
  expect(has("d-accept")).toBe(false);
  expect(has("d-unavailable-done")).toBe(false);
  expect(within(row("d-unavailable-done")).getByText("dismissed")).toBeTruthy();
  expect(has("d-unavailable")).toBe(true);
  await user.click(within(row("d-unavailable")).getByRole("button", { name: "Dismiss" }));
  expect(dismiss).toHaveBeenCalledWith("d-unavailable");
});

test("decisions show reasons as code with values, and link their Ticket, Thread and triggers", () => {
  renderRouting("/routing?tab=decisions");
  const row = within(document.querySelector<HTMLElement>('[data-decision-row="d-would"]') as HTMLElement);
  expect(row.getByText("would_accept")).toBeTruthy();
  expect(row.getByText("rule=bug")).toBeTruthy();
  expect(row.getByText("text=Fix it")).toBeTruthy();
  expect(row.getByRole("link", { name: "tr-new" }).getAttribute("href")).toContain("trigger=tr-new");
  const accepted = within(document.querySelector<HTMLElement>('[data-decision-row="d-accept"]') as HTMLElement);
  expect(accepted.getByRole("link", { name: "T-2" }).getAttribute("href")).toBe("/tickets/T-2");
});

test("tab counts equal list lengths", async () => {
  const user = userEvent.setup();
  renderRouting();
  const count = (tab: string) => Number(tabLabels().find((l) => l?.startsWith(tab))?.replace(/\D/g, ""));
  expect(tabLabels()).toEqual(["Triggers 7", "Threads 3", "Decisions 4"]);
  expect(document.querySelectorAll("[data-trigger-row]")).toHaveLength(count("Triggers"));

  await user.selectOptions(screen.getByLabelText("Outcome"), "filtered");
  expect(where()).toContain("outcome=filtered");
  expect(tabLabels()[0]).toBe("Triggers 2");
  expect(document.querySelectorAll("[data-trigger-row]")).toHaveLength(2);

  await user.type(screen.getByRole("searchbox", { name: "Search Triggers" }), "held");
  expect(tabLabels()[0]).toBe("Triggers 1");

  await user.click(screen.getByRole("tab", { name: /Threads/ }));
  expect(where()).toContain("tab=threads");
  await user.selectOptions(screen.getByLabelText("Thread state"), "closed");
  expect(tabLabels()[1]).toBe("Threads 1");
  expect(document.querySelectorAll("[data-thread-row]")).toHaveLength(1);

  await user.click(screen.getByRole("tab", { name: /Decisions/ }));
  await user.selectOptions(screen.getByLabelText("Decision"), "unavailable");
  expect(tabLabels()[2]).toBe("Decisions 2");
  expect(document.querySelectorAll("[data-decision-row]")).toHaveLength(2);
  // The other tabs keep their own filters and counts.
  expect(tabLabels()).toEqual(["Triggers 1", "Threads 1", "Decisions 2"]);

  await user.selectOptions(screen.getByLabelText("Decision"), "would-accept");
  expect(tabLabels()[2]).toBe("Decisions 1");
});

test("filters in the URL are the starting point and can be cleared", async () => {
  const user = userEvent.setup();
  renderRouting("/routing?outcome=attached&tq=zzz");
  expect(screen.getByText("No Triggers match these filters.")).toBeTruthy();
  expect((screen.getByRole("searchbox", { name: "Search Triggers" }) as HTMLInputElement).value).toBe("zzz");
  await user.click(screen.getByRole("button", { name: "Clear filters" }));
  expect(where()).toBe("/routing");
  expect(tabLabels()[0]).toBe("Triggers 7");
});

test("a record the URL names is the selected row", () => {
  renderRouting("/routing?tab=triggers&trigger=tr-attached");
  expect(triggerRow("tr-attached").getAttribute("aria-current")).toBe("true");
  expect(triggerRow("tr-new").getAttribute("aria-current")).toBeNull();
});

test("empty states on a server without GitHub", () => {
  useAthena.getState().reset();
  const { unmount } = renderRouting();
  expect(tabLabels()).toEqual(["Triggers 0", "Threads 0", "Decisions 0"]);
  expect(screen.getByText("No Triggers yet.")).toBeTruthy();
  unmount();
  renderRouting("/routing?tab=threads");
  expect(screen.getByText("No Threads yet.")).toBeTruthy();
});

test("more than 200 triggers show 200, then Show more", async () => {
  const user = userEvent.setup();
  load({
    triggers: Array.from({ length: 230 }, (_, i) => ({ id: `t${i}`, at: at(i), source: "github", thread: T, text: `m${i}`, outcome: "attached:T-1" })),
  });
  renderRouting();
  expect(tabLabels()[0]).toBe("Triggers 230");
  expect(document.querySelectorAll("[data-trigger-row]")).toHaveLength(200);
  await user.click(screen.getByRole("button", { name: "Show more (30)" }));
  expect(document.querySelectorAll("[data-trigger-row]")).toHaveLength(230);
});
