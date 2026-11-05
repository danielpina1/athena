import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { type Artifact, type TicketFiles } from "@/lib/artifacts/api";
import { bindSession } from "@/lib/auth/session";
import DocViewer, { LAZY_SENTINEL } from "./DocViewer";

const f = (path: string, size = 100, status?: string) => ({ path, size, status, modTime: "2026-11-04T09:00:00Z" });
const ready = (...files: TicketFiles["files"]): Artifact<TicketFiles> => ({ status: "ready", data: { files, truncated: false } });

/** fetch answering the view route with body (and the cut headers). */
function serve(body: string, opts: { size?: number; truncated?: boolean; status?: number } = {}) {
  const fetch = vi.fn((url: string) => {
    void url;
    const headers = new Headers({ "Content-Type": "text/plain; charset=utf-8", "X-Athena-Size": String(opts.size ?? body.length) });
    if (opts.truncated) headers.set("X-Athena-Truncated", "true");
    return Promise.resolve(new Response(body, { status: opts.status ?? 200, headers }));
  });
  vi.stubGlobal("fetch", fetch);
  return fetch;
}

beforeEach(() => bindSession({ outbox: { clear() {} } }));
afterEach(() => vi.unstubAllGlobals());

function open(file: string, files: Artifact<TicketFiles>, extra: { onClose?: () => void; onResolve?: (p: string) => void; dir?: string } = {}) {
  return render(
    <MemoryRouter initialEntries={[`/tickets/T-1?file=${encodeURIComponent(file)}`]}>
      <DocViewer
        ticketId="T-1"
        file={file}
        files={files}
        dir={extra.dir ?? ""}
        onClose={extra.onClose ?? (() => {})}
        onResolve={extra.onResolve ?? (() => {})}
      />
    </MemoryRouter>,
  );
}

const dialog = () => screen.getByRole("dialog");

test("the viewer is a lazy chunk with its own sentinel", () => {
  expect(LAZY_SENTINEL).toBe("athena-lazy-chunk:doc-viewer");
});

test("a Markdown report renders as a document, with its source a toggle away", async () => {
  const fetch = serve("# Report\n\nAll **good**.\n\n| a | b |\n| - | - |\n| 1 | 2 |\n\n- [x] tests\n- [ ] deploy\n");
  open("docs/REPORT.md", ready(f("docs/REPORT.md", 2048, "?")));
  expect(fetch).toHaveBeenCalledWith("/api/tickets/T-1/files/docs/REPORT.md?view=1", expect.anything());
  const title = within(dialog()).getByRole("heading", { level: 2, name: "REPORT.md" });
  // Focus moves to the heading when the viewer opens.
  await waitFor(() => expect(document.activeElement).toBe(title));
  expect(dialog().textContent).toContain("docs/");
  expect(dialog().textContent).toContain("2.0 KB");
  expect(await within(dialog()).findByRole("heading", { level: 3, name: "Report" })).toBeTruthy();
  expect(within(dialog()).getByRole("table")).toBeTruthy();
  expect(dialog().querySelectorAll("[data-task]")).toHaveLength(2);
  const download = within(dialog()).getByRole("link", { name: "Download" });
  expect(download.getAttribute("href")).toBe("/api/tickets/T-1/files/docs/REPORT.md");
  expect(download.getAttribute("download")).toBe("REPORT.md");

  await userEvent.click(within(dialog()).getByRole("radio", { name: "Source" }));
  const code = within(dialog()).getByRole("region", { name: "Source of REPORT.md" });
  expect(code.querySelector("[data-line-number='1']")?.textContent).toBe("1");
  expect(code.textContent).toContain("All **good**.");
  expect(within(dialog()).queryByRole("table")).toBeNull();
});

test("code shows numbered lines in mono, wrapping, with no highlighter", async () => {
  serve("package main\n\nfunc main() {}\n");
  open("main.go", ready(f("main.go")));
  const code = await within(dialog()).findByRole("region", { name: "Source of main.go" });
  const numbers = [...code.querySelectorAll("[data-line-number]")].map((n) => n.textContent);
  expect(numbers).toEqual(["1", "2", "3"]);
  expect(code.className).toContain("font-mono");
  expect(code.textContent).toContain("func main() {}");
});

