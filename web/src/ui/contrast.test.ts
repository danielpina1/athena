import { readFileSync } from "node:fs";
import { join } from "node:path";
import { expect, test } from "vitest";
import { contrastRatio, PAIRS, parseThemes } from "@/ui/contrast";

// Read from disk: Vitest stubs CSS imports, ?raw included.
const css = readFileSync(join(import.meta.dirname, "tokens.css"), "utf8");

const themes = parseThemes(css);

// The Global Constraints tables, verbatim: tokens.css must not drift from them.
const LIGHT = {
  "--bg": "#FFFFFF",
  "--surface": "#FFFFFF",
  "--surface-2": "#F5F5F5",
  "--stage": "#E9E9E9",
  "--line": "#111111",
  "--line-soft": "#D9D9D9",
  "--text": "#0A0A0A",
  "--muted": "#6B6B6B",
  "--accent": "#FF5A1F",
  "--accent-fg": "#0A0A0A",
  "--accent-ink": "#C2410C",
  "--ok": "#0A0A0A",
  "--danger": "#B42318",
};
const DARK = {
  "--bg": "#0A0A0A",
  "--surface": "#0F0F0F",
  "--surface-2": "#1A1A1A",
  "--stage": "#000000",
  "--line": "#EDEDED",
  "--line-soft": "#2E2E2E",
  "--text": "#F5F5F5",
  "--muted": "#9A9A9A",
  "--accent": "#FF5A1F",
  "--accent-fg": "#0A0A0A",
  "--accent-ink": "#FF5A1F",
  "--ok": "#F5F5F5",
  "--danger": "#FF6B5E",
};

test("tokens are exactly the Global Constraints values", () => {
  expect(themes.light).toEqual(LIGHT);
  expect(themes.dark).toEqual(DARK);
  // The no-JS default for a dark system is the same dark theme.
  expect(themes.systemDark).toEqual(DARK);
});

test("WCAG math matches known ratios", () => {
  expect(contrastRatio("#FFFFFF", "#000000")).toBeCloseTo(21, 5);
  expect(contrastRatio("#777777", "#FFFFFF")).toBeCloseTo(4.48, 2);
});

for (const name of ["light", "dark"] as const) {
  test(`contrast: every text pair ≥ 4.5 and every line/indicator pair ≥ 3, ${name}`, () => {
    const t = themes[name];
    const failures: string[] = [];
    for (const p of PAIRS) {
      const ratio = contrastRatio(t[p.fg], t[p.bg]);
      const min = p.kind === "text" ? 4.5 : 3;
      if (ratio < min) failures.push(`${p.fg} on ${p.bg}: ${ratio.toFixed(2)} < ${min}`);
    }
    expect(failures).toEqual([]);
  });
}
