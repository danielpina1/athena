// WCAG 2.2 contrast for the Precision tokens: parses tokens.css and computes relative luminance. The contrast test
// and the /design page both use it, so the page shows the same numbers the test enforces.

export type Theme = Record<string, string>;

/** Reads the colour tokens of the light, dark and dark-by-system blocks of tokens.css. */
export function parseThemes(css: string): { light: Theme; dark: Theme; systemDark: Theme } {
  const block = (selector: RegExp): Theme => {
    const m = selector.exec(css);
    if (!m) throw new Error(`tokens.css: no block for ${selector}`);
    const body = css.slice(m.index + m[0].length, css.indexOf("}", m.index));
    const out: Theme = {};
    for (const [, name, value] of body.matchAll(/(--[a-z0-9-]+):\s*(#[0-9A-Fa-f]{6})\s*;/g)) out[name] = value;
    return out;
  };
  return {
    light: block(/^:root,\s*\[data-theme="light"\]\s*\{/m),
    dark: block(/^\[data-theme="dark"\]\s*\{/m),
    systemDark: block(/:root:not\(\[data-theme="light"\]\)\s*\{/),
  };
}

function luminance(hex: string): number {
  const n = Number.parseInt(hex.slice(1), 16);
  const lin = (c: number) => {
    const s = c / 255;
    return s <= 0.04045 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * lin((n >> 16) & 255) + 0.7152 * lin((n >> 8) & 255) + 0.0722 * lin(n & 255);
}

export function contrastRatio(a: string, b: string): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

/** The pairs the system uses: text pairs need 4.5:1, hairlines and indicators 3:1. */
export const PAIRS: { fg: string; bg: string; kind: "text" | "line" }[] = [
  { fg: "--text", bg: "--bg", kind: "text" },
  { fg: "--text", bg: "--surface", kind: "text" },
  { fg: "--text", bg: "--surface-2", kind: "text" },
  { fg: "--muted", bg: "--bg", kind: "text" },
  { fg: "--muted", bg: "--surface", kind: "text" },
  { fg: "--muted", bg: "--surface-2", kind: "text" },
  { fg: "--accent-fg", bg: "--accent", kind: "text" },
  { fg: "--accent-ink", bg: "--bg", kind: "text" },
  { fg: "--accent-ink", bg: "--surface-2", kind: "text" },
  { fg: "--danger", bg: "--bg", kind: "text" },
  { fg: "--danger", bg: "--surface-2", kind: "text" },
  { fg: "--bg", bg: "--ok", kind: "text" },
  { fg: "--bg", bg: "--text", kind: "text" },
  { fg: "--line", bg: "--bg", kind: "line" },
  { fg: "--line", bg: "--surface", kind: "line" },
  { fg: "--accent", bg: "--bg", kind: "line" },
  { fg: "--accent", bg: "--surface", kind: "line" },
  { fg: "--danger", bg: "--surface", kind: "line" },
];
