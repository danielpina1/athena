import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { expect, test } from "vitest";
import { TimelineEntrySchema } from "@/gen/athena/v1/state_pb";
import { buildTimeline, clockTime, dayLabel, describeEntry } from "./timelineModel";

const NOW = new Date(2026, 10, 5, 15, 0).getTime();
const MIN = 60_000;

const e = (kind: string, subkind = "", subject = "", text = "", at = NOW, refId = "") =>
  create(TimelineEntrySchema, { kind, subkind, subject, text, refId, at: timestampFromMs(at) });

const titleOf = (...args: Parameters<typeof e>) => describeEntry(e(...args)).title;

test("a Trigger says where the Ticket came from", () => {
  expect(titleOf("trigger", "issue")).toBe("Started from a GitHub issue");
  expect(titleOf("trigger", "pull")).toBe("Started from a pull request");
  expect(titleOf("trigger", "issue-comment")).toBe("Started from an issue comment");
  expect(titleOf("trigger", "pull-comment")).toBe("Started from a PR comment");
  expect(titleOf("trigger", "review-comment")).toBe("Started from a review comment");
  expect(titleOf("trigger", "review")).toBe("Started from a review");
  expect(titleOf("trigger", "ci")).toBe("Started from CI");
  expect(titleOf("trigger", "owner")).toBe("Started from your request");
  expect(titleOf("trigger")).toBe("Request received");
  // An older server sends no subkind: the Trigger's id still tells a GitHub issue.
  expect(titleOf("trigger", "", "", "fix it", NOW, "github:issue:42")).toBe("Started from a GitHub issue");
  expect(describeEntry(e("trigger", "issue", "", "please fix the login")).subtitle).toBe("please fix the login");
});

test("a Decision and an Assignment read as what happened", () => {
  expect(titleOf("decision", "accepted")).toBe("Router started this Ticket");
  expect(titleOf("assignment", "queued")).toBe("Queued");
  expect(titleOf("assignment", "started", "claude-sonnet-4-5")).toBe("Started work · sonnet 4.5");
  expect(titleOf("assignment", "started")).toBe("Started work");
  expect(describeEntry(e("assignment", "started")).tone).toBe("ok");
  expect(titleOf("assignment", "ended")).toBe("Session ended");
  const restart = describeEntry(e("assignment", "interrupted", "", "node restart"));
  expect(restart).toMatchObject({ title: "Interrupted by a server restart", subtitle: "Resumes on your next message", tone: "warn" });
  expect(describeEntry(e("assignment", "failed", "", "exit 1"))).toMatchObject({ title: "Failed", subtitle: "exit 1", tone: "danger" });
  expect(describeEntry(e("assignment", "stopped", "", "Stopped from the Ticket page"))).toMatchObject({
    title: "Stopped by you",
    subtitle: "Stopped from the Ticket page",
    tone: "warn",
  });
  expect(titleOf("assignment", "retried")).toBe("Retried");
  // An older server's text.
  expect(titleOf("assignment", "", "", "running")).toBe("Started work");
  expect(titleOf("assignment", "", "", "ended: retried")).toBe("Session ended");
});

test("timeline says interrupted and resumed", () => {
  expect(describeEntry(e("assignment", "restart"))).toMatchObject({ title: "Interrupted by a server restart", subtitle: "", tone: "warn" });
  expect(describeEntry(e("assignment", "resumed"))).toMatchObject({ title: "Resumed automatically", subtitle: "", tone: "ok" });
  // The legacy end entry of an Assignment a restart ended is unchanged.
  expect(describeEntry(e("assignment", "interrupted", "", "node restart"))).toMatchObject({
    title: "Interrupted by a server restart",
    subtitle: "Resumes on your next message",
    tone: "warn",
  });
});

test("a Report is one line: its state, the summary's first line and its pull request", () => {
  const ready = describeEntry(e("report", "ready", "PR #12", "## Review done"));
  expect(ready).toMatchObject({ title: "Ready for review", subtitle: "Review done", badge: "PR #12", tone: "ok", report: true });
  expect(titleOf("report", "waiting")).toBe("Waiting on you");
  expect(titleOf("report", "progress")).toBe("Progress update");
  expect(titleOf("report", "dropped")).toBe("Finished");
  expect(describeEntry(e("report", "dropped")).tone).toBe("ok");
  expect(titleOf("report", "blocked")).toBe("Blocked");
  expect(describeEntry(e("report", "failed", "", "**Tests** fail")).subtitle).toBe("Tests fail");
  expect(describeEntry(e("report", "failed")).tone).toBe("danger");
  // An older server: "state: summary".
  expect(describeEntry(e("report", "", "", "ready: All done"))).toMatchObject({ title: "Ready for review", subtitle: "All done" });
});

