// The timeline in plain words: what each TimelineEntry says (describeEntry), and the rows the rail shows
// (buildTimeline): newest first, by local day, with a run of approvals of one verdict made one row. The words follow
// the entry's kind and subkind (internal/hub/views.go ticketTimeline); an older server sends no subkind, and its text
// still gives a sensible title.
import { timestampMs } from "@bufbuild/protobuf/wkt";
import type { TimelineEntry } from "@/gen/athena/v1/state_pb";
import { modelName } from "@/lib/text/model";
import type { Tone } from "@/ui/athena/Dot";

export type EntryView = {
  title: string;
  subtitle: string;
  tone: Tone;
  /** A short tag after the title, such as a Report's "PR #12". */
  badge?: string;
  /** A Report: its row offers "View report". */
  report?: boolean;
};

type Entry = Pick<TimelineEntry, "kind" | "subkind" | "subject" | "text" | "refId">;

/** Approvals of one verdict this close together are one row. */
const GROUP_GAP_MS = 10 * 60_000;

export const entryMs = (e: Pick<TimelineEntry, "at">) => (e.at ? timestampMs(e.at) : 0);

const TRIGGER: Record<string, string> = {
  issue: "Started from a GitHub issue",
  pull: "Started from a pull request",
  "issue-comment": "Started from an issue comment",
  "pull-comment": "Started from a PR comment",
  "review-comment": "Started from a review comment",
  review: "Started from a review",
  ci: "Started from CI",
  owner: "Started from your request",
};

const REPORT: Record<string, [string, Tone]> = {
  ready: ["Ready for review", "ok"],
  waiting: ["Waiting on you", "neutral"],
  progress: ["Progress update", "neutral"],
  blocked: ["Blocked", "warn"],
  failed: ["Failed", "danger"],
  dropped: ["Finished", "ok"],
};

const INPUT: Record<string, string> = {
  receipt: "Acknowledged on GitHub",
  note: "Note",
  resumed: "Resumed",
  context: "Seen on a followed Thread",
  text: "Asked for a status update",
  trigger: "Automated message",
};

/** The source of a Trigger an older server sent without a subkind, from its id. */
function legacySource(refId: string): string {
  if (refId.startsWith("github:issue:")) return "issue";
  if (refId.startsWith("github:issuecomment:")) return "issue-comment";
  if (refId.startsWith("github:review")) return "review";
  return "";
}

