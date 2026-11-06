import { create } from "@bufbuild/protobuf";
import { act, render, renderHook, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useNavigate } from "react-router";
import { afterEach, beforeEach, expect, test } from "vitest";
import { Where } from "@/app/testkit";
import { DeltaSchema } from "@/gen/athena/v1/state_pb";
import { useAthena } from "@/lib/store/state";
import { at, hasClasses, load } from "./fixtures";
import { useFilterParams } from "./Filters";
import { TicketList } from "./TicketList";

beforeEach(() => {
  load({
    repos: [
      { name: "acme/api", kind: "github" },
      { name: "acme/web", kind: "github" },
    ],
    tickets: [
      { id: "T-1", title: "Ship the API", stage: "working", repo: "acme/api", model: "opus", updatedAt: at(5000) },
      { id: "T-2", title: "Fix the web footer", stage: "idle", repo: "acme/web", model: "sonnet", updatedAt: at(4000) },
      { id: "T-3", title: "Pick a database", stage: "person", action: "answer", dir: "/work/shop", model: "sonnet", updatedAt: at(3000) },
      { id: "T-4", title: "Old migration", stage: "ended", outcome: "merged", repo: "acme/api", model: "opus", updatedAt: at(2000) },
    ],
  });
});
afterEach(() => useAthena.getState().reset());

function renderList(path = "/tickets") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <TicketList />
      <Where />
    </MemoryRouter>,
  );
}

const rows = () => screen.getAllByRole("link").map((a) => a.getAttribute("href")?.split("?")[0]);

test("filters live in the URL and select the right rows", async () => {
  const user = userEvent.setup();
  renderList();
  expect(rows()).toEqual(["/tickets/T-1", "/tickets/T-2", "/tickets/T-3", "/tickets/T-4"]);

  await user.click(screen.getByRole("radio", { name: "Working" }));
  expect((screen.getByTestId("where")).textContent).toContain("/tickets?stage=working");
  expect(rows()).toEqual(["/tickets/T-1"]);

  await user.click(screen.getByRole("radio", { name: "Working" }));
  expect((screen.getByTestId("where")).textContent).toMatch(/^\/tickets$/);

  await user.click(screen.getByRole("radio", { name: "Needs you" }));
  expect(rows()).toEqual(["/tickets/T-3"]);
  await user.click(screen.getByRole("radio", { name: "Open" }));
  expect(rows()).toEqual(["/tickets/T-1", "/tickets/T-2", "/tickets/T-3"]);

  await user.selectOptions(screen.getByLabelText("Repository"), "acme/api");
  expect((screen.getByTestId("where")).textContent).toContain("repo=acme%2Fapi");
  expect(rows()).toEqual(["/tickets/T-1"]);

  await user.selectOptions(screen.getByLabelText("Model"), "sonnet");
  expect(screen.getByText("No Tickets match these filters.")).toBeTruthy();
  await user.click(screen.getByRole("button", { name: "Clear filters" }));
  expect((screen.getByTestId("where")).textContent).toMatch(/^\/tickets$/);

  await user.type(screen.getByRole("searchbox", { name: "Search Tickets" }), "footer");
  expect((screen.getByTestId("where")).textContent).toContain("q=footer");
  expect(rows()).toEqual(["/tickets/T-2"]);
});

test("filters in the URL are the starting point", () => {
  renderList("/tickets?stage=ended&repo=acme%2Fapi&q=old");
  expect(rows()).toEqual(["/tickets/T-4"]);
  expect((screen.getByRole("radio", { name: "Ended" })).getAttribute("aria-checked")).toBe("true");
  expect((screen.getByLabelText("Repository") as HTMLInputElement).value).toBe("acme/api");
  expect((screen.getByRole("searchbox", { name: "Search Tickets" }) as HTMLInputElement).value).toBe("old");
  // A row keeps the filters, so the phone's back link returns to the same list.
  expect((screen.getByRole("link")).getAttribute("href")).toBe("/tickets/T-4?stage=ended&repo=acme%2Fapi&q=old");
});

