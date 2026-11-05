import { expect, test } from "vitest";
import { parsePatch, unquotePath } from "./diff";

test("splits files and classifies lines", () => {
  const files = parsePatch(
    [
      "diff --git a/a.ts b/a.ts",
      "--- a/a.ts",
      "+++ b/a.ts",
      "@@ -1,2 +1,2 @@",
      " keep",
      "--- not a header",
      "+++ not a header",
      "\\ No newline at end of file",
      "diff --git a/old.ts b/new.ts",
      "similarity index 90%",
      "rename from old.ts",
      "rename to new.ts",
      "diff --git a/x.bin b/x.bin",
      "Binary files a/x.bin and b/x.bin differ",
      "",
    ].join("\n"),
  );
  expect(files.map((f) => f.path)).toEqual(["a.ts", "new.ts", "x.bin"]);
  expect(files[0].lines.map((l) => l.kind)).toEqual(["hunk", "ctx", "del", "add", "note"]);
  expect(files[0].lines[2].text).toBe("-- not a header");
  expect(files[2].binary).toBe(true);
});

test("an empty patch has no files", () => {
  expect(parsePatch("")).toEqual([]);
});

test("decodes git's quoted paths", () => {
  expect(unquotePath('"caf\\303\\251.txt"')).toBe("café.txt");
  expect(unquotePath('"a\\tb\\"c\\\\d"')).toBe('a\tb"c\\d');
  expect(unquotePath("plain.txt")).toBe("plain.txt");
});

test("sections of quoted paths keep their names", () => {
  const files = parsePatch(
    [
      'diff --git "a/caf\\303\\251.txt" "b/caf\\303\\251.txt"',
      "new file mode 100644",
      "--- /dev/null",
      '+++ "b/caf\\303\\251.txt"',
      "@@ -0,0 +1 @@",
      "+x",
      'diff --git "a/tab\\there \\"q\\".md" "b/tab\\there \\"q\\".md"',
      "Binary files a/x and b/x differ",
      'diff --git a/old name.md "b/new\\tname.md"',
      'rename to "new\\tname.md"',
    ].join("\n"),
  );
  expect(files.map((f) => f.path)).toEqual(["café.txt", 'tab\there "q".md', "new\tname.md"]);
  expect(files[0].lines.map((l) => l.kind)).toEqual(["hunk", "add"]);
});

test("bidirectional controls in a file are shown as markers and flag that file only", () => {
  const files = parsePatch(
    [
      "diff --git a/auth.js b/auth.js",
      "--- a/auth.js",
      "+++ b/auth.js",
      "@@ -1 +1 @@",
      '+if (role !== "user\u202e \u2066// admin\u2069 \u2066") {',
      "diff --git a/plain.js b/plain.js",
      "--- a/plain.js",
      "+++ b/plain.js",
      "@@ -1 +1 @@",
      "+ok();",
      'diff --git "a/x\\342\\200\\256gpj.exe" "b/x\\342\\200\\256gpj.exe"',
      "new file mode 100644",
      "--- /dev/null",
      '+++ "b/x\\342\\200\\256gpj.exe"',
      "@@ -0,0 +1 @@",
      "+x",
      "",
    ].join("\n"),
  );
  expect(files.map((f) => f.bidi)).toEqual([true, false, true]);
  expect(files[0].lines[1].text).toBe('if (role !== "user⟪U+202E⟫ ⟪U+2066⟫// admin⟪U+2069⟫ ⟪U+2066⟫") {');
  // A name git quoted (octal UTF-8) is marked once decoded.
  expect(files[2].path).toBe("x⟪U+202E⟫gpj.exe");
});
