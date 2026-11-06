import { expect, test } from "vitest";
import { item } from "@/lib/stream/testkit";
import { describeTool, errorLine, toolInput } from "./tools";
import { codexToolView, exitStatusText } from "./tools.codex";

const WT = "/home/user/.athena/work/fix-parser-t-0009";
const view = (tool: string, input: object, text = "") => {
  const c = item({ kind: "tool_call", tool, toolUseId: "tu", text, detail: JSON.stringify(input) });
  return describeTool(c.tool, toolInput(c), text);
};

test("exec_command shows the command and exit status", () => {
  // Codex runs each command through a login shell: the row shows the command inside it, paths shortened.
  expect(view("exec_command", { command: `/bin/zsh -lc 'go test ${WT}/parser/...'`, cwd: WT })).toEqual({
    kind: "bash",
    label: "Ran",
    target: "go test parser/...",
  });
  expect(view("exec_command", { command: `bash -lc 'echo '"'"'hi'"'"' && ls'`, cwd: WT }).target).toBe("echo 'hi' && ls");
  expect(view("exec_command", { command: "/bin/zsh -lc 'printf '\\''a b'\\'''", cwd: WT }).target).toBe("printf 'a b'");
  // A command run elsewhere than the worktree's root says where.
  expect(view("exec_command", { command: "npm test", cwd: `${WT}/web` }).target).toBe("npm test in web");
  expect(view("exec_command", { command: "ls", cwd: "/home/user/src" }).target).toBe("ls in ~/src");
  expect(view("exec_command", {}, "make").target).toBe("make");

  // A failed command's line names its exit code, then its first line of output.
  const failed = item({ kind: "tool_result", toolUseId: "tu", isError: true, text: "cat: missing.txt: No such file or directory\n", detail: '{"exitCode":1}' });
  expect(errorLine(exitStatusText("exec_command", failed, failed.text))).toBe("Exit code 1: cat: missing.txt: No such file or directory");
  const silent = item({ kind: "tool_result", toolUseId: "tu", isError: true, text: "", detail: '{"exitCode":2}' });
  expect(errorLine(exitStatusText("exec_command", silent, ""))).toBe("Exit code 2");
  // Declined or failed without an exit code: as it is; other tools: as they are.
  const declined = item({ kind: "tool_result", toolUseId: "tu", isError: true, text: "declined" });
  expect(exitStatusText("exec_command", declined, "declined")).toBe("declined");
  expect(exitStatusText("Bash", failed, "boom")).toBe("boom");
});

test("apply_patch counts lines and names one or many files", () => {
  expect(
    view("apply_patch", { changes: [{ path: `${WT}/parser/lex.go`, kind: "update", diff: "@@ -1,3 +1,3 @@\n a\n-b\n+B\n+B2\n c\n" }] }),
  ).toMatchObject({ kind: "edit", label: "Edited", target: "parser/lex.go", counts: { add: 2, del: 1, about: false } });
  // Every change an add: Created; an add's diff may be the file's plain content.
  expect(
    view("apply_patch", {
      changes: [
        { path: `${WT}/notes.md`, kind: "add", diff: "+hello\n+world\n" },
        { path: `${WT}/todo.md`, kind: "add", diff: "one\ntwo\nthree\n" },
      ],
    }),
  ).toMatchObject({ kind: "write", label: "Created", target: "2 files", counts: { add: 5, del: 0, about: false } });
  const mixed = view("apply_patch", {
    changes: [
      { path: `${WT}/a.go`, kind: "add", diff: "+x\n" },
      { path: `${WT}/b.go`, kind: "delete", diff: "-y\n-z\n" },
      { path: `${WT}/c.go`, kind: "update", diff: "--- a/c.go\n+++ b/c.go\n-1\n+2\n" },
    ],
  });
  expect(mixed).toMatchObject({ kind: "edit", label: "Edited", target: "3 files", counts: { add: 2, del: 3, about: false } });
  expect(mixed.patch?.map((f) => [f.path, f.lines.map((l) => l.sign + l.text)])).toEqual([
    ["a.go", ["+x"]],
    ["b.go", ["−y", "−z"]],
    ["c.go", ["−1", "+2"]],
  ]);
  // A clipped diff: what is shown is counted, and the counts are about; a clipped add counts its full length.
  expect(
    view("apply_patch", {
      changes: [{ path: `${WT}/big.go`, kind: "update", diff: "-a\n+b\n+c…" }],
      _lines: { "changes.0.diff": 900 },
    }).counts,
  ).toEqual({ add: 2, del: 1, about: true });
  expect(
    view("apply_patch", { changes: [{ path: `${WT}/big.md`, kind: "add", diff: "+a\n+b…" }], _lines: { "changes.0.diff": 640 } }).counts,
  ).toEqual({ add: 640, del: 0, about: true });
  expect(view("apply_patch", { changes: [] }, "nothing").target).toBe("nothing");
});

