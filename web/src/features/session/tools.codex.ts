// Codex sessions' tools as the session view says them (describeTool dispatches here): a command it ran (the command
// inside Codex's login shell, and where when not the worktree's root), a patch it applied (one path or "N files", +N −M
// from its diffs), a web search (its query) and its plan (the steps and their status). Athena's own MCP tools read as
// they do for Claude Code, in describeTool.
import type { StreamItem } from "@/gen/athena/v1/stream_pb";
import { clipped, type Counts, type DiffLine, linesOf, type PatchFile, shortenPaths, shortPath, str, type Todo, type ToolInput, type ToolView } from "./tools";

const firstLine = (s: string) => s.split("\n", 1)[0];

// Codex runs a command as `<shell> -lc '<command>'`.
const SHELL = /^(?:\/\S*\/)?(?:ba|z|da|k)?sh\s+-l?c\s+([\s\S]+)$/;

/** One shell word made of '…' and "…" parts and \ escapes, as its value; undefined when s is not exactly one word. */
function shellWord(s: string): string | undefined {
  let out = "";
  for (let i = 0; i < s.length; ) {
    const c = s[i];
    if (c === "'" || c === '"') {
      const end = s.indexOf(c, i + 1);
      const part = s.slice(i + 1, end);
      // A double-quoted part that expands or escapes anything is not read here.
      if (end < 0 || (c === '"' && /[$`\\]/.test(part))) return undefined;
      out += part;
      i = end + 1;
    } else if (c === "\\" && i + 1 < s.length) {
      out += s[i + 1];
      i += 2;
    } else if (/\s/.test(c)) {
      return undefined;
    } else {
      out += c;
      i++;
    }
  }
  return out;
}

/** The command a Codex command item ran: inside its login shell when it is one. */
function innerCommand(command: string): string {
  const m = SHELL.exec(command.trim());
  return (m && shellWord(m[1])) || command;
}

function command(input: ToolInput, summary: string): ToolView {
  const cmd = shortenPaths(firstLine(innerCommand(str(input, "command")).trim())) || shortenPaths(firstLine(summary));
  const cwd = str(input, "cwd");
  const where = cwd ? shortPath(cwd) : ".";
  return { kind: "bash", label: "Ran", target: where === "." ? cmd : `${cmd} in ${where}` };
}

// The header lines of a unified diff, before its first hunk: they change nothing.
const DIFF_HEADER = /^(?:--- |\+\+\+ |diff --git |index [0-9a-f])/;

/**
 * A change's diff as lines, by its kind: an add is every line added and a delete every line removed (the file's
 * content, plain, or with each line signed, the sign then dropped once); an update is a unified diff, whose --- and
 * +++ lines are headers only before its first hunk.
 */
function patchLines(kind: string, diff: string): DiffLine[] {
  const lines = diff ? diff.replace(/\n$/, "").split("\n") : [];
  if (kind === "add" || kind === "delete") {
    const sign = kind === "add" ? "+" : "−";
    const mark = kind === "add" ? "+" : "-";
    const signed = lines.length > 0 && lines.every((l) => !l || l.startsWith(mark));
    return lines.map((l) => ({ sign, text: signed ? l.slice(1) : l }));
  }
  let header = true;
  return lines.flatMap((l): DiffLine[] => {
    if (l.startsWith("@@")) header = false;
    else if (header && DIFF_HEADER.test(l)) return [];
    else header = false;
    if (l.startsWith("+")) return [{ sign: "+", text: l.slice(1) }];
    if (l.startsWith("-")) return [{ sign: "−", text: l.slice(1) }];
    return [{ sign: " ", text: l.startsWith(" ") ? l.slice(1) : l }];
  });
}

function patch(input: ToolInput, summary: string): ToolView {
  const changes = Array.isArray(input.changes) ? input.changes : [];
  const files: PatchFile[] = [];
  const counts: Counts = { add: 0, del: 0, about: false };
  changes.forEach((c, i) => {
    if (!c || typeof c !== "object") return;
    const change = c as Record<string, unknown>;
    const kind = str(change, "kind");
    const diff = str(change, "diff");
    const lines = patchLines(kind, diff);
    const cut = clipped(input, `changes.${i}.diff`);
    // A clipped add or delete counts its full length; a clipped update counts what was shown.
    const full = linesOf(input, `changes.${i}.diff`, diff);
    counts.add += cut && kind === "add" ? full : lines.filter((l) => l.sign === "+").length;
    counts.del += cut && kind === "delete" ? full : lines.filter((l) => l.sign === "−").length;
    counts.about ||= cut;
    files.push({ path: shortPath(str(change, "path")), file: str(change, "path"), kind, lines, clipped: cut });
  });
  if (files.length === 0) return { kind: "edit", label: "Edited", target: shortenPaths(firstLine(summary)) };
  const created = files.every((f) => f.kind === "add");
  return {
    kind: created ? "write" : "edit",
    label: created ? "Created" : "Edited",
    target: files.length === 1 ? files[0].path : `${files.length} files`,
    counts,
    patch: files,
  };
}

function plan(input: ToolInput): ToolView {
  const steps = Array.isArray(input.steps) ? input.steps : [];
  const todos: Todo[] = [];
  let statuses = false;
  for (const s of steps) {
    if (!s || typeof s !== "object" || typeof (s as { step?: unknown }).step !== "string") continue;
    const status = (s as { status?: unknown }).status;
    statuses ||= typeof status === "string";
    todos.push({ content: (s as { step: string }).step, status: typeof status === "string" ? status : "pending" });
  }
  if (!statuses) return { kind: "todo", label: "Planned", target: `${todos.length} ${todos.length === 1 ? "step" : "steps"}`, todos };
  const done = todos.filter((t) => t.status === "completed").length;
  return { kind: "todo", label: "Updated the plan", target: `${done} of ${todos.length} done`, todos };
}

/** A Codex tool call as a verb and a target; undefined for a tool that is not Codex's own. */
export function codexToolView(tool: string, input: ToolInput, summary: string): ToolView | undefined {
  switch (tool) {
    case "exec_command":
      return command(input, summary);
    case "apply_patch":
      return patch(input, summary);
    case "web_search":
      return { kind: "web", label: "Searched the web", target: str(input, "query") || shortenPaths(firstLine(summary)) };
    case "update_plan":
      return plan(input);
  }
  return undefined;
}

/**
 * A failed tool's result text, for errorLine: a Codex command's leads with its exit code ("Exit code 1"), which its
 * result's detail holds ({"exitCode":n}); any other result's is its text.
 */
export function exitStatusText(tool: string, result: StreamItem, text: string): string {
  if (tool !== "exec_command" || !result.detail) return text;
  try {
    const code: unknown = (JSON.parse(result.detail) as { exitCode?: unknown }).exitCode;
    if (Number.isSafeInteger(code) && code !== 0) return `Exit code ${code as number}\n${text}`;
  } catch {
    // No exit code: the text alone.
  }
  return text;
}
