import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, test } from "vitest";
import { item } from "@/lib/stream/testkit";
import { ToolCall } from "./ToolCall";

const WT = "/Users/dana/.athena/work/fix-login-t-0004/";
const call = (tool: string, input: object, ms = 0) =>
  item({ kind: "tool_call", tool, toolUseId: "tu", text: "summary", detail: JSON.stringify(input), ms });
const ok = (text: string, ms = 1200) => item({ kind: "tool_result", toolUseId: "tu", text, ms });
const failed = (text: string) => item({ kind: "tool_result", toolUseId: "tu", text, isError: true, ms: 300 });

function row(c: ReturnType<typeof call> | undefined, r?: ReturnType<typeof ok>, active = true) {
  render(<ToolCall call={c} result={r} active={active} />);
  const el = screen.getByTestId("tool-call");
  return { el, summary: el.querySelector("summary")! };
}

test("a Bash row says what it does, and opens to the command and its output", async () => {
  const { el, summary } = row(call("Bash", { command: `cd ${WT} && go test ./...\n-count=1`, description: "Run the tests" }), ok("ok  acme/api 0.2s"));
  expect(el.tagName).toBe("DETAILS");
  expect(el.hasAttribute("open")).toBe(false);
  expect(el.getAttribute("data-status")).toBe("ok");
  expect(el.getAttribute("data-tool")).toBe("bash");
  expect(within(summary).getByText("Run the tests")).toBeTruthy();
  const target = within(summary).getByText("cd . && go test ./...");
  expect(target.className).toContain("font-mono");
  expect(target.className).toContain("truncate");
  expect(within(summary).getByText("1.2 s")).toBeTruthy();
  expect(summary.textContent).toContain("Succeeded");
  await userEvent.click(summary);
  expect(el.hasAttribute("open")).toBe(true);
  const term = within(el).getByLabelText("Terminal");
  expect(term.textContent).toBe(`$ cd ${WT} && go test ./...\n-count=1\nok  acme/api 0.2s`);
});

test("an errored row shows its first line inline, in red", () => {
  const { el, summary } = row(
    call("Bash", { command: "make deploy" }),
    failed("PreToolUse:Bash hook error: the session ended before the owner answered; find another way"),
  );
  expect(el.getAttribute("data-status")).toBe("error");
  expect(within(summary).getByText("Ran a command")).toBeTruthy();
  expect(within(summary).getByText("failed")).toBeTruthy();
  const line = within(summary).getByText("Blocked: the session ended before the owner answered; find another way");
  expect(line.className).toContain("text-danger");
});

test("an Edit row counts its lines and opens to a diff", async () => {
  const { el, summary } = row(call("Edit", { file_path: WT + "api/handler.go", old_string: "a\nold\nb", new_string: "a\nnew\nnewer\nb" }), ok("updated"));
  expect(within(summary).getByText("Edited")).toBeTruthy();
  expect(within(summary).getByText("api/handler.go")).toBeTruthy();
  // The counts replace the duration.
  expect(summary.textContent).toContain("+2");
  expect(summary.textContent).toContain("−1");
  expect(summary.textContent).not.toContain("1.2 s");
  await userEvent.click(summary);
  const diff = within(el).getByLabelText("Changes");
  const lines = [...diff.querySelectorAll("[data-line]")].map((l) => `${l.getAttribute("data-line")}${l.textContent}`);
  expect(lines).toEqual(["ctx a", "del−old", "add+new", "add+newer", "ctx b"]);
});

test("a Write row counts the file's lines and shows its start", async () => {
  const content = Array.from({ length: 30 }, (_, i) => `line ${i + 1}`).join("\n") + "\n";
  const { el, summary } = row(call("Write", { file_path: WT + "README.md", content }), ok("File created"));
  expect(within(summary).getByText("Created")).toBeTruthy();
  expect(summary.textContent).toContain("+30");
  await userEvent.click(summary);
  const body = within(el).getByLabelText("Content");
  expect(body.textContent).toContain("line 20");
  expect(body.textContent).not.toContain("line 21");
  expect(el.textContent).toContain("10 more lines");
});

test("a TodoWrite row is a checklist", async () => {
  const todos = [
    { content: "Write the tests", status: "completed", activeForm: "Writing" },
    { content: "Make them pass", status: "in_progress" },
    { content: "Ship", status: "pending" },
  ];
  const { el, summary } = row(call("TodoWrite", { todos }), ok("Todos have been modified"));
  expect(within(summary).getByText("Updated the plan")).toBeTruthy();
  expect(within(summary).getByText("1 of 3 done")).toBeTruthy();
  await userEvent.click(summary);
  const items = within(el).getByRole("list", { name: "Plan" }).querySelectorAll("li");
  expect([...items].map((li) => li.getAttribute("data-todo"))).toEqual(["completed", "in_progress", "pending"]);
  expect(items[0].textContent).toContain("Write the tests");
  expect(el.textContent).not.toContain("Todos have been modified");
});

test("a report row shows its state, its PR and its summary as markdown", async () => {
  const { el, summary } = row(
    call("mcp__athena__report", { status: "ready", summary: "Shipped the API.\n\n- tests pass\n- docs updated", pull: 7 }),
    ok("Report filed (ready)."),
  );
  expect(within(summary).getByText("Reported ready")).toBeTruthy();
  expect(within(summary).getByText("PR #7 · Shipped the API.")).toBeTruthy();
  await userEvent.click(summary);
  expect([...el.querySelectorAll("li")].map((li) => li.textContent)).toEqual(["tests pass", "docs updated"]);
});

test("a Read row opens to the file's text in a mono block", async () => {
  const { el, summary } = row(call("Read", { file_path: WT + "go.mod", offset: 1, limit: 20 }), ok("     1\tmodule acme"));
  expect(within(summary).getByText("go.mod:1-20")).toBeTruthy();
  await userEvent.click(summary);
  const out = within(el).getByLabelText("Output");
  expect(out.textContent).toBe("     1\tmodule acme");
  expect(out.className).toContain("font-mono");
});

test("an unknown tool shows its input and result", async () => {
  const { el, summary } = row(call("ScheduleWakeup", { delaySeconds: 1200, reason: "wait" }), ok("scheduled"));
  expect(within(summary).getByText("ScheduleWakeup")).toBeTruthy();
  await userEvent.click(summary);
  const input = within(el).getByLabelText("Input");
  expect(input.textContent).toContain("delaySeconds");
  expect(input.textContent).toContain("1200");
  expect(el.textContent).toContain("scheduled");
});

test("a call still running pulses; one with no result after the end says so", () => {
  const { el, summary } = row(call("Bash", { command: "sleep 9" }));
  expect(el.getAttribute("data-status")).toBe("running");
  expect(within(summary).getByText("running")).toBeTruthy();
  expect(summary.querySelector(".animate-pulse")).toBeTruthy();
});

test("a result without its call still shows", () => {
  const { el } = row(undefined, ok("orphan output"));
  expect(el.getAttribute("data-status")).toBe("ok");
  expect(el.textContent).toContain("Tool");
});

test("a 1 MiB result and RTL text stay inside the row", async () => {
  const huge = "‮RTL " + "x".repeat(1 << 20);
  const { el, summary } = row(call("Bash", { command: "cat big", description: "مرحبا long description " + "y".repeat(500) }), ok(huge));
  await userEvent.click(summary);
  const out = within(el).getByLabelText("Terminal");
  expect(out.className).toContain("wrap-anywhere");
  expect(out.className).toContain("overflow-y-auto");
  expect(out.textContent).not.toContain("‮");
});
