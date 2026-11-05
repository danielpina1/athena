import { expect, test } from "vitest";
import { fileHref, isFilePath, viewHref } from "./api";

test("a file's URL escapes each segment and keeps the slashes", () => {
  expect(fileHref("T-1", "docs/a b#1.txt")).toBe("/api/tickets/T-1/files/docs/a%20b%231.txt");
  expect(viewHref("T-1", "REPORT.md")).toBe("/api/tickets/T-1/files/REPORT.md?view=1");
});

test("a path that could leave the Ticket's files is refused, encoded or not", () => {
  for (const p of ["", "/etc/passwd", "../../auth/session", "../../T-0002/files/x.md", "a/../b", "a/./b", "a//b", "a/", ".", "..",
    "%2e%2e/x", "a/%2E%2E/b", "a/%2e/b", "a\u0000b", "..%2fx"]) {
    expect(isFilePath(p), JSON.stringify(p)).toBe(false);
    expect(() => fileHref("T-1", p), JSON.stringify(p)).toThrow();
    expect(() => viewHref("T-1", p), JSON.stringify(p)).toThrow();
  }
  for (const p of ["REPORT.md", "docs/REVIEW.md", ".gitignore", "a..b/c...d", "50%.txt"]) expect(isFilePath(p), p).toBe(true);
});
