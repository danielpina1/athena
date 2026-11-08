import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { Where } from "@/app/testkit";
import type { CommandResult } from "@/gen/athena/v1/commands_pb";
import * as actions from "@/lib/commands/actions";
import { useAthena } from "@/lib/store/state";
import { Toaster } from "@/ui/primitives/toast";
import { at, load } from "../tickets/fixtures";
import { canReevaluate, filterInWords, filteredText, indexDecisions, parseOutcome, parseTicketChip } from "./outcome";
import { Routing } from "./Routing";
import { selectThreads } from "./Threads";
import { selectTriggers } from "./Triggers";

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
  { id: "tr-attached", at: at(7000), source: "github", actor: "cy", kind: "issue", thread: T, text: "Attached one", outcome: "reached:T-2 attached:T-2" },
  { id: "tr-unavailable", at: at(6000), source: "github", actor: "di", kind: "comment", thread: T, text: "Unavailable one", outcome: "" },
  { id: "tr-new", at: at(5000), source: "github", actor: "ed", kind: "comment", thread: T, text: "Undecided one", outcome: "" },
  { id: "tr-held", at: at(4000), source: "github", actor: "fy", kind: "comment", thread: "github:acme/held#1", text: "Held by an eval", outcome: "filtered:not_listening requeued" },
  { id: "tr-requeued", at: at(3000), source: "github", actor: "gus", kind: "comment", thread: T, text: "Requeued one", outcome: "requeued" },
];
const decisions = [
  { id: "d-unavailable", at: at(6500), kind: "unavailable", thread: T, triggers: ["tr-unavailable"], reasons: [{ code: "timeout", values: {} }] },
  { id: "d-unavailable-done", at: at(6400), kind: "unavailable", thread: T, triggers: ["x"], reasons: [{ code: "timeout", values: {} }], dismissedAt: at(6450) },
  { id: "d-would", at: at(7500), kind: "ignore", thread: T, ticketId: "", triggers: ["tr-new"], wouldAccept: true, reasons: [{ code: "would_accept", values: { rule: "bug", text: "Fix it" } }] },
  { id: "d-accept", at: at(7100), kind: "accept", thread: T, ticketId: "T-2", triggers: ["tr-requeued"], reasons: [{ code: "routed", values: { ticket: "T-2" } }] },
  { id: "d-routed", at: at(7050), kind: "ignore", thread: T, ticketId: "", triggers: ["tr-attached"], reasons: [{ code: "routed", values: { ticket: "T-2" } }] },
];
const threads = [
  { key: "github:bigacme/api#12", queued: 0, decisions: 0, tickets: [] },
  { key: "github:acme/api#123", queued: 0, decisions: 0, tickets: [] },
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
  vi.useRealTimers();
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
  const none = { threads: new Map(), judged: indexDecisions([]) };
  expect(canReevaluate(t("filtered:not_listening"), none)).toBe(true);
  expect(canReevaluate(t("reached:T-1"), none)).toBe(true);
  expect(canReevaluate(t("attached:T-1"), none)).toBe(false);
  expect(canReevaluate(t("handedBack:T-1"), none)).toBe(false);
  expect(canReevaluate(t("filtered:not_listening", ""), none)).toBe(false);
  const d = (kind: string) => indexDecisions([{ id: "d", kind, triggers: ["t"] } as never]);
  expect(canReevaluate(t("reached:T-1"), { threads: new Map(), judged: d("ignore") })).toBe(false);
  expect(canReevaluate(t("reached:T-1"), { threads: new Map(), judged: d("stop") })).toBe(true);
  expect(canReevaluate(t(""), { threads: new Map(), judged: d("unavailable") })).toBe(true);
  expect(canReevaluate(t(""), { threads: new Map([[T, { eval: { id: "e" } } as never]]), judged: d("unavailable") })).toBe(false);
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
  // Only the clock Confirm reads is faked: Testing Library waits on real timers.
  vi.useFakeTimers({ toFake: ["performance"] });
  const user = userEvent.setup({ delay: null });
  renderRouting("/routing?tab=threads");
  const row = document.querySelector<HTMLElement>(`[data-thread-row="${T}"]`) as HTMLElement;
  await user.click(within(row).getByRole("button", { name: "Unsubscribe" }));
  // The first click only arms it and says what will happen.
  expect(unsubscribe).not.toHaveBeenCalled();
  const confirm = within(row).getByRole("button", { name: "Unsubscribe these 2 Tickets" });
  await act(async () => void vi.advanceTimersByTime(350));
  await user.click(confirm);
  expect(unsubscribe).toHaveBeenCalledTimes(1);
  expect(unsubscribe).toHaveBeenCalledWith(T);
  // A Thread nobody follows has nothing to unsubscribe.
  const web = document.querySelector<HTMLElement>('[data-thread-row="github:acme/web#3"]') as HTMLElement;
  expect(within(web).queryByRole("button", { name: /unsubscribe/i })).toBeNull();
  vi.useRealTimers();
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
  expect(tabLabels()).toEqual(["Triggers 7", "Threads 5", "Decisions 5"]);
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

test("?q=schedule:<slug> opens the Triggers with the search filled and filtered", () => {
  load({
    triggers: [
      { id: "schedule:night:cron:1", at: at(9000), source: "schedule", actor: "", kind: "schedule", thread: "schedule:night", text: "Tidy", outcome: "" },
      { id: "schedule:weekly:cron:1", at: at(8000), source: "schedule", actor: "", kind: "schedule", thread: "schedule:weekly", text: "Sweep", outcome: "" },
      ...triggers,
    ],
    decisions,
    threads,
  });
  renderRouting("/routing?q=schedule:night");
  expect((screen.getByRole("searchbox", { name: "Search Triggers" }) as HTMLInputElement).value).toBe("schedule:night");
  expect(tabLabels()[0]).toBe("Triggers 1");
  expect(triggerRow("schedule:night:cron:1")).toBeTruthy();
});

test("a record the URL names is the selected row", () => {
  renderRouting("/routing?tab=triggers&trigger=tr-attached");
  expect(triggerRow("tr-attached").getAttribute("aria-current")).toBe("true");
  expect(triggerRow("tr-new").getAttribute("aria-current")).toBeNull();
});

test("empty states on a server without GitHub", () => {
  // An empty Snapshot (not no Snapshot at all, which waits for the server).
  useAthena.getState().reset();
  load({});
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

test("a real attached Trigger reads only as attached, and is judged by its routed Decision", () => {
  renderRouting();
  const row = triggerRow("tr-attached");
  expect(within(row).getByText("Attached to", { exact: false })).toBeTruthy();
  expect(row.textContent).not.toContain("Reached");
  // The server's shape (Engine.Attach): reached and attached together, the Eval that held it ended with an ignore.
  const t = { id: "tr-attached", thread: T, outcome: "reached:T-2 attached:T-2" } as never;
  const empty = { threads: new Map(), judged: indexDecisions([]) };
  expect(canReevaluate(t, empty)).toBe(true);
  expect(canReevaluate(t, { threads: new Map(), judged: indexDecisions([...useAthena.getState().decisions.values()]) })).toBe(false);
});

test("trigger text wraps in the column and its full text is its title", () => {
  const long = "Could you retry with a longer timeout? ".repeat(3).trim();
  load({ triggers: [{ id: "tr-long", at: at(1), source: "github", thread: T, text: long, outcome: "" }] });
  renderRouting();
  const p = triggerRow("tr-long").querySelector("p") as HTMLElement;
  expect(p.textContent).toBe(long);
  expect(p.getAttribute("title")).toBe(long);
  expect(p.classList.contains("truncate")).toBe(false);
  expect(p.classList.contains("break-words")).toBe(true);
});

test("the Thread search finds a pasted GitHub URL and #12, never #123 or another owner's repository", () => {
  const keys = (q: string) => selectThreads(useAthena.getState().threads, { q }).map((t) => t.key);
  expect(keys("https://github.com/acme/api/pull/12")).toEqual([T]);
  expect(keys("https://github.com/acme/api/issues/12")).toEqual([T]);
  expect(keys("#12").sort()).toEqual(["github:acme/api#12", "github:acme/held#1"].filter((k) => k.endsWith("#12")).concat("github:bigacme/api#12").sort());
  expect(keys("#12")).not.toContain("github:acme/api#123");
  expect(keys("acme/api#12")).toEqual([T]);
  expect(keys("api#12").sort()).toEqual([T, "github:bigacme/api#12"].sort());
  // Plain text still matches anywhere in the key, and a Ticket id still finds its Threads.
  expect(keys("acme/api")).toContain("github:acme/api#123");
  expect(keys("T-3")).toEqual(["github:acme/held#1"]);
});

test("the Trigger search matches its Thread the same way", () => {
  const ids = (q: string) => selectTriggers(useAthena.getState().triggers, { q }).map((t) => t.id);
  expect(ids("https://github.com/acme/api/pull/12")).toEqual(["tr-reached", "tr-attached", "tr-unavailable", "tr-new", "tr-requeued"]);
  expect(ids("#12")).toEqual(ids("https://github.com/acme/api/pull/12"));
  load({
    triggers: [
      { id: "a", at: at(2), thread: "github:acme/api#12", text: "x1", outcome: "" },
      { id: "b", at: at(1), thread: "github:acme/api#123", text: "x2", outcome: "" },
      { id: "c", at: at(0), thread: "github:bigacme/api#12", text: "x3", outcome: "" },
    ],
  });
  expect(ids("https://github.com/acme/api/pull/12")).toEqual(["a"]);
  expect(ids("#12")).toEqual(["a", "c"]);
});

test("evaluate again says it was queued and goes away", async () => {
  const user = userEvent.setup();
  renderRouting();
  const row = triggerRow("tr-filtered");
  await user.click(within(row).getByRole("button", { name: "Evaluate again" }));
  // The row says so, and so does a toast.
  await waitFor(() => expect(screen.getAllByText("Queued for a fresh Eval")).toHaveLength(2));
  expect(within(row).queryByRole("button", { name: "Evaluate again" })).toBeNull();
  expect(within(row).getByText("Queued for a fresh Eval")).toBeTruthy();
});

test("after Unsubscribe succeeds, focus moves to the Thread's key", async () => {
  // Only the clock Confirm reads is faked: Testing Library waits on real timers.
  vi.useFakeTimers({ toFake: ["performance"] });
  try {
    const user = userEvent.setup({ delay: null });
    renderRouting("/routing?tab=threads");
    const row = document.querySelector<HTMLElement>(`[data-thread-row="${T}"]`) as HTMLElement;
    await user.click(within(row).getByRole("button", { name: "Unsubscribe" }));
    await act(async () => void vi.advanceTimersByTime(350));
    await user.click(within(row).getByRole("button", { name: "Unsubscribe these 2 Tickets" }));
    expect(unsubscribe).toHaveBeenCalledWith(T);
    await act(async () => {});
    expect(document.activeElement).toBe(row.querySelector("[data-thread-key]"));
  } finally {
    vi.useRealTimers();
  }
});

test("owner messages read as sent, never as unrouted, and are never evaluated again", () => {
  load({
    tickets: [{ id: "T-1", title: "One" }],
    triggers: [
      { id: "o-new", at: at(3), source: "owner", actor: "owner", kind: "", text: "say hello", outcome: "reached:T-1" },
      { id: "o-reply", at: at(2), source: "owner", actor: "owner", kind: "", text: "I can do that", outcome: "reached:T-1" },
      // Recorded before the server noted where owner messages went.
      { id: "o-old", at: at(1), source: "owner", actor: "owner", kind: "", text: "an old one", outcome: "" },
      { id: "gh-flight", at: at(0), source: "github", actor: "ana", kind: "comment", thread: T, text: "still in flight", outcome: "" },
    ],
  });
  renderRouting();
  const sent = within(triggerRow("o-new"));
  expect(sent.getByText("Sent to", { exact: false })).toBeTruthy();
  expect(sent.getByRole("link", { name: "T-1" }).getAttribute("href")).toBe("/tickets/T-1");
  expect(triggerRow("o-new").textContent).not.toContain("Reached");
  expect(within(triggerRow("o-old")).getByText("Sent by you")).toBeTruthy();
  expect(within(triggerRow("o-old")).queryByText("No outcome yet")).toBeNull();
  expect(within(triggerRow("gh-flight")).getByText("No outcome yet")).toBeTruthy();
  for (const id of ["o-new", "o-reply", "o-old"]) {
    expect(within(triggerRow(id)).queryByRole("button", { name: "Evaluate again" })).toBeNull();
  }
  // Even one the server would have routed: an owner message has no Thread to evaluate on.
  const none = { threads: new Map(), judged: indexDecisions([]) };
  expect(canReevaluate({ id: "o", source: "owner", thread: "", outcome: "reached:T-1" } as never, none)).toBe(false);
  expect(canReevaluate({ id: "o", source: "owner", thread: T, outcome: "reached:T-1" } as never, none)).toBe(false);
});

test("each tab says what it shows, and the empty states say what will appear", () => {
  const texts: Array<[string, RegExp]> = [
    ["/routing", /Every message that reached Athena, from you or from GitHub/],
    ["/routing?tab=threads", /The GitHub issues and pull requests Athena follows/],
    ["/routing?tab=decisions", /wasn't clearly addressed to Athena/],
  ];
  for (const [path, text] of texts) {
    const { unmount } = renderRouting(path);
    expect(screen.getByText(text)).toBeTruthy();
    unmount();
  }
  useAthena.getState().reset();
  load({});
  const empties: Array<[string, RegExp]> = [
    ["/routing", /Nothing has arrived yet\. Messages you send and GitHub mentions of the App show up here\./],
    ["/routing?tab=threads", /No GitHub issue or pull request is followed yet/],
    ["/routing?tab=decisions", /Nothing has been decided yet/],
  ];
  for (const [path, text] of empties) {
    const { unmount } = renderRouting(path);
    expect(screen.getByText(text)).toBeTruthy();
    unmount();
  }
});
