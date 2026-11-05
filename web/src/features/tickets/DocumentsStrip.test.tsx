import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { expect, test } from "vitest";
import type { Artifact, TicketFiles } from "@/lib/artifacts/api";
import { DocumentsStrip } from "./DocumentsStrip";

const f = (path: string, modTime: string) => ({ path, size: 10, modTime });
const ready = (files: TicketFiles["files"]): Artifact<TicketFiles> => ({ status: "ready", data: { files, truncated: false } });

function show(files: Artifact<TicketFiles>) {
  return render(
    <MemoryRouter initialEntries={["/tickets/T-1?tab=session"]}>
      <DocumentsStrip files={files} />
    </MemoryRouter>,
  );
}

test("the Ticket's newest Markdown documents are one tap away, with all its files after them", () => {
  show(
    ready([
      f("a.md", "2026-11-05T09:00:00Z"),
      f("main.go", "2026-11-05T12:00:00Z"),
      f("docs/REVIEW.md", "2026-11-05T11:00:00Z"),
      f("REPORT.md", "2026-11-05T10:00:00Z"),
      f("old.md", "2026-11-04T10:00:00Z"),
    ]),
  );
  const strip = screen.getByRole("region", { name: "Documents" });
  const links = within(strip).getAllByRole("link");
  expect(links.map((l) => [l.textContent, l.getAttribute("href")])).toEqual([
    ["REVIEW.md", "/tickets/T-1?tab=session&file=docs%2FREVIEW.md"],
    ["REPORT.md", "/tickets/T-1?tab=session&file=REPORT.md"],
    ["a.md", "/tickets/T-1?tab=session&file=a.md"],
    ["All files (5)", "/tickets/T-1?tab=files"],
  ]);
  expect(links[0].getAttribute("title")).toBe("docs/REVIEW.md");
});

test("without Markdown documents, or before the listing, there is no strip", () => {
  const { unmount } = show(ready([f("main.go", "2026-11-05T09:00:00Z"), f("notes.txt", "2026-11-05T09:00:00Z")]));
  expect(screen.queryByRole("region", { name: "Documents" })).toBeNull();
  unmount();
  show({ status: "loading" });
  expect(screen.queryByRole("region", { name: "Documents" })).toBeNull();
});
