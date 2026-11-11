import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { beforeEach, expect, test } from "vitest";
import { useAthena } from "@/lib/store/state";
import { load } from "./fixtures";
import { Relatives } from "./Relatives";

beforeEach(() => {
  useAthena.getState().reset();
  load({
    tickets: [
      { id: "T-1", title: "Split the parser", stage: "working", children: ["T-2", "T-3", "T-4"] },
      { id: "T-2", title: "Lexer", stage: "ended", outcome: "completed", lead: "T-1", splitKey: "lexer" },
      { id: "T-3", title: "Grammar", stage: "working", lead: "T-1", splitKey: "grammar" },
      // Waits for the lexer's sibling and for an id no summary names: the id stands in for its key.
      { id: "T-4", title: "Docs", stage: "working", action: "waiting", lead: "T-1", splitKey: "docs", deps: ["T-3", "T-9"], waiting: ["T-3", "T-9"] },
      { id: "T-5", title: "Alone", stage: "working" },
    ],
  });
});

const show = (id: string) => {
  const t = useAthena.getState().tickets.get(id)!;
  return render(
    <MemoryRouter>
      <Relatives ticket={t} />
    </MemoryRouter>,
  );
};

test("a lead lists its child tickets, each with its stage and the deps it still waits for, by key", () => {
  show("T-1");
  const list = screen.getByRole("list", { name: "Child tickets" });
  const rows = within(list).getAllByRole("listitem");
  expect(rows.map((r) => within(r).getByRole("link").textContent)).toEqual(["T-2", "T-3", "T-4"]);
  expect(rows[0].textContent).toContain("Lexer");
  expect(rows[0].textContent).toContain("Completed");
  expect(rows[2].textContent).toContain("after grammar, T-9");
  expect(rows[0].textContent).not.toContain("after");
  expect(screen.queryByText("Lead")).toBeNull();
});

test("a child shows its lead, linked, with the lead's stage", () => {
  show("T-4");
  const link = screen.getByRole("link", { name: /T-1/ });
  expect(link.getAttribute("href")).toBe("/tickets/T-1");
  expect(screen.getByText("Lead")).toBeTruthy();
  expect(screen.getByText("Working")).toBeTruthy();
  expect(screen.queryByRole("list", { name: "Child tickets" })).toBeNull();
});

test("a Ticket with neither lead nor children renders nothing", () => {
  const { container } = show("T-5");
  expect(container.innerHTML).toBe("");
});
