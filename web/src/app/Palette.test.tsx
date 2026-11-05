import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { beforeEach, expect, test } from "vitest";
import { SnapshotSchema } from "@/gen/athena/v1/state_pb";
import { Shell } from "@/app/Shell";
import { fakeLink, stubWidth, Where } from "@/app/testkit";
import { setOutbox } from "@/lib/commands/actions";
import type { Outbox } from "@/lib/commands/outbox";
import { useAthena } from "@/lib/store/state";

function load(init: MessageInitShape<typeof SnapshotSchema>) {
  useAthena.getState().applySnapshot(create(SnapshotSchema, { cursor: 1n, ...init }), "e1");
}

function renderAt(path: string) {
  stubWidth(1440);
  render(
    <MemoryRouter initialEntries={[path]}>
      <Shell link={fakeLink("open")} />
      <Where />
    </MemoryRouter>,
  );
}

const where = () => screen.getByTestId("where").textContent;

beforeEach(() => {
  // Opening a Ticket from the palette shows its detail, which watches the Ticket.
  setOutbox({
    submit: () => Promise.resolve({ ok: true, code: "" }),
    onWatchLost: () => () => {},
  } as unknown as Outbox);
  useAthena.getState().reset();
  load({
    tickets: [
      { id: "T-0012", title: "Fix flaky auth test", stage: "working" },
      { id: "T-0013", title: "Rate-limit webhook intake", stage: "idle" },
    ],
    triggers: [{ id: "TR-1", text: "please look at the flaky login" }],
    threads: [{ key: "github:o/r#12" }, { key: "github:o/r#13" }],
  });
});

test("palette finds a ticket by id", async () => {
  const user = userEvent.setup();
  renderAt("/");
  await user.keyboard("{Meta>}k{/Meta}");
  const input = await screen.findByRole("combobox");
  await user.type(input, "T-0012");
  expect(await screen.findByRole("option", { name: /Fix flaky auth test/ })).toBeTruthy();
  expect(screen.queryByRole("option", { name: /Rate-limit webhook intake/ })).toBeNull();
  await user.keyboard("{Enter}");
  expect(where()).toBe("/tickets/T-0012");
  expect(screen.queryByRole("dialog")).toBeNull();
});

test("palette finds Triggers by text and Threads by key or GitHub URL", async () => {
  const user = userEvent.setup();
  renderAt("/");
  await user.keyboard("{Control>}k{/Control}");
  const input = await screen.findByRole("combobox");
  await user.type(input, "flaky login");
  expect(await screen.findByRole("option", { name: /please look at the flaky login/ })).toBeTruthy();

  await user.clear(input);
  await user.type(input, "https://github.com/O/R/pull/12");
  expect(await screen.findByRole("option", { name: /github:o\/r#12/ })).toBeTruthy();
  expect(screen.queryByRole("option", { name: /github:o\/r#13/ })).toBeNull();
  await user.keyboard("{Enter}");
  expect(where()).toBe("/routing?tab=threads&thread=github%3Ao%2Fr%2312");
});

test("palette offers the selected Ticket's actions and goes to any route", async () => {
  const user = userEvent.setup();
  renderAt("/tickets/T-0012");
  await user.keyboard("{Meta>}k{/Meta}");
  for (const name of ["Reply to T-0012", "Stop T-0012", "Interrupt T-0012", "Watch session"]) {
    expect(screen.getByRole("option", { name })).toBeTruthy();
  }
  // Retry only when the Ticket can be retried.
  expect(screen.queryByRole("option", { name: "Retry T-0012" })).toBeNull();
  expect(screen.getByRole("option", { name: /theme/ })).toBeTruthy();
  await user.click(screen.getByRole("option", { name: "Routing" }));
  expect(where()).toBe("/routing");
});

test("results by title, Trigger text or GitHub URL never say nothing matches", async () => {
  const user = userEvent.setup();
  renderAt("/");
  await user.keyboard("{Meta>}k{/Meta}");
  const input = await screen.findByRole("combobox");

  await user.type(input, "flaky");
  expect(await screen.findByRole("option", { name: /Fix flaky auth test/ })).toBeTruthy();
  expect(screen.getByRole("option", { name: /please look at the flaky login/ })).toBeTruthy();
  expect(screen.queryByText("Nothing matches.")).toBeNull();

  await user.clear(input);
  await user.type(input, "https://github.com/o/r/pull/13");
  expect(await screen.findByRole("option", { name: /github:o\/r#13/ })).toBeTruthy();
  expect(screen.queryByText("Nothing matches.")).toBeNull();

  await user.clear(input);
  await user.type(input, "zzzz");
  expect(screen.getByText("Nothing matches.")).toBeTruthy();
  expect(screen.queryAllByRole("option")).toEqual([]);
});

test("palette opens a Thread in Routing", async () => {
  const user = userEvent.setup();
  renderAt("/");
  await user.keyboard("{Meta>}k{/Meta}");
  const input = await screen.findByRole("combobox");
  await user.type(input, "#12");
  await user.click(await screen.findByRole("option", { name: /github:o\/r#12/ }));
  expect(where()).toBe("/routing?tab=threads&thread=github%3Ao%2Fr%2312");
  expect(screen.getByRole("heading", { name: "Routing" })).toBeTruthy();
});

test("/ opens the palette where the page has no search box", async () => {
  const user = userEvent.setup();
  renderAt("/");
  await user.keyboard("/");
  expect(await screen.findByRole("combobox")).toBeTruthy();
});

test("/ focuses the list's search box where there is one", async () => {
  const user = userEvent.setup();
  renderAt("/routing");
  await user.keyboard("/");
  expect(document.activeElement).toBe(screen.getByRole("searchbox", { name: "Search Triggers" }));
  expect(screen.queryByRole("dialog")).toBeNull();
  // Typing a slash into the box does not reopen anything.
  await user.keyboard("a/");
  expect(screen.queryByRole("dialog")).toBeNull();
});

test("/ focuses the Tickets search box", async () => {
  const user = userEvent.setup();
  renderAt("/tickets");
  await user.keyboard("/");
  expect(document.activeElement).toBe(screen.getByRole("searchbox", { name: "Search Tickets" }));
});

test("palette opens a Trigger in Routing", async () => {
  const user = userEvent.setup();
  renderAt("/");
  await user.keyboard("{Meta>}k{/Meta}");
  await user.type(await screen.findByRole("combobox"), "flaky login");
  await user.click(await screen.findByRole("option", { name: /please look at the flaky login/ }));
  expect(where()).toBe("/routing?tab=triggers&trigger=TR-1");
  expect(document.querySelector('[data-trigger-row="TR-1"]')?.getAttribute("aria-current")).toBe("true");
});
