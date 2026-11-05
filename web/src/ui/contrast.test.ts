import { readFileSync } from "node:fs";
import { join } from "node:path";
import { expect, test } from "vitest";
import { contrastRatio, normalizeHex, PAIRS, parseThemes } from "@/ui/contrast";

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
  // Ruling R59: the only colours beyond the tables, a success green and a warning amber.
  "--success": "#15803D",
  "--warning": "#B45309",
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
  "--success": "#4ADE80",
  "--warning": "#FBBF24",
};

test("tokens are exactly the Global Constraints values", () => {
  expect(themes.light).toEqual(LIGHT);
  expect(themes.dark).toEqual(DARK);
  // The no-JS default for a dark system is the same dark theme.
  expect(themes.systemDark).toEqual(DARK);
});

test("success and warning are paired on every surface they sit on", () => {
  for (const fg of ["--success", "--warning"]) {
    for (const bg of ["--bg", "--surface", "--surface-2"]) {
      expect(PAIRS.some((p) => p.fg === fg && p.bg === bg && p.kind === "text"), `${fg} on ${bg}`).toBe(true);
    }
  }
  expect(PAIRS.some((p) => p.fg === "--bg" && p.bg === "--success")).toBe(true);
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

test("spacing is a 4 px grid in pixels, not rem", () => {
  const theme = /@theme inline \{([\s\S]*?)\n\}/.exec(css)?.[1] ?? "";
  expect(theme).toMatch(/^\s*--spacing:\s*4px;/m);
});

test("normalizeHex expands minified colours", () => {
  expect(normalizeHex("#fff")).toBe("#FFFFFF");
  expect(normalizeHex(" #ff5a1f ")).toBe("#FF5A1F");
  expect(normalizeHex("rgb(0 0 0)")).toBeNull();
});
