// The owner's theme choice: stored as athena.theme, applied as data-theme on <html>. A blocking init script is not
// allowed under the CSP, so main.tsx calls initTheme() before the first render, and tokens.css covers the moment
// before that with a prefers-color-scheme default.
import { useSyncExternalStore } from "react";

export type ThemeChoice = "system" | "light" | "dark";
export type ResolvedTheme = "light" | "dark";

const KEY = "athena.theme";
const QUERY = "(prefers-color-scheme: dark)";

type State = { theme: ThemeChoice; resolved: ResolvedTheme };

let state: State = { theme: "system", resolved: "light" };
const listeners = new Set<() => void>();
let media: MediaQueryList | undefined;

const isChoice = (v: unknown): v is ThemeChoice => v === "system" || v === "light" || v === "dark";

function readStored(): ThemeChoice {
  try {
    const v = localStorage.getItem(KEY);
    return isChoice(v) ? v : "system";
  } catch {
    return "system";
  }
}

const systemDark = () => media?.matches ?? false;

function apply(theme: ThemeChoice) {
  const resolved: ResolvedTheme = theme === "system" ? (systemDark() ? "dark" : "light") : theme;
  document.documentElement.dataset.theme = resolved;
  if (state.theme !== theme || state.resolved !== resolved) {
    state = { theme, resolved };
    for (const l of listeners) l();
  }
}

const onSystemChange = () => {
  if (state.theme === "system") apply("system");
};

/** Reads the stored choice and applies it; call once before the first render. */
export function initTheme() {
  media?.removeEventListener("change", onSystemChange);
  media = typeof window.matchMedia === "function" ? window.matchMedia(QUERY) : undefined;
  media?.addEventListener("change", onSystemChange);
  apply(readStored());
}

export function setTheme(theme: ThemeChoice) {
  try {
    localStorage.setItem(KEY, theme);
  } catch {
    // Storage can be unavailable (private mode); the choice still applies for this visit.
  }
  apply(theme);
}

const subscribe = (l: () => void) => {
  listeners.add(l);
  return () => listeners.delete(l);
};
const snapshot = () => state;

export function useTheme(): State & { setTheme: (t: ThemeChoice) => void } {
  const s = useSyncExternalStore(subscribe, snapshot);
  return { ...s, setTheme };
}
