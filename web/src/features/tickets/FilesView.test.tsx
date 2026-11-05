import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router";
import { expect, test } from "vitest";
import { type Artifact, ArtifactError, type TicketFiles } from "@/lib/artifacts/api";
import { FilesView } from "./FilesView";

const ready = (files: TicketFiles["files"], truncated = false): Artifact<TicketFiles> => ({ status: "ready", data: { files, truncated } });
const f = (path: string, size = 10, status?: string) => ({ path, size, status, modTime: "2026-11-04T09:00:00Z" });

function Where() {
  const l = useLocation();
  return <output data-testid="where">{l.pathname + l.search}</output>;
}

function show(files: Artifact<TicketFiles>, repository = false) {
  return render(
    <MemoryRouter initialEntries={["/tickets/T-1?tab=files"]}>
      <FilesView ticketId="T-1" files={files} repository={repository} />
      <Where />
    </MemoryRouter>,
  );
}

test("each file opens in the viewer, and keeps its own download link", async () => {
  show(ready([f("report.md", 2048), f("docs/a b#1.txt", 12)]));
  const list = screen.getByRole("list", { name: "Documents" });
  const [report, odd] = within(list).getAllByRole("listitem");
  const open = within(report).getByRole("link", { name: "report.md" });
  expect(open.getAttribute("href")).toBe("/tickets/T-1?tab=files&file=report.md");
  expect(report.textContent).toContain("2.0 KB");
  const download = within(report).getByRole("link", { name: "Download report.md" });
  expect(download.getAttribute("href")).toBe("/api/tickets/T-1/files/report.md");
  expect(download.getAttribute("download")).toBe("report.md");
  // Each path segment is escaped, the slashes are not.
  expect(within(odd).getByRole("link", { name: "Download docs/a b#1.txt" }).getAttribute("href")).toBe("/api/tickets/T-1/files/docs/a%20b%231.txt");
  await userEvent.click(open);
  expect(screen.getByTestId("where").textContent).toBe("/tickets/T-1?tab=files&file=report.md");
});

test("documents come first, then the other files", () => {
  show(ready([f("main.go"), f("REPORT.md"), f("shot.png"), f("go.mod")]));
  const docs = screen.getByRole("list", { name: "Documents" });
  const other = screen.getByRole("list", { name: "Other files" });
  expect(within(docs).getAllByRole("listitem").map((li) => li.querySelector("a")?.textContent)).toEqual(["REPORT.md", "shot.png"]);
  expect(within(other).getAllByRole("listitem").map((li) => li.querySelector("a")?.textContent)).toEqual(["main.go", "go.mod"]);
});

test("a repository Ticket's files say how the worktree has them", () => {
  show(ready([f("REPORT.md", 10, "?"), f("docs/REVIEW.md", 10, "A"), f("README", 10, "M")]), true);
  const docs = screen.getByRole("list", { name: "Documents" });
  expect(within(docs).getAllByRole("listitem")[0].textContent).toContain("New");
  expect(within(docs).getAllByRole("listitem")[1].textContent).toContain("Added");
  expect(within(screen.getByRole("list", { name: "Other files" })).getByRole("listitem").textContent).toContain("Changed");
});

test("a capped listing says so", () => {
  show(ready([f("a.txt")], true));
  expect(screen.getByText("This listing is incomplete: only the first files found are shown.")).toBeTruthy();
});

test("no files is a designed state, for either kind of Ticket", () => {
  const { unmount } = show(ready([]));
  expect(screen.getByText("No files yet.")).toBeTruthy();
  unmount();
  show(ready([]), true);
  expect(screen.getByText("No new or changed files yet.")).toBeTruthy();
});

test("an unknown Ticket is said plainly", () => {
  show({ status: "error", error: new ArtifactError("not_found", 404), retry() {} });
  expect(screen.getByText("No files to show.")).toBeTruthy();
});

test("a file name with bidirectional controls shows them and says so", () => {
  show(ready([f("invoice‮fdp.exe"), f("plain.txt")]));
  const odd = within(screen.getByRole("list", { name: "Other files" })).getByRole("listitem");
  const link = within(odd).getAllByRole("link")[0];
  expect(link.textContent).toBe("invoice⟪U+202E⟫fdp.exe");
  const download = within(odd).getByRole("link", { name: /^Download/ });
  expect(download.getAttribute("download")).toBe("invoice⟪U+202E⟫fdp.exe");
  expect(download.getAttribute("href")).toBe("/api/tickets/T-1/files/invoice%E2%80%AEfdp.exe");
  expect(within(odd).getByRole("note").textContent).toMatch(/text-direction/i);
  expect(within(screen.getByRole("list", { name: "Documents" })).queryByRole("note")).toBeNull();
});
