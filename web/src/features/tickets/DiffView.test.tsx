import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { bindSession } from "@/lib/auth/session";
import DiffView from "./DiffView";
import { hasClasses } from "./fixtures";

const PATCH = [
  "diff --git a/src/a.ts b/src/a.ts",
  "index 111..222 100644",
  "--- a/src/a.ts",
  "+++ b/src/a.ts",
  "@@ -1,3 +1,4 @@",
  " const a = 1;",
  "-const b = 2;",
  "+const b = 3;",
  `+const long = "${"y".repeat(400)}";`,
  " export {};",
  "diff --git a/notes.txt b/notes.txt",
  "new file mode 100644",
  "--- /dev/null",
  "+++ b/notes.txt",
  "@@ -0,0 +1 @@",
  "+\u001b[31mred\u001b[0m note",
  "diff --git a/logo.png b/logo.png",
  "new file mode 100644",
  "Binary files /dev/null and b/logo.png differ",
  "",
].join("\n");

const DIFF = {
  base: "1a2b3c4",
  branch: "athena/t-1",
  files: [
    { path: "src/a.ts", status: "M", added: 3, deleted: 1, binary: false },
    { path: "notes.txt", status: "A", added: 1, deleted: 0, binary: false },
    { path: "logo.png", status: "A", added: 0, deleted: 0, binary: true },
  ],
  patch: PATCH,
  truncated: false,
};

function respond(status: number, body: unknown) {
  vi.stubGlobal(
    "fetch",
    vi.fn(() => Promise.resolve(new Response(JSON.stringify(body), { status }))),
  );
}

beforeEach(() => bindSession({ outbox: { clear() {} } }));
afterEach(() => vi.unstubAllGlobals());

test("diff view renders numstat and wraps lines", async () => {
  respond(200, DIFF);
  render(<DiffView ticketId="T-1" />);

  const list = await screen.findByRole("list", { name: "Changed files" });
  const files = within(list).getAllByRole("listitem");
  expect(files).toHaveLength(3);
  expect((files[0]).textContent).toContain("src/a.ts");
  expect((files[0]).textContent).toContain("+3");
  expect((files[0]).textContent).toContain("−1");
  expect((files[0]).textContent).toContain("Modified");
  expect((files[1]).textContent).toContain("Added");
  expect((files[2]).textContent).toContain("Binary");
  expect(screen.getByText(/athena\/t-1/)).toBeTruthy();

  // The unified diff: markers, no ANSI, every line wraps instead of widening the page.
  const added = screen.getByText(/const b = 3;/).closest("[data-line]");
  expect(added?.getAttribute("data-line")).toBe("add");
  expect(screen.getByText(/const b = 2;/).closest("[data-line]")?.getAttribute("data-line")).toBe("del");
  const longLine = screen.getByText(new RegExp("y{400}"));
  expect(hasClasses(longLine, "whitespace-pre-wrap", "wrap-anywhere")).toBe(true);
  expect(hasClasses(longLine.closest("[data-diff-block]"), "overflow-x-auto", "max-w-full")).toBe(true);
  expect(document.body.textContent).not.toContain("\u001b");
  expect(screen.getByText(/red note/)).toBeTruthy();
  expect(screen.getByText("Binary file: no preview.")).toBeTruthy();
});

test("truncated diff shows the notice", async () => {
  respond(200, { ...DIFF, truncated: true });
  render(<DiffView ticketId="T-1" />);
  expect(await screen.findByText("This diff is too large to show in full: only the first part is shown.")).toBeTruthy();
});

test("an empty diff is a designed state", async () => {
  respond(200, { ...DIFF, files: [], patch: "" });
  render(<DiffView ticketId="T-1" />);
  expect(await screen.findByText("No changes yet.")).toBeTruthy();
});

test("a signed-out session shows sign-in", async () => {
  const onEnded = vi.fn();
  bindSession({ outbox: { clear() {} }, onEnded });
  respond(401, { code: "unauthenticated" });
  render(<DiffView ticketId="T-1" />);
  await waitFor(() => expect(onEnded).toHaveBeenCalledTimes(1));
  expect(await screen.findByText("You were signed out.")).toBeTruthy();
});

test("a failure can be retried", async () => {
  const user = userEvent.setup();
  respond(500, { code: "internal" });
  render(<DiffView ticketId="T-1" />);
  expect(await screen.findByText("The changes could not be loaded.")).toBeTruthy();
  respond(200, DIFF);
  await user.click(screen.getByRole("button", { name: "Retry" }));
  expect(await screen.findByRole("list", { name: "Changed files" })).toBeTruthy();
});

test("a Ticket without a branch yet is not found", async () => {
  respond(404, { code: "not_found" });
  render(<DiffView ticketId="T-1" />);
  expect(await screen.findByText("No changes to show.")).toBeTruthy();
});

test("a file with bidirectional controls shows them and says so", async () => {
  respond(200, {
    ...DIFF,
    files: [
      { path: "auth.js", status: "M", added: 1, deleted: 0, binary: false },
      { path: "plain.js", status: "M", added: 1, deleted: 0, binary: false },
    ],
    patch: [
      "diff --git a/auth.js b/auth.js",
      "--- a/auth.js",
      "+++ b/auth.js",
      "@@ -1 +1 @@",
      "+if (isAdmin\u202e) {",
      "diff --git a/plain.js b/plain.js",
      "--- a/plain.js",
      "+++ b/plain.js",
      "@@ -1 +1 @@",
      "+ok();",
      "",
    ].join("\n"),
  });
  render(<DiffView ticketId="T-1" />);
  const auth = await screen.findByRole("region", { name: "auth.js" });
  expect(within(auth).getByText("if (isAdmin⟪U+202E⟫) {")).toBeTruthy();
  const warning = within(auth).getByRole("note");
  expect(warning.textContent).toMatch(/text-direction/i);
  expect(within(screen.getByRole("region", { name: "plain.js" })).queryByRole("note")).toBeNull();
});
