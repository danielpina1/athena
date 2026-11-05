import { create } from "@bufbuild/protobuf";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, test, vi } from "vitest";
import { ReportViewSchema } from "@/gen/athena/v1/state_pb";
import { at } from "./fixtures";
import ReportCard from "./ReportCard";

const report = (init: Parameters<typeof create<typeof ReportViewSchema>>[1]) => create(ReportViewSchema, { at: at(0), ...init });
const long = "# Review\n\n" + Array.from({ length: 30 }, (_, i) => `- finding ${i + 1}`).join("\n");

test("the latest Report shows its state, pull request and summary as Markdown", () => {
  render(<ReportCard reports={[report({ assignmentId: "a1", state: "ready", summary: "## Done\n\n1. Login doesn't type-check `password`\n2. More", pull: 12 })]} repo="acme/api" />);
  const card = screen.getByRole("region", { name: "Report" });
  expect(within(card).getByText("Ready for review")).toBeTruthy();
  const pr = within(card).getByRole("link", { name: "PR #12" });
  expect(pr.getAttribute("href")).toBe("https://github.com/acme/api/pull/12");
  expect(within(card).getByText("password").tagName).toBe("CODE");
  expect(within(card).getAllByRole("listitem")).toHaveLength(2);
  expect(card.id).toBe("report-a1");
});

test("a long Report is clamped and expands in place", async () => {
  const user = userEvent.setup();
  render(<ReportCard reports={[report({ assignmentId: "a1", state: "progress", summary: long })]} repo="" />);
  const body = screen.getByTestId("report-body");
  expect(body.getAttribute("data-clamped")).toBe("true");
  await user.click(screen.getByRole("button", { name: "Show the full report" }));
  expect(body.getAttribute("data-clamped")).toBe("false");
  expect(screen.getByRole("button", { name: "Show less" })).toBeTruthy();
});

test("a question is highlighted with a Reply that focuses the composer", async () => {
  const user = userEvent.setup();
  const onReply = vi.fn();
  render(<ReportCard reports={[report({ assignmentId: "a1", state: "waiting", summary: "Two caches fit", question: "Redis or in-process?" })]} repo="" onReply={onReply} />);
  const q = screen.getByRole("note", { name: "Question" });
  expect(q.textContent).toContain("Redis or in-process?");
  await user.click(within(q).getByRole("button", { name: "Reply" }));
  expect(onReply).toHaveBeenCalled();
});

test("earlier Reports collapse under their count", async () => {
  const user = userEvent.setup();
  render(
    <ReportCard
      reports={[
        report({ assignmentId: "a1", state: "failed", summary: "first try failed" }),
        report({ assignmentId: "a2", state: "blocked", summary: "blocked on a key" }),
        report({ assignmentId: "a3", state: "ready", summary: "all done" }),
      ]}
      repo=""
    />,
  );
  expect(screen.getByText("all done")).toBeTruthy();
  const earlier = screen.getByText("Earlier reports (2)");
  expect(screen.getByText("first try failed").closest("details")?.open).toBe(false);
  await user.click(earlier);
  expect(screen.getByText("first try failed").closest("details")?.open).toBe(true);
  // Newest first.
  const states = [...document.querySelectorAll("[data-report] [data-state]")].map((e) => e.textContent);
  expect(states).toEqual(["Ready for review", "Blocked", "Failed"]);
});

test("no Report, no card", () => {
  const view = render(<ReportCard reports={[]} repo="" />);
  expect(view.container.textContent).toBe("");
});

test("the state pill is green for review and amber while waiting", () => {
  const { unmount } = render(<ReportCard reports={[report({ assignmentId: "a1", state: "ready", summary: "x" })]} repo="" />);
  const ready = document.querySelector("[data-state]")!;
  expect(ready.className).toContain("bg-success");
  unmount();
  render(<ReportCard reports={[report({ assignmentId: "a1", state: "waiting", summary: "x" })]} repo="" />);
  const waiting = document.querySelector("[data-state]")!;
  expect(waiting.className).toContain("text-warning");
  expect(waiting.className).toContain("border-warning");
});
