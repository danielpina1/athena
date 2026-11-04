import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, expect, test } from "vitest";
import { Where } from "@/app/testkit";
import { useAthena } from "@/lib/store/state";
import { at, hasClasses, load } from "./fixtures";
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
