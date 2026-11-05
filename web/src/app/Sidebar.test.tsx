import { create } from "@bufbuild/protobuf";
import { render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { SnapshotSchema } from "@/gen/athena/v1/state_pb";
import { useAthena } from "@/lib/store/state";
import { NeedsCount } from "./Sidebar";

afterEach(() => useAthena.getState().reset());

const badge = () => screen.getByText(/^\d+$/);
const pops = () => badge().classList.contains("motion-pop");

test("the Needs you badge pops when its count rises, not on first render or a fall", () => {
  useAthena.getState().applySnapshot(create(SnapshotSchema, { cursor: 1n }), "e1");
  const view = render(<NeedsCount n={2} />);
  expect(pops()).toBe(false);
  view.rerender(<NeedsCount n={3} />);
  expect(pops()).toBe(true);
  const first = badge();
  view.rerender(<NeedsCount n={1} />);
  // A fall keeps the badge as it is: the pop does not play again.
  expect(badge()).toBe(first);
  view.rerender(<NeedsCount n={2} />);
  expect(pops()).toBe(true);
  expect(badge()).not.toBe(first);
});

test("the count the first Snapshot brings does not pop", () => {
  const view = render(<NeedsCount n={0} />);
  useAthena.getState().applySnapshot(create(SnapshotSchema, { cursor: 1n }), "e1");
  view.rerender(<NeedsCount n={4} />);
  expect(pops()).toBe(false);
});
