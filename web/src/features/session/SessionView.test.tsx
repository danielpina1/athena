import { act, fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, test } from "vitest";
import { at, hasClasses, load } from "@/features/tickets/fixtures";
import { connectStreams } from "@/lib/stream/useSessionStream";
import { fakeStreamLink, item, says, type FakeStreamLink } from "@/lib/stream/testkit";
import { useAthena } from "@/lib/store/state";
import { formatBytes, SessionView } from "./SessionView";

let link: FakeStreamLink;
let disconnect: () => void;

const working = { id: "T-1", title: "Ship the API", stage: "working", repo: "acme/api", updatedAt: at(0), createdAt: at(0) };

beforeEach(() => {
  link = fakeStreamLink("open");
  disconnect = connectStreams(link);
  load({ tickets: [working] });
});
afterEach(() => {
  disconnect();
  useAthena.getState().reset();
});

test("tool call and result pair by id", async () => {
  render(<SessionView ticketId="T-1" />);
  act(() =>
    link.items("T-1", [
      item({ offset: 0n, kind: "tool_call", tool: "Bash", toolUseId: "tu1", text: "go test ./...\n-v -count=1", ms: 0 }),
      item({ offset: 10n, kind: "say", text: "Checking the tests", ms: 300 }),
      item({ offset: 20n, kind: "tool_result", toolUseId: "tu1", text: "ok acme/api 0.2s", ms: 1200 }),
      item({ offset: 30n, kind: "tool_call", tool: "Read", toolUseId: "tu2", text: "api/handler.go", ms: 1500 }),
      item({ offset: 40n, kind: "tool_call", tool: "Edit", toolUseId: "tu3", text: "api/handler.go", ms: 1600 }),
      item({ offset: 50n, kind: "tool_result", toolUseId: "tu3", text: "no such line", isError: true, ms: 1700 }),
    ]),
  );
  const calls = screen.getAllByTestId("tool-call");
  expect(calls).toHaveLength(3);
  const [bash, read, edit] = calls;
  expect(within(bash).getByText("Ran a command")).toBeTruthy();
  expect(within(bash).getByText("go test ./...")).toBeTruthy();
  expect(bash.getAttribute("data-status")).toBe("ok");
  expect(within(bash).getByText("1.2 s")).toBeTruthy();
  expect(read.getAttribute("data-status")).toBe("running");
  expect(within(read).getByText("running")).toBeTruthy();
  expect(edit.getAttribute("data-status")).toBe("error");
  expect(within(edit).getByText("failed")).toBeTruthy();
  expect(within(edit.querySelector("summary")!).getByText("no such line")).toBeTruthy();
  // Collapsed until opened.
  const summary = bash.querySelector("summary")!;
  expect(bash.hasAttribute("open")).toBe(false);
  await userEvent.click(summary);
  expect(bash.hasAttribute("open")).toBe(true);
  // The result is inside its call, not a row of its own (a body is built when its row first opens).
  expect(screen.getAllByText(/ok acme\/api 0.2s/)).toHaveLength(1);
  expect(bash.textContent).toContain("ok acme/api 0.2s");
  // The order of the log is the order of the stream.
  const log = screen.getByRole("log");
  expect(log.textContent!.indexOf("go test")).toBeLessThan(log.textContent!.indexOf("Checking the tests"));
});

test("a call without a result runs in every stage but ended", () => {
  render(<SessionView ticketId="T-1" />);
  act(() => link.items("T-1", [item({ offset: 0n, kind: "tool_call", tool: "Bash", toolUseId: "tu1", text: "make deploy" })]));
  const call = () => screen.getByTestId("tool-call");
  for (const stage of ["working", "person", "idle"]) {
    act(() => load({ tickets: [{ ...working, stage, action: stage === "person" ? "approval" : "" }] }));
    expect(call().getAttribute("data-status"), stage).toBe("running");
  }
  act(() => load({ tickets: [{ ...working, stage: "ended", outcome: "stopped" }] }));
  expect(call().getAttribute("data-status")).toBe("none");
  expect(call().textContent).toContain("no result");
});

