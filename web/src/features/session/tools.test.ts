import { expect, test } from "vitest";
import { item } from "@/lib/stream/testkit";
import {
  describeTool,
  editDiff,
  errorLine,
  formatDuration,
  lineCount,
  modelName,
  parseResult,
  shortenPaths,
  shortPath,
  todoList,
  toolInput,
} from "./tools";

const call = (tool: string, input: object, text = "") =>
  item({ kind: "tool_call", tool, toolUseId: "tu", text, detail: JSON.stringify(input) });
const view = (tool: string, input: object, text = "") => {
  const c = call(tool, input, text);
  return describeTool(c.tool, toolInput(c), text);
};

test("durations read as seconds, then minutes", () => {
  expect(formatDuration(400)).toBe("0.4 s");
  expect(formatDuration(12_300)).toBe("12 s");
  expect(formatDuration(372_000)).toBe("6 m 12 s");
  expect(formatDuration(-5)).toBe("0.0 s");
});

test("model ids read as a short name, or stay as they are", () => {
  expect(modelName("claude-haiku-4-5-20251001")).toBe("haiku 4.5");
  expect(modelName("claude-sonnet-5")).toBe("sonnet 5");
  expect(modelName("claude-opus-4-1")).toBe("opus 4.1");
  expect(modelName("claude-3-5-sonnet-20241022")).toBe("sonnet 3.5");
  expect(modelName("gpt-x")).toBe("gpt-x");
  expect(modelName("")).toBe("");
});

test("paths read relative to the Ticket's worktree, else from home", () => {
  expect(shortPath("/Users/dana/.athena/work/fix-login-t-0004/frontend/index.html")).toBe("frontend/index.html");
  expect(shortPath("/home/dana/.athena/work/fix-login-t-0004")).toBe(".");
  expect(shortPath("/Users/dana/src/app/main.go")).toBe("~/src/app/main.go");
  expect(shortPath("/home/dana/notes.md")).toBe("~/notes.md");
  expect(shortPath("internal/web/stream.go")).toBe("internal/web/stream.go");
  expect(shortenPaths("cd /Users/dana/.athena/work/fix-t-0004 && go test /Users/dana/.athena/work/fix-t-0004/api/...")).toBe(
    "cd . && go test api/...",
  );
});

test("an error reads as its first useful line, without the hook's prefix", () => {
  expect(errorLine("PreToolUse:Bash hook error: the session ended before the owner answered; do not retry")).toBe(
    "Blocked: the session ended before the owner answered; do not retry",
  );
  expect(errorLine("<tool_use_error>File does not exist.</tool_use_error>")).toBe("File does not exist.");
  expect(errorLine('Exit code 1\nPost "https://api.github.com/graphql": tls: failed')).toBe(
    'Exit code 1: Post "https://api.github.com/graphql": tls: failed',
  );
  expect(errorLine("\n\nExit code 128")).toBe("Exit code 128");
  expect(errorLine("")).toBe("");
});

test("line counts end at a final newline", () => {
  expect(lineCount("")).toBe(0);
  expect(lineCount("a")).toBe(1);
  expect(lineCount("a\nb\n")).toBe(2);
  expect(lineCount("a\n\nb")).toBe(3);
});

test("the result line reads as an outcome and its numbers", () => {
  expect(parseResult("success turns=24 cost=$0.4312 duration_ms=372000", false)).toEqual({
    title: "Done",
    meta: "24 turns · $0.43 · 6 m 12 s",
  });
  expect(parseResult("success turns=1 cost=$0.0040", false)).toEqual({ title: "Done", meta: "1 turn · $0.00" });
  expect(parseResult("error_during_execution turns=3 cost=$0.1000 duration_ms=0", true).title).toBe("Failed");
  expect(parseResult("error_max_turns turns=40 cost=$1.2000", true).title).toBe("Stopped");
  expect(parseResult("interrupted turns=2 cost=$0.0100", false).title).toBe("Stopped");
  expect(parseResult("", false)).toEqual({ title: "Done", meta: "" });
});

