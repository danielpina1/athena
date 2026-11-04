import { render, screen } from "@testing-library/react";
import { expect, test } from "vitest";
import { StageBadge } from "@/ui/athena/StageBadge";

// The Global Constraints stage label table, verbatim.
const LABELS: [stage: string, action: string, outcome: string, label: string][] = [
  ["person", "answer", "", "Needs your answer"],
  ["person", "approval", "", "Needs approval"],
  ["person", "review", "", "Ready for review"],
  ["person", "delivery", "", "Delivery failed"],
  ["person", "blocked", "", "Blocked"],
  ["person", "failed", "", "Failed"],
  ["working", "queued", "", "Queued"],
  ["working", "", "", "Working"],
  ["idle", "", "", "Idle"],
  ["ended", "", "merged", "Merged"],
  ["ended", "", "stopped", "Stopped"],
  ["ended", "", "dropped", "Dropped"],
  ["ended", "", "completed", "Completed"],
  ["ended", "", "failed", "Failed"],
];

test("StageBadge labels every stage and action exactly", () => {
  for (const [stage, action, outcome, label] of LABELS) {
    const { container, unmount } = render(<StageBadge stage={stage} action={action} outcome={outcome} />);
    expect(container.textContent, `${stage}/${action}/${outcome}`).toBe(label);
    unmount();
  }
});

test("StageBadge uses accent only for person stages", () => {
  for (const [stage, action, outcome] of LABELS) {
    const { container, unmount } = render(<StageBadge stage={stage} action={action} outcome={outcome} />);
    const usesAccent = /\b(bg|text|border)-accent\b/.test(container.innerHTML);
    expect(usesAccent, `${stage}/${action}/${outcome}`).toBe(stage === "person");
    unmount();
  }
});

test("working shows a ring indicator and nothing else does", () => {
  const { unmount } = render(<StageBadge stage="working" action="" outcome="" />);
  expect(screen.getByTestId("stage-ring")).toBeTruthy();
  unmount();
  render(<StageBadge stage="working" action="queued" outcome="" />);
  expect(screen.queryByTestId("stage-ring")).toBeNull();
});

test("an unknown stage still says something honest", () => {
  const { container } = render(<StageBadge stage="ended" action="" outcome="" />);
  expect(container.textContent).toBe("Ended");
});