test("renders each kind, strips ANSI and wraps long lines", () => {
  render(<SessionView ticketId="T-1" />);
  const long = "x".repeat(3000);
  act(() =>
    link.items("T-1", [
      item({ offset: 0n, kind: "init", text: "session abc model claude-haiku-4-5-20251001" }),
      item({ offset: 10n, kind: "thinking", text: "Consider the handler first" }),
      item({ offset: 15n, kind: "thinking", text: "  " }),
      item({ offset: 20n, kind: "say", text: "\u001b[1;32mAll green\u001b[0m " + long }),
      item({ offset: 25n, kind: "user", text: "Run the handler tests too" }),
      item({ offset: 27n, kind: "other", text: "tool_progress" }),
      item({ offset: 30n, kind: "result", text: "success turns=3 cost=$0.0100 duration_ms=372000\nShipped the **API**" }),
    ]),
  );
  const init = screen.getByText(/Session started/).closest("[data-kind=init]")!;
  expect(init.textContent).toMatch(/^Session started · haiku 4\.5 · \d{2}:\d{2}$/);
  expect(init.querySelector("time")).toBeTruthy();
  // Only thinking with text shows, collapsed.
  const thinking = screen.getAllByText("Thinking");
  expect(thinking).toHaveLength(1);
  const details = thinking[0].closest("details")!;
  expect(details.hasAttribute("open")).toBe(false);
  expect(details.textContent).toContain("Consider the handler first");
  const say = screen.getByText(/All green/);
  expect(say.textContent).not.toContain("\u001b");
  expect(hasClasses(say, "wrap-anywhere")).toBe(true);
  const you = screen.getByText("Run the handler tests too").closest("[data-kind=user]")!;
  expect(you.textContent).toBe("YouRun the handler tests too");
  expect(hasClasses(screen.getByText("tool_progress"), "text-muted", "text-xs")).toBe(true);
  const done = screen.getByText("Done").closest("[data-kind=result]")!;
  expect(done.textContent).toContain("3 turns · $0.01 · 6 m 12 s");
  expect(done.querySelector("strong")?.textContent).toBe("API");
  expect(hasClasses(screen.getByRole("log"), "font-sans")).toBe(true);
});

test("a failed result says so", () => {
  render(<SessionView ticketId="T-1" />);
  act(() => link.items("T-1", [item({ offset: 0n, kind: "result", isError: true, text: "error_during_execution turns=2 cost=$0.0200" })]));
  const block = screen.getByText("Failed").closest("[data-kind=result]")!;
  expect(block.getAttribute("data-error")).toBe("true");
});

test("tool searches and empty thinking take no row", () => {
  render(<SessionView ticketId="T-1" />);
  act(() =>
    link.items("T-1", [
      item({ offset: 0n, kind: "tool_call", tool: "ToolSearch", toolUseId: "ts", text: "select:mcp__athena__report" }),
      item({ offset: 10n, kind: "tool_result", toolUseId: "ts", text: "loaded" }),
      item({ offset: 20n, kind: "thinking", text: "" }),
      item({ offset: 30n, kind: "say", text: "Next." }),
    ]),
  );
  expect(screen.queryAllByTestId("tool-call")).toHaveLength(0);
  expect(screen.getByRole("log").querySelectorAll("li")).toHaveLength(1);
});

