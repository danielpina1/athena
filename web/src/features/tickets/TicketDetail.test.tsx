import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useParams } from "react-router";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import type { CommandResult } from "@/gen/athena/v1/commands_pb";
import * as actions from "@/lib/commands/actions";
import { useAthena } from "@/lib/store/state";
import { Toaster } from "@/ui/primitives/toast";
import { at, detail, hasClasses, load } from "./fixtures";
import { TicketDetail } from "./TicketDetail";

vi.mock("@/lib/commands/actions", async (orig) => ({
  ...(await orig<typeof import("@/lib/commands/actions")>()),
  watch: vi.fn(),
  unwatch: vi.fn(),
  onWatchLost: vi.fn(),
  reply: vi.fn(),
  stop: vi.fn(),
  retry: vi.fn(),
  interrupt: vi.fn(),
  answerApproval: vi.fn(),
}));

const ok = { ok: true, code: "" } as CommandResult;
const watch = vi.mocked(actions.watch);
const unwatch = vi.mocked(actions.unwatch);
const reply = vi.mocked(actions.reply);
const onWatchLost = vi.mocked(actions.onWatchLost);
let lostCb: ((id: string) => void) | undefined;

const working = { id: "T-1", title: "Ship the API", stage: "working", repo: "acme/api", branch: "athena/t-1", model: "opus", runtime: "claude", updatedAt: at(5000), createdAt: at(0) };

beforeEach(() => {
  watch.mockResolvedValue(ok);
  unwatch.mockResolvedValue(ok);
  reply.mockResolvedValue(ok);
  onWatchLost.mockImplementation((cb) => {
    lostCb = cb;
    return () => {};
  });
  vi.stubGlobal("fetch", vi.fn(() => new Promise(() => {})));
});
afterEach(() => {
  vi.clearAllMocks();
  vi.unstubAllGlobals();
  useAthena.getState().reset();
});

function renderDetail(path = "/tickets/T-1") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="tickets/:id" element={<Detail />} />
      </Routes>
      <Toaster />
    </MemoryRouter>,
  );
}
function Detail() {
  return <TicketDetail id={useParams().id ?? ""} />;
}

test("detail watches and unwatches", async () => {
  load({ tickets: [working] });
  const view = renderDetail();
  await waitFor(() => expect(watch).toHaveBeenCalledWith("T-1"));
  expect(unwatch).not.toHaveBeenCalled();
  view.unmount();
  await waitFor(() => expect(unwatch).toHaveBeenCalledWith("T-1"));
});

test("trimmed timeline notice", async () => {
  load({ tickets: [working] });
  detail({
    summary: working,
    timelineTrimmed: true,
    timeline: [{ kind: "report", text: "Halfway there", at: at(1000) }],
  });
  renderDetail();
  expect(await screen.findByText("Older entries are hidden")).toBeTruthy();
  expect(screen.getByText("Halfway there")).toBeTruthy();
});