/** One line of Markdown as plain words: no heading, list or quote marker, emphasis or code ticks. */
export function plainLine(s: string): string {
  return s
    .replace(/^\s*(?:#{1,6}\s+|>\s*|[-*+]\s+|\d+[.)]\s+)/, "")
    .replace(/(\*\*|__|`)/g, "")
    .trim();
}

/** "Bash: npm test" as its tool and what it ran. */
function splitWhat(subject: string): [string, string] {
  const at = subject.indexOf(": ");
  return at < 0 ? [subject, ""] : [subject.slice(0, at), subject.slice(at + 2)];
}

const joined = (...parts: string[]) => parts.filter(Boolean).join(" — ");

export function describeEntry(e: Entry): EntryView {
  const sub = e.subkind;
  const text = e.text;
  switch (e.kind) {
    case "trigger": {
      const title = TRIGGER[sub || legacySource(e.refId)] ?? "Request received";
      return { title, subtitle: text, tone: "neutral" };
    }
    case "decision":
      return { title: "Router started this Ticket", subtitle: "", tone: "neutral" };
    case "assignment": {
      const kind = sub || (text === "running" ? "started" : text === "pending" ? "queued" : "ended");
      switch (kind) {
        case "queued":
          return { title: "Queued", subtitle: "", tone: "neutral" };
        case "started":
          return { title: e.subject ? `Started work · ${modelName(e.subject)}` : "Started work", subtitle: "", tone: "ok" };
        case "interrupted":
          return { title: "Interrupted by a server restart", subtitle: "Resumes on your next message", tone: "warn" };
        case "restart":
          return { title: "Interrupted by a server restart", subtitle: "", tone: "warn" };
        case "resumed":
          return { title: "Resumed automatically", subtitle: "", tone: "ok" };
        case "failed":
          return { title: "Failed", subtitle: text, tone: "danger" };
        case "stopped":
          return { title: "Stopped by you", subtitle: text, tone: "warn" };
        case "retried":
          return { title: "Retried", subtitle: "A new session took over", tone: "warn" };
        default:
          return { title: "Session ended", subtitle: sub ? text : "", tone: "neutral" };
      }
    }
    case "report": {
      let state = sub;
      let line = text;
      if (!state) {
        const m = /^([a-z]+): ([\s\S]*)$/.exec(text);
        if (m && m[1] in REPORT) [state, line] = [m[1], m[2]];
      }
      const [title, tone] = REPORT[state] ?? ["Report", "neutral"];
      return { title, subtitle: plainLine(line), tone, badge: e.subject || undefined, report: true };
    }
    case "reply":
      if (sub === "github") return { title: e.subject ? `@${e.subject} replied on GitHub` : "Reply on GitHub", subtitle: text, tone: "neutral" };
      return { title: "You replied", subtitle: text, tone: "neutral" };
    case "input":
      return { title: INPUT[sub] ?? "Message", subtitle: text, tone: "neutral" };
    case "approval": {
      let verdict = sub;
      let subject = e.subject;
      if (!verdict) {
        // An older server: "Approved: Bash (why)", or "Bash: summary" for the pending one.
        const m = /^(Approved|Denied|Expired): (.*)$/.exec(text);
        verdict = m ? m[1].toLowerCase() : "pending";
        subject = m ? m[2] : text;
      }
      const [tool, what] = splitWhat(subject);
      switch (verdict) {
        case "approved":
          return { title: `You approved ${tool || "an action"}`, subtitle: joined(what, sub ? text : ""), tone: "ok" };
        case "denied":
          return { title: `You denied ${tool || "an action"}`, subtitle: joined(what, sub ? text : ""), tone: "danger" };
        case "expired":
          return { title: "Approval expired", subtitle: joined(subject, sub ? text : ""), tone: "warn" };
        default:
          return { title: "Needs your approval", subtitle: joined(subject, sub ? text : ""), tone: "attention" };
      }
    }
  }
  return { title: e.kind || "Event", subtitle: text, tone: "neutral" };
}

export type Row =
  | { type: "entry"; key: string; at: number; entry: TimelineEntry; view: EntryView }
  | { type: "group"; key: string; at: number; entries: TimelineEntry[]; items: string[]; view: EntryView };

export type Day = { key: string; label: string; rows: Row[] };

/** A stable key for an entry: the same entry in the next detail Delta has the same key. */
export const entryKey = (e: TimelineEntry) => `${e.kind}:${e.subkind}:${e.refId}:${entryMs(e)}`;

const answered = (e: TimelineEntry) => e.kind === "approval" && (e.subkind === "approved" || e.subkind === "denied");

function groupView(entries: TimelineEntry[]): EntryView {
  const tools = new Set(entries.map((e) => splitWhat(e.subject)[0]));
  const noun = tools.size === 1 && tools.has("Bash") ? "commands" : "actions";
  const verb = entries[0].subkind === "denied" ? "denied" : "approved";
  return { title: `You ${verb} ${entries.length} ${noun}`, subtitle: "", tone: verb === "denied" ? "danger" : "ok" };
}

/** The rows, oldest first, with each run of answered approvals of one verdict, each within 10 minutes of the last, one row. */
function rowsOf(sorted: TimelineEntry[]): Row[] {
  const rows: Row[] = [];
  for (let i = 0; i < sorted.length; ) {
    const e = sorted[i];
    let j = i + 1;
    if (answered(e)) {
      while (j < sorted.length && sorted[j].kind === "approval" && sorted[j].subkind === e.subkind && entryMs(sorted[j]) - entryMs(sorted[j - 1]) <= GROUP_GAP_MS) j++;
    }
    if (j - i > 1) {
      const run = sorted.slice(i, j);
      rows.push({ type: "group", key: entryKey(e), at: entryMs(run[run.length - 1]), entries: run, items: run.map((r) => joined(r.subject, r.text)), view: groupView(run) });
    } else {
      rows.push({ type: "entry", key: entryKey(e), at: entryMs(e), entry: e, view: describeEntry(e) });
      j = i + 1;
    }
    i = j;
  }
  return rows;
}

const startOfDay = (ms: number) => new Date(ms).setHours(0, 0, 0, 0);

const weekday = new Intl.DateTimeFormat("en-GB", { weekday: "short", day: "numeric", month: "short" });
const weekdayYear = new Intl.DateTimeFormat("en-GB", { weekday: "short", day: "numeric", month: "short", year: "numeric" });
const clock = new Intl.DateTimeFormat("en-GB", { hour: "2-digit", minute: "2-digit", hourCycle: "h23" });

/** "Today", "Yesterday", or the date ("Fri 30 Oct", with the year when it is not this year's). */
export function dayLabel(at: number, now: number): string {
  const days = Math.round((startOfDay(now) - startOfDay(at)) / 86_400_000);
  if (days === 0) return "Today";
  if (days === 1) return "Yesterday";
  const d = new Date(at);
  return (d.getFullYear() === new Date(now).getFullYear() ? weekday : weekdayYear).format(d).replace(",", "");
}

/** The local time of day, HH:MM. */
export const clockTime = (at: number) => clock.format(new Date(at));

/**
 * The rail's days, newest first, each with its rows newest first. An open question (the Ticket waits on the owner's
 * answer) is the newest report's: that row says so and has the orange dot.
 */
export function buildTimeline(entries: readonly TimelineEntry[], now: number, question = ""): Day[] {
  const sorted = [...entries].sort((a, b) => entryMs(a) - entryMs(b));
  const rows = rowsOf(sorted).reverse();
  const asking = question.trim() ? rows.find((r) => r.type === "entry" && r.entry.kind === "report") : undefined;
  if (asking) asking.view = { ...asking.view, title: "Asked you a question", subtitle: question, tone: "attention" };
  const days: Day[] = [];
  for (const row of rows) {
    const key = String(startOfDay(row.at));
    let day = days[days.length - 1];
    if (day?.key !== key) {
      day = { key, label: dayLabel(row.at, now), rows: [] };
      days.push(day);
    }
    day.rows.push(row);
  }
  return days;
}
