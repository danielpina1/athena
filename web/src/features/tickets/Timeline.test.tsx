import { create } from "@bufbuild/protobuf";
import { render, screen } from "@testing-library/react";
import { expect, test } from "vitest";
import { TimelineEntrySchema } from "@/gen/athena/v1/state_pb";
import { at } from "./fixtures";
import { Timeline } from "./Timeline";

const entry = (kind: string, text: string, ms: number) => create(TimelineEntrySchema, { kind, text, at: at(ms) });

test("shows entries newest last, each with its kind", () => {
  render(<Timeline entries={[entry("report", "Second", 2000), entry("trigger", "First", 1000), entry("reply", "Third", 3000)]} trimmed={false} />);
  const items = screen.getAllByRole("listitem");
  expect(items.map((li) => li.textContent)).toEqual([
    expect.stringContaining("First"),
    expect.stringContaining("Second"),
    expect.stringContaining("Third"),
  ]);
  expect((items[0]).textContent).toContain("Trigger");
  expect((items[1]).textContent).toContain("Report");
  expect(screen.queryByText("Older entries are hidden")).toBeNull();
});

test("trimmed timeline notice", () => {
  render(<Timeline entries={[entry("report", "Only the newest", 1000)]} trimmed />);
  expect(screen.getByText("Older entries are hidden")).toBeTruthy();
});

test("an empty timeline says so", () => {
  render(<Timeline entries={[]} trimmed={false} />);
  expect(screen.getByText("Nothing has happened on this Ticket yet.")).toBeTruthy();
});
