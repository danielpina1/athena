import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { create } from "@bufbuild/protobuf";
import { CommandResultSchema } from "@/gen/athena/v1/commands_pb";
import * as actions from "@/lib/commands/actions";
import { useAthena } from "@/lib/store/state";
import { detail, load } from "@/features/tickets/fixtures";
import { PlanCard } from "./PlanCard";

vi.mock("@/lib/commands/actions", async (orig) => ({
  ...(await orig<typeof import("@/lib/commands/actions")>()),
  watch: vi.fn(),
  unwatch: vi.fn(),
}));

beforeEach(() => {
  useAthena.getState().reset();
  vi.mocked(actions.watch).mockResolvedValue(create(CommandResultSchema, { ok: true }));
  vi.mocked(actions.unwatch).mockResolvedValue(create(CommandResultSchema, { ok: true }));
});
afterEach(() => vi.clearAllMocks());

const show = (id: string) =>
  render(
    <MemoryRouter>
      <PlanCard ticket={useAthena.getState().tickets.get(id)!} />
    </MemoryRouter>,
  );

test("a draft says how many sub-tasks its split has, and offers no review here", () => {
  load({ tickets: [{ id: "T-1", title: "Port the parser", stage: "person", action: "plan", planState: "draft" }] });
  detail({
    summary: { id: "T-1", title: "Port the parser", stage: "person", action: "plan", planState: "draft" },
    breakdown: { state: "draft", revision: 1, tasks: [{ key: "a", title: "A" }, { key: "b", title: "B" }, { key: "c", title: "C" }] },
  });
  show("T-1");
  expect(screen.getByText("A split of Port the parser waits for you (3 sub-tasks)")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Review split" })).toBeNull();
  expect(actions.watch).toHaveBeenCalledWith("T-1");
});

test("a failed plan says why", () => {
  load({ tickets: [{ id: "T-2", title: "Port the parser", stage: "person", action: "plan", planState: "failed" }] });
  detail({
    summary: { id: "T-2", title: "Port the parser", stage: "person", action: "plan", planState: "failed" },
    breakdown: { state: "failed", error: "the planner timed out" },
  });
  show("T-2");
  expect(screen.getByText("Planning Port the parser failed: the planner timed out")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Review split" })).toBeNull();
});