test("JSON is pretty-printed, and small CSV is a table", async () => {
  serve('{"a":1}');
  const { unmount } = open("data.json", ready(f("data.json")));
  const code = await within(dialog()).findByRole("region", { name: "Source of data.json" });
  expect(code.textContent).toContain('"a": 1');
  unmount();
  serve('name,count\n"Smith, J",3\n');
  open("t.csv", ready(f("t.csv")));
  const table = await within(dialog()).findByRole("table");
  expect([...table.querySelectorAll("th")].map((c) => c.textContent)).toEqual(["name", "count"]);
  expect([...table.querySelectorAll("td")].map((c) => c.textContent)).toEqual(["Smith, J", "3"]);
});

test("SVG and HTML are shown as their source, never rendered", async () => {
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"><script>alert(2)</script></svg>`;
  serve(svg);
  const { unmount } = open("logo.svg", ready(f("logo.svg")));
  const code = await within(dialog()).findByRole("region", { name: "Source of logo.svg" });
  expect(code.textContent).toContain("<script>alert(2)</script>");
  expect(within(dialog()).getByText(/never rendered here/)).toBeTruthy();
  expect(document.querySelector("dialog svg[onload], dialog script, dialog iframe, dialog object, dialog embed")).toBeNull();
  unmount();
  serve("<!doctype html><h1>Hi</h1><script>alert(1)</script>");
  open("page.html", ready(f("page.html")));
  await within(dialog()).findByRole("region", { name: "Source of page.html" });
  expect(document.querySelector("dialog h1, dialog script, dialog iframe")).toBeNull();
});

test("an image is shown inline from the view route", async () => {
  const fetch = serve("");
  open("shot.png", ready(f("shot.png", 5000)));
  const img = within(dialog()).getByRole("img", { name: "shot.png" });
  expect(img.getAttribute("src")).toBe("/api/tickets/T-1/files/shot.png?view=1");
  expect(fetch).not.toHaveBeenCalled();
});

test("a file past the view cap shows its start and offers the whole file", async () => {
  serve("line\n".repeat(1000), { size: 3 * 1024 * 1024, truncated: true });
  open("big.log", ready(f("big.log", 3 * 1024 * 1024)));
  const note = await within(dialog()).findByRole("note");
  expect(note.textContent).toBe("Showing the first 1 MB. Download the whole file (3.0 MB).");
  expect(within(note).getByRole("link").getAttribute("href")).toBe("/api/tickets/T-1/files/big.log");
});

test("an image too big to view, a PDF and a binary file are offered as downloads", async () => {
  const fetch = serve("");
  const { unmount } = open("huge.png", ready(f("huge.png", 9 * 1024 * 1024)));
  expect(within(dialog()).queryByRole("img")).toBeNull();
  expect(dialog().textContent).toContain("too large to show here");
  unmount();
  const second = open("spec.pdf", ready(f("spec.pdf", 4096)));
  expect(dialog().textContent).toContain("PDF files open with Download.");
  expect(fetch).not.toHaveBeenCalled();
  second.unmount();
  serve("PK\u0003\u0004\u0000\u0000\u0000");
  open("blob.bin", ready(f("blob.bin", 7)));
  expect(await within(dialog()).findByText("No preview for this file.")).toBeTruthy();
  expect(within(dialog()).getAllByRole("link", { name: "Download" }).length).toBeGreaterThan(0);
});

test("a session's absolute path opens the listed file it names", async () => {
  serve("# R");
  const onResolve = vi.fn();
  open("/Users/dana/.athena/work/fix-t-1/REPORT.md", ready(f("REPORT.md")), { onResolve });
  await waitFor(() => expect(onResolve).toHaveBeenCalledWith("REPORT.md"));
});

test("a path that is not among the Ticket's files says so", () => {
  serve("");
  open("/tmp/elsewhere.md", ready(f("REPORT.md")));
  expect(dialog().textContent).toContain("This file is not among the Ticket's files.");
  expect(within(dialog()).getByRole("link", { name: "All files" }).getAttribute("href")).toBe("/tickets/T-1?tab=files");
});

test("a file that is gone says so", async () => {
  serve(JSON.stringify({ code: "not_found" }), { status: 404 });
  open("REPORT.md", ready(f("REPORT.md")));
  expect(await within(dialog()).findByText("This file is not there any more.")).toBeTruthy();
});

test("Escape and Close close the viewer", async () => {
  serve("text");
  const onClose = vi.fn();
  open("notes.txt", ready(f("notes.txt")), { onClose });
  fireEvent(dialog(), new Event("cancel", { cancelable: true }));
  expect(onClose).toHaveBeenCalledTimes(1);
  await userEvent.click(within(dialog()).getAllByRole("button", { name: /^(Close|Back)$/ })[0]);
  expect(onClose).toHaveBeenCalledTimes(2);
});
