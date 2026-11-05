import { expect, test } from "vitest";
import { documentsFirst, fileKind, isDocument, isMarkdown, resolveFile } from "./files";

const f = (path: string, modTime = "2026-11-05T09:00:00Z") => ({ path, size: 1, modTime });

test("a file's kind comes from its extension, in any letter case", () => {
  expect(fileKind("REPORT.md")).toBe("markdown");
  expect(fileKind("notes.MARKDOWN")).toBe("markdown");
  expect(fileKind("main.go")).toBe("code");
  expect(fileKind("a/b/config.yml")).toBe("code");
  expect(fileKind("out.log")).toBe("code");
  expect(fileKind("Makefile")).toBe("code");
  expect(fileKind("data.json")).toBe("json");
  expect(fileKind("table.csv")).toBe("csv");
  expect(fileKind("shot.PNG")).toBe("image");
  expect(fileKind("a.jpeg")).toBe("image");
  expect(fileKind("a.webp")).toBe("image");
  // Never rendered: shown as their source.
  expect(fileKind("logo.svg")).toBe("source");
  expect(fileKind("page.html")).toBe("source");
  expect(fileKind("page.HTM")).toBe("source");
  expect(fileKind("spec.pdf")).toBe("pdf");
  expect(fileKind("archive.tar.gz")).toBe("other");
  expect(fileKind("blob")).toBe("other");
});

test("documents are prose, tables, PDFs and images", () => {
  for (const p of ["REPORT.md", "a.markdown", "notes.txt", "spec.pdf", "t.csv", "shot.png", "x.gif"]) expect(isDocument(p), p).toBe(true);
  for (const p of ["main.go", "logo.svg", "page.html", "data.json", "blob"]) expect(isDocument(p), p).toBe(false);
  expect(isMarkdown("docs/REVIEW.md")).toBe(true);
  expect(isMarkdown("notes.txt")).toBe(false);
});

test("documents come first, each group in its own order", () => {
  const files = [f("b.go"), f("z.md"), f("a.go"), f("shot.png"), f("notes.txt")];
  expect(documentsFirst(files).map((x) => x.path)).toEqual(["z.md", "shot.png", "notes.txt", "b.go", "a.go"]);
});

test("a session's absolute path finds its file in the listing", () => {
  const files = [f("REPORT.md"), f("docs/REPORT.md"), f("docs/REVIEW.md")];
  // A listed path is itself.
  expect(resolveFile("docs/REVIEW.md", files, "")).toBe("docs/REVIEW.md");
  // In a worktree: the part after .athena/work/<slug>/.
  expect(resolveFile("/Users/dana/.athena/work/fix-t-0004/docs/REPORT.md", files, "")).toBe("docs/REPORT.md");
  // In the Ticket's own directory.
  expect(resolveFile("/srv/notes/REPORT.md", files, "/srv/notes")).toBe("REPORT.md");
  expect(resolveFile("/srv/notes/REPORT.md", files, "/srv/notes/")).toBe("REPORT.md");
  // Elsewhere: the longest listed path it ends with.
  expect(resolveFile("/private/var/w/docs/REPORT.md", files, "/var/w")).toBe("docs/REPORT.md");
  expect(resolveFile("/tmp/other.md", files, "")).toBeUndefined();
  expect(resolveFile("missing.md", files, "")).toBeUndefined();
});