test("rows say what they are", () => {
  renderList();
  const row = screen.getByRole("link", { name: /Pick a database/ });
  expect(within(row).getByText("Needs your answer")).toBeTruthy();
  expect(within(row).getByText("/work/shop")).toBeTruthy();
  expect(within(row).getByText("sonnet")).toBeTruthy();
  expect(within(row).getByTestId("needs-you-dot")).toBeTruthy();
  expect(within(screen.getByRole("link", { name: /Ship the API/ })).queryByTestId("needs-you-dot")).toBeNull();
  expect(screen.getByText("4 Tickets")).toBeTruthy();
});

test("an empty list invites the first Ticket", () => {
  useAthena.getState().reset();
  load({});
  renderList();
  expect(screen.getByText("No Tickets yet.")).toBeTruthy();
  expect((screen.getByRole("link", { name: "New task" })).getAttribute("href")).toBe("/new");
});

test("shows 200 rows, then Show more", async () => {
  const user = userEvent.setup();
  load({
    tickets: Array.from({ length: 230 }, (_, i) => ({
      id: `T-${i}`,
      title: `Ticket ${i}`,
      stage: "idle",
      updatedAt: at(i),
    })),
  });
  renderList();
  expect(screen.getAllByRole("link")).toHaveLength(200);
  await user.click(screen.getByRole("button", { name: "Show more (30)" }));
  expect(screen.getAllByRole("link")).toHaveLength(230);
});

test("a long title stays on one line", () => {
  load({ tickets: [{ id: "T-9", title: "x".repeat(2000), stage: "working", updatedAt: at(0) }] });
  renderList();
  const title = screen.getByText("x".repeat(2000));
  expect(hasClasses(title, "truncate")).toBe(true);
  expect((title).getAttribute("dir")).toBe("auto");
});

test("a title loses escapes and bidirectional controls", () => {
  load({ tickets: [{ id: "T-9", title: "\u001b[31mfix\u001b[0m the \u202elogin\u202c page", stage: "working", updatedAt: at(0) }] });
  renderList();
  expect(screen.getByText("fix the login page")).toBeTruthy();
});

test("two filter changes before a re-render both land", () => {
  const { result } = renderHook(() => useFilterParams(), { wrapper: ({ children }) => <MemoryRouter initialEntries={["/tickets?tab=changes"]}>{children}</MemoryRouter> });
  act(() => {
    result.current.set("stage", "working");
    result.current.set("model", "opus");
    result.current.set("q", "api");
  });
  expect(Object.fromEntries(result.current.sp)).toEqual({ tab: "changes", stage: "working", model: "opus", q: "api" });
});

test("typing in the middle of the search keeps the text and the caret", async () => {
  const user = userEvent.setup();
  renderList();
  const box = screen.getByRole("searchbox", { name: "Search Tickets" }) as HTMLInputElement;
  await user.type(box, "abcd");
  box.setSelectionRange(2, 2);
  await user.keyboard("X");
  expect(box.value).toBe("abXcd");
  expect(box.selectionStart).toBe(3);
  expect(screen.getByTestId("where").textContent).toContain("q=abXcd");
});

test("the search box follows a change of the URL from outside", async () => {
  const user = userEvent.setup();
  function Jump() {
    const navigate = useNavigate();
    return <button onClick={() => navigate("/tickets?q=zebra")}>jump</button>;
  }
  render(
    <MemoryRouter initialEntries={["/tickets?q=old"]}>
      <TicketList />
      <Jump />
    </MemoryRouter>,
  );
  const box = () => screen.getByRole("searchbox", { name: "Search Tickets" }) as HTMLInputElement;
  expect(box().value).toBe("old");
  await user.click(screen.getByRole("button", { name: "jump" }));
  expect(box().value).toBe("zebra");
});