test("replies and inputs say who and what", () => {
  expect(titleOf("reply", "owner", "", "continue")).toBe("You replied");
  expect(describeEntry(e("reply", "owner", "", "continue")).subtitle).toBe("continue");
  expect(titleOf("reply", "github", "alice")).toBe("@alice replied on GitHub");
  expect(titleOf("reply", "github")).toBe("Reply on GitHub");
  expect(titleOf("reply")).toBe("You replied");
  expect(titleOf("input", "receipt", "", "Validating")).toBe("Acknowledged on GitHub");
  expect(titleOf("input", "note")).toBe("Note");
  expect(titleOf("input", "resumed")).toBe("Resumed");
  expect(titleOf("input", "context")).toBe("Seen on a followed Thread");
  expect(titleOf("input", "text")).toBe("Asked for a status update");
  expect(titleOf("input", "trigger")).toBe("Automated message");
  expect(titleOf("input")).toBe("Message");
});

test("an Approval says what was approved, and a pending one is the owner's", () => {
  expect(describeEntry(e("approval", "approved", "Bash: npm test"))).toMatchObject({
    title: "You approved Bash",
    subtitle: "npm test",
    tone: "ok",
  });
  expect(describeEntry(e("approval", "denied", "Bash: rm -rf /", "too risky"))).toMatchObject({
    title: "You denied Bash",
    subtitle: "rm -rf / — too risky",
    tone: "danger",
  });
  expect(describeEntry(e("approval", "expired", "Edit"))).toMatchObject({ title: "Approval expired", subtitle: "Edit", tone: "warn" });
  expect(describeEntry(e("approval", "pending", "Bash: git push", "Pushes a branch"))).toMatchObject({
    title: "Needs your approval",
    subtitle: "Bash: git push — Pushes a branch",
    tone: "attention",
  });
  expect(titleOf("approval", "", "", "Approved: Bash")).toBe("You approved Bash");
});

test("the timeline is newest first, by day, with the time of each entry", () => {
  const yesterday = NOW - 24 * 60 * MIN;
  const days = buildTimeline([e("trigger", "owner", "", "go", yesterday), e("report", "ready", "", "done", NOW), e("assignment", "started", "", "", NOW - MIN)], NOW);
  expect(days.map((d) => d.label)).toEqual(["Today", "Yesterday"]);
  expect(days[0].rows.map((r) => r.view.title)).toEqual(["Ready for review", "Started work"]);
  expect(days[1].rows.map((r) => r.view.title)).toEqual(["Started from your request"]);
  expect(clockTime(new Date(2026, 10, 5, 9, 7).getTime())).toBe("09:07");
  expect(dayLabel(new Date(2026, 9, 30, 12).getTime(), NOW)).toBe("Fri 30 Oct");
  expect(dayLabel(new Date(2025, 9, 30, 12).getTime(), NOW)).toBe("Thu 30 Oct 2025");
});

test("approvals in a row with the same verdict are one row that lists what was approved", () => {
  const rows = buildTimeline(
    [
      e("approval", "approved", "Bash: npm test", "", NOW - 30 * MIN, "A-1"),
      e("approval", "approved", "Bash: go vet ./...", "", NOW - 28 * MIN, "A-2"),
      e("approval", "approved", "Bash: make check", "", NOW - 25 * MIN, "A-3"),
      e("approval", "approved", "Bash: git push", "", NOW - 20 * MIN, "A-4"),
      // Too long after the last one: a row of its own.
      e("approval", "approved", "Edit: main.go", "", NOW - 5 * MIN, "A-5"),
      e("report", "progress", "", "half way", NOW - 4 * MIN),
      e("approval", "denied", "Bash: rm", "", NOW - 3 * MIN, "A-6"),
      e("approval", "approved", "Bash: ls", "", NOW - 2 * MIN, "A-7"),
    ],
    NOW,
  )[0].rows;
  expect(rows.map((r) => r.view.title)).toEqual([
    "You approved Bash",
    "You denied Bash",
    "Progress update",
    "You approved Edit",
    "You approved 4 commands",
  ]);
  const group = rows[4];
  expect(group.type).toBe("group");
  expect(group.type === "group" && group.items).toEqual(["Bash: npm test", "Bash: go vet ./...", "Bash: make check", "Bash: git push"]);
  // A group of different tools counts actions.
  const mixed = buildTimeline([e("approval", "approved", "Bash: ls", "", NOW - MIN), e("approval", "approved", "Edit: a.go", "", NOW)], NOW)[0].rows;
  expect(mixed.map((r) => r.view.title)).toEqual(["You approved 2 actions"]);
});

