// What the session view says about a stream item, as plain data: a tool call as a verb and a target (Bash's own
// description, a path relative to the Ticket's worktree), its line counts, an error's first useful line, the init
// line's model and the result line's numbers. The rows in ToolCall.tsx and SessionView.tsx draw it.
import type { StreamItem } from "@/gen/athena/v1/stream_pb";
import { stripAnsi } from "@/lib/text/ansi";

/** A tool call's input: StreamItem.detail parsed, every string stripped of escapes. */
export type ToolInput = { [key: string]: unknown; _lines?: Record<string, unknown> };

export type ToolKind =
  | "bash"
  | "read"
  | "write"
  | "edit"
  | "search"
  | "web"
  | "todo"
  | "agent"
  | "report"
  | "ack"
  | "mcp"
  | "other"
  | "hidden";

/** Lines an edit adds and deletes; about when a value was clipped, so the counts are an estimate. */
export type Counts = { add: number; del: number; about: boolean };

export type ToolView = { kind: ToolKind; label: string; target: string; counts?: Counts };

/** A duration as the session view says it: 0.4 s, 1.2 s, 12 s, 2 m 5 s. */
export function formatDuration(ms: number): string {
  const s = Math.max(0, ms) / 1000;
  if (s < 10) return `${s.toFixed(1)} s`;
  if (s < 60) return `${Math.round(s)} s`;
  const m = Math.floor(s / 60);
  return `${m} m ${Math.round(s - m * 60)} s`;
}

export { modelName } from "@/lib/text/model";