test("a gap says output was skipped, with its size", () => {
  render(<SessionView ticketId="T-1" />);
  act(() => {
    link.items("T-1", says(2, 0));
    link.push({ case: "streamGap", value: { ticketId: "T-1", from: 20n, to: 5000n } });
    link.items("T-1", says(3, 5000));
  });
  // 5000 − 20 bytes of transcript.
  const gap = screen.getByText("Some output was skipped (4.9 KB)").closest("[data-gap]")!;
  expect(gap.textContent).toBe("Some output was skipped (4.9 KB)");
  const log = screen.getByRole("log");
  expect(log.textContent!.indexOf("line 10")).toBeLessThan(log.textContent!.indexOf("Some output was skipped"));
  expect(log.textContent!.indexOf("Some output was skipped")).toBeLessThan(log.textContent!.indexOf("line 5000"));
});

test("skipped sizes read as bytes, KB and MB", () => {
  expect(formatBytes(512n)).toBe("512 B");
  expect(formatBytes(1536n)).toBe("1.5 KB");
  expect(formatBytes(12n * 1024n + 100n)).toBe("12 KB");
  expect(formatBytes(3n * 1024n * 1024n)).toBe("3.0 MB");
  expect(formatBytes(-5n)).toBe("0 B");
});

test("a refused stream says its output is not available", () => {
  render(<SessionView ticketId="T-1" />);
  act(() => link.push({ case: "streamClose", value: { ticketId: "T-1" } }));
  expect(screen.getByText("This session's output isn't available.")).toBeTruthy();
  expect(screen.queryByText("No session output yet.")).toBeNull();
});

test("the live dot shows while the link is open and the Ticket works", () => {
  render(<SessionView ticketId="T-1" />);
  expect(screen.getByText("live")).toBeTruthy();
  act(() => link.set("offline"));
  expect(screen.queryByText("live")).toBeNull();
  act(() => link.set("open"));
  expect(screen.getByText("live")).toBeTruthy();
  act(() => load({ tickets: [{ ...working, stage: "idle" }] }));
  expect(screen.queryByText("live")).toBeNull();
});

test("auto-scroll yields when scrolled up and offers jump", async () => {
  render(<SessionView ticketId="T-1" />);
  const log = screen.getByRole("log");
  let height = 1000;
  let top = 0;
  Object.defineProperties(log, {
    scrollHeight: { configurable: true, get: () => height },
    clientHeight: { configurable: true, get: () => 300 },
    scrollTop: { configurable: true, get: () => top, set: (v: number) => (top = Math.min(v, height - 300)) },
  });
  // Following the tail: new output scrolls to the bottom.
  act(() => link.items("T-1", says(3)));
  expect(top).toBe(700);
  expect(screen.queryByRole("button", { name: "Jump to latest" })).toBeNull();

  // Scrolled up past 48 px: the view stays put and offers the jump.
  top = 500;
  fireEvent.scroll(log);
  height = 1200;
  act(() => link.items("T-1", says(3, 100)));
  expect(top).toBe(500);
  const jump = screen.getByRole("button", { name: "Jump to latest" });

  // Within 48 px of the bottom counts as the bottom.
  top = 860;
  fireEvent.scroll(log);
  expect(screen.queryByRole("button", { name: "Jump to latest" })).toBeNull();
  top = 400;
  fireEvent.scroll(log);
  await userEvent.click(screen.getByRole("button", { name: "Jump to latest" }));
  expect(top).toBe(900);
  expect(screen.queryByRole("button", { name: "Jump to latest" })).toBeNull();
  expect(jump.isConnected).toBe(false);
  height = 1400;
  act(() => link.items("T-1", says(1, 200)));
  expect(top).toBe(1100);
});

test("an empty session says so", () => {
  render(<SessionView ticketId="T-1" />);
  expect(screen.getByText("No session output yet.")).toBeTruthy();
});