test("interrupt only while working", () => {
  load({
    tickets: [
      working,
      { id: "T-2", title: "Idle one", stage: "idle", repo: "acme/api", canRetry: true, updatedAt: at(0) },
      { id: "T-3", title: "Ended one", stage: "ended", outcome: "merged", updatedAt: at(0) },
    ],
  });
  const view = renderDetail("/tickets/T-1");
  expect(screen.getByRole("button", { name: "Interrupt" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Reply" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Stop" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  view.unmount();

  const idle = renderDetail("/tickets/T-2");
  expect(screen.queryByRole("button", { name: "Interrupt" })).toBeNull();
  expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  idle.unmount();

  renderDetail("/tickets/T-3");
  expect(screen.queryByRole("button", { name: "Interrupt" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Stop" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Reply" })).toBeNull();
});

test("long and ANSI content stays inside the column", async () => {
  const title = "T".repeat(2000);
  const ansi = "\u001b[31mred error\u001b[0m \u001b[2K" + "z".repeat(1500);
  load({ tickets: [{ ...working, title, repo: "r".repeat(300) + "/api", prUrl: "https://github.com/acme/api/pull/1" }] });
  detail({
    summary: { ...working, title },
    threads: ["github:" + "t".repeat(400)],
    timeline: [
      { kind: "report", text: ansi, at: at(1000) },
      { kind: "input", text: "שלום עולם ".repeat(200), at: at(2000) },
    ],
  });
  const view = renderDetail();

  const column = (await screen.findByTestId("ticket-detail")) as HTMLElement;
  // The timeline is a lazy chunk.
  await screen.findAllByTestId("timeline-row");
  expect(hasClasses(column, "min-w-0", "max-w-full")).toBe(true);
  const heading = screen.getByRole("heading", { level: 1 });
  expect((heading).textContent).toContain(title);
  expect(hasClasses(heading, "wrap-anywhere")).toBe(true);
  expect((heading).getAttribute("dir")).toBe("auto");
  expect(view.container.textContent).not.toContain("\u001b");
  expect(view.container.textContent).toContain("red error");

  // Every block of long text wraps anywhere, and nothing in the column is sized to its content or a fixed width.
  for (const el of column.querySelectorAll("p, dd, h1, a")) {
    if ((el.textContent ?? "").length < 200) continue;
    expect(el.className, `${el.tagName} ${el.textContent?.slice(0, 20)}`).toMatch(/wrap-anywhere|break-all|truncate/);
  }
  for (const el of column.querySelectorAll("*")) {
    expect(el.className.toString()).not.toMatch(/(^|\s)(w-max|min-w-max|w-\[\d{3,})/);
  }
  expect((screen.getByText(/שלום עולם/)).getAttribute("dir")).toBe("auto");
});

test("the pending approval is inline at the top", async () => {
  load({
    tickets: [{ ...working, stage: "person", action: "approval", pendingApprovalId: "A-1" }],
    approvals: [{ id: "A-1", ticketId: "T-1", tool: "Bash", summary: "Run make deploy", state: "pending", createdAt: at(0) }],
  });
  renderDetail();
  const card = screen.getByRole("group", { name: /Ship the API/ });
  expect(within(card).getByText("Run make deploy")).toBeTruthy();
  expect(within(card).getByRole("button", { name: /Approve/ })).toBeTruthy();
  // The Ticket's own header is above it: the card does not repeat the title.
  expect(within(card).queryByRole("heading")).toBeNull();
});

test("the pending question is inline and answers", async () => {
  const user = userEvent.setup();
  vi.mocked(actions.reply).mockResolvedValue(ok);
  load({ tickets: [{ ...working, stage: "person", action: "answer", attentionQuestion: "Postgres or SQLite?" }] });
  renderDetail();
  expect(screen.getByText("Postgres or SQLite?")).toBeTruthy();
  await user.type(screen.getByRole("textbox", { name: "Your answer" }), "SQLite{Enter}");
  expect(reply).toHaveBeenCalledWith("T-1", "SQLite");
});

test("facts list what the Ticket is", () => {
  load({
    tickets: [
      {
        ...working,
        stage: "ended",
        outcome: "merged",
        endedAt: at(9000),
        prUrl: "https://github.com/acme/api/pull/7",
      },
    ],
  });
  detail({ summary: working, threads: ["github:acme/api#7"] });
  renderDetail();
  const facts = screen.getByRole("region", { name: "Facts" });
  for (const text of ["acme/api", "athena/t-1", "opus", "Claude Code", "Merged"]) expect(within(facts).getByText(text)).toBeTruthy();
  expect((within(facts).getByRole("link", { name: /pull\/7/ })).getAttribute("href")).toBe("https://github.com/acme/api/pull/7");
  expect(within(facts).getByRole("link", { name: "github:acme/api#7" }).getAttribute("href")).toBe(
    "/routing?tab=threads&thread=github%3Aacme%2Fapi%237",
  );
});

test("a pull request link that is not GitHub is not linked", () => {
  load({ tickets: [{ ...working, prUrl: "javascript:alert(1)" }] });
  renderDetail();
  expect(screen.queryByRole("link", { name: /javascript/ })).toBeNull();
});

test("tabs follow the Ticket: Changes and Files with a branch, Files without a repository", async () => {
  load({
    tickets: [
      working,
      { id: "T-2", title: "Scratch work", stage: "working", dir: "/work/x", updatedAt: at(0) },
      { id: "T-3", title: "Repo not placed yet", stage: "working", repo: "acme/api", updatedAt: at(0) },
    ],
  });
  const a = renderDetail("/tickets/T-1");
  expect(screen.getByRole("tab", { name: "Session" })).toBeTruthy();
  expect(screen.getByRole("tab", { name: "Timeline" })).toBeTruthy();
  expect(screen.getByRole("tab", { name: "Changes" })).toBeTruthy();
  expect(screen.getByRole("tab", { name: "Files" })).toBeTruthy();
  a.unmount();
  const b = renderDetail("/tickets/T-2");
  expect(screen.getByRole("tab", { name: "Files" })).toBeTruthy();
  expect(screen.queryByRole("tab", { name: "Changes" })).toBeNull();
  b.unmount();
  renderDetail("/tickets/T-3");
  expect(screen.queryByRole("tab", { name: "Files" })).toBeNull();
  expect(screen.queryByRole("tab", { name: "Changes" })).toBeNull();
});

test("the Changes tab loads the diff lazily", async () => {
  const user = userEvent.setup();
  load({ tickets: [working] });
  const fetchMock = vi.fn(() =>
    Promise.resolve(
      new Response(JSON.stringify({ base: "abc", branch: "athena/t-1", files: [], patch: "", truncated: false }), { status: 200 }),
    ),
  );
  vi.stubGlobal("fetch", fetchMock);
  renderDetail();
  expect(fetchMock).not.toHaveBeenCalledWith("/api/tickets/T-1/diff", expect.anything());
  await user.click(screen.getByRole("tab", { name: "Changes" }));
  expect(await screen.findByText("No changes yet.")).toBeTruthy();
  expect(fetchMock).toHaveBeenCalledWith("/api/tickets/T-1/diff", expect.anything());
});

test("the Session tab shows the live session and its composer", async () => {
  renderDetail("/tickets/T-1?tab=session");
  load({ tickets: [working] });
  expect(await screen.findByRole("heading", { name: "Session", level: 2 })).toBeTruthy();
  expect(screen.getByRole("log", { name: "Session output" })).toBeTruthy();
  expect(screen.getByRole("textbox", { name: "Reply to the session" })).toBeTruthy();
});

test("an unknown Ticket is not found, a loading store is a skeleton", async () => {
  const view = renderDetail("/tickets/T-404");
  expect((screen.getByTestId("ticket-detail")).getAttribute("aria-busy")).toBe("true");
  act(() => load({ tickets: [working] }));
  expect(await screen.findByText("Ticket not found")).toBeTruthy();
  expect((screen.getByRole("link", { name: "Back to Tickets" })).getAttribute("href")).toBe("/tickets");
  view.unmount();
});

test("lost live details are said, and can resume", async () => {
  const user = userEvent.setup();
  load({ tickets: [working] });
  renderDetail();
  await waitFor(() => expect(watch).toHaveBeenCalled());
  act(() => lostCb?.("T-1"));
  expect(screen.getByText("Live details stopped for this Ticket.")).toBeTruthy();
  watch.mockClear();
  await user.click(screen.getByRole("button", { name: "Resume" }));
  expect(watch).toHaveBeenCalledWith("T-1");
  expect(screen.queryByText("Live details stopped for this Ticket.")).toBeNull();
});

test("reply from the header focuses the composer", async () => {
  const user = userEvent.setup();
  load({ tickets: [working] });
  renderDetail();
  expect(screen.queryByRole("textbox", { name: "Reply to the session" })).toBeNull();
  await user.click(screen.getByRole("button", { name: "Reply" }));
  const box = await screen.findByRole("textbox", { name: "Reply to the session" });
  expect(screen.getByRole("tab", { name: "Session" }).getAttribute("aria-selected")).toBe("true");
  await waitFor(() => expect(document.activeElement).toBe(box));
  await user.keyboard("Use SQLite{Enter}");
  expect(reply).toHaveBeenCalledWith("T-1", "Use SQLite");
  await waitFor(() => expect((box as HTMLTextAreaElement).value).toBe(""));

  // r does the same from anywhere outside a field.
  await user.click(screen.getByRole("tab", { name: "Timeline" }));
  expect(screen.queryByRole("textbox", { name: "Reply to the session" })).toBeNull();
  await user.keyboard("r");
  await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("textbox", { name: "Reply to the session" })));
});

test("the Report is above the tabs: View report brings it into view, and its question's Reply opens the composer", async () => {
  const user = userEvent.setup();
  load({ tickets: [{ ...working, stage: "idle" }] });
  detail({
    summary: { ...working, stage: "idle" },
    timeline: [{ kind: "report", subkind: "waiting", text: "Two caches fit", refId: "as1", at: at(1000) }],
    reports: [
      { assignmentId: "as0", state: "failed", summary: "first try", at: at(500) },
      { assignmentId: "as1", state: "waiting", summary: "Two caches fit\n\n- Redis\n- in-process", question: "Which one?", at: at(1000) },
    ],
  });
  renderDetail("/tickets/T-1?tab=timeline");
  const card = await screen.findByRole("region", { name: "Report" });
  expect(within(card).getByText("Waiting on you")).toBeTruthy();
  expect(within(card).getAllByRole("listitem")).toHaveLength(2);
  // The card comes before the tabs.
  expect(card.compareDocumentPosition(screen.getByRole("tablist")) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();

  const row = (await screen.findAllByTestId("timeline-row"))[0];
  await user.click(within(row).getByRole("button", { name: "View report" }));
  expect(document.activeElement).toBe(card);

  await user.click(within(within(card).getByRole("note", { name: "Question" })).getByRole("button", { name: "Reply" }));
  const box = await screen.findByRole("textbox", { name: "Reply to the session" });
  await waitFor(() => expect(document.activeElement).toBe(box));
});

/** fetch answering the files listing with files, and any file viewed with its path as text. */
function serveFiles(files: { path: string; size?: number; status?: string; modTime?: string }[]) {
  const fetchMock = vi.fn((url: string) => {
    if (url.endsWith("/files")) {
      const body = { files: files.map((f) => ({ size: 10, modTime: "2026-11-05T09:00:00Z", ...f })), truncated: false };
      return Promise.resolve(new Response(JSON.stringify(body), { status: 200 }));
    }
    return Promise.resolve(new Response(`# ${decodeURIComponent(url)}`, { status: 200, headers: { "X-Athena-Size": "10" } }));
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

test("the Ticket's Markdown documents are a strip above the tabs, read from its files once", async () => {
  load({ tickets: [working] });
  const fetchMock = serveFiles([{ path: "REPORT.md", status: "?" }, { path: "main.go", status: "A" }]);
  renderDetail("/tickets/T-1?tab=timeline");
  const strip = await screen.findByRole("region", { name: "Documents" });
  expect(within(strip).getByRole("link", { name: "REPORT.md" }).getAttribute("href")).toBe("/tickets/T-1?tab=timeline&file=REPORT.md");
  await userEvent.click(screen.getByRole("tab", { name: "Files" }));
  expect(await screen.findByRole("list", { name: "Documents" })).toBeTruthy();
  // The strip and the tab share one read of the listing.
  expect(fetchMock.mock.calls.filter(([u]) => u === "/api/tickets/T-1/files")).toHaveLength(1);
});

test("?file= opens the viewer over the Ticket, and closing it gives focus back to the file", async () => {
  load({ tickets: [working] });
  serveFiles([{ path: "REPORT.md", status: "?" }]);
  renderDetail("/tickets/T-1?tab=files");
  const list = await screen.findByRole("list", { name: "Documents" });
  const row = within(list).getByRole("link", { name: "REPORT.md" });
  row.focus();
  await userEvent.click(row);
  const viewer = await screen.findByRole("dialog");
  expect(await within(viewer).findByRole("heading", { level: 2, name: "REPORT.md" })).toBeTruthy();
  expect(await within(viewer).findByRole("heading", { level: 3 })).toBeTruthy();
  await userEvent.click(within(viewer).getAllByRole("button", { name: /^(Close|Back)$/ })[0]);
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(document.activeElement).toBe(within(list).getByRole("link", { name: "REPORT.md" }));
});
