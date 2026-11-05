import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { render } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { SnapshotSchema, type TicketSummarySchema } from "@/gen/athena/v1/state_pb";
import { useAthena } from "@/lib/store/state";
import { AthenaMark } from "./AthenaMark";

afterEach(() => useAthena.getState().reset());

const load = (tickets: MessageInitShape<typeof TicketSummarySchema>[]) =>
  useAthena.getState().applySnapshot(create(SnapshotSchema, { cursor: 1n, tickets }), "e1");
const state = () => render(<AthenaMark />).container.querySelector("[data-mark]")!.getAttribute("data-state");

test("the mark is idle, working or needs you", () => {
  expect(state()).toBe("idle");
  load([{ id: "T-1", stage: "idle" }]);
  expect(state()).toBe("idle");
  load([{ id: "T-1", stage: "working" }]);
  expect(state()).toBe("working");
  load([{ id: "T-1", stage: "working" }, { id: "T-2", stage: "person", action: "review" }]);
  expect(state()).toBe("needs-you");
});
