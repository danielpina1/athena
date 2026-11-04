import { expect, test } from "vitest";
import { parsePatch } from "./diff";

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
