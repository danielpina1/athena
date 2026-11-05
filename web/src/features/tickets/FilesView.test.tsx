import { render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { bindSession } from "@/lib/auth/session";
import { FilesView } from "./FilesView";

function respond(status: number, body: unknown) {
  vi.stubGlobal(
    "fetch",
    vi.fn(() => Promise.resolve(new Response(JSON.stringify(body), { status }))),
  );
}

beforeEach(() => bindSession({ outbox: { clear() {} } }));
afterEach(() => vi.unstubAllGlobals());

test("files view links downloads", async () => {
  respond(200, {
    files: [
      { path: "report.md", size: 2048, modTime: "2026-11-04T09:00:00Z" },
      { path: "docs/a b#1.txt", size: 12, modTime: "2026-11-04T09:00:00Z" },
    ],
    truncated: false,
  });
  render(<FilesView ticketId="T-1" />);
  const list = await screen.findByRole("list", { name: "Files" });
  const [report, odd] = within(list).getAllByRole("listitem");
  const link = within(report).getByRole("link", { name: /report\.md/ });
  expect((link).getAttribute("href")).toBe("/api/tickets/T-1/files/report.md");
  expect((link).getAttribute("download")).toBe("report.md");
  expect((report).textContent).toContain("2.0 KB");
  // Each path segment is escaped, the slashes are not.
  expect((within(odd).getByRole("link")).getAttribute("href")).toBe("/api/tickets/T-1/files/docs/a%20b%231.txt");
  expect((odd).textContent).toContain("12 B");
});

test("a capped listing says so", async () => {
  respond(200, { files: [{ path: "a.txt", size: 1, modTime: "2026-11-04T09:00:00Z" }], truncated: true });
  render(<FilesView ticketId="T-1" />);
  expect(await screen.findByText("This listing is incomplete: only the first files found are shown.")).toBeTruthy();
});

test("no files is a designed state", async () => {
  respond(200, { files: [], truncated: false });
  render(<FilesView ticketId="T-1" />);
  expect(await screen.findByText("No files yet.")).toBeTruthy();
});

test("an unknown Ticket is said plainly", async () => {
  respond(404, { code: "not_found" });
  render(<FilesView ticketId="T-1" />);
  expect(await screen.findByText("No files to show.")).toBeTruthy();
});

test("a file name with bidirectional controls shows them and says so", async () => {
  respond(200, {
    files: [
      { path: "invoice\u202efdp.exe", size: 10, modTime: "2026-11-04T09:00:00Z" },
      { path: "plain.txt", size: 10, modTime: "2026-11-04T09:00:00Z" },
    ],
    truncated: false,
  });
  render(<FilesView ticketId="T-1" />);
  const [odd, plain] = within(await screen.findByRole("list", { name: "Files" })).getAllByRole("listitem");
  const link = within(odd).getByRole("link");
  expect(link.textContent).toBe("invoice⟪U+202E⟫fdp.exe");
  expect(link.getAttribute("download")).toBe("invoice⟪U+202E⟫fdp.exe");
  expect(link.getAttribute("href")).toBe("/api/tickets/T-1/files/invoice%E2%80%AEfdp.exe");
  expect(within(odd).getByRole("note").textContent).toMatch(/text-direction/i);
  expect(within(plain).queryByRole("note")).toBeNull();
});