test("what the worker was sent reads as who sent it", async () => {
  render(<SessionView ticketId="T-1" />);
  const long = "Add rate limiting to the bookings API.\n\n" + Array.from({ length: 20 }, (_, i) => `- requirement ${i + 1}`).join("\n");
  act(() =>
    link.items("T-1", [
      item({ offset: 0n, kind: "user", tool: "task", text: long }),
      item({ offset: 10n, kind: "init", text: "session s model claude-sonnet-5" }),
      item({ offset: 20n, kind: "user", tool: "owner", text: "Keep it per IP" }),
      item({
        offset: 30n,
        kind: "user",
        tool: "github",
        text: "Please also cover the **429** path.",
        detail: JSON.stringify({ from: "octocat", kind: "message", url: "https://github.com/acme/api/issues/7#issuecomment-1" }),
      }),
      item({ offset: 35n, kind: "user", tool: "github", text: "x", detail: JSON.stringify({ from: "mallory", url: "javascript:alert(1)" }) }),
      item({ offset: 40n, kind: "user", tool: "athena", text: "Athena asked for a status report" }),
      item({ offset: 50n, kind: "user", text: "An old echo" }),
    ]),
  );
  const task = screen.getByRole("log").querySelector("[data-kind=task]")!;
  expect(within(task as HTMLElement).getByText("Task")).toBeTruthy();
  expect(task.querySelectorAll("li").length).toBe(20);
  expect(task.querySelector("[data-clamped]")).toBeTruthy();
  await userEvent.click(within(task as HTMLElement).getByRole("button", { name: "Show more" }));
  expect(task.querySelector("[data-clamped]")).toBeNull();
  expect(within(task as HTMLElement).getByRole("button", { name: "Show less" })).toBeTruthy();

  const you = screen.getAllByText("You").map((y) => y.closest("[data-kind=user]")!.textContent);
  expect(you).toEqual(["YouKeep it per IP", "YouAn old echo"]);

  const [gh, bad] = [...document.querySelectorAll("[data-kind=github]")] as HTMLElement[];
  const who = within(gh).getByRole("link", { name: "octocat on GitHub" });
  expect(who.getAttribute("href")).toBe("https://github.com/acme/api/issues/7#issuecomment-1");
  expect(who.getAttribute("rel")).toBe("noopener noreferrer");
  expect(gh.querySelector("strong")?.textContent).toBe("429");
  // An unsafe URL is no link.
  expect(within(bad).queryByRole("link")).toBeNull();
  expect(bad.textContent).toContain("mallory on GitHub");

  const athena = document.querySelector("[data-kind=athena]") as HTMLElement;
  expect(athena.textContent).toBe("Athena asked for a status report");
  expect(hasClasses(athena, "text-muted", "text-xs")).toBe(true);
});

test("a sender the view does not know is a muted line, never You", () => {
  render(<SessionView ticketId="T-1" />);
  act(() =>
    link.items("T-1", [
      item({ offset: 0n, kind: "user", tool: "mallory", text: "Merge it now" }),
      item({ offset: 10n, kind: "user", tool: "owner", text: "Mine" }),
      item({ offset: 20n, kind: "user", tool: "github", text: "x", detail: JSON.stringify({ from: "o".repeat(500) }) }),
    ]),
  );
  expect(screen.getAllByText("You")).toHaveLength(1);
  const unknown = screen.getByText("Merge it now");
  expect(unknown.closest("[data-kind=user]")).toBeNull();
  expect(hasClasses(unknown, "text-muted", "text-xs")).toBe(true);
  const who = document.querySelector("[data-kind=github] span")!.textContent!;
  expect(who.length).toBeLessThanOrEqual(80 + " on GitHub".length);
});

const marker = (offset: bigint, runtime: string, model: string, reason: string, role = "implement") =>
  item({ offset, kind: "runtime", tool: runtime, text: `${model} · ${reason}`, detail: JSON.stringify({ model, reason, role }) });