test("500 Tickets: a Delta applies and the Tickets list renders it under budget", () => {
  const tickets = Array.from({ length: 500 }, (_, i) => ({
    id: `T-${i}`,
    title: `ticket ${i}`,
    stage: "working",
    repo: "acme/api",
    model: "opus",
    updatedAt: at(i * 1000),
  }));
  load({ tickets });
  renderList();
  expect(screen.getAllByRole("link", { name: /ticket \d+/ })).toHaveLength(200);
  // Each Delta moves one Ticket to the top (the list is newest first) and changes its stage, the worst case for the
  // list: its order and a row change.
  const deltas = (from: number) =>
    tickets.map((t, i) =>
      create(DeltaSchema, {
        seq: BigInt(from + i),
        change: { case: "ticket", value: { ...t, stage: i % 2 ? "idle" : "person", action: "answer", updatedAt: at(1e9 + from + i) } },
      }),
    );
  const apply = (d: ReturnType<typeof deltas>[number]) => act(() => void useAthena.getState().applyDelta(d));
  // Warm up the JIT and React on 50 Deltas, then time 150 (the 499th Ticket's is the last).
  const all = deltas(2).slice(300);
  for (const d of all.slice(0, 50)) apply(d);
  const timed = all.slice(50);
  const start = performance.now();
  for (const d of timed) apply(d);
  const mean = (performance.now() - start) / timed.length;
  // The budget is 16 ms to apply and render a Delta (a frame); a shared CI runner gets four times that.
  const budget = 16 * (process.env.CI ? 4 : 1);
  expect(mean).toBeLessThan(budget);
  expect(rows()[0]).toBe("/tickets/T-499");
});

test("runtime filter matches list length", async () => {
  const user = userEvent.setup();
  load({
    tickets: [
      { id: "T-1", title: "Ship the API", stage: "working", model: "opus", runtime: "claude", runtimeChoice: "auto", updatedAt: at(5000) },
      { id: "T-2", title: "Port the parser", stage: "working", model: "gpt-5.5", runtime: "codex", runtimeChoice: "codex", updatedAt: at(4000) },
      { id: "T-3", title: "Review the docs", stage: "idle", runtime: "codex", runtimeChoice: "auto", updatedAt: at(3000) },
    ],
  });
  renderList();
  // Each row says its runtime beside the model, and whether auto chose it.
  expect(within(screen.getByRole("link", { name: /Ship the API/ })).getByText("Claude Code · auto")).toBeTruthy();
  expect(within(screen.getByRole("link", { name: /Port the parser/ })).getByText("Codex")).toBeTruthy();

  const select = screen.getByLabelText("Runtime") as HTMLSelectElement;
  expect([...select.options].map((o) => o.text)).toEqual(["All runtimes", "Claude Code", "Codex"]);
  await user.selectOptions(select, "codex");
  expect(screen.getByTestId("where").textContent).toContain("runtime=codex");
  expect(rows()).toEqual(["/tickets/T-2", "/tickets/T-3"]);
  expect(screen.getByText(`${rows().length} Tickets`)).toBeTruthy();
  await user.selectOptions(select, "claude");
  expect(rows()).toEqual(["/tickets/T-1"]);
  expect(screen.getByText("1 Ticket")).toBeTruthy();
  // A row keeps the filter.
  expect(screen.getByRole("link").getAttribute("href")).toBe("/tickets/T-1?runtime=claude");
  await user.selectOptions(select, "");
  expect(rows()).toHaveLength(3);
});

test("a runtime in the URL is the starting point; an unknown one is no filter", () => {
  load({ tickets: [{ id: "T-2", title: "Port", stage: "working", runtime: "codex", updatedAt: at(1) }, { id: "T-5", title: "Other", stage: "working", runtime: "claude", updatedAt: at(0) }] });
  const { unmount } = renderList("/tickets?runtime=codex");
  expect(rows()).toEqual(["/tickets/T-2"]);
  unmount();
  renderList("/tickets?runtime=gemini");
  expect(rows()).toEqual(["/tickets/T-2", "/tickets/T-5"]);
});