test("web_search names its query", () => {
  expect(view("web_search", { query: "go json rpc" })).toEqual({ kind: "web", label: "Searched the web", target: "go json rpc" });
});

test("update_plan lists steps", () => {
  const plan = view("update_plan", {
    steps: [
      { step: "Read the parser", status: "completed" },
      { step: "Fix the lexer", status: "in_progress" },
      { step: "Run the tests", status: "pending" },
      { step: 3 },
    ],
  });
  expect(plan).toMatchObject({ kind: "todo", label: "Updated the plan", target: "1 of 3 done" });
  expect(plan.todos).toEqual([
    { content: "Read the parser", status: "completed" },
    { content: "Fix the lexer", status: "in_progress" },
    { content: "Run the tests", status: "pending" },
  ]);
  // A proposed plan (no statuses yet) is its steps.
  expect(view("update_plan", { steps: [{ step: "a" }, { step: "b" }] })).toMatchObject({ label: "Planned", target: "2 steps" });
  expect(view("update_plan", { steps: [{ step: "a" }] }).target).toBe("1 step");
});

test("athena's tools read as they do for Claude Code", () => {
  expect(view("mcp__athena__report", { status: "ready", summary: "Fixed the parser.\nTests pass." })).toMatchObject({
    kind: "report",
    label: "Reported ready",
    target: "Fixed the parser.",
  });
  expect(codexToolView("mcp__athena__report", {}, "")).toBeUndefined();
  expect(codexToolView("Bash", {}, "")).toBeUndefined();
});

test("a patch's lines follow its change: an add is all added, a delete all removed, an update its diff", () => {
  // A new Markdown file with bullets: every line added, the bullets kept as written.
  const doc = view("apply_patch", { changes: [{ path: `${WT}/NOTES.md`, kind: "add", diff: "# T\n- one\n- two\n" }] });
  expect(doc).toMatchObject({ kind: "write", label: "Created", target: "NOTES.md", counts: { add: 3, del: 0, about: false } });
  expect(doc.patch?.[0].lines.map((l) => l.sign + l.text)).toEqual(["+# T", "+- one", "+- two"]);
  // An add sent as a diff (every line signed) loses the signs once.
  expect(
    view("apply_patch", { changes: [{ path: `${WT}/a.md`, kind: "add", diff: "+- one\n+\n+two\n" }] }).patch?.[0].lines.map((l) => l.sign + l.text),
  ).toEqual(["+- one", "+", "+two"]);
  // A delete: every line removed, plain or signed.
  const gone = view("apply_patch", { changes: [{ path: `${WT}/old.md`, kind: "delete", diff: "+ kept?\nbye\n" }] });
  expect(gone).toMatchObject({ kind: "edit", label: "Edited", counts: { add: 0, del: 2, about: false } });
  expect(gone.patch?.[0].lines.map((l) => l.sign + l.text)).toEqual(["−+ kept?", "−bye"]);
  expect(
    view("apply_patch", { changes: [{ path: `${WT}/old.go`, kind: "delete", diff: "-a\n-b\n" }] }).patch?.[0].lines.map((l) => l.sign + l.text),
  ).toEqual(["−a", "−b"]);
  // A clipped delete counts its full length.
  expect(
    view("apply_patch", { changes: [{ path: `${WT}/big.md`, kind: "delete", diff: "a\nb…" }], _lines: { "changes.0.diff": 300 } }).counts,
  ).toEqual({ add: 0, del: 300, about: true });
  // In an update, --- and +++ are headers only before the first hunk: after it, a removed "-- comment" is a line.
  const sql = view("apply_patch", {
    changes: [{ path: `${WT}/q.sql`, kind: "update", diff: "--- a/q.sql\n+++ b/q.sql\n@@ -1,2 +1,2 @@\n--- old comment\n+-- new comment\n select 1;\n" }],
  });
  expect(sql.counts).toEqual({ add: 1, del: 1, about: false });
  expect(sql.patch?.[0].lines.map((l) => l.sign + l.text)).toEqual([" @@ -1,2 +1,2 @@", "−-- old comment", "+-- new comment", " select 1;"]);
});
