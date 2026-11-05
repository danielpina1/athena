import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, test, vi } from "vitest";
import { TimelineEntrySchema } from "@/gen/athena/v1/state_pb";
import { Timeline } from "./Timeline";

const NOW = Date.now();
const entry = (kind: string, subkind: string, text: string, ago: number, more: { subject?: string; refId?: string } = {}) =>
  create(TimelineEntrySchema, { kind, subkind, text, at: timestampFromMs(NOW - ago * 60_000), ...more });

const rows = () => screen.getAllByTestId("timeline-row");

test("shows entries newest first, each in plain words with its time", () => {
  render(
    <Timeline
      entries={[entry("report", "progress", "Second", 2), entry("trigger", "owner", "First", 3), entry("reply", "owner", "Third", 1)]}
      trimmed={false}
    />,
  );
  expect(rows().map((r) => r.textContent)).toEqual([
    expect.stringContaining("You replied"),
    expect.stringContaining("Progress update"),
    expect.stringContaining("Started from your request"),
  ]);
  expect(rows()[2].textContent).toContain("First");
  expect(within(rows()[0]).getByText(/^\d\d:\d\d$/)).toBeTruthy();
  expect(screen.getByText("Today")).toBeTruthy();
  expect(screen.queryByText("Older entries are hidden")).toBeNull();
});

test("the approval waiting on the owner has the pulsing accent dot; answered ones do not", () => {
  render(
    <Timeline
      entries={[entry("approval", "pending", "Pushes a branch", 0, { subject: "Bash: git push" }), entry("approval", "denied", "", 5, { subject: "Bash: rm" })]}
      trimmed={false}
    />,
  );
  const [pending, denied] = rows();
  expect(pending.textContent).toContain("Needs your approval");
  const dot = pending.querySelector("[data-dot]")!;
  expect(dot.getAttribute("data-tone")).toBe("attention");
  expect(dot.classList.contains("athena-ring")).toBe(true);
  expect(denied.querySelector("[data-dot]")!.getAttribute("data-tone")).toBe("danger");
  expect(denied.querySelector(".athena-ring")).toBeNull();
});

test("approvals in a row are one row that opens to what was approved", async () => {
  const user = userEvent.setup();
  render(
    <Timeline
      entries={["npm test", "go vet ./...", "make check", "git push"].map((c, i) =>
        entry("approval", "approved", "", 10 - i, { subject: `Bash: ${c}`, refId: `A-${i}` }),
      )}
      trimmed={false}
    />,
  );
  expect(rows()).toHaveLength(1);
  const group = rows()[0];
  expect(group.textContent).toContain("You approved 4 commands");
  await user.click(within(group).getByText("You approved 4 commands"));
  const list = within(group).getByRole("list", { name: "What you approved" });
  expect(within(list).getAllByRole("listitem").map((li) => li.textContent)).toEqual([
    "Bash: npm test",
    "Bash: go vet ./...",
    "Bash: make check",
    "Bash: git push",
  ]);
});

test("a Report is one line with View report", async () => {
  const user = userEvent.setup();
  const onViewReport = vi.fn();
  render(
    <Timeline
      entries={[entry("report", "ready", "## Review done", 1, { subject: "PR #12", refId: "as2" })]}
      trimmed={false}
      onViewReport={onViewReport}
    />,
  );
  const row = rows()[0];
  expect(row.textContent).toContain("Ready for review");
  expect(row.textContent).toContain("Review done");
  expect(row.textContent).toContain("PR #12");
  await user.click(within(row).getByRole("button", { name: "View report" }));
  expect(onViewReport).toHaveBeenCalledWith("as2");
});

test("entries that arrive after the first render wash in; the first ones do not", () => {
  const first = [entry("trigger", "owner", "go", 3)];
  const view = render(<Timeline entries={first} trimmed={false} />);
  expect(rows()[0].classList.contains("motion-wash")).toBe(false);
  view.rerender(<Timeline entries={[...first, entry("report", "progress", "half way", 0)]} trimmed={false} />);
  const [fresh, old] = rows();
  expect(fresh.textContent).toContain("Progress update");
  expect(fresh.classList.contains("motion-wash")).toBe(true);
  expect(old.classList.contains("motion-wash")).toBe(false);
});

test("hostile text is stripped of escapes and clamped", () => {
  const view = render(<Timeline entries={[entry("reply", "owner", "\u001b[31mred\u001b[0m " + "z".repeat(3000), 1)]} trimmed={false} />);
  expect(view.container.textContent).not.toContain("\u001b");
  expect(view.container.querySelector("p.line-clamp-2")).toBeTruthy();
});

test("trimmed timeline notice", () => {
  render(<Timeline entries={[entry("report", "progress", "Only the newest", 1)]} trimmed />);
  expect(screen.getByText("Older entries are hidden")).toBeTruthy();
});

test("an empty timeline says so", () => {
  render(<Timeline entries={[]} trimmed={false} />);
  expect(screen.getByText("Nothing has happened on this Ticket yet.")).toBeTruthy();
});
