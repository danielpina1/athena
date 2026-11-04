import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { beforeEach, expect, test } from "vitest";
import { SnapshotSchema } from "@/gen/athena/v1/state_pb";
import { threadQuery } from "@/app/Palette";
import { Shell } from "@/app/Shell";
import { fakeLink, stubWidth, Where } from "@/app/testkit";
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

test("thread queries", () => {
  expect(threadQuery("https://github.com/Owner/Name/pull/7")).toBe("github:owner/name#7");
  expect(threadQuery("github.com/owner/name/issues/8")).toBe("github:owner/name#8");
  expect(threadQuery("#12")).toBe("#12");
});