const rt = (kind: string, subkind: string, runtime: string, subject = "", text = "", at = NOW, refId = "") =>
  create(TimelineEntrySchema, { kind, subkind, subject, text, refId, runtime, at: timestampFromMs(at) });

test("timeline copy for restart, resumed, limited, continued and review verdicts", () => {
  // The start says its runtime and model.
  expect(describeEntry(rt("assignment", "started", "claude", "claude-sonnet-4-5"))).toMatchObject({ title: "Started work · Claude Code · sonnet 4.5", tone: "ok" });
  expect(describeEntry(rt("assignment", "started", "codex", "gpt-5.5"))).toMatchObject({ title: "Started work · Codex · gpt-5.5" });
  expect(describeEntry(rt("assignment", "started", "codex"))).toMatchObject({ title: "Started work · Codex" });
  expect(describeEntry(rt("assignment", "restart", "claude"))).toMatchObject({ title: "Interrupted by a server restart", subtitle: "", tone: "warn" });
  expect(describeEntry(rt("assignment", "resumed", "claude"))).toMatchObject({ title: "Resumed automatically", subtitle: "", tone: "ok" });

  const reset = new Date(2026, 10, 5, 17, 30);
  expect(describeEntry(rt("assignment", "limited", "claude", "claude", reset.toISOString()))).toMatchObject({
    title: "Usage limit reached on Claude Code",
    subtitle: "Resets at 17:30",
    tone: "warn",
  });
  expect(describeEntry(rt("assignment", "limited", "codex", "codex", ""))).toMatchObject({
    title: "Usage limit reached on Codex",
    subtitle: "Reset time unknown",
    tone: "warn",
  });
  // A reset on another day than the limit says its date.
  const later = new Date(2026, 10, 9, 8, 0);
  expect(describeEntry(rt("assignment", "limited", "codex", "codex", later.toISOString())).subtitle).toBe("Resets at Mon 9 Nov 08:00");

  expect(describeEntry(rt("assignment", "review", "codex", "gpt-5.5"))).toMatchObject({ title: "Review started · Codex" });
  expect(describeEntry(rt("review", "approved", "codex", "codex"))).toMatchObject({ title: "Review approved", subtitle: "", tone: "ok" });
  expect(describeEntry(rt("review", "changes", "codex", "codex", "The parser drops the last token"))).toMatchObject({
    title: "Changes requested",
    subtitle: "The parser drops the last token",
    tone: "neutral",
  });
  expect(describeEntry(rt("review", "failed", "claude", "claude", "no runtime could review"))).toMatchObject({
    title: "Review could not finish",
    subtitle: "no runtime could review",
    tone: "warn",
  });

  // The start right after a limit continues on the other runtime; a later start is a start again.
  const days = buildTimeline(
    [
      rt("assignment", "started", "claude", "claude-opus-4-1", "", NOW - 30 * MIN, "A-1"),
      rt("assignment", "limited", "claude", "claude", "", NOW - 20 * MIN, "A-1"),
      rt("assignment", "started", "codex", "gpt-5.5", "", NOW - 19 * MIN, "A-2"),
      rt("assignment", "ended", "codex", "", "", NOW - 10 * MIN, "A-2"),
      rt("assignment", "started", "claude", "claude-opus-4-1", "", NOW - 5 * MIN, "A-3"),
    ],
    NOW,
  );
  expect(days[0].rows.map((r) => r.view.title)).toEqual([
    "Started work · Claude Code · opus 4.1",
    "Session ended",
    "Continued on Codex · gpt-5.5",
    "Usage limit reached on Claude Code",
    "Started work · Claude Code · opus 4.1",
  ]);
});