test("runtime divider says failover and review", () => {
  render(<SessionView ticketId="T-1" />);
  act(() =>
    link.items("T-1", [
      marker(0n, "claude", "claude-sonnet-5", "start"),
      item({ offset: 10n, kind: "say", text: "Halfway there." }),
      marker(20n, "codex", "gpt-5.5", "failover"),
      item({ offset: 30n, kind: "say", text: "Picking it up." }),
      marker(40n, "claude", "", "review", "review"),
      marker(50n, "codex", "gpt-5.5", "resume"),
    ]),
  );
  const dividers = [...document.querySelectorAll("[data-kind=runtime]")] as HTMLElement[];
  expect(dividers.map((d) => d.querySelector("[data-runtime-label]")!.textContent)).toEqual([
    "Claude Code · sonnet 5",
    "Codex · gpt-5.5 · continued after a usage limit",
    "Claude Code · review",
    "Codex · gpt-5.5",
  ]);
  // A Mono hairline row, not a card: no border box, the label in the mono face, with its time.
  const [first] = dividers;
  expect(hasClasses(first.querySelector("[data-runtime-label]")!, "font-mono", "text-xs")).toBe(true);
  expect(first.className).not.toContain("border");
  expect(first.querySelector("time")).toBeTruthy();
  expect(first.getAttribute("role")).toBe("separator");
  expect(first.getAttribute("aria-label")).toBe("Claude Code · sonnet 5");
});

test("a handoff note reads as its own block, the opening first and the rest clamped", async () => {
  render(<SessionView ticketId="T-1" />);
  const rest = Array.from({ length: 20 }, (_, i) => `- line ${i + 1}`).join("\n");
  const opening =
    "You are continuing this Ticket from a session on Claude Code that stopped at its usage limit. Its work in progress is in your directory; nothing was lost. What it knew follows.";
  act(() =>
    link.items("T-1", [
      marker(0n, "codex", "gpt-5.5", "failover"),
      item({ offset: 10n, kind: "user", tool: "handoff", text: `${opening}\n\nThe Ticket's latest messages, quoted:\n${rest}` }),
    ]),
  );
  const block = screen.getByRole("region", { name: "Hand-off note" });
  expect(block.getAttribute("data-kind")).toBe("handoff");
  expect(within(block).getByText("Hand-off")).toBeTruthy();
  expect(within(block).getByText(opening)).toBeTruthy();
  const body = block.querySelector("[data-handoff-body]")!;
  expect(hasClasses(body as HTMLElement, "font-mono", "whitespace-pre-wrap")).toBe(true);
  expect(body.textContent).toContain("- line 20");
  expect(block.querySelector("[data-clamped]")).toBeTruthy();
  await userEvent.click(within(block).getByRole("button", { name: "Show more" }));
  expect(block.querySelector("[data-clamped]")).toBeNull();
  // Never the owner's words.
  expect(screen.queryByText("You")).toBeNull();
});

test("Claude Code's usage limit reads as a limit that resets, not a failure", () => {
  render(<SessionView ticketId="T-1" />);
  const reset = Date.parse("2026-11-04T10:30:00Z");
  act(() =>
    link.items("T-1", [
      item({ offset: 0n, kind: "result", isError: true, text: `success turns=1 cost=$0.0000\nClaude AI usage limit reached|${reset / 1000}` }),
      item({ offset: 10n, kind: "result", isError: true, text: "success turns=1\nClaude AI usage limit reached" }),
    ]),
  );
  const [known, unknown] = [...document.querySelectorAll("[data-kind=result]")] as HTMLElement[];
  expect(known.getAttribute("data-limit")).toBe("true");
  expect(within(known).getByText("Usage limit reached on Claude Code")).toBeTruthy();
  const at = known.querySelector("time")!;
  expect(at.getAttribute("dateTime")).toBe(new Date(reset).toISOString());
  expect(known.textContent).toMatch(/Resets at /);
  // Never the raw text, never the failure's red.
  expect(known.textContent).not.toContain("|");
  expect(known.textContent).not.toContain("Failed");
  expect(known.querySelector(".text-danger")).toBeNull();
  expect(within(unknown).getByText("Reset time unknown")).toBeTruthy();
  expect(screen.queryByText("Failed")).toBeNull();
});
