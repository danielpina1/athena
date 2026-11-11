import { readFileSync } from "node:fs";
import { join } from "node:path";
import { expect, test } from "vitest";
import { LAZY_SENTINEL as SESSION } from "./SessionView";
import { LAZY_SENTINEL as DIFF } from "@/features/tickets/DiffView";
import { LAZY_SENTINEL as DOCS } from "@/features/tickets/DocViewer";
import { LAZY_SENTINEL as SPLIT } from "@/features/split/BreakdownEditor";

// The bundle budget (scripts/bundle-budget.mjs) fails when a lazy chunk's sentinel is in the initial route: each lazy
// view carries one, and the script looks for the shared prefix, not for UI copy that may change.
test("each lazy view carries a sentinel the bundle budget looks for", () => {
  const script = readFileSync(join(import.meta.dirname, "../../../scripts/bundle-budget.mjs"), "utf8");
  for (const s of [SESSION, DIFF, DOCS, SPLIT]) expect(s.startsWith("athena-lazy-chunk:")).toBe(true);
  expect(new Set([SESSION, DIFF, DOCS, SPLIT]).size).toBe(4);
  expect(script).toContain('"athena-lazy-chunk:"');
  for (const s of [SESSION, DIFF, DOCS, SPLIT]) expect(script).toContain(`"${s}"`);
});