test("each tool reads as a verb and a target", () => {
  const wt = "/Users/dana/.athena/work/fix-login-t-0004/";
  expect(view("Bash", { command: "go test ./...\n-v", description: "Run the tests" })).toMatchObject({
    kind: "bash",
    label: "Run the tests",
    target: "go test ./...",
  });
  expect(view("Bash", { command: `cd ${wt} && make` })).toMatchObject({ label: "Ran a command", target: "cd . && make" });
  expect(view("Read", { file_path: wt + "api/handler.go" })).toMatchObject({ kind: "read", label: "Read", target: "api/handler.go" });
  expect(view("Read", { file_path: wt + "a.go", offset: 120, limit: 50 }).target).toBe("a.go:120-169");
  expect(view("Read", { file_path: wt + "a.go", offset: 120 }).target).toBe("a.go:120-…");
  expect(view("Write", { file_path: wt + "a.go", content: "a\nb\nc\n" })).toMatchObject({
    kind: "write",
    label: "Created",
    target: "a.go",
    counts: { add: 3, del: 0 },
  });
  expect(view("Write", { file_path: wt + "a.go", content: "a…", _lines: { content: 412 } }).counts).toEqual({ add: 412, del: 0 });
  expect(view("Edit", { file_path: wt + "a.go", old_string: "x\ny\nz", new_string: "x\nY\nY2\nz" })).toMatchObject({
    kind: "edit",
    label: "Edited",
    counts: { add: 2, del: 1 },
  });
  expect(
    view("MultiEdit", {
      file_path: wt + "a.go",
      edits: [
        { old_string: "a", new_string: "b" },
        { old_string: "c", new_string: "d\ne" },
      ],
    }).counts,
  ).toEqual({ add: 3, del: 2 });
  expect(view("NotebookEdit", { notebook_path: wt + "n.ipynb", new_source: "x" })).toMatchObject({ label: "Edited", target: "n.ipynb" });
  expect(view("Grep", { pattern: "func Items", path: wt + "internal" })).toMatchObject({
    kind: "search",
    label: "Searched for",
    target: "func Items in internal",
  });
  expect(view("Glob", { pattern: "**/*.go" })).toMatchObject({ label: "Found files", target: "**/*.go" });
  expect(view("WebFetch", { url: "https://go.dev/doc", prompt: "x" })).toMatchObject({ kind: "web", label: "Fetched", target: "https://go.dev/doc" });
  expect(view("WebSearch", { query: "vite lazy chunk" })).toMatchObject({ label: "Searched the web", target: "vite lazy chunk" });
  expect(
    view("TodoWrite", {
      todos: [
        { content: "a", status: "completed" },
        { content: "b", status: "in_progress" },
        { content: "c", status: "pending" },
      ],
    }),
  ).toMatchObject({ kind: "todo", label: "Updated the plan", target: "1 of 3 done" });
  expect(view("Agent", { description: "Review the handlers", prompt: "Read them" })).toMatchObject({
    kind: "agent",
    label: "Started a subagent",
    target: "Review the handlers",
  });
  expect(view("Task", { description: "Map the repo" }).kind).toBe("agent");
  expect(view("mcp__athena__report", { status: "ready", summary: "Shipped the API.\nTests pass.", pull: 12 })).toMatchObject({
    kind: "report",
    label: "Reported ready",
    target: "PR #12 · Shipped the API.",
  });
  expect(view("mcp__athena__ack", { text: "On it." })).toMatchObject({ kind: "ack", label: "Acknowledged", target: "On it." });
  expect(view("mcp__github__create_pull_request", { title: "x" }, "title=x")).toMatchObject({
    kind: "mcp",
    label: "create pull request",
    target: "title=x",
  });
  expect(view("ScheduleWakeup", { delaySeconds: 60 }, "delaySeconds=60")).toMatchObject({
    kind: "other",
    label: "ScheduleWakeup",
    target: "delaySeconds=60",
  });
  expect(view("ToolSearch", { query: "select:x" }).kind).toBe("hidden");
});

test("a detail that is not JSON reads as no input, and escapes are stripped", () => {
  const bad = item({ kind: "tool_call", tool: "Bash", detail: "{not json" });
  expect(toolInput(bad)).toEqual({});
  expect(toolInput(item({ kind: "tool_call", tool: "Bash", detail: "[1,2]" }))).toEqual({});
  const esc = call("Bash", { command: "\u001b[31mrm\u001b[0m -rf build", nested: [{ a: "\u001b[1mb" }] });
  expect(toolInput(esc)).toEqual({ command: "rm -rf build", nested: [{ a: "b" }] });
  expect(describeTool("Bash", {}, "ls").target).toBe("ls");
});

test("an edit reads as a diff with a little context", () => {
  expect(editDiff("a\nb\nc\nd\ne\nf\nOLD\ng", "a\nb\nc\nd\ne\nf\nNEW\nNEW2\ng")).toEqual([
    { sign: " ", text: "d" },
    { sign: " ", text: "e" },
    { sign: " ", text: "f" },
    { sign: "−", text: "OLD" },
    { sign: "+", text: "NEW" },
    { sign: "+", text: "NEW2" },
    { sign: " ", text: "g" },
  ]);
  expect(editDiff("", "x")).toEqual([{ sign: "+", text: "x" }]);
});

test("a plan reads as its todos", () => {
  expect(todoList({ todos: [{ content: "a", status: "completed" }, { content: 3 }, "junk", { content: "c", status: "in_progress" }] })).toEqual([
    { content: "a", status: "completed" },
    { content: "c", status: "in_progress" },
  ]);
  expect(todoList({})).toEqual([]);
});