// A Ticket's worktree (…/.athena/work/<slug>/) and a home directory, wherever they appear in a text.
const WORKTREE = /(?:\/[^\s/'"`]+)*\/\.athena\/work\/[^\s/'"`]+(\/?)/g;
const HOME = /(^|[\s'"`=(])\/(?:Users|home)\/[^\s/'"`]+\//g;

/** Every path in text shortened: relative to the Ticket's worktree (its root is "."), else from ~ for home. */
export function shortenPaths(text: string): string {
  // The worktree itself is "."; a path inside it loses the prefix.
  const inside = (m: string, at: number, whole: string) => m.endsWith("/") && !!whole[at + m.length] && !/[\s'"`;&|)]/.test(whole[at + m.length]);
  return text
    .replace(WORKTREE, (m: string, _slash: string, at: number, whole: string) => (inside(m, at, whole) ? "" : "."))
    .replace(HOME, "$1~/");
}

/** A path as the session view shows it: relative to the Ticket's worktree, else with home as ~. */
export function shortPath(p: string): string {
  return shortenPaths(p) || ".";
}

/** How many lines s has: a final newline ends the last line rather than starting one. */
export function lineCount(s: string): number {
  if (!s) return 0;
  const n = s.split("\n").length;
  return s.endsWith("\n") ? n - 1 : n;
}

const firstLine = (s: string) => s.split("\n", 1)[0];

/**
 * A failed tool's result as one line: its first non-empty line, without a hook's prefix or the tool error tags. A
 * hook's denial reads "Blocked: <reason>", once: a reason that already says it was blocked or denied is shown as it is.
 */
export function errorLine(text: string): string {
  let s = text.trim().replace(/^<tool_use_error>/, "").replace(/<\/tool_use_error>$/, "");
  let hook = false;
  s = s.replace(/^\w+(?::[\w*.-]+)? hook error:\s*/, () => {
    hook = true;
    return "";
  });
  const lines = s.split("\n").map((l) => l.trim()).filter(Boolean);
  let line = lines[0] ?? "";
  // "Exit code 1" alone says little: the line after it says why.
  if (/^Exit code \d+$/.test(line) && lines[1]) line += ": " + lines[1];
  if (!hook || !line) return line;
  return /\b(blocked|denied)\b/i.test(line) ? line[0].toUpperCase() + line.slice(1) : "Blocked: " + line;
}

export type ResultView = { title: "Done" | "Failed" | "Stopped"; meta: string };

/** The result line's head (success turns=24 cost=$0.4312 duration_ms=372000) as an outcome and its numbers. */
export function parseResult(head: string, isError: boolean): ResultView {
  const [subtype = "", ...pairs] = head.trim().split(/\s+/);
  const kv = new Map(pairs.map((p) => p.split("=", 2) as [string, string]));
  const title = /interrupt|cancel|max_turns|max_budget/.test(subtype)
    ? "Stopped"
    : isError || subtype.startsWith("error")
      ? "Failed"
      : "Done";
  const meta: string[] = [];
  const turns = Number(kv.get("turns"));
  if (kv.has("turns") && Number.isFinite(turns)) meta.push(`${turns} ${turns === 1 ? "turn" : "turns"}`);
  const cost = Number(kv.get("cost")?.replace("$", ""));
  if (kv.has("cost") && Number.isFinite(cost)) meta.push(`$${cost.toFixed(2)}`);
  const ms = Number(kv.get("duration_ms"));
  if (ms > 0) meta.push(formatDuration(ms));
  return { title, meta: meta.join(" · ") };
}

function strip(v: unknown): unknown {
  if (typeof v === "string") return stripAnsi(v);
  if (Array.isArray(v)) return v.map(strip);
  if (v && typeof v === "object") return Object.fromEntries(Object.entries(v).map(([k, e]) => [k, strip(e)]));
  return v;
}

const inputs = new WeakMap<StreamItem, ToolInput>();

/** A call's input from its detail, parsed and stripped once per item; {} when there is none or it is not an object. */
export function toolInput(call?: StreamItem): ToolInput {
  if (!call) return {};
  let v = inputs.get(call);
  if (v === undefined) {
    v = {};
    try {
      const parsed: unknown = call.detail ? JSON.parse(call.detail) : {};
      if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) v = strip(parsed) as ToolInput;
    } catch {
      // Not JSON: the row shows the summary instead.
    }
    inputs.set(call, v);
  }
  return v;
}

/** input[key] when it is a string, else "". */
export function str(input: Record<string, unknown>, key: string): string {
  const v = input[key];
  return typeof v === "string" ? v : "";
}

/** The full line count of input's value at path: "_lines" has it when the value was clipped. */
export function linesOf(input: ToolInput, path: string, value: string): number {
  const n = input._lines?.[path];
  return typeof n === "number" ? n : lineCount(value);
}

export type DiffLine = { sign: "+" | "−" | " "; text: string };

const CONTEXT = 3;
const splitLines = (s: string) => (s ? s.replace(/\n$/, "").split("\n") : []);

/** An edit as a small diff: the lines it changes, with up to three unchanged lines on each side. */
export function editDiff(oldText: string, newText: string): DiffLine[] {
  const a = splitLines(oldText);
  const b = splitLines(newText);
  let pre = 0;
  while (pre < a.length && pre < b.length && a[pre] === b[pre]) pre++;
  let suf = 0;
  while (suf < a.length - pre && suf < b.length - pre && a[a.length - 1 - suf] === b[b.length - 1 - suf]) suf++;
  return [
    ...a.slice(Math.max(0, pre - CONTEXT), pre).map((text) => ({ sign: " " as const, text })),
    ...a.slice(pre, a.length - suf).map((text) => ({ sign: "−" as const, text })),
    ...b.slice(pre, b.length - suf).map((text) => ({ sign: "+" as const, text })),
    ...a.slice(a.length - suf, a.length - suf + CONTEXT).map((text) => ({ sign: " " as const, text })),
  ];
}

/** Whether input's value at path reached the dashboard shortened ("_lines" has its full line count). */
export function clipped(input: ToolInput, path: string): boolean {
  return !!input._lines && path in input._lines;
}

/**
 * The +N −M of one edit: of its diff. When either value was clipped the diff cannot be taken: the counts are the full
 * line counts less the lines both shown versions share at their start (the last, cut line never counts as shared), and
 * are marked about.
 */
function editCounts(input: ToolInput, path: string, edit: Record<string, unknown>): Counts {
  const o = str(edit, "old_string");
  const n = str(edit, "new_string");
  if (clipped(input, path + "old_string") || clipped(input, path + "new_string")) {
    const a = splitLines(o);
    const b = splitLines(n);
    let pre = 0;
    while (pre < a.length - 1 && pre < b.length - 1 && a[pre] === b[pre]) pre++;
    return {
      add: Math.max(0, linesOf(input, path + "new_string", n) - pre),
      del: Math.max(0, linesOf(input, path + "old_string", o) - pre),
      about: true,
    };
  }
  const d = editDiff(o, n);
  return { add: d.filter((l) => l.sign === "+").length, del: d.filter((l) => l.sign === "−").length, about: false };
}

/** A MultiEdit's edits (an Edit is one), each with its _lines path prefix. */
export function editsOf(input: ToolInput): { edit: Record<string, unknown>; path: string }[] {
  if (!Array.isArray(input.edits)) return [{ edit: input, path: "" }];
  return input.edits
    .map((e, i) => ({ edit: e as Record<string, unknown>, path: `edits.${i}.` }))
    .filter(({ edit }) => edit && typeof edit === "object");
}

export type Todo = { content: string; status: string };

/** A TodoWrite's todos, those that have a text. */
export function todoList(input: ToolInput): Todo[] {
  if (!Array.isArray(input.todos)) return [];
  return input.todos.flatMap((t) =>
    t && typeof t === "object" && typeof (t as Todo).content === "string"
      ? [{ content: (t as Todo).content, status: typeof (t as Todo).status === "string" ? (t as Todo).status : "pending" }]
      : [],
  );
}

/** A tool call as a verb and a target: what the row says before it is opened. summary is the call's own text. */
export function describeTool(tool: string, input: ToolInput, summary: string): ToolView {
  const path = (key: string) => shortPath(str(input, key));
  const fallback = shortenPaths(firstLine(summary));
  switch (tool) {
    case "Bash": {
      const cmd = str(input, "command");
      return { kind: "bash", label: firstLine(str(input, "description")) || "Ran a command", target: shortenPaths(firstLine(cmd)) || fallback };
    }
    case "Read": {
      const off = Number(input.offset) || 0;
      const lim = Number(input.limit) || 0;
      const range = off || lim ? `:${off || 1}-${lim ? (off || 1) + lim - 1 : "…"}` : "";
      return { kind: "read", label: "Read", target: (str(input, "file_path") ? path("file_path") : fallback) + range };
    }
    case "Write": {
      const content = str(input, "content");
      return { kind: "write", label: "Created", target: path("file_path"), counts: { add: linesOf(input, "content", content), del: 0, about: false } };
    }
    case "Edit":
    case "MultiEdit": {
      const counts: Counts = { add: 0, del: 0, about: false };
      for (const { edit, path: p } of editsOf(input)) {
        const c = editCounts(input, p, edit);
        counts.add += c.add;
        counts.del += c.del;
        counts.about ||= c.about;
      }
      return { kind: "edit", label: "Edited", target: path("file_path"), counts };
    }
    case "NotebookEdit":
      return { kind: "edit", label: "Edited", target: path("notebook_path") };
    case "Grep": {
      const where = str(input, "path");
      return { kind: "search", label: "Searched for", target: str(input, "pattern") + (where ? ` in ${shortPath(where)}` : "") };
    }
    case "Glob":
      return { kind: "search", label: "Found files", target: str(input, "pattern") };
    case "WebFetch":
      return { kind: "web", label: "Fetched", target: str(input, "url") };
    case "WebSearch":
      return { kind: "web", label: "Searched the web", target: str(input, "query") };
    case "TodoWrite": {
      const todos = todoList(input);
      return { kind: "todo", label: "Updated the plan", target: `${todos.filter((t) => t.status === "completed").length} of ${todos.length} done` };
    }
    case "Task":
    case "Agent":
      return { kind: "agent", label: "Started a subagent", target: firstLine(str(input, "description")) };
    case "ToolSearch":
      return { kind: "hidden", label: "Loaded tools", target: "" };
    case "mcp__athena__report": {
      // A pull request number is a positive integer; anything else (1e999, 2.5) shows none.
      const pull = Number.isSafeInteger(input.pull) ? Number(input.pull) : 0;
      const head = firstLine(str(input, "summary"));
      return { kind: "report", label: `Reported ${str(input, "status") || "progress"}`, target: pull > 0 ? `PR #${pull} · ${head}` : head };
    }
    case "mcp__athena__ack":
      return { kind: "ack", label: "Acknowledged", target: firstLine(str(input, "text")) };
  }
  const mcp = /^mcp__[^_]+(?:_[^_]+)*?__(.+)$/.exec(tool);
  if (mcp) return { kind: "mcp", label: mcp[1].replace(/_/g, " "), target: fallback };
  return { kind: "other", label: tool || "Tool", target: fallback };
}

export type GithubFrom = { from: string; url?: string; kind: string };

/** Who wrote a card and its kind are at most this many characters (the server caps them too). */
const FIELD_MAX = 80;

const SAFE_LINK = /^https?:\/\/[^\s\u0000-\u001f]+$/i;

/** Who wrote a GitHub message the worker was sent (a "user" item's detail), its link only when it is http(s). */
export function githubFrom(it: StreamItem): GithubFrom {
  let d: Record<string, unknown> = {};
  try {
    const v: unknown = it.detail ? JSON.parse(it.detail) : {};
    if (v && typeof v === "object" && !Array.isArray(v)) d = v as Record<string, unknown>;
  } catch {
    // No detail: a message from someone on GitHub.
  }
  const url = typeof d.url === "string" && SAFE_LINK.test(d.url) ? d.url : undefined;
  const cap = (v: unknown) => (typeof v === "string" ? [...stripAnsi(v)].slice(0, FIELD_MAX).join("") : "");
  return { from: cap(d.from) || "Someone", url, kind: cap(d.kind) };
}
