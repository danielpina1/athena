import { create } from "@bufbuild/protobuf";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { expect, test } from "vitest";
import { TicketSummarySchema } from "@/gen/athena/v1/state_pb";
import { Facts } from "./Facts";

const facts = (init: Parameters<typeof create<typeof TicketSummarySchema>>[1]) => {
  render(
    <MemoryRouter>
      <Facts ticket={create(TicketSummarySchema, { id: "T-1", stage: "working", ...init })} threads={[]} />
    </MemoryRouter>,
  );
  const dl = screen.getByRole("region", { name: "Facts" });
  const value = (label: string) => within(dl).queryByText(label)?.nextElementSibling?.textContent ?? null;
  return { value };
};

test("facts say the runtime, with auto's choice, and review rounds when paired", () => {
  const { value } = facts({ runtime: "codex", runtimeChoice: "auto", pairing: "review", reviewRounds: 1 });
  expect(value("Runtime")).toBe("Codex · auto");
  expect(value("Review rounds")).toBe("1");
});

test("no pairing, no review rounds; a pinned runtime is its name", () => {
  const { value } = facts({ runtime: "claude", runtimeChoice: "claude" });
  expect(value("Runtime")).toBe("Claude Code");
  expect(value("Review rounds")).toBeNull();
});
