import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { initTheme, useTheme } from "@/ui/theme";

type Listener = (e: { matches: boolean }) => void;
let systemDark = false;
const listeners = new Set<Listener>();

beforeEach(() => {
  localStorage.clear();
  document.documentElement.removeAttribute("data-theme");
  systemDark = false;
  listeners.clear();
  vi.stubGlobal("matchMedia", (query: string) => ({
    media: query,
    get matches() {
      return systemDark;
    },
    addEventListener: (_: string, l: Listener) => listeners.add(l),
    removeEventListener: (_: string, l: Listener) => listeners.delete(l),
  }));
});

afterEach(() => vi.unstubAllGlobals());

const flipSystem = (dark: boolean) => {
  systemDark = dark;
  for (const l of listeners) l({ matches: dark });
};

test("theme persists and resolves system", () => {
  initTheme();
  const { result } = renderHook(() => useTheme());
  expect(result.current.theme).toBe("system");
  expect(result.current.resolved).toBe("light");
  expect(document.documentElement.dataset.theme).toBe("light");

  act(() => flipSystem(true));
  expect(result.current.resolved).toBe("dark");
  expect(document.documentElement.dataset.theme).toBe("dark");

  act(() => result.current.setTheme("light"));
  expect(localStorage.getItem("athena.theme")).toBe("light");
  expect(result.current).toMatchObject({ theme: "light", resolved: "light" });
  expect(document.documentElement.dataset.theme).toBe("light");

  act(() => result.current.setTheme("dark"));
  expect(localStorage.getItem("athena.theme")).toBe("dark");
  expect(document.documentElement.dataset.theme).toBe("dark");

  act(() => result.current.setTheme("system"));
  expect(localStorage.getItem("athena.theme")).toBe("system");
  expect(result.current.resolved).toBe("dark");
});

test("a stored choice applies before the first render, and junk is ignored", () => {
  localStorage.setItem("athena.theme", "dark");
  initTheme();
  expect(document.documentElement.dataset.theme).toBe("dark");

  localStorage.setItem("athena.theme", "purple");
  initTheme();
  expect(document.documentElement.dataset.theme).toBe("light");
});
