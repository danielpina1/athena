import { readFileSync } from "node:fs";
import { join } from "node:path";
import { expect, test } from "vitest";

// Every animation is off under prefers-reduced-motion. The motion lives in CSS only (index.css); the global rule in
// tokens.css turns every animation and transition off, on every element and pseudo-element, with !important.
const tokens = readFileSync(join(import.meta.dirname, "tokens.css"), "utf8");
const index = readFileSync(join(import.meta.dirname, "..", "index.css"), "utf8");

test("reduced motion turns every animation off", () => {
  const block = /@media \(prefers-reduced-motion: reduce\) \{([\s\S]*?)\n\}/.exec(tokens)?.[1] ?? "";
  expect(block).toMatch(/\*,\s*::before,\s*::after\s*\{/);
  expect(block).toContain("animation: none !important");
  expect(block).toContain("transition: none !important");
});

test("the motion classes animate only through animation, so the rule makes them inert", () => {
  for (const name of ["motion-wash", "motion-pop", "motion-glow", "athena-ring"]) {
    expect(index, name).toContain(`.${name}`);
  }
  // No motion is driven any other way the rule would miss: no transition-based loops and no JS timers in CSS.
  expect(index).not.toMatch(/transition\s*:/);
  // What a still element shows is the resting state: a ring and a glow are invisible without their animation.
  expect(/\.athena-ring::after\s*\{[^}]*opacity:\s*0/.test(index)).toBe(true);
  expect(/\.motion-glow\s*\{[^}]*opacity:\s*0/.test(index)).toBe(true);
});
