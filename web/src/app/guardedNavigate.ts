// Navigation that a screen with unsaved changes may stop to ask first. Links are caught by Settings' own click guard;
// the palette and the g shortcuts navigate in code, so they go through here. Browser Back is not covered: the router
// has already moved when it hears of it.
import type { NavigateFunction } from "react-router";

/** Answers true when it took over the navigation (it will call go itself, or not at all). */
export type NavigationGuard = (to: string, go: () => void) => boolean;

let guard: NavigationGuard | undefined;

/** Installs the guard; returns its removal. One at a time: the latest wins. */
export function setNavigationGuard(g: NavigationGuard): () => void {
  guard = g;
  return () => {
    if (guard === g) guard = undefined;
  };
}

export function guardedNavigate(navigate: NavigateFunction, to: string): void {
  const go = () => void navigate(to);
  if (guard?.(to, go)) return;
  go();
}
