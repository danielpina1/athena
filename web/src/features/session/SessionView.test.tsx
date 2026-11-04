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
  expect(within(bash).getByText("Bash")).toBeTruthy();
  expect(within(bash).getByText("go test ./...")).toBeTruthy();
  expect(bash.getAttribute("data-status")).toBe("ok");
  expect(within(bash).getByText("ok · 1.2 s")).toBeTruthy();
  // The result is inside its call, not a row of its own.
  expect(screen.getAllByText("ok acme/api 0.2s")).toHaveLength(1);
  expect(bash.textContent).toContain("ok acme/api 0.2s");
  expect(read.getAttribute("data-status")).toBe("running");
  expect(within(read).getByText("running")).toBeTruthy();
  expect(edit.getAttribute("data-status")).toBe("error");
  expect(within(edit).getByText("error · 0.1 s")).toBeTruthy();
  // Collapsed until opened.
  const summary = within(bash).getByText("Bash").closest("summary")!;
  expect(bash.hasAttribute("open")).toBe(false);
  await userEvent.click(summary);
  expect(bash.hasAttribute("open")).toBe(true);
  // The order of the log is the order of the stream.
  const log = screen.getByRole("log");
  expect(log.textContent!.indexOf("Bash")).toBeLessThan(log.textContent!.indexOf("Checking the tests"));
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
      item({ offset: 0n, kind: "init", text: "session abc model opus" }),
      item({ offset: 10n, kind: "thinking", text: "Consider the handler first" }),
      item({ offset: 20n, kind: "say", text: "\u001b[1;32mAll green\u001b[0m " + long }),
      item({ offset: 30n, kind: "result", text: "success turns=3 cost=$0.0100\nShipped the API" }),
    ]),
  );
  expect(hasClasses(screen.getByText("session abc model opus"), "text-muted")).toBe(true);
  const thinking = screen.getByText("Thinking").closest("details")!;
  expect(thinking.hasAttribute("open")).toBe(false);
  expect(thinking.textContent).toContain("Consider the handler first");
  const say = screen.getByText(/All green/);
  expect(say.textContent).not.toContain("\u001b");
  expect(hasClasses(say, "whitespace-pre-wrap", "wrap-anywhere")).toBe(true);
  expect(screen.getByText("success turns=3 cost=$0.0100")).toBeTruthy();
  expect(screen.getByText("Shipped the API")).toBeTruthy();
  expect(hasClasses(screen.getByRole("log"), "font-mono", "text-sm")).toBe(true); // text-sm is 13 px
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
